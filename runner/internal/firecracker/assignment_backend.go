package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"google.golang.org/protobuf/proto"
)

type activeRunnerAssignment struct {
	fence            *runnerprotocol.AssignmentFence
	correlation      *runnerprotocol.Correlation
	backendReference string
}

type signedArtifactManifest struct {
	ArtifactVersion string `json:"artifactVersion"`
	Architecture    string `json:"architecture"`
	GuestProtocol   struct {
		Minimum uint32 `json:"minimum"`
		Maximum uint32 `json:"maximum"`
	} `json:"guestProtocol"`
	RuntimeBundle   signedArtifactComponent `json:"runtimeBundle"`
	ToolchainBundle signedArtifactComponent `json:"toolchainBundle"`
}

type signedArtifactComponent struct {
	ArtifactID             string   `json:"artifactId"`
	Path                   string   `json:"path"`
	ManifestDigest         string   `json:"manifestDigest"`
	MandatoryGuestFeatures []string `json:"mandatoryGuestFeatures"`
}

// AssignmentBackend adapts profile-resolved runner assignments to Firecracker.
type AssignmentBackend struct {
	manager           *Manager
	storagePressure   *storagePressureController
	mu                sync.Mutex
	assignments       map[string]activeRunnerAssignment
	instanceTerminals chan runnercontrol.BackendInstanceTerminal
}

// NewAssignmentBackend exposes the Firecracker compute conformance port.
func NewAssignmentBackend(manager *Manager) (*AssignmentBackend, error) {
	if manager == nil || manager.cfg == nil {
		return nil, fmt.Errorf("SecondBox Firecracker assignment backend requires a manager")
	}
	if manager.workspaceStore == nil {
		return nil, fmt.Errorf("SecondBox Firecracker assignment backend requires a WorkspaceStore")
	}
	emitStoragePressure := func(ctx context.Context, terminalKind string) error {
		record := runnerevidence.NewRecord(
			runnerevidence.EventStoragePressure,
			"observed",
			terminalKind,
			time.Now().UTC(),
		)
		record.RunnerID = manager.runnerID
		return manager.evidenceSink().Emit(ctx, record)
	}
	storagePressure, err := newConfiguredStoragePressureController(manager.cfg, emitStoragePressure)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Firecracker assignment backend storage pressure: %w", err)
	}
	return &AssignmentBackend{
		manager:           manager,
		storagePressure:   storagePressure,
		assignments:       make(map[string]activeRunnerAssignment),
		instanceTerminals: make(chan runnercontrol.BackendInstanceTerminal, manager.cfg.MicroVMMaxConcurrentGlobal),
	}, nil
}

// SetRunnerEvidenceSink binds Manager-local evidence to the protocol service sink.
func (b *AssignmentBackend) SetRunnerEvidenceSink(sink runnerevidence.Sink, runnerID string) {
	b.manager.SetRunnerEvidenceSink(sink, runnerID)
}

// InstanceTerminals exposes at most one natural post-ready terminal event per
// active assignment.
func (b *AssignmentBackend) InstanceTerminals() <-chan runnercontrol.BackendInstanceTerminal {
	return b.instanceTerminals
}

