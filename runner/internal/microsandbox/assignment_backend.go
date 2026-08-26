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
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	"github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
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
	// launched closes when the claimed start finishes (successfully
	// registered or removed after failure); nil on a completed assignment.
	launched   chan struct{}
	launchDone func()
	readyPublished bool
	fenced         bool
	terminalSent   bool
	operations     map[uint64]context.CancelFunc
	nextOperation  uint64
	ready          *microsandboxprotocol.ReadyEvent
	evidenceErr    error
}

// AssignmentBackend composes one local helper process per fenced Instance.
type AssignmentBackend struct {
	config            validatedConfig
	mu                sync.Mutex
	assignments       map[string]*activeAssignment
	reserved          capacityReservation
	instanceTerminals chan runnercontrol.BackendInstanceTerminal
	startupSamples    []time.Duration
	evidence          runnerevidence.Sink
	runnerID          string
}

type BackendDimensions struct {
	BackendKind  string
	HostPlatform string
}

type MetricsSnapshot struct {
	Dimensions       BackendDimensions
	ActiveInstances  uint32
	ActiveOperations uint32
	ColdStartCount   uint64
	ColdStartP95     time.Duration
}

type cleanupStack struct {
	steps []func() error
	armed []bool
}

type assignmentFailure struct {
	decision runnerprotocol.AssignmentDecision
	terminal runnerprotocol.AssignmentTerminalKind
	cause    error
}

func (failure assignmentFailure) Error() string { return failure.cause.Error() }
func (failure assignmentFailure) Unwrap() error { return failure.cause }
func (failure assignmentFailure) AssignmentDecision() runnerprotocol.AssignmentDecision {
	return failure.decision
}
func (failure assignmentFailure) AssignmentTerminal() runnerprotocol.AssignmentTerminalKind {
	return failure.terminal
}

func incompatibleAssignment(cause error) error {
	return assignmentFailure{
		decision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_INCOMPATIBLE_PROFILE,
		terminal: runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_ADMISSION_FAILED,
		cause:    cause,
	}
}

func capacityAssignment(cause error) error {
	return assignmentFailure{
		decision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_CAPACITY,
		terminal: runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_ADMISSION_FAILED,
		cause:    cause,
	}
}

func artifactAssignment(cause error) error {
	return assignmentFailure{
		decision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_ARTIFACT,
		terminal: runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_ADMISSION_FAILED,
		cause:    cause,
	}
}

func infrastructureAssignment(cause error) error {
	return assignmentFailure{
		decision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_PREREQUISITE,
		terminal: runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_INFRASTRUCTURE_FAILED,
		cause:    cause,
	}
}

func (stack *cleanupStack) push(step func() error) int {
	stack.steps = append(stack.steps, step)
	stack.armed = append(stack.armed, true)
	return len(stack.steps) - 1
}

func (stack *cleanupStack) disarm(index int) {
	stack.armed[index] = false
}

func (stack *cleanupStack) clear() {
	for index := range stack.armed {
		stack.armed[index] = false
	}
}

func (stack *cleanupStack) run() error {
	var result error
	for index := len(stack.steps) - 1; index >= 0; index-- {
		if stack.armed[index] {
			result = errors.Join(result, stack.steps[index]())
			stack.armed[index] = false
		}
	}
	return result
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

func (backend *AssignmentBackend) SetRunnerEvidenceSink(sink runnerevidence.Sink, runnerID string) {
	if sink == nil || strings.TrimSpace(runnerID) == "" {
		return
	}
	backend.mu.Lock()
	backend.evidence = sink
	backend.runnerID = strings.TrimSpace(runnerID)
	backend.mu.Unlock()
}

func (backend *AssignmentBackend) DiagnosticDimensions() BackendDimensions {
	return BackendDimensions{BackendKind: "microsandbox", HostPlatform: microsandboxHostPlatform()}
}

func microsandboxHostPlatform() string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	architecture := runtime.GOARCH
	switch architecture {
	case "amd64":
		architecture = "x86_64"
	case "arm64":
		architecture = "aarch64"
	}
	return osName + "-" + architecture
}

