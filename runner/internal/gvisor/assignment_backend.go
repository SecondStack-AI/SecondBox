//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
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
	handles        *SupervisorHandles
	session        *firecracker.GuestProtocolSession
	workspace      workspacestore.ComputeAttachment
	network        instanceNetwork
	instanceDir    string
	reservation    capacityReservation
	backendRef     string
	readyPublished bool
	fenced         bool
	terminalSent   bool
	operations     map[uint64]context.CancelFunc
	nextOperation  uint64
	done           chan struct{}
	exitMu         sync.Mutex
	computeOutcome string
	computeCode    string
	evidenceErr    error
}

// AssignmentBackend composes one mount supervisor and runsc sandbox per
// fenced Instance, with the guest agent negotiated over gofer host sockets.
type AssignmentBackend struct {
	config            validatedConfig
	mu                sync.Mutex
	assignments       map[string]*activeAssignment
	reserved          capacityReservation
	instanceTerminals chan runnercontrol.BackendInstanceTerminal
	startupSamples    []time.Duration
	evidence          runnerevidence.Sink
	runnerID          string
	platformProbe     sync.Once
	platformProbeErr  error
	networkSlots      map[uint32]bool
	enforcer          *firecracker.NFTablesNetworkPolicyEnforcer
}

type cleanupStack struct {
	steps []func() error
	armed []bool
}

func (stack *cleanupStack) push(step func() error) int {
	stack.steps = append(stack.steps, step)
	stack.armed = append(stack.armed, true)
	return len(stack.steps) - 1
}

func (stack *cleanupStack) disarm(index int) { stack.armed[index] = false }

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

// NewAssignmentBackend validates all immutable local inputs before the
// backend can advertise.
func NewAssignmentBackend(config Config) (*AssignmentBackend, error) {
	validated, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	// The runtime directory usually lives on tmpfs (for example under /run),
	// so the backend creates it on every start rather than expecting the host
	// to have preserved it.
	if err := os.MkdirAll(validated.RuntimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox gVisor runtime directory: %w", err)
	}
	nftPath, err := exec.LookPath("nft")
	if err != nil {
		return nil, fmt.Errorf("SecondBox gVisor network enforcement requires nft: %w", err)
	}
	upstream, err := resolveDNSUpstream(config.DNSUpstream)
	if err != nil {
		return nil, err
	}
	dnsListen := netip.MustParseAddr(dnsAddressForProfile(config.NetworkProfile))
	return &AssignmentBackend{
		config:            validated,
		assignments:       make(map[string]*activeAssignment),
		instanceTerminals: make(chan runnercontrol.BackendInstanceTerminal, config.MaximumInstances),
		enforcer: firecracker.NewNetworkPolicyEnforcer(
			nftPath, dnsListen, upstream, renderInetPolicy, deleteInetPolicyTables,
		),
	}, nil
}

func resolveDNSUpstream(override string) (netip.AddrPort, error) {
	if strings.TrimSpace(override) != "" {
		upstream, err := netip.ParseAddrPort(override)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("SecondBox gVisor DNS upstream override is invalid: %w", err)
		}
		return upstream, nil
	}
	return systemDNSUpstream()
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

// BackendDimensions are the fixed-cardinality diagnostic dimensions.
type BackendDimensions struct {
	BackendKind  string
	HostPlatform string
}

// MetricsSnapshot exposes bounded backend metrics with no correlation
// identifiers.
type MetricsSnapshot struct {
	Dimensions       BackendDimensions
	ActiveInstances  uint32
	ActiveOperations uint32
	ColdStartCount   uint64
	ColdStartP95     time.Duration
}

