package microsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type capacityReservation struct {
	vcpus      uint32
	memory     uint64
	disk       uint64
	instances  uint32
	operations uint32
}

type activeAssignment struct {
	fence          *runnerprotocol.AssignmentFence
	correlation    *runnerprotocol.Correlation
	process        *helperProcess
	reservation    capacityReservation
	backendRef     string
	readyPublished bool
	fenced         bool
	terminalSent   bool
	operations     map[uint64]context.CancelFunc
	nextOperation  uint64
}

// AssignmentBackend composes one local helper process per fenced Instance.
type AssignmentBackend struct {
	config            validatedConfig
	mu                sync.Mutex
	assignments       map[string]*activeAssignment
	reserved          capacityReservation
	instanceTerminals chan runnercontrol.BackendInstanceTerminal
	startupSamples    []time.Duration
}

// NewAssignmentBackend validates all immutable local inputs before the backend can advertise.
func NewAssignmentBackend(config Config) (*AssignmentBackend, error) {
	validated, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	return &AssignmentBackend{
		config:            validated,
		assignments:       make(map[string]*activeAssignment),
		instanceTerminals: make(chan runnercontrol.BackendInstanceTerminal, config.MaximumInstances),
	}, nil
}

func (backend *AssignmentBackend) InstanceTerminals() <-chan runnercontrol.BackendInstanceTerminal {
	return backend.instanceTerminals
}

func (backend *AssignmentBackend) StartupTiming() (uint64, time.Duration) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.startupSamples) == 0 {
		return 0, 0
	}
	values := slices.Clone(backend.startupSamples)
	slices.Sort(values)
	index := (len(values)*95 + 99) / 100
	return uint64(len(values)), values[index-1]
}

// Readiness proves local KVM, immutable assets, exact materialization, and integer capacity.
func (backend *AssignmentBackend) Readiness(context.Context) (runnercontrol.BackendReadiness, error) {
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Microsandbox readiness KVM: %w", err)
	}
	if err := kvm.Close(); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Microsandbox readiness close KVM: %w", err)
	}
	backend.mu.Lock()
	reserved := backend.reserved
	backend.mu.Unlock()
	manifest := backend.config.manifest
	return runnercontrol.BackendReadiness{
		Architecture: runtime.GOARCH,
		Capacity: &runnerprotocol.Capacity{
			VcpuCount:   backend.config.MaximumVCPUs,
			MemoryBytes: backend.config.MaximumMemoryBytes,
			DiskBytes:   backend.config.MaximumDiskBytes,
			Instances:   backend.config.MaximumInstances,
			Operations:  backend.config.MaximumOperations,
		},
		Reserved: &runnerprotocol.Capacity{
			VcpuCount:   reserved.vcpus,
			MemoryBytes: reserved.memory,
			DiskBytes:   reserved.disk,
			Instances:   reserved.instances,
			Operations:  reserved.operations,
		},
		Capabilities: &runnerprotocol.RunnerCapabilities{
			Architecture:          runtime.GOARCH,
			ComputeBackendVersion: manifest.BackendBuildID,
			HypervisorReady:       true,
			IsolationReady:        true,
			NetworkPolicyReady:    true,
			StorageReady:          true,
			CleanupReady:          true,
			GuestProtocolGenerations: &runnerprotocol.ProtocolVersionRange{
				Minimum: manifest.AgentProtocolGeneration,
				Maximum: manifest.AgentProtocolGeneration,
			},
			SnapshotResumeReady: false,
		},
		BackendKind: runnerprotocol.ComputeBackendKind_COMPUTE_BACKEND_KIND_MICROSANDBOX,
		Materializations: []*runnerprotocol.BackendMaterializationEvidence{{
			SchemaVersion:           1,
			BackendKind:             runnerprotocol.ComputeBackendKind_COMPUTE_BACKEND_KIND_MICROSANDBOX,
			GuestArchitecture:       manifest.Key.GuestArchitecture,
			RuntimeManifestDigest:   manifest.Key.RuntimeManifestDigest,
			ToolchainManifestDigest: manifest.Key.ToolchainManifestDigest,
			MaterializationDigest:   backend.config.MaterializationDigest,
			SourceOciManifestDigest: manifest.SourceOCIManifestDigest,
			FlatRootDigest:          manifest.FlatRootDigest,
			AgentProtocolGeneration: manifest.AgentProtocolGeneration,
			AgentFeatures:           slices.Clone(manifest.AgentFeatures),
			BackendBuildId:          manifest.BackendBuildID,
			HelperBuildId:           manifest.HelperBuildID,
			VerifiedAtUnixMs:        uint64(time.Now().UTC().UnixMilli()),
		}},
	}, nil
}