// MarkAssignmentReady starts natural-exit observation only after the ready
// AssignmentResult has been emitted on the Runner stream.
func (b *AssignmentBackend) MarkAssignmentReady(fence *runnerprotocol.AssignmentFence) error {
	if fence == nil {
		return fmt.Errorf("SecondBox Firecracker ready assignment fence is required")
	}
	b.mu.Lock()
	active, ok := b.assignments[fence.AssignmentId]
	b.mu.Unlock()
	if !ok || !sameAssignmentFence(active.fence, fence) {
		return fmt.Errorf("SecondBox Firecracker ready assignment fence is stale")
	}
	return b.manager.MarkAssignmentReady(
		active.backendReference,
		func(ctx context.Context, observation InstanceTerminalObservation) error {
			b.mu.Lock()
			current, currentOK := b.assignments[fence.AssignmentId]
			if !currentOK ||
				current.backendReference != observation.BackendReference ||
				!sameAssignmentFence(current.fence, fence) {
				b.mu.Unlock()
				return fmt.Errorf("SecondBox Firecracker terminal observation lost assignment authority")
			}
			terminal := runnercontrol.BackendInstanceTerminal{
				Fence:          cloneAssignmentFence(current.fence),
				Correlation:    proto.Clone(current.correlation).(*runnerprotocol.Correlation),
				Reason:         protocolObservedTerminationReason(observation.Reason),
				ObservedAt:     observation.ObservedAt,
				EvidenceDigest: observation.EvidenceDigest,
			}
			b.mu.Unlock()
			if !runnercontrolValidObservedTerminationReason(terminal.Reason) {
				return fmt.Errorf("SecondBox Firecracker terminal observation reason is invalid")
			}
			select {
			case b.instanceTerminals <- terminal:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	)
}

func protocolObservedTerminationReason(
	reason string,
) runnerprotocol.InstanceObservedTerminationReason {
	switch reason {
	case string(observedTerminationGuestShutdown):
		return runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_GUEST_SHUTDOWN
	case string(observedTerminationResourceExhaustion):
		return runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_RESOURCE_EXHAUSTION
	case string(observedTerminationInternalFailure):
		return runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE
	default:
		return runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_UNSPECIFIED
	}
}

func runnercontrolValidObservedTerminationReason(
	reason runnerprotocol.InstanceObservedTerminationReason,
) bool {
	return reason != runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_UNSPECIFIED
}

// StartupTiming reports bounded process-lifetime Sandbox startup observations.
func (b *AssignmentBackend) StartupTiming() (uint64, time.Duration) {
	snapshot := b.manager.RuntimeMetricsSnapshot()
	return uint64(snapshot.ColdStartCount), snapshot.ColdStartP95
}

// Readiness verifies signed artifacts and host prerequisites before capacity advertisement.
func (b *AssignmentBackend) Readiness(ctx context.Context) (runnercontrol.BackendReadiness, error) {
	if err := b.manager.CleanupHealth(); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness cleanup evidence: %w", err)
	}
	if err := b.manager.VerifyArtifactHealth(); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness artifact verification: %w", err)
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness KVM: %w", err)
	}
	if err := kvm.Close(); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness close KVM probe: %w", err)
	}
	cfg := b.manager.cfg
	manifest, err := loadSignedArtifactManifest(filepath.Join(filepath.Dir(cfg.MicroVMKernelPath), "manifest.json"))
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness signed compatibility metadata: %w", err)
	}
	if strings.TrimSpace(cfg.MicroVMJailerChrootBaseDir) == "" ||
		cfg.MicroVMJailerUIDStart < 1 ||
		cfg.MicroVMJailerUIDCount < cfg.MicroVMMaxConcurrentGlobal ||
		cfg.MicroVMJailerGID < 1 {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness jailer isolation is incomplete")
	}
	if cfg.MicroVMJailerCgroupVersion < 1 ||
		strings.TrimSpace(cfg.MicroVMJailerParentCgroup) == "" {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness cgroup policy is incomplete")
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness cgroup controllers: %w", err)
	}
	for _, requiredController := range []string{"cpu", "memory", "pids"} {
		if !containsSpaceSeparated(string(controllers), requiredController) {
			return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness cgroup controller %q is unavailable", requiredController)
		}
	}
	if strings.TrimSpace(cfg.MicroVMBridgeName) == "" ||
		strings.TrimSpace(cfg.MicroVMBridgeCIDR) == "" ||
		strings.TrimSpace(cfg.MicroVMTapPrefix) == "" {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness network policy is incomplete")
	}
	bridge, err := net.InterfaceByName(cfg.MicroVMBridgeName)
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness bridge %q: %w", cfg.MicroVMBridgeName, err)
	}
	addresses, err := bridge.Addrs()
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness bridge %q addresses: %w", cfg.MicroVMBridgeName, err)
	}
	if !containsNetworkAddress(addresses, cfg.MicroVMBridgeCIDR) {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness bridge %q lacks %s", cfg.MicroVMBridgeName, cfg.MicroVMBridgeCIDR)
	}
	if b.manager.networkPolicy == nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness host network policy enforcement is unavailable")
	}
	if err := b.manager.networkPolicy.Ready(context.Background()); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness host network policy: %w", err)
	}
	workspaceInfo, err := os.Stat(cfg.RunnerWorkspaceRoot)
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness workspace storage %q: %w", cfg.RunnerWorkspaceRoot, err)
	}
	if !workspaceInfo.IsDir() {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness workspace storage %q is not a directory", cfg.RunnerWorkspaceRoot)
	}
	storagePressureState, err := b.storagePressure.Observe(ctx)
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf(
			"SecondBox Firecracker readiness storage pressure: %w",
			err,
		)
	}
	if storagePressureState == storagePressureStateAdmissionDenied {
		return runnercontrol.BackendReadiness{}, fmt.Errorf(
			"SecondBox Firecracker readiness storage pressure: %w",
			ErrStoragePressureAdmissionDenied,
		)
	}
	kernelRelease, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness host kernel evidence: %w", err)
	}
	return runnercontrol.BackendReadiness{
		Architecture: runtime.GOARCH,
		Capacity:     runnerAllocatableCapacity(cfg),
		Reserved:     &runnerprotocol.Capacity{},
		Capabilities: &runnerprotocol.RunnerCapabilities{
			Architecture:       runtime.GOARCH,
			KernelRelease:      strings.TrimSpace(string(kernelRelease)),
			FirecrackerVersion: expectedFirecrackerVersionString(),
			KvmReady:           true,
			JailerReady:        firecrackerJailerReady(cfg),
			CgroupReady:        true,
			NetworkPolicyReady: true,
			StorageReady:       true,
			CleanupReady:       true,
			GuestProtocolGenerations: &runnerprotocol.ProtocolVersionRange{
				Minimum: manifest.GuestProtocol.Minimum,
				Maximum: manifest.GuestProtocol.Maximum,
			},
		},
		ArtifactCache: artifactCacheEvidenceForManifest(manifest, time.Now().UTC()),
	}, nil
}