func (backend *AssignmentBackend) MetricsSnapshot() MetricsSnapshot {
	count, p95 := backend.StartupTiming()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return MetricsSnapshot{
		Dimensions: backend.DiagnosticDimensions(), ActiveInstances: backend.reserved.instances,
		ActiveOperations: backend.reserved.operations, ColdStartCount: count, ColdStartP95: p95,
	}
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

// Readiness proves the host hypervisor boundary, immutable assets, exact materialization, and
// integer capacity for the selected build target.
func (backend *AssignmentBackend) Readiness(ctx context.Context) (runnercontrol.BackendReadiness, error) {
	if _, err := validateConfig(backend.config.Config); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Microsandbox readiness materialization: %w", err)
	}
	if err := platformReadiness(ctx, backend.config); err != nil {
		return runnercontrol.BackendReadiness{}, err
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
			ResourceLimitsReady:   true,
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
	return backend.validateAssignmentClaimed(ctx, assignment, nil)
}

func (backend *AssignmentBackend) validateAssignmentClaimed(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	ownClaim *activeAssignment,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if assignment == nil || assignment.Fence == nil || assignment.Requirements == nil || assignment.Correlation == nil {
		return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment is incomplete"))
	}
	if strings.TrimSpace(assignment.WorkspaceId) == "" || !completeFence(assignment.Fence) {
		return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment Workspace or fence identity is incomplete"))
	}
	// A replay of the active fence is valid regardless of the original
	// deadline: the Instance already exists, so validation short-circuits
	// before any expiry or capacity reasoning about a new launch.
	backend.mu.Lock()
	if active, exists := backend.assignments[assignment.Fence.AssignmentId]; exists && active != ownClaim {
		defer backend.mu.Unlock()
		if sameFence(active.fence, assignment.Fence) {
			return nil
		}
		return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment ID was reused with different fencing"))
	}
	backend.mu.Unlock()
	if assignment.DeadlineUnixMs == 0 || time.Now().UnixMilli() >= int64(assignment.DeadlineUnixMs) {
		return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment deadline has expired"))
	}
	requirements := assignment.Requirements
	const mib = uint64(1 << 20)
	if requirements.VcpuCount == 0 || requirements.MemoryBytes == 0 || requirements.DiskBytes == 0 ||
		requirements.MemoryBytes%mib != 0 || requirements.DiskBytes%mib != 0 {
		return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment requires whole-MiB nonzero resources"))
	}
	if requirements.Architecture != runtime.GOARCH || requirements.StartupMode != "cold_boot" {
		return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment architecture or startup mode is unsupported"))
	}
	if requirements.VcpuCount > backend.config.MaximumVCPUs ||
		requirements.MemoryBytes > backend.config.MaximumMemoryBytes ||
		requirements.DiskBytes > backend.config.MaximumDiskBytes {
		return capacityAssignment(fmt.Errorf("SecondBox Microsandbox assignment exceeds immutable local capacity"))
	}
	supported := map[string]bool{
		"cleanup": true, "evidence": true, "kvm": true, "local-workspace": true,
		"microsandbox": true, "network-policy": true, "storage": true,
	}
	for _, capability := range requirements.RequiredCapabilities {
		if !supported[capability] {
			return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment requires unsupported capability %q", capability))
		}
	}
	if err := backend.validateAssignmentMaterialization(assignment); err != nil {
		return artifactAssignment(err)
	}
	if _, err := validateConfig(backend.config.Config); err != nil {
		return artifactAssignment(fmt.Errorf("SecondBox Microsandbox revalidate local materialization: %w", err))
	}
	if _, err := translateNetworkPolicy(assignment.NetworkPolicy); err != nil {
		return incompatibleAssignment(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for _, active := range backend.assignments {
		if active != ownClaim && active.fence.SandboxId == assignment.Fence.SandboxId && !active.fenced {
			return incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox Sandbox already has an unfenced assignment"))
		}
	}
	if backend.reserved.vcpus+requirements.VcpuCount > backend.config.MaximumVCPUs ||
		backend.reserved.memory+requirements.MemoryBytes > backend.config.MaximumMemoryBytes ||
		backend.reserved.disk+requirements.DiskBytes > backend.config.MaximumDiskBytes ||
		backend.reserved.instances+1 > backend.config.MaximumInstances {
		return capacityAssignment(fmt.Errorf("SecondBox Microsandbox assignment capacity is unavailable"))
	}
	return nil
}

func (backend *AssignmentBackend) StartAssignment(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (result runnercontrol.BackendInstance, resultErr error) {
	started := time.Now()
	if assignment == nil || assignment.Fence == nil || !completeFence(assignment.Fence) {
		return result, incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment fence identity is incomplete"))
	}
	// At-least-once command delivery replays starts, and identical starts can
	// arrive concurrently. The assignment is claimed atomically before any
	// validation: a replay of the active fence returns the existing backend
	// reference even past the original deadline, and a concurrent identical
	// start waits for the claimed launch instead of launching a second helper.
	assignmentID := assignment.Fence.AssignmentId
	backend.mu.Lock()
	if existing, exists := backend.assignments[assignmentID]; exists {
		if !sameFence(existing.fence, assignment.Fence) {
			backend.mu.Unlock()
			return result, incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox assignment ID was reused with different fencing"))
		}
		pendingLaunch := existing.launched
		reference := existing.backendRef
		backend.mu.Unlock()
		if pendingLaunch != nil {
			select {
			case <-pendingLaunch:
			case <-ctx.Done():
				return result, ctx.Err()
			}
			backend.mu.Lock()
			reference = ""
			if current, still := backend.assignments[assignmentID]; still && sameFence(current.fence, assignment.Fence) {
				reference = current.backendRef
			}
			backend.mu.Unlock()
		}
		if reference != "" {
			return runnercontrol.BackendInstance{BackendKind: "microsandbox", BackendReference: reference}, nil
		}
		return result, infrastructureAssignment(fmt.Errorf("SecondBox Microsandbox replayed start observed a failed launch"))
	}
	for _, active := range backend.assignments {
		if active.fence.SandboxId == assignment.Fence.SandboxId && !active.fenced {
			backend.mu.Unlock()
			return result, incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox Sandbox already has an unfenced assignment"))
		}
	}
	claim := &activeAssignment{fence: cloneFence(assignment.Fence), launched: make(chan struct{})}
	claim.launchDone = sync.OnceFunc(func() { close(claim.launched) })
	backend.assignments[assignmentID] = claim
	backend.mu.Unlock()
	defer func() {
		if resultErr != nil {
			backend.mu.Lock()
			if backend.assignments[assignmentID] == claim {
				delete(backend.assignments, assignmentID)
			}
			backend.mu.Unlock()
		}
		claim.launchDone()
	}()
	if err := backend.validateAssignmentClaimed(ctx, assignment, claim); err != nil {
		return result, err
	}
	reservation := capacityReservation{
		vcpus: assignment.Requirements.VcpuCount, memory: assignment.Requirements.MemoryBytes,
		disk: assignment.Requirements.DiskBytes, instances: 1,
	}
	if err := backend.reserve(reservation); err != nil {
		return result, capacityAssignment(err)
	}
	cleanup := &cleanupStack{}
	cleanup.push(func() error {
		backend.release(reservation)
		return nil
	})
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, cleanup.run())
		}
	}()
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
		return result, err
	}
	workspace, err := backend.config.WorkspaceStore.Open(ctx, assignment.WorkspaceId, assignment.Fence.SandboxGeneration)
	if err != nil {
		return result, infrastructureAssignment(fmt.Errorf("SecondBox Microsandbox resolve Workspace attachment: %w", err))
	}
	workspaceCleanup := cleanup.push(workspace.Close)
	if uint64(workspace.CapacityBytes()) != assignment.Requirements.DiskBytes {
		return result, incompatibleAssignment(fmt.Errorf("SecondBox Microsandbox Workspace capacity differs from immutable Profile"))
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
	process, ready, err := launchHelper(ctx, backend.config, assignment, workspace)
	if err != nil {
		return result, infrastructureAssignment(err)
	}
	cleanup.disarm(workspaceCleanup)
	cleanup.push(func() error {
		process.forceStop()
		return nil
	})
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
		ready:         ready,
	}
	if err := backend.emitLifecycle(ctx, active, "ready", "control:1", "completed", "ready"); err != nil {
		return result, infrastructureAssignment(err)
	}
	backend.mu.Lock()
	backend.assignments[assignment.Fence.AssignmentId] = active
	backend.startupSamples = append(backend.startupSamples, time.Since(started))
	backend.mu.Unlock()
	// The deferred launchDone wakes replay waiters only after the remaining
	// fallible startup steps: a failure below removes the assignment before
	// any replay can observe success.
	go backend.observeExit(active)
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY); err != nil {
		backend.mu.Lock()
		delete(backend.assignments, assignment.Fence.AssignmentId)
		backend.mu.Unlock()
		return result, err
	}
	cleanup.clear()
	return runnercontrol.BackendInstance{BackendKind: "microsandbox", BackendReference: active.backendRef}, nil
}

