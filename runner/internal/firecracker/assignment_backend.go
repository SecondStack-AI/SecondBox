package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"google.golang.org/protobuf/proto"
)

var (
	restoreCheckpointIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
	restoreSHA256Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const maxStoragePressureCleanupBatch = 16

type activeRunnerAssignment struct {
	fence                 *runnerprotocol.AssignmentFence
	correlation           *runnerprotocol.Correlation
	backendReference      string
	compatibility         map[string]string
	workspaceAttachmentID string
	checkpointCreated     bool
}

type releasedWorkspaceCleanup struct {
	sandboxID    string
	attachmentID string
}

type restoredCheckpoint struct {
	fence           *runnerprotocol.AssignmentFence
	storageObjectID string
	sha256          string
	sizeBytes       uint64
	compatibility   map[string]string
	path            string
	complete        bool
	deadlineUnixMs  uint64
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
	manager                 *Manager
	storagePressure         *storagePressureController
	restoreSpoolPressure    *storagePressureController
	mu                      sync.Mutex
	assignments             map[string]activeRunnerAssignment
	restores                map[string]*restoredCheckpoint
	releasedWorkspaces      map[string]releasedWorkspaceCleanup
	removeReleasedWorkspace func(context.Context, string, string) error
	instanceTerminals       chan runnercontrol.BackendInstanceTerminal
}

// NewAssignmentBackend exposes the Firecracker compute conformance port.
func NewAssignmentBackend(manager *Manager) (*AssignmentBackend, error) {
	if manager == nil || manager.cfg == nil {
		return nil, fmt.Errorf("SecondBox Firecracker assignment backend requires a manager")
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
	restoreSpoolPressure, err := newConfiguredRestoreSpoolPressureController(manager.cfg, emitStoragePressure)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Firecracker assignment backend restore spool pressure: %w", err)
	}
	return &AssignmentBackend{
		manager:                 manager,
		storagePressure:         storagePressure,
		restoreSpoolPressure:    restoreSpoolPressure,
		assignments:             make(map[string]activeRunnerAssignment),
		restores:                make(map[string]*restoredCheckpoint),
		releasedWorkspaces:      make(map[string]releasedWorkspaceCleanup),
		removeReleasedWorkspace: manager.removeReleasedWorkspace,
		instanceTerminals:       make(chan runnercontrol.BackendInstanceTerminal, manager.cfg.MicroVMMaxConcurrentGlobal),
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
		cfg.MicroVMJailerUID < 1 ||
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
	workspaceInfo, err := os.Stat(cfg.MicroVMWorkspaceDir)
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness workspace storage %q: %w", cfg.MicroVMWorkspaceDir, err)
	}
	if !workspaceInfo.IsDir() {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness workspace storage %q is not a directory", cfg.MicroVMWorkspaceDir)
	}
	if strings.EqualFold(cfg.MicroVMWorkspaceBackend, "dm-thin") {
		if _, err := os.Stat(cfg.MicroVMThinPoolDevice); err != nil {
			return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness dm-thin pool: %w", err)
		}
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
	if err := b.cleanupExpiredRestores(ctx, uint64(time.Now().UnixMilli())); err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness expired restore cleanup: %w", err)
	}
	restoreSpoolState, err := b.restoreSpoolPressure.Observe(ctx)
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness restore spool pressure: %w", err)
	}
	if restoreSpoolState == storagePressureStateAdmissionDenied {
		return runnercontrol.BackendReadiness{}, fmt.Errorf(
			"SecondBox Firecracker readiness restore spool pressure: %w",
			ErrStoragePressureAdmissionDenied,
		)
	}
	kernelRelease, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return runnercontrol.BackendReadiness{}, fmt.Errorf("SecondBox Firecracker readiness host kernel evidence: %w", err)
	}
	return runnercontrol.BackendReadiness{
		Architecture: runtime.GOARCH,
		Capacity: &runnerprotocol.Capacity{
			VcpuMillis:  uint32(cfg.MicroVMVCPUs * cfg.MicroVMMaxConcurrentGlobal * 1000),
			MemoryBytes: uint64(cfg.MicroVMMemoryBudgetMiB) << 20,
			DiskBytes:   uint64(cfg.MicroVMWorkspaceSizeMiB*cfg.MicroVMMaxConcurrentGlobal) << 20,
			Instances:   uint32(cfg.MicroVMMaxConcurrentGlobal),
			Operations:  uint32(cfg.MicroVMMaxConcurrentGlobal),
		},
		Reserved: &runnerprotocol.Capacity{},
		Capabilities: &runnerprotocol.RunnerCapabilities{
			Architecture:       runtime.GOARCH,
			KernelRelease:      strings.TrimSpace(string(kernelRelease)),
			FirecrackerVersion: expectedFirecrackerVersionString(),
			KvmReady:           true,
			JailerReady:        true,
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
		"evidence":         true,
		"firecracker":      true,
		"jailer":           true,
		"kvm":              true,
		"signed-artifacts": true,
		"tap":              true,
		"vsock":            true,
	}
	for _, capability := range requirements.RequiredCapabilities {
		if !supportedCapabilities[capability] {
			return fmt.Errorf("SecondBox Firecracker assignment requires unsupported capability %q", capability)
		}
	}
	if int(requirements.VcpuCount) > b.manager.cfg.MicroVMVCPUs ||
		int(requirements.MemoryBytes/mib) > b.manager.cfg.MicroVMMemoryMiB ||
		int(requirements.DiskBytes/mib) > b.manager.cfg.MicroVMWorkspaceSizeMiB {
		return fmt.Errorf("SecondBox Firecracker assignment exceeds local immutable profile capacity")
	}
	if _, err := b.compileAssignmentNetworkPolicy(assignment); err != nil {
		return err
	}
	guestStart, err := b.assignmentGuestProtocolStart(assignment)
	if err != nil {
		return err
	}
	if strings.TrimSpace(assignment.SourceCheckpointId) != "" {
		b.mu.Lock()
		restore := b.restores[assignment.SourceCheckpointId]
		b.mu.Unlock()
		expectedCompatibility, err := assignmentCheckpointCompatibility(assignment, guestStart)
		if err != nil {
			return err
		}
		if restore == nil || !restore.complete ||
			!sameAssignmentFence(restore.fence, assignment.Fence) ||
			restore.sizeBytes != requirements.DiskBytes ||
			!mapsEqual(restore.compatibility, expectedCompatibility) {
			return fmt.Errorf("SecondBox Firecracker assignment source checkpoint is incomplete or incompatible")
		}
	}
	if err := b.checkWorkspaceAdmission(ctx, requirements.DiskBytes); err != nil {
		return fmt.Errorf("SecondBox Firecracker assignment storage pressure: %w", err)
	}
	return nil
}