func firecrackerJailerReady(cfg *config.Config) bool {
	return !cfg.MicroVMAllowUnjailed
}

func runnerAllocatableCapacity(cfg *config.Config) *runnerprotocol.Capacity {
	return &runnerprotocol.Capacity{
		VcpuMillis: uint32(
			cfg.MicroVMVCPUs * cfg.MicroVMMaxConcurrentGlobal * 1000,
		),
		MemoryBytes: uint64(cfg.MicroVMMemoryBudgetMiB) << 20,
		DiskBytes: uint64(
			cfg.MicroVMWorkspaceSizeMiB*cfg.MicroVMMaxConcurrentGlobal,
		) << 20,
		Instances:  uint32(cfg.MicroVMMaxConcurrentGlobal),
		Operations: uint32(cfg.MicroVMMaxConcurrentOperationsGlobal),
	}
}

func artifactCacheEvidenceForManifest(
	manifest signedArtifactManifest,
	verifiedAt time.Time,
) []*runnerprotocol.ArtifactCacheEvidence {
	verifiedAtUnixMs := uint64(verifiedAt.UnixMilli())
	return []*runnerprotocol.ArtifactCacheEvidence{
		{
			ArtifactId:       manifest.RuntimeBundle.ArtifactID,
			ManifestDigest:   manifest.RuntimeBundle.ManifestDigest,
			VerifiedAtUnixMs: verifiedAtUnixMs,
		},
		{
			ArtifactId:       manifest.ToolchainBundle.ArtifactID,
			ManifestDigest:   manifest.ToolchainBundle.ManifestDigest,
			VerifiedAtUnixMs: verifiedAtUnixMs,
		},
	}
}

func containsSpaceSeparated(values, required string) bool {
	for _, value := range strings.Fields(values) {
		if value == required {
			return true
		}
	}
	return false
}

func containsNetworkAddress(addresses []net.Addr, required string) bool {
	requiredIP, requiredNetwork, err := net.ParseCIDR(required)
	if err != nil {
		return false
	}
	requiredPrefix, _ := requiredNetwork.Mask.Size()
	for _, address := range addresses {
		addressIP, addressNetwork, err := net.ParseCIDR(address.String())
		if err != nil {
			continue
		}
		prefix, _ := addressNetwork.Mask.Size()
		if addressIP.Equal(requiredIP) && prefix == requiredPrefix {
			return true
		}
	}
	return false
}