func (backend *AssignmentBackend) MarkAssignmentReady(fence *runnerprotocol.AssignmentFence) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	active, exists := backend.assignments[fence.GetAssignmentId()]
	if !exists || active.launched != nil || !sameFence(active.fence, fence) || active.fenced {
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
	evidenceErr := backend.emitLifecycle(context.Background(), active, "unexpected_exit", "control:terminal", "failed", "helper_exit")
	backend.mu.Lock()
	active.evidenceErr = errors.Join(active.evidenceErr, evidenceErr)
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
	var active *activeAssignment
	for {
		current, exists := backend.assignments[command.Fence.AssignmentId]
		if !exists {
			backend.mu.Unlock()
			return runnercontrol.FenceEvidence{
				Result:                    runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED,
				TerminationEvidenceDigest: fenceDigest(command.Fence),
			}, nil
		}
		if !sameFence(current.fence, command.Fence) {
			backend.mu.Unlock()
			return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox Microsandbox fence token or generation mismatch")
		}
		if current.launched == nil {
			active = current
			break
		}
		// A pending claimed launch holds no process or operation state yet;
		// fencing waits for the launch to settle and then fences whatever it
		// produced.
		pendingLaunch := current.launched
		backend.mu.Unlock()
		select {
		case <-pendingLaunch:
		case <-ctx.Done():
			return runnercontrol.FenceEvidence{}, ctx.Err()
		}
		backend.mu.Lock()
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
	outcome, terminalKind := "completed", "stopped"
	if err != nil {
		outcome, terminalKind = "failed", "cleanup_failed"
	}
	evidenceErr := backend.emitLifecycle(ctx, active, "teardown", "control:shutdown", outcome, terminalKind)
	backend.mu.Lock()
	err = errors.Join(err, evidenceErr, active.evidenceErr)
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
	if active == nil || active.launched != nil || active.fenced || !sameFence(active.fence, fence) {
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

func (backend *AssignmentBackend) emitLifecycle(
	ctx context.Context,
	active *activeAssignment,
	stage string,
	streamID string,
	outcome string,
	terminalKind string,
) error {
	backend.mu.Lock()
	sink, runnerID := backend.evidence, backend.runnerID
	backend.mu.Unlock()
	if sink == nil || runnerID == "" {
		return nil
	}
	record := runnerevidence.NewRecord(runnerevidence.EventLifecycleStage, outcome, terminalKind, time.Now().UTC())
	record.RunnerID = runnerID
	record.RequestID = active.correlation.GetRequestId()
	record.OperationID = active.correlation.GetOperationId()
	record.LeaseID = active.correlation.GetLeaseId()
	record.SandboxID = active.fence.SandboxId
	record.InstanceID = active.fence.InstanceId
	record.SandboxGeneration = active.fence.SandboxGeneration
	record.AssignmentID = active.fence.AssignmentId
	record.BackendKind = "microsandbox"
	record.HostPlatform = active.ready.GetHostPlatform()
	record.BackendVersion = active.ready.GetDependencyVersion()
	record.Materialization = backend.config.MaterializationDigest
	record.Stage = stage
	record.StreamID = streamID
	record.HelperPID = active.process.command.Process.Pid
	if stage == "unexpected_exit" {
		record.ExitCode, record.Signal, record.HelperReason = helperExitClassification(active.process)
		record.StderrDigest = digestString(active.process.stderr.String())
		record.EventTailDigest = digestString(fmt.Sprintf("%s\x00%d\x00%d", record.HelperReason, record.ExitCode, record.Signal))
	}
	if err := record.Validate(); err != nil {
		return err
	}
	return sink.Emit(ctx, record)
}

func helperExitClassification(process *helperProcess) (int, int, string) {
	err := process.processWaitError()
	if err == nil {
		return 0, 0, "exited"
	}
	var exit *os.PathError
	if errors.As(err, &exit) {
		return -1, 0, "launch_io_failure"
	}
	if process.command.ProcessState != nil {
		if status, ok := process.command.ProcessState.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return -1, int(status.Signal()), "signaled"
			}
			return status.ExitStatus(), 0, "exited_nonzero"
		}
		return process.command.ProcessState.ExitCode(), 0, "exited_nonzero"
	}
	return -1, 0, "unknown"
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var _ runnercontrol.AssignmentBackend = (*AssignmentBackend)(nil)
var _ = materialization.BackendMicrosandbox
