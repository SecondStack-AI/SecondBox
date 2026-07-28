// Package service coordinates durable Sandbox Environment lifecycle.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/pkg/contracts"
)

const (
	maxReferenceLength        = 500
	maxMetadataItems          = 100
	maxAllowedConnectionIDs   = 100
	maxArtifactBytes          = 2 << 30
	emptyWorkspaceContentHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	startObservationInterval  = 5 * time.Millisecond
)

// SandboxService owns durable Environment intent and generation-fenced compute coordination.
type SandboxService struct {
	store                ports.EnvironmentStore
	compute              ports.ComputeBackend
	executionRevoker     ports.ExecutionRevoker
	leaseTTL             time.Duration
	maxFileTransferBytes int64
	now                  func() time.Time
	newID                func(string) string
	metrics              Metrics
}

// Config supplies every Sandbox Service dependency explicitly.
type Config struct {
	Store                ports.EnvironmentStore
	Compute              ports.ComputeBackend
	ExecutionRevoker     ports.ExecutionRevoker
	LeaseTTL             time.Duration
	MaxFileTransferBytes int64
	Now                  func() time.Time
	NewID                func(string) string
}

// NewSandboxService constructs the Environment lifecycle coordinator.
func NewSandboxService(config Config) (*SandboxService, error) {
	if config.Store == nil {
		return nil, errors.New("Sandbox Service EnvironmentStore is required")
	}
	if config.Compute == nil {
		return nil, errors.New("Sandbox Service ComputeBackend is required")
	}
	if config.ExecutionRevoker == nil {
		return nil, errors.New("Sandbox Service ExecutionRevoker is required")
	}
	if config.LeaseTTL <= 0 {
		return nil, errors.New("Sandbox Service lease TTL must be positive")
	}
	if config.MaxFileTransferBytes <= 0 {
		return nil, errors.New("Sandbox Service file transfer limit must be positive")
	}
	if config.Now == nil {
		return nil, errors.New("Sandbox Service clock is required")
	}
	if config.NewID == nil {
		return nil, errors.New("Sandbox Service ID source is required")
	}
	return &SandboxService{
		store: config.Store, compute: config.Compute, executionRevoker: config.ExecutionRevoker,
		leaseTTL:             config.LeaseTTL,
		maxFileTransferBytes: config.MaxFileTransferBytes,
		now:                  config.Now, newID: config.NewID,
	}, nil
}

// NewOpaqueID returns an unpredictable prefixed durable identity.
func NewOpaqueID(prefix string) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("Sandbox Service random ID generation failed: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(value[:])
}

// Ready verifies both durable authority and privileged Sandbox Host readiness.
func (s *SandboxService) Ready(ctx context.Context) error {
	if err := s.store.Ping(ctx); err != nil {
		return err
	}
	return s.compute.Ready(ctx)
}

// ResolveEnvironment creates or returns durable Environment intent without starting compute.
func (s *SandboxService) ResolveEnvironment(ctx context.Context, request contracts.ResolveEnvironmentRequest) (contracts.ResolveEnvironmentResponse, error) {
	if err := validateResolveRequest(request); err != nil {
		return contracts.ResolveEnvironmentResponse{}, err
	}
	resourceClass, err := s.store.GetResourceClass(ctx, request.ResourceClassID)
	if err != nil {
		return contracts.ResolveEnvironmentResponse{}, fmt.Errorf("resolve Environment Resource Class: %w", err)
	}
	policy, err := s.store.GetLifecyclePolicy(ctx, request.LifecyclePolicyID)
	if err != nil {
		return contracts.ResolveEnvironmentResponse{}, fmt.Errorf("resolve Environment Lifecycle Policy: %w", err)
	}
	if resourceClass.MaxExposedPorts < 0 || policy.RetentionSeconds <= 0 {
		return contracts.ResolveEnvironmentResponse{}, errors.New("Sandbox Service persisted policy is invalid")
	}
	now := s.now().UTC()
	environmentID := s.newID("env")
	workspaceID := s.newID("wsp")
	environment := contracts.Environment{
		ContractVersion: contracts.ContractVersionV1,
		ID:              environmentID, TenantRef: request.TenantRef, SubjectRef: request.SubjectRef,
		EnvironmentKey: request.EnvironmentKey, WorkspaceID: workspaceID, ImageRef: request.ImageRef,
		ToolchainRef: request.ToolchainRef, ResourceClassID: request.ResourceClassID,
		LifecyclePolicyID: request.LifecyclePolicyID, DesiredState: contracts.DesiredStateStopped,
		State: contracts.EnvironmentStateStopped, ExposedPorts: []contracts.ExposedPort{},
		Metadata: cloneStringMap(request.Metadata), LastActivityAt: now, CreatedAt: now, UpdatedAt: now,
	}
	workspace := contracts.Workspace{
		ContractVersion: contracts.ContractVersionV1,
		ID:              workspaceID, TenantRef: request.TenantRef, SubjectRef: request.SubjectRef,
		StorageRef: "workspace:" + workspaceID, Generation: 1,
		RetainUntil: now.Add(time.Duration(policy.RetentionSeconds) * time.Second),
		CreatedAt:   now, UpdatedAt: now,
	}
	resolved, created, err := s.store.ResolveEnvironment(ctx, ports.ResolveEnvironmentInput{
		Environment: environment, Workspace: workspace,
	})
	if err != nil {
		return contracts.ResolveEnvironmentResponse{}, err
	}
	s.metrics.recordResolve(created)
	return contracts.ResolveEnvironmentResponse{Environment: resolved, Created: created}, nil
}