// StartAssignment validates local artifact identity and launches one fenced instance.
func (b *AssignmentBackend) ValidateAssignment(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
) error {
	if assignment == nil || assignment.Fence == nil || assignment.Requirements == nil {
		return fmt.Errorf("SecondBox Firecracker assignment is incomplete")
	}
	if strings.TrimSpace(assignment.WorkspaceId) == "" {
		return fmt.Errorf("SecondBox Firecracker assignment Workspace identity is required")
	}
	b.mu.Lock()
	active, alreadyActive := b.assignments[assignment.Fence.AssignmentId]
	b.mu.Unlock()
	if alreadyActive {
		if sameAssignmentFence(active.fence, assignment.Fence) {
			return nil
		}
		return fmt.Errorf("SecondBox Firecracker assignment ID was reused with different fencing")
	}
	const mib = uint64(1 << 20)
	requirements := assignment.Requirements
	if requirements.MemoryBytes%mib != 0 || requirements.DiskBytes%mib != 0 {
		return fmt.Errorf("SecondBox Firecracker assignment memory and disk must use whole MiB")
	}
	if requirements.Architecture != runtime.GOARCH {
		return fmt.Errorf(
			"SecondBox Firecracker assignment architecture %s does not match %s",
			requirements.Architecture,
			runtime.GOARCH,
		)
	}
	if assignment.DeadlineUnixMs == 0 || time.Now().UnixMilli() >= int64(assignment.DeadlineUnixMs) {
		return fmt.Errorf("SecondBox Firecracker assignment deadline has expired")
	}
	supportedCapabilities := map[string]bool{
		"cgroup":           true,
		"cleanup":          true,
		"evidence":         true,
		"firecracker":      true,
		"jailer":           true,
		"kvm":              true,
		"local-workspace":  true,
		"network-policy":   true,
		"signed-artifacts": true,
		"storage":          true,
		"tap":              true,
		"vsock":            true,
	}
	for _, capability := range requirements.RequiredCapabilities {
		if !supportedCapabilities[capability] {
			return fmt.Errorf("SecondBox Firecracker assignment requires unsupported capability %q", capability)
		}
	}
	if int(requirements.VcpuCount) > b.manager.cfg.MicroVMVCPUs ||
		int(requirements.VcpuMillis) > b.manager.cfg.MicroVMVCPUs*1000 ||
		int(requirements.MemoryBytes/mib) > b.manager.cfg.MicroVMMemoryMiB ||
		int(requirements.DiskBytes/mib) > b.manager.cfg.MicroVMWorkspaceSizeMiB {
		return fmt.Errorf("SecondBox Firecracker assignment exceeds local immutable profile capacity")
	}
	if _, err := b.compileAssignmentNetworkPolicy(assignment); err != nil {
		return err
	}
	if _, err := b.assignmentGuestProtocolStart(assignment); err != nil {
		return err
	}
	if err := b.checkWorkspaceAdmission(ctx, requirements.DiskBytes); err != nil {
		return fmt.Errorf("SecondBox Firecracker assignment storage pressure: %w", err)
	}
	return nil
}

func (b *AssignmentBackend) checkWorkspaceAdmission(ctx context.Context, requestedBytes uint64) error {
	return b.storagePressure.CheckAdmission(ctx, requestedBytes)
}