func (b *AssignmentBackend) checkWorkspaceAdmission(ctx context.Context, requestedBytes uint64) error {
	err := b.storagePressure.CheckAdmission(ctx, requestedBytes)
	if !errors.Is(err, ErrStoragePressureAdmissionDenied) {
		return err
	}
	if cleanupErr := b.cleanupReleasedWorkspaces(ctx); cleanupErr != nil {
		return errors.Join(err, cleanupErr)
	}
	return b.storagePressure.CheckAdmission(ctx, requestedBytes)
}

func (b *AssignmentBackend) cleanupReleasedWorkspaces(ctx context.Context) error {
	b.mu.Lock()
	candidates := make(map[string]releasedWorkspaceCleanup, maxStoragePressureCleanupBatch)
	for assignmentID, candidate := range b.releasedWorkspaces {
		candidates[assignmentID] = candidate
		if len(candidates) == maxStoragePressureCleanupBatch {
			break
		}
	}
	b.mu.Unlock()
	for assignmentID, candidate := range candidates {
		if err := b.removeReleasedWorkspace(ctx, candidate.sandboxID, candidate.attachmentID); err != nil {
			record := runnerevidence.NewRecord(
				runnerevidence.EventStoragePressure,
				"failed",
				"storage_pressure_cleanup_failed",
				time.Now().UTC(),
			)
			record.AssignmentID = assignmentID
			record.SandboxID = candidate.sandboxID
			record.RunnerID = b.manager.runnerID
			return errors.Join(err, b.manager.evidenceSink().Emit(ctx, record))
		}
		b.mu.Lock()
		delete(b.releasedWorkspaces, assignmentID)
		b.mu.Unlock()
	}
	return nil
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

	if err := b.ValidateAssignment(ctx, assignment); err != nil {
		return runnercontrol.BackendInstance{}, err
	}
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
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_WORKSPACE_MATERIALIZE); err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_NETWORK_SETUP); err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	guestStart, err := b.assignmentGuestProtocolStart(assignment)
	if err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	checkpointCompatibility, err := assignmentCheckpointCompatibility(assignment, guestStart)
	if err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	compiledNetworkPolicy, err := b.compileAssignmentNetworkPolicy(assignment)
	if err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	workspaceCheckpointPath := ""
	if assignment.SourceCheckpointId != "" {
		b.mu.Lock()
		restore := b.restores[assignment.SourceCheckpointId]
		if restore != nil && restore.complete {
			workspaceCheckpointPath = restore.path
		}
		b.mu.Unlock()
		if workspaceCheckpointPath == "" {
			return runnercontrol.BackendInstance{}, fmt.Errorf("SecondBox Firecracker assignment restore bytes are unavailable")
		}
	}
	backendReference, err := b.manager.createAndStart(ctx, assignment.Fence.SandboxId, runtimemanager.StartOpts{
		CompartmentID:           assignment.Fence.InstanceId,
		WorkspaceAttachmentID:   fmt.Sprintf("%s-generation-%d", assignment.Fence.InstanceId, assignment.Fence.SandboxGeneration),
		WorkspaceCheckpointPath: workspaceCheckpointPath,
		ShapeFingerprint:        assignment.ProfileRevisionId,
		SandboxGeneration:       assignment.Fence.SandboxGeneration,
		GuestBuildID:            guestStart.GuestBuildID,
		ImageManifestDigest:     guestStart.ImageManifestDigest,
		ToolchainManifestDigest: guestStart.ToolchainManifestDigest,
		MandatoryGuestFeatures:  guestStart.MandatoryFeatures,
		RuntimeClass:            runtimemanager.RuntimeClassToolExecutor,
		SandboxPolicy: &runtimemanager.SandboxRuntimePolicy{
			VCPUs:             int(requirements.VcpuCount),
			MemoryMiB:         int(requirements.MemoryBytes / mib),
			WorkspaceSizeMiB:  int(requirements.DiskBytes / mib),
			WorkspaceWritable: true,
			SharedReadOnly:    true,
		},
		NetworkPolicy: compiledNetworkPolicy,
		RequestID:     assignment.GetCorrelation().GetRequestId(),
		OperationID:   assignment.GetCorrelation().GetOperationId(),
		LeaseID:       assignment.GetCorrelation().GetLeaseId(),
		AssignmentID:  assignment.Fence.AssignmentId,
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
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_FIRECRACKER_LAUNCH); err != nil {
		return cleanupLaunchFailure(err)
	}
	if err := b.manager.NegotiateAssignmentGuest(ctx, backendReference, assignment.Fence.AssignmentId, guestStart); err != nil {
		return cleanupLaunchFailure(err)
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_GUEST_NEGOTIATION); err != nil {
		return cleanupLaunchFailure(err)
	}
	if assignment.SourceCheckpointId != "" {
		if err := b.consumeRestore(ctx, assignment.SourceCheckpointId); err != nil {
			return cleanupLaunchFailure(err)
		}
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
		compatibility:    checkpointCompatibility,
		workspaceAttachmentID: fmt.Sprintf(
			"%s-generation-%d",
			assignment.Fence.InstanceId,
			assignment.Fence.SandboxGeneration,
		),
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

// BeginRestore opens or resumes one assignment-fenced checkpoint spool.
func (b *AssignmentBackend) BeginRestore(
	ctx context.Context,
	begin *runnerprotocol.RestoreBegin,
) (resultErr error) {
	if begin == nil || begin.Fence == nil ||
		!restoreCheckpointIDPattern.MatchString(begin.CheckpointId) ||
		begin.StorageObjectId == "" || !restoreSHA256Pattern.MatchString(begin.Sha256) ||
		begin.SizeBytes == 0 || begin.DeadlineUnixMs == 0 ||
		time.Now().UnixMilli() >= int64(begin.DeadlineUnixMs) ||
		begin.Compatibility["architecture"] != runtime.GOARCH ||
		begin.Compatibility["backend"] != "firecracker" ||
		begin.Compatibility["workspaceFormat"] != "ext4" {
		return fmt.Errorf("SecondBox Firecracker restore authority is incomplete or incompatible")
	}
	maximumBytes := uint64(b.manager.cfg.MicroVMWorkspaceSizeMiB) * 1024 * 1024
	if begin.SizeBytes > maximumBytes {
		return fmt.Errorf("SecondBox Firecracker restore exceeds local workspace capacity")
	}
	if err := b.cleanupExpiredRestores(ctx, uint64(time.Now().UnixMilli())); err != nil {
		return err
	}
	b.mu.Lock()
	existing := b.restores[begin.CheckpointId]
	b.mu.Unlock()
	if existing != nil {
		if existing.storageObjectID != begin.StorageObjectId ||
			existing.sha256 != begin.Sha256 || existing.sizeBytes != begin.SizeBytes ||
			!mapsEqual(existing.compatibility, begin.Compatibility) {
			return fmt.Errorf("SecondBox Firecracker restore checkpoint identity was reused")
		}
		if !sameAssignmentFence(existing.fence, begin.Fence) {
			if !existing.complete {
				return fmt.Errorf("SecondBox Firecracker partial restore belongs to another assignment")
			}
			b.mu.Lock()
			existing.fence = cloneAssignmentFence(begin.Fence)
			existing.deadlineUnixMs = begin.DeadlineUnixMs
			b.mu.Unlock()
		}
		return nil
	}
	reservationID := "restore:" + begin.CheckpointId
	restoreDirectory := b.manager.cfg.MicroVMCheckpointRestoreSpoolDir
	partialPath := filepath.Join(restoreDirectory, begin.CheckpointId+".partial")
	verifiedPath := filepath.Join(restoreDirectory, begin.CheckpointId+".verified")
	if err := b.restoreSpoolPressure.Reserve(ctx, reservationID, begin.SizeBytes); err != nil {
		return fmt.Errorf("SecondBox Firecracker restore spool admission: %w", err)
	}
	keepReservation := false
	defer func() {
		if !keepReservation {
			cleanupErr := errors.Join(
				removeRestoreSpoolPath(partialPath),
				removeRestoreSpoolPath(verifiedPath),
			)
			resultErr = errors.Join(resultErr, cleanupErr)
			if cleanupErr == nil {
				resultErr = errors.Join(
					resultErr,
					b.restoreSpoolPressure.Release(context.Background(), reservationID),
				)
			}
		}
	}()
	if err := os.MkdirAll(restoreDirectory, 0o700); err != nil {
		return fmt.Errorf("SecondBox Firecracker restore spool directory: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	path := partialPath
	complete := false
	if info, err := os.Stat(verifiedPath); err == nil {
		if uint64(info.Size()) != begin.SizeBytes {
			return fmt.Errorf("SecondBox Firecracker verified restore size differs")
		}
		digest, err := fileSHA256(verifiedPath)
		if err != nil {
			return err
		}
		if digest != begin.Sha256 {
			return fmt.Errorf("SecondBox Firecracker verified restore digest differs")
		}
		path = verifiedPath
		complete = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox Firecracker restore spool inspection: %w", err)
	} else if info, err := os.Stat(partialPath); err == nil {
		if uint64(info.Size()) > begin.SizeBytes {
			return fmt.Errorf("SecondBox Firecracker partial restore exceeds declared size")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox Firecracker partial restore inspection: %w", err)
	}
	b.restores[begin.CheckpointId] = &restoredCheckpoint{
		fence:           cloneAssignmentFence(begin.Fence),
		storageObjectID: begin.StorageObjectId, sha256: begin.Sha256,
		sizeBytes: begin.SizeBytes, compatibility: cloneStringMap(begin.Compatibility),
		path: path, complete: complete,
		deadlineUnixMs: begin.DeadlineUnixMs,
	}
	keepReservation = true
	return nil
}

func removeRestoreSpoolPath(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (b *AssignmentBackend) cleanupExpiredRestores(ctx context.Context, nowUnixMs uint64) error {
	b.mu.Lock()
	var expired []*restoredCheckpoint
	var checkpointIDs []string
	for checkpointID, restore := range b.restores {
		if restore != nil && restore.deadlineUnixMs != 0 && restore.deadlineUnixMs <= nowUnixMs {
			expired = append(expired, restore)
			checkpointIDs = append(checkpointIDs, checkpointID)
		}
	}
	b.mu.Unlock()
	var cleanupErr error
	for index, restore := range expired {
		if err := os.Remove(restore.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		b.mu.Lock()
		delete(b.restores, checkpointIDs[index])
		b.mu.Unlock()
		cleanupErr = errors.Join(
			cleanupErr,
			b.restoreSpoolPressure.Release(ctx, "restore:"+checkpointIDs[index]),
		)
	}
	return cleanupErr
}

func (b *AssignmentBackend) consumeRestore(ctx context.Context, checkpointID string) error {
	b.mu.Lock()
	restore := b.restores[checkpointID]
	b.mu.Unlock()
	if restore == nil || !restore.complete {
		return fmt.Errorf("SecondBox Firecracker consumed restore is unavailable")
	}
	if err := os.Remove(restore.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox Firecracker consumed restore cleanup: %w", err)
	}
	b.mu.Lock()
	delete(b.restores, checkpointID)
	b.mu.Unlock()
	if err := b.restoreSpoolPressure.Release(ctx, "restore:"+checkpointID); err != nil {
		return fmt.Errorf("SecondBox Firecracker consumed restore pressure release: %w", err)
	}
	return nil
}

func cloneAssignmentFence(fence *runnerprotocol.AssignmentFence) *runnerprotocol.AssignmentFence {
	return &runnerprotocol.AssignmentFence{
		AssignmentId: fence.AssignmentId, SandboxId: fence.SandboxId,
		InstanceId: fence.InstanceId, SandboxGeneration: fence.SandboxGeneration,
		FencingToken: bytes.Clone(fence.FencingToken),
	}
}

// WriteRestoreChunk appends or idempotently replays bytes and verifies the terminal digest.
func (b *AssignmentBackend) WriteRestoreChunk(
	ctx context.Context,
	chunk *runnerprotocol.RestoreChunk,
) (resultErr error) {
	if chunk == nil || chunk.Fence == nil || !restoreCheckpointIDPattern.MatchString(chunk.CheckpointId) {
		return fmt.Errorf("SecondBox Firecracker restore chunk identity is incomplete")
	}
	b.mu.Lock()
	restore := b.restores[chunk.CheckpointId]
	if restore == nil || !sameAssignmentFence(restore.fence, chunk.Fence) ||
		restore.storageObjectID != chunk.StorageObjectId {
		b.mu.Unlock()
		return fmt.Errorf("SecondBox Firecracker restore chunk fence is stale")
	}
	defer func() {
		b.mu.Unlock()
		if resultErr == nil {
			return
		}
		cleanupErr := removeRestoreSpoolPath(restore.path)
		resultErr = errors.Join(resultErr, cleanupErr)
		if cleanupErr != nil {
			return
		}
		b.mu.Lock()
		delete(b.restores, chunk.CheckpointId)
		b.mu.Unlock()
		resultErr = errors.Join(
			resultErr,
			b.restoreSpoolPressure.Release(ctx, "restore:"+chunk.CheckpointId),
		)
	}()
	file, err := os.OpenFile(restore.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("SecondBox Firecracker restore spool open: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("SecondBox Firecracker restore spool stat: %w", err)
	}
	size := uint64(info.Size())
	if chunk.Offset > size || uint64(len(chunk.Data)) > restore.sizeBytes-chunk.Offset {
		return fmt.Errorf("SecondBox Firecracker restore chunk offset or size is invalid")
	}
	if chunk.Offset < size {
		existing := make([]byte, len(chunk.Data))
		count, err := file.ReadAt(existing, int64(chunk.Offset))
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("SecondBox Firecracker restore replay read: %w", err)
		}
		if count != len(chunk.Data) || !bytes.Equal(existing, chunk.Data) {
			return fmt.Errorf("SecondBox Firecracker restore replay content differs")
		}
	} else if len(chunk.Data) != 0 {
		if _, err := file.WriteAt(chunk.Data, int64(chunk.Offset)); err != nil {
			return fmt.Errorf("SecondBox Firecracker restore append: %w", err)
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("SecondBox Firecracker restore sync: %w", err)
		}
		size += uint64(len(chunk.Data))
	}
	if !chunk.EndOfObject {
		return nil
	}
	if size != restore.sizeBytes {
		return fmt.Errorf("SecondBox Firecracker restore ended before declared size")
	}
	digest, err := fileSHA256(restore.path)
	if err != nil {
		return err
	}
	if digest != restore.sha256 {
		return fmt.Errorf("SecondBox Firecracker restore digest differs")
	}
	if !restore.complete {
		verifiedPath := strings.TrimSuffix(restore.path, ".partial") + ".verified"
		if err := os.Rename(restore.path, verifiedPath); err != nil {
			return fmt.Errorf("SecondBox Firecracker restore publication: %w", err)
		}
		restore.path = verifiedPath
		restore.complete = true
	}
	return nil
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func assignmentCheckpointCompatibility(
	assignment *runnerprotocol.AssignmentCommand,
	guestStart assignmentGuestProtocolStart,
) (map[string]string, error) {
	var runtimeGeneration, toolchainGeneration uint32
	for _, asset := range assignment.Assets {
		if asset == nil {
			continue
		}
		switch asset.ManifestDigest {
		case guestStart.ImageManifestDigest:
			runtimeGeneration = asset.GuestProtocolGeneration
		case guestStart.ToolchainManifestDigest:
			toolchainGeneration = asset.GuestProtocolGeneration
		}
	}
	if runtimeGeneration == 0 || runtimeGeneration != toolchainGeneration {
		return nil, fmt.Errorf("SecondBox Firecracker checkpoint asset generations are incomplete")
	}
	features := append([]string(nil), guestStart.MandatoryFeatures...)
	sort.Strings(features)
	return map[string]string{
		"architecture":            runtime.GOARCH,
		"backend":                 "firecracker",
		"profileRevisionId":       assignment.ProfileRevisionId,
		"workspaceFormat":         "ext4",
		"runtimeManifestDigest":   guestStart.ImageManifestDigest,
		"toolchainManifestDigest": guestStart.ToolchainManifestDigest,
		"guestProtocolGeneration": strconv.FormatUint(uint64(runtimeGeneration), 10),
		"mandatoryGuestFeatures":  strings.Join(features, ","),
	}, nil
}

// CreateCheckpoint freezes and streams one active generation without exposing a local path.
func (b *AssignmentBackend) CreateCheckpoint(
	ctx context.Context,
	command *runnerprotocol.CheckpointCommand,
	emit func([]byte) error,
) (runnercontrol.CheckpointEvidence, error) {
	if command == nil || command.Fence == nil || command.CheckpointId == "" ||
		command.StorageObjectId == "" || command.MaximumSizeBytes == 0 {
		return runnercontrol.CheckpointEvidence{}, fmt.Errorf("SecondBox Firecracker checkpoint command is incomplete")
	}
	b.mu.Lock()
	active, ok := b.assignments[command.Fence.AssignmentId]
	b.mu.Unlock()
	if !ok || !sameAssignmentFence(active.fence, command.Fence) {
		return runnercontrol.CheckpointEvidence{}, fmt.Errorf("SecondBox Firecracker checkpoint fence is stale")
	}
	digest, sizeBytes, err := b.manager.StreamWorkspaceCheckpoint(
		ctx, active.backendReference, command.MaximumSizeBytes, emit,
	)
	if err != nil {
		return runnercontrol.CheckpointEvidence{}, err
	}
	b.mu.Lock()
	current, stillActive := b.assignments[command.Fence.AssignmentId]
	sameGeneration := stillActive && sameAssignmentFence(current.fence, command.Fence)
	if sameGeneration {
		current.checkpointCreated = true
		b.assignments[command.Fence.AssignmentId] = current
	}
	b.mu.Unlock()
	if !sameGeneration {
		return runnercontrol.CheckpointEvidence{}, fmt.Errorf("SecondBox Firecracker checkpoint generation was fenced before publication")
	}
	terminal := sha256.Sum256([]byte(
		command.CheckpointId + "\x00" + command.StorageObjectId + "\x00" + digest,
	))
	return runnercontrol.CheckpointEvidence{
		SHA256: digest, SizeBytes: sizeBytes,
		Compatibility:             cloneStringMap(active.compatibility),
		TerminationEvidenceDigest: "sha256:" + hex.EncodeToString(terminal[:]),
	}, nil
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
			Result: runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED,
		}, nil
	}
	if !sameAssignmentFence(active.fence, command.Fence) {
		return runnercontrol.FenceEvidence{}, fmt.Errorf("SecondBox Firecracker fence token or generation mismatch")
	}
	if err := b.manager.StopAndRemove(ctx, active.backendReference); err != nil {
		return runnercontrol.FenceEvidence{}, err
	}
	b.mu.Lock()
	if active.checkpointCreated {
		b.releasedWorkspaces[command.Fence.AssignmentId] = releasedWorkspaceCleanup{
			sandboxID:    active.fence.SandboxId,
			attachmentID: active.workspaceAttachmentID,
		}
	}
	delete(b.assignments, command.Fence.AssignmentId)
	b.mu.Unlock()
	if err := b.storagePressure.Release(ctx, command.Fence.AssignmentId); err != nil {
		return runnercontrol.FenceEvidence{}, fmt.Errorf(
			"SecondBox Firecracker fence storage release: %w",
			err,
		)
	}
	digest := sha256.Sum256([]byte(
		command.Fence.AssignmentId + "\x00" +
			command.Fence.InstanceId + "\x00" +
			fmt.Sprintf("%d", command.Fence.SandboxGeneration),
	))
	return runnercontrol.FenceEvidence{
		Result:                    runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
		TerminationEvidenceDigest: "sha256:" + hex.EncodeToString(digest[:]),
	}, nil
}

func (b *AssignmentBackend) verifyAssignmentAsset(assignment *runnerprotocol.AssignmentCommand) error {
	_, err := b.assignmentGuestProtocolStart(assignment)
	return err
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