func (backend *AssignmentBackend) DiagnosticDimensions() BackendDimensions {
	return BackendDimensions{BackendKind: "gvisor", HostPlatform: gvisorHostPlatform()}
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

func gvisorHostPlatform() string {
	architecture := runtime.GOARCH
	if architecture == "amd64" {
		architecture = "x86_64"
	}
	return "linux-" + architecture
}

// Readiness proves the sentry platform, pinned launch artifacts, exact
// materialization, loop reconciliation, and integer capacity.
func (backend *AssignmentBackend) Readiness(ctx context.Context) (runnercontrol.BackendReadiness, error) {
	if _, err := validateConfig(backend.config.Config); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox gVisor readiness materialization: %w", err)
	}
	if err := backend.probePlatform(ctx); err != nil {
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
			ComputeBackendVersion: manifest.HelperBuildID,
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
		BackendKind: runnerprotocol.ComputeBackendKind_COMPUTE_BACKEND_KIND_GVISOR,
		Materializations: []*runnerprotocol.BackendMaterializationEvidence{{
			SchemaVersion:           1,
			BackendKind:             runnerprotocol.ComputeBackendKind_COMPUTE_BACKEND_KIND_GVISOR,
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
		return incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment is incomplete"))
	}
	if strings.TrimSpace(assignment.WorkspaceId) == "" || !completeFence(assignment.Fence) {
		return incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment Workspace or fence identity is incomplete"))
	}
	// An already-active assignment replays validly under its exact fence,
	// including after its original admission deadline.
	backend.mu.Lock()
	if active, exists := backend.assignments[assignment.Fence.AssignmentId]; exists {
		same := sameFence(active.fence, assignment.Fence)
		backend.mu.Unlock()
		if same {
			return nil
		}
		return incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment ID was reused with different fencing"))
	}
	backend.mu.Unlock()
	if assignment.DeadlineUnixMs == 0 || time.Now().UnixMilli() >= int64(assignment.DeadlineUnixMs) {
		return incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment deadline has expired"))
	}
	requirements := assignment.Requirements
	const mib = uint64(1 << 20)
	if requirements.VcpuCount == 0 || requirements.MemoryBytes == 0 || requirements.DiskBytes == 0 ||
		requirements.MemoryBytes%mib != 0 || requirements.DiskBytes%mib != 0 {
		return incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment requires whole-MiB nonzero resources"))
	}
	if requirements.Architecture != runtime.GOARCH || requirements.StartupMode != "cold_boot" {
		return incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment architecture or startup mode is unsupported"))
	}
	if requirements.VcpuCount > backend.config.MaximumVCPUs ||
		requirements.MemoryBytes > backend.config.MaximumMemoryBytes ||
		requirements.DiskBytes > backend.config.MaximumDiskBytes {
		return capacityAssignment(fmt.Errorf("SecondBox gVisor assignment exceeds immutable local capacity"))
	}
	supported := map[string]bool{
		"cleanup": true, "evidence": true, "gvisor": true, "local-workspace": true,
		"network-policy": true, "storage": true,
	}
	for _, capability := range requirements.RequiredCapabilities {
		if !supported[capability] {
			return incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment requires unsupported capability %q", capability))
		}
	}
	if err := backend.validateAssignmentMaterialization(assignment); err != nil {
		return artifactAssignment(err)
	}
	if _, err := validateConfig(backend.config.Config); err != nil {
		return artifactAssignment(fmt.Errorf("SecondBox gVisor revalidate local materialization: %w", err))
	}
	if _, err := translateNetworkPolicy(assignment.NetworkPolicy); err != nil {
		return incompatibleAssignment(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for _, active := range backend.assignments {
		if active.fence.SandboxId == assignment.Fence.SandboxId && !active.fenced {
			return incompatibleAssignment(fmt.Errorf("SecondBox gVisor Sandbox already has an unfenced assignment"))
		}
	}
	if backend.reserved.vcpus+requirements.VcpuCount > backend.config.MaximumVCPUs ||
		backend.reserved.memory+requirements.MemoryBytes > backend.config.MaximumMemoryBytes ||
		backend.reserved.disk+requirements.DiskBytes > backend.config.MaximumDiskBytes ||
		backend.reserved.instances+1 > backend.config.MaximumInstances {
		return capacityAssignment(fmt.Errorf("SecondBox gVisor assignment capacity is unavailable"))
	}
	return nil
}

func (backend *AssignmentBackend) StartAssignment(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (result runnercontrol.BackendInstance, resultErr error) {
	started := time.Now()
	if assignment == nil || assignment.Fence == nil {
		return result, incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment is incomplete"))
	}
	backend.mu.Lock()
	if active, exists := backend.assignments[assignment.Fence.AssignmentId]; exists {
		same := sameFence(active.fence, assignment.Fence)
		reference := active.backendRef
		backend.mu.Unlock()
		if same {
			return runnercontrol.BackendInstance{BackendKind: "gvisor", BackendReference: reference}, nil
		}
		return result, incompatibleAssignment(fmt.Errorf("SecondBox gVisor assignment ID was reused with different fencing"))
	}
	backend.mu.Unlock()
	if err := backend.ValidateAssignment(ctx, assignment); err != nil {
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
		return result, infrastructureAssignment(fmt.Errorf("SecondBox gVisor resolve Workspace attachment: %w", err))
	}
	workspaceCleanup := cleanup.push(workspace.Close)
	if uint64(workspace.CapacityBytes()) != assignment.Requirements.DiskBytes {
		return result, incompatibleAssignment(fmt.Errorf("SecondBox gVisor Workspace capacity differs from immutable Profile"))
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_WORKSPACE_ATTACH); err != nil {
		return result, err
	}
	compiled, err := translateNetworkPolicy(assignment.NetworkPolicy)
	if err != nil {
		return result, incompatibleAssignment(err)
	}
	network, supervisorPid, err := backend.installInstanceNetwork(ctx, assignment, compiled)
	if err != nil {
		return result, err
	}
	networkCleanup := cleanup.push(func() error {
		return errors.Join(
			backend.teardownInstanceNetwork(assignment.Fence.InstanceId, network),
			removeInstanceCgroup(backend.config.NetworkProfile, assignment.Fence.InstanceId),
		)
	})
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_NETWORK_SETUP); err != nil {
		return result, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_COMPUTE_LAUNCH); err != nil {
		return result, err
	}
	active, err := backend.launchInstance(ctx, assignment, workspace, network, supervisorPid)
	if err != nil {
		return result, infrastructureAssignment(err)
	}
	cleanup.disarm(networkCleanup)
	cleanup.disarm(workspaceCleanup)
	cleanup.push(func() error {
		return backend.destroyInstance(active)
	})
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_GUEST_NEGOTIATION); err != nil {
		return result, err
	}
	session, err := backend.negotiateSession(ctx, assignment, active)
	if err != nil {
		return result, infrastructureAssignment(err)
	}
	active.session = session
	active.reservation = reservation
	if err := backend.emitLifecycle(ctx, active, "ready", "control:1", "completed", "ready"); err != nil {
		return result, infrastructureAssignment(err)
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
	cleanup.clear()
	return runnercontrol.BackendInstance{BackendKind: "gvisor", BackendReference: active.backendRef}, nil
}

// launchInstance builds the per-Instance runtime area and starts the mount
// supervisor, which attaches the Workspace in its private namespace and
// launches runsc.
func (backend *AssignmentBackend) launchInstance(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	workspace workspacestore.ComputeAttachment,
	network instanceNetwork,
	supervisorPid *atomic.Int64,
) (*activeAssignment, error) {
	// A short digest keeps every per-Instance socket path inside sun_path
	// regardless of Instance ID length or runtime-directory depth.
	instanceDir := filepath.Join(backend.config.RuntimeDir, shortInstanceDirName(assignment.Fence.InstanceId))
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return nil, fmt.Errorf("create instance runtime directory: %w", err)
	}
	directories := map[string]string{
		"bundle":          filepath.Join(instanceDir, "bundle"),
		"state":           filepath.Join(instanceDir, "state"),
		"sockets":         filepath.Join(instanceDir, "sockets"),
		"runtime-private": filepath.Join(instanceDir, "runtime-private"),
		"mnt":             filepath.Join(instanceDir, "mnt"),
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, errors.Join(fmt.Errorf("create instance directory: %w", err), os.RemoveAll(instanceDir))
		}
	}
	resolvConfPath, err := writeGuestResolvConf(instanceDir, dnsAddressForProfile(backend.config.NetworkProfile))
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(instanceDir))
	}
	manifest := backend.config.manifest
	if err := writeInstanceBundle(instanceBundle{
		BundleDir:            directories["bundle"],
		FlatRootPath:         backend.config.FlatRootPath,
		AgentBinaryPath:      backend.config.AgentPath,
		WorkspaceMountpoint:  directories["mnt"],
		SocketDirectory:      directories["sockets"],
		RuntimePrivateDir:    directories["runtime-private"],
		InstanceID:           assignment.Fence.InstanceId,
		SandboxID:            assignment.Fence.SandboxId,
		SandboxGeneration:    assignment.Fence.SandboxGeneration,
		GuestBuildID:         manifest.BackendBuildID,
		ImageDigest:          manifest.Key.RuntimeManifestDigest,
		ToolchainDigest:      manifest.Key.ToolchainManifestDigest,
		VCPUCount:            assignment.Requirements.VcpuCount,
		MemoryBytes:          assignment.Requirements.MemoryBytes,
		CgroupsPath:          instanceCgroupPath(backend.config.NetworkProfile, assignment.Fence.InstanceId),
		NetworkNamespacePath: network.namespacePath(),
		ResolvConfPath:       resolvConfPath,
	}); err != nil {
		return nil, errors.Join(err, os.RemoveAll(instanceDir))
	}
	handles, err := StartMountSupervisor(backend.config.SelfExecutable, MountSupervisorPlan{
		Mountpoint:    directories["mnt"],
		ExpectedUUID:  workspace.FilesystemUUID(),
		CapacityBytes: workspace.CapacityBytes(),
		RunscPath:     backend.config.RunscPath,
		StateRoot:     directories["state"],
		BundleDir:     directories["bundle"],
		ContainerID:   "secondbox-" + assignment.Fence.InstanceId,
		RunscGlobal: []string{
			"--network=sandbox", "--platform=systrap", "--host-uds=all", "--overlay2=root:memory",
		},
	}, workspace.Descriptor())
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(instanceDir))
	}
	supervisorPid.Store(int64(handles.Command.Process.Pid))
	active := &activeAssignment{
		fence:         cloneFence(assignment.Fence),
		correlation:   proto.Clone(assignment.Correlation).(*runnerprotocol.Correlation),
		handles:       handles,
		workspace:     workspace,
		network:       network,
		instanceDir:   instanceDir,
		backendRef:    fmt.Sprintf("gvisor:%d", handles.Command.Process.Pid),
		operations:    make(map[uint64]context.CancelFunc),
		nextOperation: 1,
		done:          make(chan struct{}),
	}
	go func() {
		_ = handles.Command.Wait()
		close(active.done)
	}()

	ready := make(chan error, 1)
	go func() {
		status, err := handles.ReadStatusLine()
		if err != nil {
			ready <- fmt.Errorf("supervisor ended before ready: %w", err)
			return
		}
		if status.Kind != "ready" || status.Fields["rw_probe"] != "ok" {
			ready <- fmt.Errorf("supervisor reported %q instead of ready", status.Kind)
			return
		}
		ready <- nil
	}()
	select {
	case err := <-ready:
		if err != nil {
			return nil, errors.Join(err, backend.destroyInstance(active))
		}
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), backend.destroyInstance(active))
	case <-time.After(60 * time.Second):
		return nil, errors.Join(fmt.Errorf("supervisor ready deadline exceeded"), backend.destroyInstance(active))
	}
	// Only after the synchronous ready read may the status consumer own the
	// stream; two concurrent readers would race for the ready line.
	go backend.consumeStatus(active)
	return active, nil
}