// GetEnvironment returns durable intent and its current replaceable Instance.
func (s *SandboxService) GetEnvironment(ctx context.Context, environmentID string) (contracts.LifecycleResponse, error) {
	if err := validateReference("environment ID", environmentID); err != nil {
		return contracts.LifecycleResponse{}, err
	}
	environment, err := s.store.GetEnvironment(ctx, environmentID)
	if err != nil {
		return contracts.LifecycleResponse{}, err
	}
	instance, err := s.store.GetCurrentInstance(ctx, environmentID)
	if err != nil {
		return contracts.LifecycleResponse{}, err
	}
	return contracts.LifecycleResponse{Environment: environment, Instance: instance}, nil
}

// GetWorkspaceUsage returns retained workspace quota and checkpoint usage for one subject.
func (s *SandboxService) GetWorkspaceUsage(ctx context.Context, tenantRef, subjectRef string) (contracts.WorkspaceUsage, error) {
	if err := validateReference("tenant reference", tenantRef); err != nil {
		return contracts.WorkspaceUsage{}, err
	}
	if err := validateReference("subject reference", subjectRef); err != nil {
		return contracts.WorkspaceUsage{}, err
	}
	return s.store.GetWorkspaceUsage(ctx, tenantRef, subjectRef)
}

// StartEnvironment prepares and starts one new fenced Instance generation.
func (s *SandboxService) StartEnvironment(ctx context.Context, environmentID string, expectedGeneration int64) (contracts.LifecycleResponse, error) {
	if err := validateReference("environment ID", environmentID); err != nil {
		return contracts.LifecycleResponse{}, err
	}
	now := s.now().UTC()
	started, err := s.store.BeginStart(ctx, environmentID, expectedGeneration, s.newID("ins"), now)
	if err != nil {
		return contracts.LifecycleResponse{}, err
	}
	if !started.Created {
		s.metrics.startsReused.Add(1)
		if started.Instance.State == contracts.InstanceStatePreparing {
			return s.waitForConcurrentStart(ctx, started)
		}
		return contracts.LifecycleResponse{Environment: started.Environment, Instance: &started.Instance}, nil
	}
	workspace, err := s.store.GetWorkspace(ctx, started.Environment.WorkspaceID)
	if err != nil {
		return contracts.LifecycleResponse{}, s.failStart(ctx, started, "workspace_unavailable", err)
	}
	resourceClass, err := s.store.GetResourceClass(ctx, started.Environment.ResourceClassID)
	if err != nil {
		return contracts.LifecycleResponse{}, s.failStart(ctx, started, "resource_class_unavailable", err)
	}
	computeRequest := ports.ComputeRequest{
		Environment: started.Environment, Workspace: workspace, ResourceClass: resourceClass, Instance: started.Instance,
	}
	prepared, err := s.compute.Prepare(ctx, computeRequest)
	if err != nil {
		return contracts.LifecycleResponse{}, s.failStart(ctx, started, "prepare_failed", err)
	}
	running, err := s.compute.Start(ctx, prepared, computeRequest)
	if err != nil {
		return contracts.LifecycleResponse{}, s.failStart(ctx, started, "start_failed", err)
	}
	if running.State != contracts.InstanceStateReady || strings.TrimSpace(running.BackendRef) == "" {
		return contracts.LifecycleResponse{}, s.failStartedCompute(ctx, started, running.BackendRef,
			errors.New("Sandbox Host start response is not ready"))
	}
	environment, err := s.store.MarkInstanceReady(
		ctx, environmentID, started.Instance.ID, started.Instance.Generation, running.BackendRef, s.now().UTC(),
	)
	if err != nil {
		return contracts.LifecycleResponse{}, s.failStartedCompute(ctx, started, running.BackendRef, err)
	}
	instance := started.Instance
	instance.State = contracts.InstanceStateReady
	instance.BackendRef = running.BackendRef
	instance.ReadyAt = environment.UpdatedAt
	instance.UpdatedAt = environment.UpdatedAt
	s.metrics.startsReady.Add(1)
	return contracts.LifecycleResponse{Environment: environment, Instance: &instance}, nil
}

// waitForConcurrentStart observes the one durable generation reserved by another caller.
func (s *SandboxService) waitForConcurrentStart(
	ctx context.Context,
	started ports.StartGenerationResult,
) (contracts.LifecycleResponse, error) {
	ticker := time.NewTicker(startObservationInterval)
	defer ticker.Stop()
	for {
		environment, err := s.store.GetEnvironment(ctx, started.Environment.ID)
		if err != nil {
			return contracts.LifecycleResponse{}, fmt.Errorf(
				"observe concurrent Environment start: %w",
				err,
			)
		}
		instance, err := s.store.GetCurrentInstance(ctx, started.Environment.ID)
		if err != nil {
			return contracts.LifecycleResponse{}, fmt.Errorf(
				"observe concurrent Environment Instance start: %w",
				err,
			)
		}
		if instance == nil ||
			environment.CurrentGeneration != started.Instance.Generation ||
			environment.CurrentInstanceID != started.Instance.ID ||
			instance.Generation != started.Instance.Generation ||
			instance.ID != started.Instance.ID {
			return contracts.LifecycleResponse{}, ports.ErrGenerationFenced
		}
		switch instance.State {
		case contracts.InstanceStateReady:
			return contracts.LifecycleResponse{
				Environment: environment,
				Instance:    instance,
			}, nil
		case contracts.InstanceStateFailed:
			return contracts.LifecycleResponse{}, fmt.Errorf(
				"concurrent Environment start failed: Instance failure code %q",
				instance.FailureCode,
			)
		case contracts.InstanceStatePreparing:
		default:
			return contracts.LifecycleResponse{}, fmt.Errorf(
				"concurrent Environment start entered unexpected Instance state %q",
				instance.State,
			)
		}
		select {
		case <-ctx.Done():
			return contracts.LifecycleResponse{}, fmt.Errorf(
				"wait for concurrent Environment start: %w",
				ctx.Err(),
			)
		case <-ticker.C:
		}
	}
}