// StartAssignment launches one already-validated profile-resolved assignment.
func (b *AssignmentBackend) StartAssignment(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (result runnercontrol.BackendInstance, resultErr error) {
	if assignment == nil || assignment.Fence == nil || assignment.Requirements == nil {
		return runnercontrol.BackendInstance{}, fmt.Errorf("SecondBox Firecracker assignment is incomplete")
	}
	b.mu.Lock()
	if active, ok := b.assignments[assignment.Fence.AssignmentId]; ok {
		if sameAssignmentFence(active.fence, assignment.Fence) {
			b.mu.Unlock()
			return runnercontrol.BackendInstance{
				BackendKind:      "firecracker",
				BackendReference: active.backendReference,
			}, nil
		}
		b.mu.Unlock()
		return runnercontrol.BackendInstance{}, fmt.Errorf("SecondBox Firecracker assignment ID was reused with different fencing")
	}
	for _, active := range b.assignments {
		if active.fence.SandboxId == assignment.Fence.SandboxId {
			b.mu.Unlock()
			return runnercontrol.BackendInstance{}, fmt.Errorf("SecondBox Firecracker sandbox already has an unfenced assignment")
		}
	}
	b.mu.Unlock()

	// Admission dominates start latency under concurrency: measured p50 rose
	// from 79 ms at one concurrent assignment to 5,852 ms at thirty-two, while
	// every other startup stage stayed flat. Record when each assignment enters
	// the backend and how long each admission phase takes, so queueing ahead of
	// this call can be told apart from work inside it.
	admissionTimer := newAssignmentAdmissionTimer(
		assignment.Fence.AssignmentId, assignment.Fence.SandboxId,
	)
	admissionTimer.mark("dedupe_scanned")
	if err := b.ValidateAssignment(ctx, assignment); err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	admissionTimer.mark("assignment_validated")
	if err := b.storagePressure.Reserve(
		ctx,
		assignment.Fence.AssignmentId,
		assignment.Requirements.DiskBytes,
	); err != nil {
		return runnercontrol.BackendInstance{}, fmt.Errorf(
			"SecondBox Firecracker assignment storage reservation: %w",
			err,
		)
	}
	admissionTimer.mark("storage_reserved")
	reservationHeld := true
	releaseReservationOnReturn := true
	defer func() {
		if reservationHeld && releaseReservationOnReturn {
			resultErr = errors.Join(
				resultErr,
				b.storagePressure.Release(context.Background(), assignment.Fence.AssignmentId),
			)
		}
	}()
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	const mib = uint64(1 << 20)
	requirements := assignment.Requirements
	guestStart, err := b.assignmentGuestProtocolStart(assignment)
	if err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	compiledNetworkPolicy, err := b.compileAssignmentNetworkPolicy(assignment)
	if err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	admissionTimer.mark("guest_start_resolved")
	workspaceAttachment, err := b.manager.workspaceStore.Open(
		ctx,
		assignment.WorkspaceId,
		assignment.Fence.SandboxGeneration,
	)
	if err != nil {
		return runnercontrol.BackendInstance{}, fmt.Errorf(
			"SecondBox Firecracker resolve Workspace attachment: %w",
			err,
		)
	}
	admissionTimer.mark("workspace_opened")
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_WORKSPACE_ATTACH); err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, workspaceAttachment.Close())
		}
	}()
	backendReference, err := b.manager.createAndStart(ctx, assignment.Fence.SandboxId, runtimemanager.StartOpts{
		CompartmentID:           assignment.Fence.InstanceId,
		WorkspaceAttachment:     workspaceAttachment,
		ShapeFingerprint:        assignment.ProfileRevisionId,
		SandboxGeneration:       assignment.Fence.SandboxGeneration,
		GuestBuildID:            guestStart.GuestBuildID,
		ImageManifestDigest:     guestStart.ImageManifestDigest,
		ToolchainManifestDigest: guestStart.ToolchainManifestDigest,
		MandatoryGuestFeatures:  guestStart.MandatoryFeatures,
		RuntimeClass:            runtimemanager.RuntimeClassToolExecutor,
		SandboxPolicy: &runtimemanager.SandboxRuntimePolicy{
			VCPUs:             int(requirements.VcpuCount),
			CPUMillis:         int(requirements.VcpuMillis),
			MemoryMiB:         int(requirements.MemoryBytes / mib),
			WorkspaceSizeMiB:  int(requirements.DiskBytes / mib),
			ProcessLimit:      int(requirements.ProcessLimit),
			WorkspaceWritable: true,
			SharedReadOnly:    true,
		},
		NetworkPolicy: compiledNetworkPolicy,
		RequestID:     assignment.GetCorrelation().GetRequestId(),
		OperationID:   assignment.GetCorrelation().GetOperationId(),
		LeaseID:       assignment.GetCorrelation().GetLeaseId(),
		AssignmentID:  assignment.Fence.AssignmentId,
		StartupProgress: func(stage runtimemanager.StartupStage) error {
			switch stage {
			case runtimemanager.StartupStageNetworkReady:
				return progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_NETWORK_SETUP)
			case runtimemanager.StartupStageComputeStarted:
				return progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_FIRECRACKER_LAUNCH)
			case runtimemanager.StartupStageGuestNegotiated:
				return progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_GUEST_NEGOTIATION)
			default:
				return fmt.Errorf("SecondBox runtime reported unknown startup stage %q", stage)
			}
		},
	})
	if err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	cleanupLaunchFailure := func(progressErr error) (runnercontrol.BackendInstance, error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupErr := b.manager.StopAndRemove(cleanupCtx, backendReference)
		if cleanupErr != nil {
			releaseReservationOnReturn = false
		}
		return runnercontrol.BackendInstance{}, errors.Join(progressErr, cleanupErr)
	}
	if err := b.manager.NegotiateAssignmentGuest(ctx, backendReference, assignment.Fence.AssignmentId, guestStart); err != nil {
		return cleanupLaunchFailure(err)
	}
	b.mu.Lock()
	b.assignments[assignment.Fence.AssignmentId] = activeRunnerAssignment{
		fence: &runnerprotocol.AssignmentFence{
			AssignmentId:      assignment.Fence.AssignmentId,
			SandboxId:         assignment.Fence.SandboxId,
			InstanceId:        assignment.Fence.InstanceId,
			SandboxGeneration: assignment.Fence.SandboxGeneration,
			FencingToken:      append([]byte(nil), assignment.Fence.FencingToken...),
		},
		correlation: &runnerprotocol.Correlation{
			RequestId:         assignment.GetCorrelation().GetRequestId(),
			OperationId:       assignment.GetCorrelation().GetOperationId(),
			LeaseId:           assignment.GetCorrelation().GetLeaseId(),
			SandboxId:         assignment.Fence.SandboxId,
			InstanceId:        assignment.Fence.InstanceId,
			SandboxGeneration: assignment.Fence.SandboxGeneration,
			AssignmentId:      assignment.Fence.AssignmentId,
			RunnerId:          b.manager.runnerID,
		},
		backendReference: backendReference,
	}
	b.mu.Unlock()
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY); err != nil {
		b.mu.Lock()
		delete(b.assignments, assignment.Fence.AssignmentId)
		b.mu.Unlock()
		return cleanupLaunchFailure(err)
	}
	reservationHeld = false
	return runnercontrol.BackendInstance{
		BackendKind:      "firecracker",
		BackendReference: backendReference,
	}, nil
}