// consumeStatus retains bounded compute-exit evidence from the supervisor.
func (backend *AssignmentBackend) consumeStatus(active *activeAssignment) {
	for {
		status, err := active.handles.ReadStatusLine()
		if err != nil {
			return
		}
		if status.Kind == "compute-exit" {
			active.exitMu.Lock()
			active.computeOutcome = status.Fields["outcome"]
			active.computeCode = status.Fields["code"]
			active.exitMu.Unlock()
		}
	}
}

func (backend *AssignmentBackend) negotiateSession(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	active *activeAssignment,
) (*firecracker.GuestProtocolSession, error) {
	manifest := backend.config.manifest
	negotiateCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return firecracker.NegotiateGuestProtocol(negotiateCtx, firecracker.GuestProtocolNegotiation{
		UDSPath:                         filepath.Join(active.instanceDir, "sockets", "protocol.sock"),
		DirectUnixSocket:                true,
		InstanceID:                      assignment.Fence.InstanceId,
		SandboxID:                       assignment.Fence.SandboxId,
		SandboxGeneration:               assignment.Fence.SandboxGeneration,
		ExpectedGuestBuildID:            manifest.BackendBuildID,
		ExpectedImageManifestDigest:     manifest.Key.RuntimeManifestDigest,
		ExpectedToolchainManifestDigest: manifest.Key.ToolchainManifestDigest,
		RequestedFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
			guestv1.GuestFeature_GUEST_FEATURE_ACTIVITY_EVENTS,
			guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
		},
		MandatoryFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
			guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
		},
	})
}