func (backend *AssignmentBackend) ValidateAssignment(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if assignment == nil || assignment.Fence == nil || assignment.Requirements == nil || assignment.Correlation == nil {
		return fmt.Errorf("SecondBox Microsandbox assignment is incomplete")
	}
	if strings.TrimSpace(assignment.WorkspaceId) == "" || !completeFence(assignment.Fence) {
		return fmt.Errorf("SecondBox Microsandbox assignment Workspace or fence identity is incomplete")
	}
	if assignment.DeadlineUnixMs == 0 || time.Now().UnixMilli() >= int64(assignment.DeadlineUnixMs) {
		return fmt.Errorf("SecondBox Microsandbox assignment deadline has expired")
	}
	requirements := assignment.Requirements
	const mib = uint64(1 << 20)
	if requirements.VcpuCount == 0 || requirements.MemoryBytes == 0 || requirements.DiskBytes == 0 ||
		requirements.MemoryBytes%mib != 0 || requirements.DiskBytes%mib != 0 {
		return fmt.Errorf("SecondBox Microsandbox assignment requires whole-MiB nonzero resources")
	}
	if requirements.Architecture != runtime.GOARCH || requirements.StartupMode != "cold_boot" {
		return fmt.Errorf("SecondBox Microsandbox assignment architecture or startup mode is unsupported")
	}
	if requirements.VcpuCount > backend.config.MaximumVCPUs ||
		requirements.MemoryBytes > backend.config.MaximumMemoryBytes ||
		requirements.DiskBytes > backend.config.MaximumDiskBytes {
		return fmt.Errorf("SecondBox Microsandbox assignment exceeds immutable local capacity")
	}
	supported := map[string]bool{
		"cleanup": true, "evidence": true, "kvm": true, "local-workspace": true,
		"microsandbox": true, "network-policy": true, "storage": true,
	}
	for _, capability := range requirements.RequiredCapabilities {
		if !supported[capability] {
			return fmt.Errorf("SecondBox Microsandbox assignment requires unsupported capability %q", capability)
		}
	}
	if err := backend.validateAssignmentMaterialization(assignment); err != nil {
		return err
	}
	if _, err := translateNetworkPolicy(assignment.NetworkPolicy); err != nil {
		return err
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if active, exists := backend.assignments[assignment.Fence.AssignmentId]; exists {
		if sameFence(active.fence, assignment.Fence) {
			return nil
		}
		return fmt.Errorf("SecondBox Microsandbox assignment ID was reused with different fencing")
	}
	for _, active := range backend.assignments {
		if active.fence.SandboxId == assignment.Fence.SandboxId && !active.fenced {
			return fmt.Errorf("SecondBox Microsandbox Sandbox already has an unfenced assignment")
		}
	}
	return nil
}

func (backend *AssignmentBackend) StartAssignment(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (result runnercontrol.BackendInstance, resultErr error) {
	started := time.Now()
	if err := backend.ValidateAssignment(ctx, assignment); err != nil {
		return result, err
	}
	reservation := capacityReservation{
		vcpus: assignment.Requirements.VcpuCount, memory: assignment.Requirements.MemoryBytes,
		disk: assignment.Requirements.DiskBytes, instances: 1,
	}
	if err := backend.reserve(reservation); err != nil {
		return result, err
	}
	reserved := true
	defer func() {
		if resultErr != nil && reserved {
			backend.release(reservation)
		}
	}()
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
		return result, err
	}
	workspace, err := backend.config.WorkspaceStore.Open(ctx, assignment.WorkspaceId, assignment.Fence.SandboxGeneration)
	if err != nil {
		return result, fmt.Errorf("SecondBox Microsandbox resolve Workspace attachment: %w", err)
	}
	workspaceOwned := true
	defer func() {
		if resultErr != nil && workspaceOwned {
			resultErr = errors.Join(resultErr, workspace.Close())
		}
	}()
	if uint64(workspace.CapacityBytes()) != assignment.Requirements.DiskBytes {
		return result, fmt.Errorf("SecondBox Microsandbox Workspace capacity differs from immutable Profile")
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_WORKSPACE_ATTACH); err != nil {
		return result, err
	}
	if _, err := translateNetworkPolicy(assignment.NetworkPolicy); err != nil {
		return result, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_NETWORK_SETUP); err != nil {
		return result, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_COMPUTE_LAUNCH); err != nil {
		return result, err
	}
	process, _, err := launchHelper(ctx, backend.config, assignment, workspace)
	if err != nil {
		return result, err
	}
	workspaceOwned = false
	cleanupProcess := true
	defer func() {
		if resultErr != nil && cleanupProcess {
			process.forceStop()
		}
	}()
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_GUEST_NEGOTIATION); err != nil {
		return result, err
	}
	active := &activeAssignment{
		fence:         cloneFence(assignment.Fence),
		correlation:   proto.Clone(assignment.Correlation).(*runnerprotocol.Correlation),
		process:       process,
		reservation:   reservation,
		backendRef:    fmt.Sprintf("microsandbox:%d", process.command.Process.Pid),
		operations:    make(map[uint64]context.CancelFunc),
		nextOperation: 1,
	}
	backend.mu.Lock()
	backend.assignments[assignment.Fence.AssignmentId] = active
	backend.startupSamples = append(backend.startupSamples, time.Since(started))
	backend.mu.Unlock()
	go backend.observeExit(active)
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY); err != nil {
		backend.mu.Lock()
		delete(backend.assignments, assignment.Fence.AssignmentId)
		backend.mu.Unlock()
		return result, err
	}
	cleanupProcess = false
	reserved = false
	return runnercontrol.BackendInstance{BackendKind: "microsandbox", BackendReference: active.backendRef}, nil
}