func cloneAssignmentFence(
	fence *runnerprotocol.AssignmentFence,
) *runnerprotocol.AssignmentFence {
	return &runnerprotocol.AssignmentFence{
		AssignmentId: fence.AssignmentId, SandboxId: fence.SandboxId,
		InstanceId: fence.InstanceId, SandboxGeneration: fence.SandboxGeneration,
		FencingToken: bytes.Clone(fence.FencingToken),
	}
}

func (b *AssignmentBackend) compileAssignmentNetworkPolicy(
	assignment *runnerprotocol.AssignmentCommand,
) (*networkpolicy.CompiledPolicy, error) {
	if b.manager.networkPolicy == nil {
		return nil, fmt.Errorf("SecondBox Firecracker host network policy enforcement is unavailable")
	}
	policy, err := networkpolicy.FromRunnerProtocol(assignment.NetworkPolicy)
	if err != nil {
		return nil, err
	}
	compiled, err := networkpolicy.Compile(policy, b.manager.networkPolicyCompileOptions())
	if err != nil {
		return nil, fmt.Errorf("SecondBox Firecracker assignment network policy: %w", err)
	}
	return compiled, nil
}

// FenceAssignment stops only the exact generation and opaque token assigned.
func (b *AssignmentBackend) FenceAssignment(
	ctx context.Context,
	command *runnerprotocol.FenceCommand,
) (runnercontrol.FenceEvidence, error) {
	if command == nil || command.Fence == nil {
		return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox Firecracker fence identity is required")
	}
	b.mu.Lock()
	active, ok := b.assignments[command.Fence.AssignmentId]
	b.mu.Unlock()
	if !ok {
		return runnercontrol.FenceEvidence{
			Result:                    runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED,
			TerminationEvidenceDigest: fenceTerminationEvidenceDigest(command.Fence),
		}, nil
	}
	if !sameAssignmentFence(active.fence, command.Fence) {
		return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox Firecracker fence token or generation mismatch")
	}
	if err := b.manager.StopAndRemove(ctx, active.backendReference); err != nil {
		return runnercontrol.FenceEvidence{}, err
	}
	b.mu.Lock()
	delete(b.assignments, command.Fence.AssignmentId)
	b.mu.Unlock()
	if err := b.storagePressure.Release(ctx, command.Fence.AssignmentId); err != nil {
		return runnercontrol.FenceEvidence{}, fmt.Errorf(
			"SecondBox Firecracker fence storage release: %w",
			err,
		)
	}
	return runnercontrol.FenceEvidence{
		Result:                    runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
		TerminationEvidenceDigest: fenceTerminationEvidenceDigest(command.Fence),
	}, nil
}