// destroyInstance force-stops the supervisor tree and releases the Workspace
// attachment only after the supervisor has exited (its exit proves the mount
// and loop device are gone).
func (backend *AssignmentBackend) destroyInstance(active *activeAssignment) error {
	if active.session != nil {
		_ = active.session.Close()
	}
	_, _ = active.handles.Control.Write([]byte{controlKill})
	_ = active.handles.Control.Close()
	select {
	case <-active.done:
	case <-time.After(supervisorStopBound):
		_ = syscallKillGroup(active.handles.Command.Process.Pid)
		select {
		case <-active.done:
		case <-time.After(supervisorStopBound):
			return fmt.Errorf("SecondBox gVisor supervisor did not exit after force kill")
		}
	}
	closeErr := errors.Join(active.handles.CloseParentSide(), active.workspace.Close())
	networkErr := backend.teardownInstanceNetwork(active.fence.InstanceId, active.network)
	return errors.Join(closeErr, networkErr,
		removeInstanceCgroup(backend.config.NetworkProfile, active.fence.InstanceId), os.RemoveAll(active.instanceDir))
}

// installInstanceNetwork creates the routed per-Instance namespace and
// installs the compiled policy on its host veth. The returned pid holder is
// armed after launch so an enforcement failure can force the compute down,
// which delivers exactly one provider-neutral terminal.
func (backend *AssignmentBackend) installInstanceNetwork(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	compiled *networkpolicy.CompiledPolicy,
) (instanceNetwork, *atomic.Int64, error) {
	network, err := backend.acquireNetworkSlot()
	if err != nil {
		return instanceNetwork{}, nil, capacityAssignment(err)
	}
	if err := createInstanceNetwork(ctx, network); err != nil {
		backend.releaseNetworkSlot(network)
		return instanceNetwork{}, nil, infrastructureAssignment(err)
	}
	supervisorPid := &atomic.Int64{}
	if err := backend.enforcer.Install(ctx, firecracker.PolicyNetworkConfig{
		InstanceID: assignment.Fence.InstanceId,
		TapName:    network.hostVeth,
		GuestIP:    network.guestAddress,
		DNSAddress: netip.MustParseAddr(dnsAddressForProfile(backend.config.NetworkProfile)),
		Policy:     compiled,
		OnFailure: func(error) {
			// Enforcement loss fails closed: the compute is forced down and
			// the supervisor-exit path delivers the single terminal.
			if pid := supervisorPid.Load(); pid > 0 {
				_ = syscallKillGroup(int(pid))
			}
		},
	}); err != nil {
		teardown := backend.teardownInstanceNetwork(assignment.Fence.InstanceId, network)
		return instanceNetwork{}, nil, infrastructureAssignment(errors.Join(err, teardown))
	}
	return network, supervisorPid, nil
}