// InspectEnvironment reconciles only the current Instance generation with Sandbox Host.
func (s *SandboxService) InspectEnvironment(ctx context.Context, environmentID string) (contracts.LifecycleResponse, error) {
	lifecycle, err := s.GetEnvironment(ctx, environmentID)
	if err != nil || lifecycle.Instance == nil {
		return lifecycle, err
	}
	identity := computeIdentity(*lifecycle.Instance)
	status, err := s.compute.Inspect(ctx, identity)
	if err != nil {
		return contracts.LifecycleResponse{}, err
	}
	switch status.State {
	case contracts.InstanceStateReady:
		if err := s.store.TouchEnvironment(ctx, environmentID, identity.Generation, s.now().UTC()); err != nil {
			return contracts.LifecycleResponse{}, err
		}
		return s.GetEnvironment(ctx, environmentID)
	case contracts.InstanceStateLost:
		environment, err := s.store.MarkInstanceLost(ctx, environmentID, identity.InstanceID, identity.Generation, s.now().UTC())
		if err != nil {
			return contracts.LifecycleResponse{}, err
		}
		lifecycle.Environment = environment
		lifecycle.Instance.State = contracts.InstanceStateLost
		s.metrics.instancesLost.Add(1)
		return lifecycle, nil
	default:
		return contracts.LifecycleResponse{}, fmt.Errorf("Sandbox Host returned unsupported compute state %q", status.State)
	}
}

// StopEnvironment fences leases, destroys disposable compute, and retains the Environment workspace.
func (s *SandboxService) StopEnvironment(ctx context.Context, environmentID string, expectedGeneration int64) (contracts.LifecycleResponse, error) {
	lifecycle, err := s.stopEnvironmentCompute(ctx, environmentID, expectedGeneration)
	if err != nil {
		return contracts.LifecycleResponse{}, err
	}
	if err := s.revokeStoppedAgentEnvironment(ctx, lifecycle.Environment); err != nil {
		return contracts.LifecycleResponse{}, err
	}
	return lifecycle, nil
}

// stopEnvironmentCompute fences the Environment generation without treating its completing turn as an emergency revocation.
func (s *SandboxService) stopEnvironmentCompute(ctx context.Context, environmentID string, expectedGeneration int64) (contracts.LifecycleResponse, error) {
	environment, instance, err := s.store.BeginStop(ctx, environmentID, expectedGeneration, s.now().UTC())
	if err != nil {
		return contracts.LifecycleResponse{}, err
	}
	if instance == nil {
		return contracts.LifecycleResponse{Environment: environment}, nil
	}
	identity := computeIdentity(*instance)
	if err := s.compute.Stop(ctx, identity); err != nil {
		return contracts.LifecycleResponse{}, err
	}
	if err := s.compute.Destroy(ctx, identity); err != nil {
		return contracts.LifecycleResponse{}, err
	}
	environment, err = s.store.CompleteStop(
		ctx, environmentID, instance.ID, instance.Generation, contracts.InstanceStateDestroyed, s.now().UTC(),
	)
	if err != nil {
		return contracts.LifecycleResponse{}, err
	}
	instance.State = contracts.InstanceStateDestroyed
	instance.StoppedAt = environment.UpdatedAt
	instance.UpdatedAt = environment.UpdatedAt
	s.metrics.stopsCompleted.Add(1)
	return contracts.LifecycleResponse{Environment: environment, Instance: instance}, nil
}

func (s *SandboxService) revokeStoppedAgentEnvironment(ctx context.Context, environment contracts.Environment) error {
	if environment.LifecyclePolicyID != contracts.LifecyclePolicyAgentCompartment {
		return nil
	}
	if err := s.executionRevoker.RevokeEnvironmentExecutions(ctx, environment.SubjectRef); err != nil {
		return fmt.Errorf("revoke executions after Environment stop: %w", err)
	}
	return nil
}

// AcquireLease creates bounded access to the exact ready Environment generation.
func (s *SandboxService) AcquireLease(ctx context.Context, environmentID string, request contracts.AcquireLeaseRequest) (contracts.Lease, error) {
	if request.ContractVersion != contracts.ContractVersionV1 {
		return contracts.Lease{}, errors.New("unsupported Sandbox Service contract version")
	}
	if err := validateReference("lease holder", request.HolderRef); err != nil {
		return contracts.Lease{}, err
	}
	environment, err := s.store.GetEnvironment(ctx, environmentID)
	if err != nil {
		return contracts.Lease{}, err
	}
	ttl, err := s.requestedLeaseTTL(request.TTLSeconds)
	if err != nil {
		return contracts.Lease{}, err
	}
	now := s.now().UTC()
	return s.store.CreateLease(ctx, contracts.Lease{
		ContractVersion: contracts.ContractVersionV1, ID: s.newID("lea"),
		EnvironmentID: environmentID, Generation: environment.CurrentGeneration,
		HolderRef: request.HolderRef, ExpiresAt: now.Add(ttl),
	}, now)
}