func fenceTerminationEvidenceDigest(fence *runnerprotocol.AssignmentFence) string {
	digest := sha256.Sum256([]byte(
		fence.AssignmentId + "\x00" +
			fence.InstanceId + "\x00" +
			fmt.Sprintf("%d", fence.SandboxGeneration),
	))
	return "sha256:" + hex.EncodeToString(digest[:])
}

type assignmentGuestProtocolStart struct {
	GuestBuildID            string
	ImageManifestDigest     string
	ToolchainManifestDigest string
	MandatoryFeatures       []string
}

func (b *AssignmentBackend) assignmentGuestProtocolStart(assignment *runnerprotocol.AssignmentCommand) (assignmentGuestProtocolStart, error) {
	manifestPath := filepath.Join(filepath.Dir(b.manager.cfg.MicroVMKernelPath), "manifest.json")
	manifest, err := loadSignedArtifactManifest(manifestPath)
	if err != nil {
		return assignmentGuestProtocolStart{}, fmt.Errorf("SecondBox Firecracker assignment signed compatibility metadata: %w", err)
	}
	expectedKeyID := b.manager.cfg.MicroVMPublicKeySHA256
	if len(assignment.Assets) != 2 {
		return assignmentGuestProtocolStart{}, fmt.Errorf("SecondBox Firecracker assignment must select exactly runtime and toolchain assets")
	}
	runtimeAsset, err := matchSignedAssignmentComponent(
		assignment.Assets,
		manifest.RuntimeBundle,
		expectedKeyID,
		manifest,
	)
	if err != nil {
		return assignmentGuestProtocolStart{}, fmt.Errorf("SecondBox Firecracker runtime asset: %w", err)
	}
	toolchainAsset, err := matchSignedAssignmentComponent(
		assignment.Assets,
		manifest.ToolchainBundle,
		expectedKeyID,
		manifest,
	)
	if err != nil {
		return assignmentGuestProtocolStart{}, fmt.Errorf("SecondBox Firecracker toolchain asset: %w", err)
	}
	if runtimeAsset == toolchainAsset {
		return assignmentGuestProtocolStart{}, fmt.Errorf("SecondBox Firecracker runtime and toolchain assets must be distinct")
	}
	if assignment.Assets[runtimeAsset].GuestProtocolGeneration !=
		assignment.Assets[toolchainAsset].GuestProtocolGeneration {
		return assignmentGuestProtocolStart{}, fmt.Errorf("SecondBox Firecracker runtime and toolchain guest generations differ")
	}
	features := mergeUniqueStrings(
		manifest.RuntimeBundle.MandatoryGuestFeatures,
		manifest.ToolchainBundle.MandatoryGuestFeatures,
	)
	return assignmentGuestProtocolStart{
		GuestBuildID:            manifest.ArtifactVersion,
		ImageManifestDigest:     manifest.RuntimeBundle.ManifestDigest,
		ToolchainManifestDigest: manifest.ToolchainBundle.ManifestDigest,
		MandatoryFeatures:       features,
	}, nil
}

func mergeUniqueStrings(groups ...[]string) []string {
	var merged []string
	seen := make(map[string]bool)
	for _, group := range groups {
		for _, value := range group {
			if seen[value] {
				continue
			}
			seen[value] = true
			merged = append(merged, value)
		}
	}
	return merged
}