func (backend *AssignmentBackend) teardownInstanceNetwork(instanceID string, network instanceNetwork) error {
	removeErr := backend.enforcer.Remove(context.Background(), instanceID)
	destroyErr := destroyInstanceNetwork(context.Background(), network)
	backend.releaseNetworkSlot(network)
	return errors.Join(removeErr, destroyErr)
}

const supervisorStopBound = 15 * time.Second

func (backend *AssignmentBackend) MarkAssignmentReady(fence *runnerprotocol.AssignmentFence) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	active, exists := backend.assignments[fence.GetAssignmentId()]
	if !exists || !sameFence(active.fence, fence) || active.fenced {
		return fmt.Errorf("SecondBox gVisor ready assignment fence is stale")
	}
	active.readyPublished = true
	return nil
}

func (backend *AssignmentBackend) observeExit(active *activeAssignment) {
	<-active.done
	backend.mu.Lock()
	current, exists := backend.assignments[active.fence.AssignmentId]
	if !exists || current != active || active.fenced || !active.readyPublished || active.terminalSent {
		backend.mu.Unlock()
		return
	}
	active.terminalSent = true
	terminal := runnercontrol.BackendInstanceTerminal{
		Fence:          cloneFence(active.fence),
		Correlation:    proto.Clone(active.correlation).(*runnerprotocol.Correlation),
		Reason:         runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE,
		ObservedAt:     time.Now().UTC(),
		EvidenceDigest: supervisorTerminalDigest(active),
	}
	backend.mu.Unlock()
	evidenceErr := backend.emitLifecycle(context.Background(), active, "unexpected_exit", "control:terminal", "failed", "supervisor_exit")
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
		return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox gVisor fence identity is required")
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
		return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox gVisor fence token or generation mismatch")
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
	if active.session != nil {
		_ = active.session.Close()
	}
	deadline := time.UnixMilli(int64(command.DeadlineUnixMs))
	if command.DeadlineUnixMs == 0 || deadline.Before(time.Now()) {
		deadline = time.Now().Add(10 * time.Second)
	}
	// Graceful first: SIGTERM reaches the agent, which stops its listeners
	// and lets runsc exit; the supervisor then detaches and exits.
	_, _ = active.handles.Control.Write([]byte{controlTerminate})
	var err error
	select {
	case <-active.done:
	case <-time.After(time.Until(deadline)):
		_, _ = active.handles.Control.Write([]byte{controlKill})
		_ = active.handles.Control.Close()
		select {
		case <-active.done:
		case <-time.After(supervisorStopBound):
			_ = syscallKillGroup(active.handles.Command.Process.Pid)
			<-active.done
		}
	}
	err = errors.Join(err, active.handles.CloseParentSide(), active.workspace.Close(),
		backend.teardownInstanceNetwork(active.fence.InstanceId, active.network),
		removeInstanceCgroup(backend.config.NetworkProfile, active.fence.InstanceId), os.RemoveAll(active.instanceDir))
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

// Shutdown fences every locally owned instance without adopting work after
// restart.
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
	return errors.Join(result, backend.enforcer.Close())
}