// RenewLease extends an active lease only while its exact generation remains current.
func (s *SandboxService) RenewLease(ctx context.Context, leaseID string, request contracts.RenewLeaseRequest) (contracts.Lease, error) {
	if request.ContractVersion != contracts.ContractVersionV1 {
		return contracts.Lease{}, errors.New("unsupported Sandbox Service contract version")
	}
	ttl, err := s.requestedLeaseTTL(request.TTLSeconds)
	if err != nil {
		return contracts.Lease{}, err
	}
	now := s.now().UTC()
	return s.store.RenewLease(ctx, leaseID, now.Add(ttl), now)
}

// ReleaseLease ends access without changing Environment intent.
func (s *SandboxService) ReleaseLease(ctx context.Context, leaseID string) (contracts.Lease, error) {
	return s.store.ReleaseLease(ctx, leaseID, s.now().UTC())
}

// CheckpointEnvironment persists an immutable checkpoint for the exact current generation.
func (s *SandboxService) CheckpointEnvironment(ctx context.Context, environmentID string, request contracts.CheckpointRequest) (contracts.Snapshot, error) {
	if request.ContractVersion != contracts.ContractVersionV1 || request.ExpectedGeneration < 1 {
		return contracts.Snapshot{}, errors.New("checkpoint requires the current Sandbox Service contract and generation")
	}
	if err := validateMetadata(request.Metadata); err != nil {
		return contracts.Snapshot{}, err
	}
	lifecycle, err := s.GetEnvironment(ctx, environmentID)
	if err != nil {
		return contracts.Snapshot{}, err
	}
	if lifecycle.Instance == nil || lifecycle.Environment.CurrentGeneration != request.ExpectedGeneration {
		return contracts.Snapshot{}, ports.ErrGenerationFenced
	}
	result, err := s.compute.Checkpoint(ctx, computeIdentity(*lifecycle.Instance))
	if err != nil {
		return contracts.Snapshot{}, err
	}
	if err := validateReference("snapshot opaque reference", result.OpaqueRef); err != nil {
		return contracts.Snapshot{}, err
	}
	if err := validateSHA256(result.ContentHash); err != nil {
		return contracts.Snapshot{}, err
	}
	snapshot := contracts.Snapshot{
		ContractVersion: contracts.ContractVersionV1, ID: s.newID("snp"),
		EnvironmentID: environmentID, WorkspaceID: lifecycle.Environment.WorkspaceID,
		Generation: request.ExpectedGeneration, ParentSnapshotID: lifecycle.Environment.SnapshotID,
		OpaqueRef: result.OpaqueRef, ContentHash: result.ContentHash, SizeBytes: result.SizeBytes,
		Metadata: cloneStringMap(request.Metadata),
	}
	return s.store.SaveSnapshot(ctx, snapshot, s.now().UTC())
}