func (backend *AssignmentBackend) MarkAssignmentReady(fence *runnerprotocol.AssignmentFence) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	active, exists := backend.assignments[fence.GetAssignmentId()]
	if !exists || !sameFence(active.fence, fence) || active.fenced {
		return fmt.Errorf("SecondBox Microsandbox ready assignment fence is stale")
	}
	active.readyPublished = true
	return nil
}

func (backend *AssignmentBackend) observeExit(active *activeAssignment) {
	<-active.process.done
	backend.mu.Lock()
	current, exists := backend.assignments[active.fence.AssignmentId]
	if !exists || current != active || active.fenced || !active.readyPublished || active.terminalSent {
		backend.mu.Unlock()
		return
	}
	active.terminalSent = true
	digest := helperTerminalDigest(active)
	terminal := runnercontrol.BackendInstanceTerminal{
		Fence:          cloneFence(active.fence),
		Correlation:    proto.Clone(active.correlation).(*runnerprotocol.Correlation),
		Reason:         runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE,
		ObservedAt:     time.Now().UTC(),
		EvidenceDigest: digest,
	}
	backend.mu.Unlock()
	backend.instanceTerminals <- terminal
}

func (backend *AssignmentBackend) FenceAssignment(
	ctx context.Context,
	command *runnerprotocol.FenceCommand,
) (runnercontrol.FenceEvidence, error) {
	if command == nil || command.Fence == nil {
		return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox Microsandbox fence identity is required")
	}
	backend.mu.Lock()
	active, exists := backend.assignments[command.Fence.AssignmentId]
	if !exists {
		backend.mu.Unlock()
		return runnercontrol.FenceEvidence{
			Result:                    runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED,
			TerminationEvidenceDigest: fenceDigest(command.Fence),
		}, nil
	}
	if !sameFence(active.fence, command.Fence) {
		backend.mu.Unlock()
		return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox Microsandbox fence token or generation mismatch")
	}
	active.fenced = true
	cancels := make([]context.CancelFunc, 0, len(active.operations))
	for _, cancel := range active.operations {
		cancels = append(cancels, cancel)
	}
	backend.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	deadline := time.UnixMilli(int64(command.DeadlineUnixMs))
	if command.DeadlineUnixMs == 0 || deadline.Before(time.Now()) {
		deadline = time.Now().Add(10 * time.Second)
	}
	shutdownCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var err error
	select {
	case <-active.process.done:
		err = active.process.closeResources()
	default:
		err = active.process.shutdown(shutdownCtx)
	}
	backend.mu.Lock()
	delete(backend.assignments, command.Fence.AssignmentId)
	backend.releaseLocked(active.reservation)
	backend.mu.Unlock()
	if err != nil {
		return runnercontrol.FenceEvidence{}, err
	}
	return runnercontrol.FenceEvidence{
		Result:                    runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
		TerminationEvidenceDigest: fenceDigest(command.Fence),
	}, nil
}

// Shutdown fences every locally owned helper without adopting work after restart.
func (backend *AssignmentBackend) Shutdown(ctx context.Context) error {
	backend.mu.Lock()
	fences := make([]*runnerprotocol.AssignmentFence, 0, len(backend.assignments))
	for _, active := range backend.assignments {
		fences = append(fences, cloneFence(active.fence))
	}
	backend.mu.Unlock()
	var result error
	for _, fence := range fences {
		_, err := backend.FenceAssignment(ctx, &runnerprotocol.FenceCommand{
			Fence: fence, DeadlineUnixMs: uint64(time.Now().Add(10 * time.Second).UnixMilli()),
		})
		result = errors.Join(result, err)
	}
	return result
}

func (backend *AssignmentBackend) reserve(request capacityReservation) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.reserved.vcpus+request.vcpus > backend.config.MaximumVCPUs ||
		backend.reserved.memory+request.memory > backend.config.MaximumMemoryBytes ||
		backend.reserved.disk+request.disk > backend.config.MaximumDiskBytes ||
		backend.reserved.instances+request.instances > backend.config.MaximumInstances {
		return fmt.Errorf("SecondBox Microsandbox assignment capacity is unavailable")
	}
	backend.reserved.vcpus += request.vcpus
	backend.reserved.memory += request.memory
	backend.reserved.disk += request.disk
	backend.reserved.instances += request.instances
	return nil
}