func matchSignedAssignmentComponent(
	assets []*runnerprotocol.SignedAssetReference,
	component signedArtifactComponent,
	expectedKeyID string,
	manifest signedArtifactManifest,
) (int, error) {
	for index, asset := range assets {
		if asset == nil ||
			asset.ArtifactId != component.ArtifactID ||
			asset.ManifestDigest != component.ManifestDigest {
			continue
		}
		if asset.SignatureKeyId != expectedKeyID ||
			asset.Architecture != manifest.Architecture ||
			asset.GuestProtocolGeneration < manifest.GuestProtocol.Minimum ||
			asset.GuestProtocolGeneration > manifest.GuestProtocol.Maximum ||
			!equalStringSets(asset.MandatoryGuestFeatures, component.MandatoryGuestFeatures) {
			return -1, fmt.Errorf("assignment trust or compatibility evidence does not match the signed component")
		}
		for _, name := range asset.MandatoryGuestFeatures {
			if _, err := guestFeatureFromContractName(name); err != nil {
				return -1, err
			}
		}
		return index, nil
	}
	return -1, fmt.Errorf("assignment does not select signed component %q", component.ArtifactID)
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func loadSignedArtifactManifest(path string) (signedArtifactManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return signedArtifactManifest{}, err
	}
	var manifest signedArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return signedArtifactManifest{}, fmt.Errorf("decode signed artifact manifest: %w", err)
	}
	if strings.TrimSpace(manifest.ArtifactVersion) == "" {
		return signedArtifactManifest{}, fmt.Errorf("signed artifact manifest is missing artifactVersion")
	}
	if manifest.Architecture != runtime.GOARCH {
		return signedArtifactManifest{}, fmt.Errorf(
			"signed artifact architecture %q does not match runner architecture %q",
			manifest.Architecture,
			runtime.GOARCH,
		)
	}
	if manifest.GuestProtocol.Minimum == 0 ||
		manifest.GuestProtocol.Maximum == 0 ||
		manifest.GuestProtocol.Minimum > 1 ||
		manifest.GuestProtocol.Maximum < 1 {
		return signedArtifactManifest{}, fmt.Errorf(
			"signed artifact guest protocol range %d..%d is incompatible with runner generation 1",
			manifest.GuestProtocol.Minimum,
			manifest.GuestProtocol.Maximum,
		)
	}
	for label, component := range map[string]signedArtifactComponent{
		"runtime": manifest.RuntimeBundle, "toolchain": manifest.ToolchainBundle,
	} {
		if strings.TrimSpace(component.ArtifactID) == "" ||
			!strings.HasPrefix(component.ManifestDigest, "sha256:") ||
			len(component.ManifestDigest) != len("sha256:")+sha256.Size*2 ||
			filepath.Base(component.Path) != component.Path ||
			component.Path == "." {
			return signedArtifactManifest{}, fmt.Errorf("signed artifact %s component metadata is incomplete", label)
		}
		actual, err := fileSHA256(filepath.Join(filepath.Dir(path), component.Path))
		if err != nil {
			return signedArtifactManifest{}, fmt.Errorf("signed artifact %s component manifest: %w", label, err)
		}
		if "sha256:"+actual != component.ManifestDigest {
			return signedArtifactManifest{}, fmt.Errorf("signed artifact %s component manifest digest mismatch", label)
		}
	}
	if manifest.RuntimeBundle.ArtifactID == manifest.ToolchainBundle.ArtifactID ||
		manifest.RuntimeBundle.ManifestDigest == manifest.ToolchainBundle.ManifestDigest {
		return signedArtifactManifest{}, fmt.Errorf("signed runtime and toolchain components must be distinct")
	}
	return manifest, nil
}

func sameAssignmentFence(left, right *runnerprotocol.AssignmentFence) bool {
	return left != nil &&
		right != nil &&
		left.AssignmentId == right.AssignmentId &&
		left.SandboxId == right.SandboxId &&
		left.InstanceId == right.InstanceId &&
		left.SandboxGeneration == right.SandboxGeneration &&
		bytes.Equal(left.FencingToken, right.FencingToken)
}