// CommitWorkspaceVersion records immutable terminal-turn workspace evidence.
func (s *SandboxService) CommitWorkspaceVersion(ctx context.Context, environmentID string, request contracts.CommitWorkspaceVersionRequest) (contracts.WorkspaceVersion, error) {
	if request.ContractVersion != contracts.ContractVersionV1 || request.ExpectedGeneration < 0 {
		return contracts.WorkspaceVersion{}, errors.New("workspace version commit requires the current contract and a non-negative generation")
	}
	request.TerminalTurnID = strings.TrimSpace(request.TerminalTurnID)
	request.TerminalStatus = strings.TrimSpace(request.TerminalStatus)
	if request.TerminalTurnID == "" {
		return contracts.WorkspaceVersion{}, errors.New("terminal turn ID is required")
	}
	switch request.TerminalStatus {
	case contracts.WorkspaceTerminalCompleted, contracts.WorkspaceTerminalFailed, contracts.WorkspaceTerminalCancelled:
	default:
		return contracts.WorkspaceVersion{}, errors.New("terminal status is invalid")
	}
	now := s.now().UTC()
	active, err := s.store.HasActiveLease(ctx, environmentID, now)
	if err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	if active {
		return contracts.WorkspaceVersion{}, ports.ErrEnvironmentBusy
	}
	lifecycle, err := s.GetEnvironment(ctx, environmentID)
	if err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	if lifecycle.Environment.CurrentGeneration != request.ExpectedGeneration {
		return contracts.WorkspaceVersion{}, ports.ErrGenerationFenced
	}
	workspace, err := s.store.GetWorkspace(ctx, lifecycle.Environment.WorkspaceID)
	if err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	previous, err := s.store.GetCurrentWorkspaceVersion(ctx, environmentID)
	if err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	present := request.ExpectedGeneration > 0
	contentHash := emptyWorkspaceContentHash
	var checkpoint ports.CheckpointResult
	if present {
		if lifecycle.Instance != nil {
			if _, err := s.stopEnvironmentCompute(ctx, environmentID, request.ExpectedGeneration); err != nil {
				return contracts.WorkspaceVersion{}, err
			}
			lifecycle, err = s.GetEnvironment(ctx, environmentID)
			if err != nil {
				return contracts.WorkspaceVersion{}, err
			}
		}
		checkpoint, err = s.compute.CheckpointWorkspace(ctx, lifecycle.Environment, workspace)
		if err != nil {
			return contracts.WorkspaceVersion{}, err
		}
		resourceClass, classErr := s.store.GetResourceClass(ctx, lifecycle.Environment.ResourceClassID)
		if classErr != nil {
			return contracts.WorkspaceVersion{}, classErr
		}
		if checkpoint.SizeBytes < 1 || checkpoint.SizeBytes > resourceClass.DiskBytes {
			return contracts.WorkspaceVersion{}, errors.New("workspace Snapshot size is outside the allowed range")
		}
		if err := validateSHA256(checkpoint.ContentHash); err != nil {
			return contracts.WorkspaceVersion{}, err
		}
		contentHash = checkpoint.ContentHash
	}
	dirty := previous == nil ||
		previous.SourceGeneration != request.ExpectedGeneration ||
		previous.WorkspacePresent != present ||
		previous.ContentHash != contentHash
	snapshotID := ""
	if dirty && present {
		snapshot := contracts.Snapshot{
			ContractVersion: contracts.ContractVersionV1, ID: s.newID("snp"),
			EnvironmentID: environmentID, WorkspaceID: workspace.ID, Generation: request.ExpectedGeneration,
			ParentSnapshotID: lifecycle.Environment.SnapshotID, OpaqueRef: checkpoint.OpaqueRef,
			ContentHash: checkpoint.ContentHash, SizeBytes: checkpoint.SizeBytes,
			Metadata: map[string]string{"terminalTurnId": request.TerminalTurnID, "terminalStatus": request.TerminalStatus},
		}
		snapshot, err = s.store.SaveSnapshot(ctx, snapshot, now)
		if err != nil {
			return contracts.WorkspaceVersion{}, err
		}
		snapshotID = snapshot.ID
	}
	return s.store.CommitWorkspaceVersion(ctx, contracts.WorkspaceVersion{
		ContractVersion: contracts.ContractVersionV1, EnvironmentID: environmentID,
		SourceGeneration: request.ExpectedGeneration, TerminalTurnID: request.TerminalTurnID,
		TerminalStatus: request.TerminalStatus, WorkspacePresent: present, Dirty: dirty,
		ContentHash: contentHash, SnapshotID: snapshotID, CreatedAt: now,
	})
}

// GetCurrentWorkspaceVersion returns the latest terminal workspace version.
func (s *SandboxService) GetCurrentWorkspaceVersion(ctx context.Context, environmentID string) (*contracts.WorkspaceVersion, error) {
	return s.store.GetCurrentWorkspaceVersion(ctx, environmentID)
}

// GetWorkspaceVersion returns one exact terminal workspace version.
func (s *SandboxService) GetWorkspaceVersion(ctx context.Context, environmentID string, logicalVersion int64) (contracts.WorkspaceVersion, error) {
	if logicalVersion < 1 {
		return contracts.WorkspaceVersion{}, errors.New("workspace logical version must be positive")
	}
	return s.store.GetWorkspaceVersion(ctx, environmentID, logicalVersion)
}

// MaterializeWorkspaceVersion copies one immutable version into an empty target Environment.
func (s *SandboxService) MaterializeWorkspaceVersion(ctx context.Context, targetEnvironmentID string, request contracts.MaterializeWorkspaceVersionRequest) error {
	if request.ContractVersion != contracts.ContractVersionV1 || request.SourceLogicalVersion < 1 || request.ExpectedTargetGeneration < 0 {
		return errors.New("workspace materialization request is invalid")
	}
	target, err := s.store.GetEnvironment(ctx, targetEnvironmentID)
	if err != nil {
		return err
	}
	if target.CurrentGeneration != request.ExpectedTargetGeneration || target.CurrentInstanceID != "" {
		return ports.ErrGenerationFenced
	}
	active, err := s.store.HasActiveLease(ctx, targetEnvironmentID, s.now().UTC())
	if err != nil {
		return err
	}
	if active {
		return ports.ErrEnvironmentBusy
	}
	version, err := s.store.GetWorkspaceVersion(ctx, request.SourceEnvironmentID, request.SourceLogicalVersion)
	if err != nil {
		return err
	}
	if !version.WorkspacePresent {
		return nil
	}
	if version.SnapshotID == "" {
		return errors.New("workspace version has no retained Snapshot")
	}
	snapshot, err := s.store.GetSnapshot(ctx, version.SnapshotID)
	if err != nil {
		return err
	}
	workspace, err := s.store.GetWorkspace(ctx, target.WorkspaceID)
	if err != nil {
		return err
	}
	return s.compute.MaterializeWorkspace(ctx, target, workspace, snapshot)
}