func (backend *AssignmentBackend) release(request capacityReservation) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.releaseLocked(request)
}

func (backend *AssignmentBackend) releaseLocked(request capacityReservation) {
	backend.reserved.vcpus -= request.vcpus
	backend.reserved.memory -= request.memory
	backend.reserved.disk -= request.disk
	backend.reserved.instances -= request.instances
	backend.reserved.operations -= request.operations
}

func (backend *AssignmentBackend) acquireOperation(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
) (*activeAssignment, context.Context, func(), error) {
	backend.mu.Lock()
	active := backend.assignments[fence.GetAssignmentId()]
	if active == nil || active.fenced || !sameFence(active.fence, fence) {
		backend.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("SecondBox Microsandbox operation fence is stale")
	}
	if backend.reserved.operations >= backend.config.MaximumOperations {
		backend.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("SecondBox Microsandbox operation capacity is unavailable")
	}
	opCtx, cancel := context.WithCancel(ctx)
	id := active.nextOperation
	active.nextOperation++
	active.operations[id] = cancel
	backend.reserved.operations++
	backend.mu.Unlock()
	released := false
	release := func() {
		backend.mu.Lock()
		if !released {
			released = true
			delete(active.operations, id)
			backend.reserved.operations--
		}
		backend.mu.Unlock()
		cancel()
	}
	return active, opCtx, release, nil
}

func (backend *AssignmentBackend) operationFenceActive(active *activeAssignment, fence *runnerprotocol.AssignmentFence) bool {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.assignments[fence.GetAssignmentId()] == active && !active.fenced && sameFence(active.fence, fence)
}

func (backend *AssignmentBackend) validateAssignmentMaterialization(assignment *runnerprotocol.AssignmentCommand) error {
	manifest := backend.config.manifest
	if len(assignment.Assets) != 2 {
		return fmt.Errorf("SecondBox Microsandbox assignment must select exactly runtime and toolchain assets")
	}
	expected := map[string]bool{
		manifest.Key.RuntimeManifestDigest:   false,
		manifest.Key.ToolchainManifestDigest: false,
	}
	for _, asset := range assignment.Assets {
		if asset == nil || asset.Architecture != manifest.Key.GuestArchitecture ||
			asset.GuestProtocolGeneration != manifest.AgentProtocolGeneration {
			return fmt.Errorf("SecondBox Microsandbox assignment asset compatibility differs from the materialization")
		}
		if _, exists := expected[asset.ManifestDigest]; !exists || expected[asset.ManifestDigest] {
			return fmt.Errorf("SecondBox Microsandbox assignment asset digest differs from the materialization")
		}
		expected[asset.ManifestDigest] = true
		for _, feature := range asset.MandatoryGuestFeatures {
			if !slices.Contains(manifest.AgentFeatures, feature) {
				return fmt.Errorf("SecondBox Microsandbox assignment requires unsupported agent feature %q", feature)
			}
		}
	}
	return nil
}

func completeFence(fence *runnerprotocol.AssignmentFence) bool {
	return strings.TrimSpace(fence.AssignmentId) != "" && strings.TrimSpace(fence.SandboxId) != "" &&
		strings.TrimSpace(fence.InstanceId) != "" && fence.SandboxGeneration != 0 && len(fence.FencingToken) != 0
}

func sameFence(left, right *runnerprotocol.AssignmentFence) bool {
	return left != nil && right != nil && left.AssignmentId == right.AssignmentId &&
		left.SandboxId == right.SandboxId && left.InstanceId == right.InstanceId &&
		left.SandboxGeneration == right.SandboxGeneration && bytes.Equal(left.FencingToken, right.FencingToken)
}

func cloneFence(fence *runnerprotocol.AssignmentFence) *runnerprotocol.AssignmentFence {
	if fence == nil {
		return nil
	}
	return &runnerprotocol.AssignmentFence{
		AssignmentId: fence.AssignmentId, SandboxId: fence.SandboxId, InstanceId: fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration, FencingToken: bytes.Clone(fence.FencingToken),
	}
}

func fenceDigest(fence *runnerprotocol.AssignmentFence) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", fence.AssignmentId, fence.InstanceId, fence.SandboxGeneration)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func helperTerminalDigest(active *activeAssignment) string {
	value := fmt.Sprintf("%s\x00%s\x00%s", active.backendRef, active.process.stderr.String(), active.process.processWaitError())
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var _ runnercontrol.AssignmentBackend = (*AssignmentBackend)(nil)
var _ = materialization.BackendMicrosandbox