func (backend *AssignmentBackend) reserve(request capacityReservation) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.reserved.vcpus+request.vcpus > backend.config.MaximumVCPUs ||
		backend.reserved.memory+request.memory > backend.config.MaximumMemoryBytes ||
		backend.reserved.disk+request.disk > backend.config.MaximumDiskBytes ||
		backend.reserved.instances+request.instances > backend.config.MaximumInstances {
		return fmt.Errorf("SecondBox gVisor assignment capacity is unavailable")
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
		return nil, nil, nil, fmt.Errorf("SecondBox gVisor operation fence is stale")
	}
	if backend.reserved.operations >= backend.config.MaximumOperations {
		backend.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("SecondBox gVisor operation capacity is unavailable")
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
		return fmt.Errorf("SecondBox gVisor assignment must select exactly runtime and toolchain assets")
	}
	expected := map[string]bool{
		manifest.Key.RuntimeManifestDigest:   false,
		manifest.Key.ToolchainManifestDigest: false,
	}
	for _, asset := range assignment.Assets {
		if asset == nil || asset.Architecture != manifest.Key.GuestArchitecture ||
			asset.GuestProtocolGeneration != manifest.AgentProtocolGeneration {
			return fmt.Errorf("SecondBox gVisor assignment asset compatibility differs from the materialization")
		}
		if _, exists := expected[asset.ManifestDigest]; !exists || expected[asset.ManifestDigest] {
			return fmt.Errorf("SecondBox gVisor assignment asset digest differs from the materialization")
		}
		expected[asset.ManifestDigest] = true
		for _, feature := range asset.MandatoryGuestFeatures {
			if !slices.Contains(manifest.AgentFeatures, feature) {
				return fmt.Errorf("SecondBox gVisor assignment requires unsupported agent feature %q", feature)
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

func supervisorTerminalDigest(active *activeAssignment) string {
	active.exitMu.Lock()
	outcome, code := active.computeOutcome, active.computeCode
	active.exitMu.Unlock()
	value := fmt.Sprintf("%s\x00%s\x00%s", active.backendRef, outcome, code)
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
	record.BackendKind = "gvisor"
	record.HostPlatform = gvisorHostPlatform()
	record.BackendVersion = backend.config.manifest.HelperBuildID
	record.Materialization = backend.config.MaterializationDigest
	record.Stage = stage
	record.StreamID = streamID
	record.HelperPID = active.handles.Command.Process.Pid
	if stage == "unexpected_exit" {
		active.exitMu.Lock()
		outcomeDetail := active.computeOutcome + "\x00" + active.computeCode
		record.HelperReason = active.computeOutcome
		active.exitMu.Unlock()
		if record.HelperReason == "" {
			record.HelperReason = "supervisor_exit"
		}
		record.StderrDigest = digestString(outcomeDetail)
		record.EventTailDigest = digestString(record.HelperReason)
	}
	if err := record.Validate(); err != nil {
		return err
	}
	return sink.Emit(ctx, record)
}

func shortInstanceDirName(instanceID string) string {
	sum := sha256.Sum256([]byte(instanceID))
	return hex.EncodeToString(sum[:8])
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func syscallKillGroup(pid int) error {
	return unixKill(-pid)
}

var _ runnercontrol.AssignmentBackend = (*AssignmentBackend)(nil)