// PurgeEnvironment permanently removes one fenced Environment and retained workspace.
func (s *SandboxService) PurgeEnvironment(ctx context.Context, environmentID string, request contracts.PurgeEnvironmentRequest) error {
	if request.ContractVersion != contracts.ContractVersionV1 || request.ExpectedGeneration < 0 {
		return errors.New("Environment purge request is invalid")
	}
	environment, err := s.store.GetEnvironment(ctx, environmentID)
	if err != nil {
		return err
	}
	if environment.CurrentGeneration != request.ExpectedGeneration {
		return ports.ErrGenerationFenced
	}
	active, err := s.store.HasActiveLease(ctx, environmentID, s.now().UTC())
	if err != nil {
		return err
	}
	if active {
		return ports.ErrEnvironmentBusy
	}
	return s.purgeEnvironment(ctx, environment)
}

// ExchangeArtifact persists bounded opaque artifact evidence for the exact current generation.
func (s *SandboxService) ExchangeArtifact(ctx context.Context, environmentID string, request contracts.ExchangeArtifactRequest) (contracts.Artifact, error) {
	if request.ContractVersion != contracts.ContractVersionV1 || request.ExpectedGeneration < 1 {
		return contracts.Artifact{}, errors.New("artifact exchange requires the current Sandbox Service contract and generation")
	}
	for name, value := range map[string]string{
		"artifact source reference": request.SourceRef, "artifact name": request.Name, "artifact MIME type": request.MimeType,
	} {
		if err := validateReference(name, value); err != nil {
			return contracts.Artifact{}, err
		}
	}
	if err := validateMetadata(request.Metadata); err != nil {
		return contracts.Artifact{}, err
	}
	lifecycle, err := s.GetEnvironment(ctx, environmentID)
	if err != nil {
		return contracts.Artifact{}, err
	}
	if lifecycle.Instance == nil || lifecycle.Environment.CurrentGeneration != request.ExpectedGeneration {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	result, err := s.compute.ExchangeArtifact(ctx, ports.ArtifactExchangeInput{
		Identity: computeIdentity(*lifecycle.Instance), SourceRef: request.SourceRef,
		Name: request.Name, MimeType: request.MimeType, Metadata: cloneStringMap(request.Metadata),
	})
	if err != nil {
		return contracts.Artifact{}, err
	}
	if result.SizeBytes < 0 || result.SizeBytes > maxArtifactBytes {
		return contracts.Artifact{}, errors.New("Sandbox Host artifact size is outside the allowed range")
	}
	if err := validateSHA256(result.SHA256); err != nil {
		return contracts.Artifact{}, err
	}
	if err := validateReference("artifact opaque reference", result.OpaqueRef); err != nil {
		return contracts.Artifact{}, err
	}
	artifact := contracts.Artifact{
		ContractVersion: contracts.ContractVersionV1, ID: s.newID("art"),
		EnvironmentID: environmentID, Generation: request.ExpectedGeneration,
		Name: request.Name, MimeType: request.MimeType, SizeBytes: result.SizeBytes,
		SHA256: result.SHA256, OpaqueRef: result.OpaqueRef, Metadata: cloneStringMap(request.Metadata),
	}
	saved, err := s.store.SaveArtifact(ctx, artifact, s.now().UTC())
	if err != nil {
		return contracts.Artifact{}, err
	}
	s.metrics.artifactsExchanged.Add(1)
	s.metrics.artifactBytes.Add(uint64(saved.SizeBytes))
	return saved, nil
}

// ExecuteEnvironment runs a bounded workspace operation under an active exact-generation lease.
func (s *SandboxService) ExecuteEnvironment(ctx context.Context, environmentID string, request contracts.ExecuteRequest) (contracts.ExecuteResult, error) {
	if request.ContractVersion != contracts.ContractVersionV1 || request.ExpectedGeneration < 1 {
		return contracts.ExecuteResult{}, errors.New("execution requires the current Sandbox Service contract and generation")
	}
	for name, value := range map[string]string{
		"lease ID": request.LeaseID, "operation": request.Operation,
	} {
		if err := validateReference(name, value); err != nil {
			return contracts.ExecuteResult{}, err
		}
	}
	if request.TimeoutMillis < 0 || request.TimeoutMillis > int64((10*time.Minute)/time.Millisecond) {
		return contracts.ExecuteResult{}, errors.New("execution timeout is outside the allowed range")
	}
	if err := validateAllowedConnectionIDs(request.AllowedConnectionIDs); err != nil {
		return contracts.ExecuteResult{}, err
	}
	identity, now, err := s.leasedComputeIdentity(ctx, environmentID, request.ExpectedGeneration, request.LeaseID)
	if err != nil {
		return contracts.ExecuteResult{}, err
	}
	result, err := s.compute.Execute(ctx, ports.ExecuteInput{
		Identity: identity, Operation: request,
	})
	if err != nil {
		return contracts.ExecuteResult{}, err
	}
	result.InstanceID = identity.InstanceID
	if err := s.store.TouchEnvironment(ctx, environmentID, request.ExpectedGeneration, now); err != nil {
		return contracts.ExecuteResult{}, err
	}
	return result, nil
}

// OpenWorkspaceFile streams one file from an exact leased Environment generation.
func (s *SandboxService) OpenWorkspaceFile(ctx context.Context, environmentID, path string, expectedGeneration int64, leaseID string) (io.ReadCloser, int64, error) {
	if err := validateReference("workspace file path", path); err != nil {
		return nil, 0, err
	}
	identity, now, err := s.leasedComputeIdentity(ctx, environmentID, expectedGeneration, leaseID)
	if err != nil {
		return nil, 0, err
	}
	reader, size, err := s.compute.OpenWorkspaceFile(ctx, ports.WorkspaceFileInput{Identity: identity, Path: path})
	if err != nil {
		return nil, 0, err
	}
	if size < 0 || size > s.maxFileTransferBytes {
		return nil, 0, errors.Join(
			errors.New("workspace file exceeds the configured transfer limit"),
			reader.Close(),
		)
	}
	if err := s.store.TouchEnvironment(ctx, environmentID, expectedGeneration, now); err != nil {
		return nil, 0, errors.Join(err, reader.Close())
	}
	return reader, size, nil
}

// PutWorkspaceFile streams one file into an exact leased Environment generation.
func (s *SandboxService) PutWorkspaceFile(ctx context.Context, environmentID, path string, expectedGeneration int64, leaseID string, reader io.Reader) (ports.WorkspaceFileWriteResult, error) {
	if err := validateReference("workspace file path", path); err != nil {
		return ports.WorkspaceFileWriteResult{}, err
	}
	if reader == nil {
		return ports.WorkspaceFileWriteResult{}, errors.New("workspace file body is required")
	}
	identity, now, err := s.leasedComputeIdentity(ctx, environmentID, expectedGeneration, leaseID)
	if err != nil {
		return ports.WorkspaceFileWriteResult{}, err
	}
	result, err := s.compute.PutWorkspaceFile(ctx, ports.WorkspaceFileInput{Identity: identity, Path: path}, io.LimitReader(reader, s.maxFileTransferBytes+1))
	if err != nil {
		return ports.WorkspaceFileWriteResult{}, err
	}
	if result.SizeBytes < 0 || result.SizeBytes > s.maxFileTransferBytes {
		return ports.WorkspaceFileWriteResult{}, errors.New("workspace file exceeds the configured transfer limit")
	}
	if err := validateSHA256(result.SHA256); err != nil {
		return ports.WorkspaceFileWriteResult{}, err
	}
	if err := s.store.TouchEnvironment(ctx, environmentID, expectedGeneration, now); err != nil {
		return ports.WorkspaceFileWriteResult{}, err
	}
	return result, nil
}

func (s *SandboxService) leasedComputeIdentity(ctx context.Context, environmentID string, expectedGeneration int64, leaseID string) (ports.ComputeIdentity, time.Time, error) {
	if expectedGeneration < 1 {
		return ports.ComputeIdentity{}, time.Time{}, errors.New("current Sandbox Service generation is required")
	}
	if err := validateReference("lease ID", leaseID); err != nil {
		return ports.ComputeIdentity{}, time.Time{}, err
	}
	lease, err := s.store.GetLease(ctx, leaseID)
	if err != nil {
		return ports.ComputeIdentity{}, time.Time{}, err
	}
	now := s.now().UTC()
	if lease.EnvironmentID != environmentID || lease.Generation != expectedGeneration {
		return ports.ComputeIdentity{}, time.Time{}, ports.ErrGenerationFenced
	}
	if lease.State != contracts.LeaseStateActive {
		return ports.ComputeIdentity{}, time.Time{}, ports.ErrLeaseReleased
	}
	if !lease.ExpiresAt.After(now) {
		return ports.ComputeIdentity{}, time.Time{}, ports.ErrLeaseExpired
	}
	lifecycle, err := s.GetEnvironment(ctx, environmentID)
	if err != nil {
		return ports.ComputeIdentity{}, time.Time{}, err
	}
	if lifecycle.Instance == nil || lifecycle.Environment.CurrentGeneration != expectedGeneration {
		return ports.ComputeIdentity{}, time.Time{}, ports.ErrGenerationFenced
	}
	return computeIdentity(*lifecycle.Instance), now, nil
}

// PrometheusMetrics returns fixed-cardinality Sandbox Service metrics.
func (s *SandboxService) PrometheusMetrics(ctx context.Context) (string, error) {
	retainedWorkspaces, err := s.store.CountRetainedWorkspaces(ctx)
	if err != nil {
		return "", err
	}
	s.metrics.retainedWorkspaces.Store(retainedWorkspaces)
	return s.metrics.PrometheusText(), nil
}

// ListResourceClasses returns bounded configured quota classes.
func (s *SandboxService) ListResourceClasses(ctx context.Context) ([]contracts.ResourceClass, error) {
	return s.store.ListResourceClasses(ctx)
}

// ListLifecyclePolicies returns the supported independent compute and retention policies.
func (s *SandboxService) ListLifecyclePolicies(ctx context.Context) ([]contracts.LifecyclePolicy, error) {
	return s.store.ListLifecyclePolicies(ctx)
}

// ReconcileLifecycle applies idle shutdown and bounded retention without requiring wake ownership.
func (s *SandboxService) ReconcileLifecycle(ctx context.Context, limit int) error {
	if limit < 1 || limit > 1000 {
		return errors.New("Sandbox lifecycle reconciliation limit must be between 1 and 1000")
	}
	now := s.now().UTC()
	environments, err := s.store.ListLifecycleCandidates(ctx, now, limit)
	if err != nil {
		return err
	}
	for _, environment := range environments {
		policy, err := s.store.GetLifecyclePolicy(ctx, environment.LifecyclePolicyID)
		if err != nil {
			return err
		}
		if environment.State == contracts.EnvironmentStateReady && policy.StopComputeWhenIdle &&
			policy.IdleStopAfterSeconds > 0 &&
			!environment.LastActivityAt.Add(time.Duration(policy.IdleStopAfterSeconds)*time.Second).After(now) {
			if _, err := s.StopEnvironment(ctx, environment.ID, environment.CurrentGeneration); err != nil {
				return err
			}
			continue
		}
		if environment.State != contracts.EnvironmentStateReady &&
			!environment.UpdatedAt.Add(time.Duration(policy.RetentionSeconds)*time.Second).After(now) {
			if err := s.purgeEnvironment(ctx, environment); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *SandboxService) purgeEnvironment(ctx context.Context, environment contracts.Environment) error {
	instances, err := s.store.ListInstances(ctx, environment.ID)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if instance.BackendRef == "" || instance.State == contracts.InstanceStateDestroyed {
			continue
		}
		if err := s.compute.Destroy(ctx, computeIdentity(instance)); err != nil {
			return err
		}
	}
	workspace, err := s.store.GetWorkspace(ctx, environment.WorkspaceID)
	if err != nil {
		return err
	}
	if err := s.compute.Purge(ctx, environment, workspace); err != nil {
		return err
	}
	if err := s.store.PurgeEnvironment(ctx, environment.ID, environment.CurrentGeneration, s.now().UTC()); err != nil {
		return err
	}
	s.metrics.workspacesPurged.Add(1)
	return nil
}

func (s *SandboxService) failStart(ctx context.Context, started ports.StartGenerationResult, failureCode string, cause error) error {
	s.metrics.recordStartFailure(failureCode)
	markErr := s.store.MarkInstanceFailed(
		ctx, started.Environment.ID, started.Instance.ID, started.Instance.Generation, failureCode, s.now().UTC(),
	)
	return errors.Join(cause, markErr)
}

func (s *SandboxService) failStartedCompute(ctx context.Context, started ports.StartGenerationResult, backendRef string, cause error) error {
	s.metrics.recordStartFailure("ready_publish_failed")
	identity := computeIdentity(started.Instance)
	identity.BackendRef = backendRef
	stopErr := s.compute.Stop(ctx, identity)
	destroyErr := s.compute.Destroy(ctx, identity)
	markErr := s.store.MarkInstanceFailed(
		ctx, started.Environment.ID, started.Instance.ID, started.Instance.Generation, "ready_publish_failed", s.now().UTC(),
	)
	return errors.Join(cause, stopErr, destroyErr, markErr)
}

func (s *SandboxService) requestedLeaseTTL(seconds int64) (time.Duration, error) {
	if seconds == 0 {
		return s.leaseTTL, nil
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl <= 0 || ttl > s.leaseTTL {
		return 0, errors.New("requested lease TTL exceeds the configured maximum")
	}
	return ttl, nil
}

func validateResolveRequest(request contracts.ResolveEnvironmentRequest) error {
	if request.ContractVersion != contracts.ContractVersionV1 {
		return errors.New("unsupported Sandbox Service contract version")
	}
	for name, value := range map[string]string{
		"tenant reference": request.TenantRef, "subject reference": request.SubjectRef,
		"environment key": request.EnvironmentKey, "image reference": request.ImageRef,
		"toolchain reference": request.ToolchainRef, "Resource Class ID": request.ResourceClassID,
		"Lifecycle Policy ID": request.LifecyclePolicyID,
	} {
		if err := validateReference(name, value); err != nil {
			return err
		}
	}
	return validateMetadata(request.Metadata)
}

func validateReference(name, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > maxReferenceLength {
		return fmt.Errorf("%s is required and must not exceed %d bytes", name, maxReferenceLength)
	}
	return nil
}

func validateMetadata(metadata map[string]string) error {
	if len(metadata) > maxMetadataItems {
		return fmt.Errorf("Sandbox metadata exceeds %d items", maxMetadataItems)
	}
	for key, value := range metadata {
		if err := validateReference("metadata key", key); err != nil {
			return err
		}
		if len(value) > maxReferenceLength {
			return fmt.Errorf("metadata value must not exceed %d bytes", maxReferenceLength)
		}
	}
	return nil
}

func validateAllowedConnectionIDs(connectionIDs []string) error {
	if len(connectionIDs) > maxAllowedConnectionIDs {
		return fmt.Errorf("Sandbox execution exceeds %d allowed connection IDs", maxAllowedConnectionIDs)
	}
	seenConnectionIDs := make(map[string]struct{}, len(connectionIDs))
	for _, connectionID := range connectionIDs {
		if strings.TrimSpace(connectionID) != connectionID {
			return errors.New("Sandbox allowed connection ID must not contain surrounding whitespace")
		}
		if err := validateReference("allowed connection ID", connectionID); err != nil {
			return err
		}
		if _, exists := seenConnectionIDs[connectionID]; exists {
			return fmt.Errorf("Sandbox execution contains duplicate allowed connection ID %q", connectionID)
		}
		seenConnectionIDs[connectionID] = struct{}{}
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != 64 {
		return errors.New("Sandbox content hash must be a lowercase SHA-256")
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("Sandbox content hash must be a lowercase SHA-256")
		}
	}
	return nil
}

func computeIdentity(instance contracts.Instance) ports.ComputeIdentity {
	return ports.ComputeIdentity{
		EnvironmentID: instance.EnvironmentID, InstanceID: instance.ID,
		Generation: instance.Generation, BackendRef: instance.BackendRef,
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
