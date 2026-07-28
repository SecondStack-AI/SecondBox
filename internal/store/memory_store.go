// Package store provides durable and test EnvironmentStore implementations.
package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/pkg/contracts"
)

// MemoryEnvironmentStore is a deterministic EnvironmentStore used by conformance tests.
type MemoryEnvironmentStore struct {
	mu                sync.Mutex
	environments      map[string]contracts.Environment
	environmentKeys   map[string]string
	workspaces        map[string]contracts.Workspace
	instances         map[string]contracts.Instance
	leases            map[string]contracts.Lease
	snapshots         map[string]contracts.Snapshot
	workspaceVersions map[string][]contracts.WorkspaceVersion
	artifacts         map[string]contracts.Artifact
	resourceClasses   map[string]contracts.ResourceClass
	policies          map[string]contracts.LifecyclePolicy
}

// NewMemoryEnvironmentStore returns a store seeded with the two supported policies and resource classes.
func NewMemoryEnvironmentStore(now time.Time) *MemoryEnvironmentStore {
	now = now.UTC()
	return &MemoryEnvironmentStore{
		environments:      map[string]contracts.Environment{},
		environmentKeys:   map[string]string{},
		workspaces:        map[string]contracts.Workspace{},
		instances:         map[string]contracts.Instance{},
		leases:            map[string]contracts.Lease{},
		snapshots:         map[string]contracts.Snapshot{},
		workspaceVersions: map[string][]contracts.WorkspaceVersion{},
		artifacts:         map[string]contracts.Artifact{},
		resourceClasses: map[string]contracts.ResourceClass{
			contracts.ResourceClassAgentStandard: {
				ContractVersion: contracts.ContractVersionV1, ID: contracts.ResourceClassAgentStandard,
				CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 8 << 30, ProcessLimit: 256,
				MaxExposedPorts: 0, CreatedAt: now, UpdatedAt: now,
			},
			contracts.ResourceClassCodingStandard: {
				ContractVersion: contracts.ContractVersionV1, ID: contracts.ResourceClassCodingStandard,
				CPUMillis: 4000, MemoryBytes: 8 << 30, DiskBytes: 32 << 30, ProcessLimit: 512,
				MaxExposedPorts: 16, CreatedAt: now, UpdatedAt: now,
			},
		},
		policies: map[string]contracts.LifecyclePolicy{
			contracts.LifecyclePolicyAgentCompartment: {
				ContractVersion: contracts.ContractVersionV1, ID: contracts.LifecyclePolicyAgentCompartment,
				IdleStopAfterSeconds: 300, RetentionSeconds: 604800, StopComputeWhenIdle: true,
				RetainOnExplicitStop: true, KeepRunningWithoutWake: false, CreatedAt: now, UpdatedAt: now,
			},
			contracts.LifecyclePolicyCodingEnvironment: {
				ContractVersion: contracts.ContractVersionV1, ID: contracts.LifecyclePolicyCodingEnvironment,
				IdleStopAfterSeconds: 0, RetentionSeconds: 7776000, StopComputeWhenIdle: false,
				RetainOnExplicitStop: true, KeepRunningWithoutWake: true, CreatedAt: now, UpdatedAt: now,
			},
		},
	}
}

func (s *MemoryEnvironmentStore) Ping(context.Context) error { return nil }

func (s *MemoryEnvironmentStore) CountRetainedWorkspaces(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.workspaces)), nil
}

func (s *MemoryEnvironmentStore) GetWorkspaceUsage(_ context.Context, tenantRef, subjectRef string) (contracts.WorkspaceUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	usage := contracts.WorkspaceUsage{
		ContractVersion: contracts.ContractVersionV1,
		TenantRef:       tenantRef,
		SubjectRef:      subjectRef,
	}
	for _, environment := range s.environments {
		if environment.TenantRef != tenantRef || environment.SubjectRef != subjectRef {
			continue
		}
		resourceClass, ok := s.resourceClasses[environment.ResourceClassID]
		if !ok {
			return contracts.WorkspaceUsage{}, ports.ErrEnvironmentNotFound
		}
		usage.EnvironmentCount++
		usage.QuotaBytes += resourceClass.DiskBytes
		if environment.SnapshotID != "" {
			snapshot, ok := s.snapshots[environment.SnapshotID]
			if !ok {
				return contracts.WorkspaceUsage{}, ports.ErrEnvironmentNotFound
			}
			usage.UsageBytes += snapshot.SizeBytes
		}
	}
	return usage, nil
}

func environmentNaturalKey(environment contracts.Environment) string {
	return environment.TenantRef + "\x00" + environment.SubjectRef + "\x00" + environment.EnvironmentKey
}

func (s *MemoryEnvironmentStore) ResolveEnvironment(_ context.Context, input ports.ResolveEnvironmentInput) (contracts.Environment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := environmentNaturalKey(input.Environment)
	if id := s.environmentKeys[key]; id != "" {
		return cloneEnvironment(s.environments[id]), false, nil
	}
	s.environments[input.Environment.ID] = cloneEnvironment(input.Environment)
	s.environmentKeys[key] = input.Environment.ID
	s.workspaces[input.Workspace.ID] = input.Workspace
	return cloneEnvironment(input.Environment), true, nil
}

func (s *MemoryEnvironmentStore) GetEnvironment(_ context.Context, id string) (contracts.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[id]
	if !ok {
		return contracts.Environment{}, ports.ErrEnvironmentNotFound
	}
	return cloneEnvironment(environment), nil
}

func (s *MemoryEnvironmentStore) GetWorkspace(_ context.Context, id string) (contracts.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	workspace, ok := s.workspaces[id]
	if !ok {
		return contracts.Workspace{}, ports.ErrEnvironmentNotFound
	}
	return workspace, nil
}

func (s *MemoryEnvironmentStore) GetCurrentInstance(_ context.Context, environmentID string) (*contracts.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[environmentID]
	if !ok {
		return nil, ports.ErrEnvironmentNotFound
	}
	if environment.CurrentInstanceID == "" {
		return nil, nil
	}
	instance := s.instances[environment.CurrentInstanceID]
	return &instance, nil
}

func (s *MemoryEnvironmentStore) ListInstances(_ context.Context, environmentID string) ([]contracts.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.environments[environmentID]; !ok {
		return nil, ports.ErrEnvironmentNotFound
	}
	items := make([]contracts.Instance, 0)
	for _, instance := range s.instances {
		if instance.EnvironmentID == environmentID {
			items = append(items, instance)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Generation < items[j].Generation })
	return items, nil
}

func (s *MemoryEnvironmentStore) GetResourceClass(_ context.Context, id string) (contracts.ResourceClass, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resourceClass, ok := s.resourceClasses[id]
	if !ok {
		return contracts.ResourceClass{}, ports.ErrEnvironmentNotFound
	}
	return resourceClass, nil
}

func (s *MemoryEnvironmentStore) ListResourceClasses(context.Context) ([]contracts.ResourceClass, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]contracts.ResourceClass, 0, len(s.resourceClasses))
	for _, resourceClass := range s.resourceClasses {
		items = append(items, resourceClass)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (s *MemoryEnvironmentStore) GetLifecyclePolicy(_ context.Context, id string) (contracts.LifecyclePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, ok := s.policies[id]
	if !ok {
		return contracts.LifecyclePolicy{}, ports.ErrEnvironmentNotFound
	}
	return policy, nil
}

func (s *MemoryEnvironmentStore) ListLifecyclePolicies(context.Context) ([]contracts.LifecyclePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]contracts.LifecyclePolicy, 0, len(s.policies))
	for _, policy := range s.policies {
		items = append(items, policy)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (s *MemoryEnvironmentStore) BeginStart(_ context.Context, environmentID string, expectedGeneration int64, instanceID string, now time.Time) (ports.StartGenerationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[environmentID]
	if !ok {
		return ports.StartGenerationResult{}, ports.ErrEnvironmentNotFound
	}
	if expectedGeneration > 0 && expectedGeneration != environment.CurrentGeneration {
		return ports.StartGenerationResult{}, ports.ErrGenerationFenced
	}
	if (environment.State == contracts.EnvironmentStatePreparing ||
		environment.State == contracts.EnvironmentStateReady) &&
		environment.CurrentInstanceID != "" {
		instance := s.instances[environment.CurrentInstanceID]
		return ports.StartGenerationResult{Environment: cloneEnvironment(environment), Instance: instance}, nil
	}
	environment.CurrentGeneration++
	environment.CurrentInstanceID = instanceID
	environment.DesiredState = contracts.DesiredStateRunning
	environment.State = contracts.EnvironmentStatePreparing
	environment.UpdatedAt = now.UTC()
	instance := contracts.Instance{
		ContractVersion: contracts.ContractVersionV1,
		ID:              instanceID, EnvironmentID: environmentID, Generation: environment.CurrentGeneration,
		State: contracts.InstanceStatePreparing, PreparedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	s.environments[environmentID] = environment
	s.instances[instanceID] = instance
	s.fenceEnvironmentLeasesLocked(environmentID, environment.CurrentGeneration, now)
	return ports.StartGenerationResult{Environment: cloneEnvironment(environment), Instance: instance, Created: true}, nil
}

func (s *MemoryEnvironmentStore) MarkInstanceReady(_ context.Context, environmentID, instanceID string, generation int64, backendRef string, now time.Time) (contracts.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, instance, err := s.currentGenerationLocked(environmentID, instanceID, generation)
	if err != nil {
		return contracts.Environment{}, err
	}
	instance.State = contracts.InstanceStateReady
	instance.BackendRef = backendRef
	instance.ReadyAt = now.UTC()
	instance.UpdatedAt = now.UTC()
	environment.State = contracts.EnvironmentStateReady
	environment.DesiredState = contracts.DesiredStateRunning
	environment.LastActivityAt = now.UTC()
	environment.UpdatedAt = now.UTC()
	s.instances[instanceID] = instance
	s.environments[environmentID] = environment
	return cloneEnvironment(environment), nil
}

func (s *MemoryEnvironmentStore) MarkInstanceFailed(_ context.Context, environmentID, instanceID string, generation int64, failureCode string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, instance, err := s.currentGenerationLocked(environmentID, instanceID, generation)
	if err != nil {
		return err
	}
	instance.State = contracts.InstanceStateFailed
	instance.FailureCode = failureCode
	instance.UpdatedAt = now.UTC()
	environment.State = contracts.EnvironmentStateFailed
	environment.DesiredState = contracts.DesiredStateStopped
	environment.UpdatedAt = now.UTC()
	s.instances[instanceID] = instance
	s.environments[environmentID] = environment
	return nil
}

func (s *MemoryEnvironmentStore) BeginStop(_ context.Context, environmentID string, expectedGeneration int64, now time.Time) (contracts.Environment, *contracts.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[environmentID]
	if !ok {
		return contracts.Environment{}, nil, ports.ErrEnvironmentNotFound
	}
	if expectedGeneration > 0 && expectedGeneration != environment.CurrentGeneration {
		return contracts.Environment{}, nil, ports.ErrGenerationFenced
	}
	environment.DesiredState = contracts.DesiredStateStopped
	environment.UpdatedAt = now.UTC()
	s.fenceEnvironmentLeasesLocked(environmentID, environment.CurrentGeneration+1, now)
	if environment.CurrentInstanceID == "" || environment.State == contracts.EnvironmentStateStopped {
		environment.State = contracts.EnvironmentStateStopped
		s.environments[environmentID] = environment
		return cloneEnvironment(environment), nil, nil
	}
	instance := s.instances[environment.CurrentInstanceID]
	instance.State = contracts.InstanceStateStopping
	instance.UpdatedAt = now.UTC()
	environment.State = contracts.EnvironmentStateStopping
	s.instances[instance.ID] = instance
	s.environments[environmentID] = environment
	return cloneEnvironment(environment), &instance, nil
}

func (s *MemoryEnvironmentStore) CompleteStop(_ context.Context, environmentID, instanceID string, generation int64, instanceState string, now time.Time) (contracts.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, instance, err := s.currentGenerationLocked(environmentID, instanceID, generation)
	if err != nil {
		return contracts.Environment{}, err
	}
	instance.State = instanceState
	instance.StoppedAt = now.UTC()
	instance.UpdatedAt = now.UTC()
	environment.State = contracts.EnvironmentStateStopped
	environment.DesiredState = contracts.DesiredStateStopped
	environment.CurrentInstanceID = ""
	environment.UpdatedAt = now.UTC()
	s.instances[instanceID] = instance
	s.environments[environmentID] = environment
	return cloneEnvironment(environment), nil
}

func (s *MemoryEnvironmentStore) MarkInstanceLost(_ context.Context, environmentID, instanceID string, generation int64, now time.Time) (contracts.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, instance, err := s.currentGenerationLocked(environmentID, instanceID, generation)
	if err != nil {
		return contracts.Environment{}, err
	}
	instance.State = contracts.InstanceStateLost
	instance.UpdatedAt = now.UTC()
	environment.State = contracts.EnvironmentStateLost
	environment.DesiredState = contracts.DesiredStateStopped
	environment.CurrentInstanceID = ""
	environment.UpdatedAt = now.UTC()
	s.instances[instanceID] = instance
	s.environments[environmentID] = environment
	s.fenceEnvironmentLeasesLocked(environmentID, generation+1, now)
	return cloneEnvironment(environment), nil
}

func (s *MemoryEnvironmentStore) TouchEnvironment(_ context.Context, environmentID string, generation int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[environmentID]
	if !ok {
		return ports.ErrEnvironmentNotFound
	}
	if environment.CurrentGeneration != generation {
		return ports.ErrGenerationFenced
	}
	environment.LastActivityAt = now.UTC()
	environment.UpdatedAt = now.UTC()
	s.environments[environmentID] = environment
	return nil
}

func (s *MemoryEnvironmentStore) CreateLease(_ context.Context, lease contracts.Lease, now time.Time) (contracts.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[lease.EnvironmentID]
	if !ok {
		return contracts.Lease{}, ports.ErrEnvironmentNotFound
	}
	if environment.CurrentGeneration != lease.Generation || environment.State != contracts.EnvironmentStateReady {
		return contracts.Lease{}, ports.ErrGenerationFenced
	}
	lease.ContractVersion = contracts.ContractVersionV1
	lease.State = contracts.LeaseStateActive
	lease.CreatedAt = now.UTC()
	lease.UpdatedAt = now.UTC()
	s.leases[lease.ID] = lease
	return lease, nil
}

func (s *MemoryEnvironmentStore) GetLease(_ context.Context, id string) (contracts.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[id]
	if !ok {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	return lease, nil
}

func (s *MemoryEnvironmentStore) HasActiveLease(_ context.Context, environmentID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.environments[environmentID]; !ok {
		return false, ports.ErrEnvironmentNotFound
	}
	return s.hasActiveLeaseLocked(environmentID, now.UTC()), nil
}

func (s *MemoryEnvironmentStore) RenewLease(_ context.Context, id string, expiresAt, now time.Time) (contracts.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[id]
	if !ok {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	environment, environmentExists := s.environments[lease.EnvironmentID]
	if !environmentExists || environment.CurrentGeneration != lease.Generation {
		return contracts.Lease{}, ports.ErrGenerationFenced
	}
	if lease.State != contracts.LeaseStateActive {
		return contracts.Lease{}, ports.ErrLeaseReleased
	}
	if !lease.ExpiresAt.After(now) {
		lease.State = contracts.LeaseStateExpired
		lease.UpdatedAt = now.UTC()
		s.leases[id] = lease
		return contracts.Lease{}, ports.ErrLeaseExpired
	}
	lease.ExpiresAt = expiresAt.UTC()
	lease.UpdatedAt = now.UTC()
	s.leases[id] = lease
	return lease, nil
}

func (s *MemoryEnvironmentStore) ReleaseLease(_ context.Context, id string, now time.Time) (contracts.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[id]
	if !ok {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	lease.State = contracts.LeaseStateReleased
	lease.UpdatedAt = now.UTC()
	s.leases[id] = lease
	return lease, nil
}

func (s *MemoryEnvironmentStore) SaveSnapshot(_ context.Context, snapshot contracts.Snapshot, now time.Time) (contracts.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[snapshot.EnvironmentID]
	if !ok {
		return contracts.Snapshot{}, ports.ErrEnvironmentNotFound
	}
	if environment.CurrentGeneration != snapshot.Generation {
		return contracts.Snapshot{}, ports.ErrGenerationFenced
	}
	snapshot.ContractVersion = contracts.ContractVersionV1
	snapshot.CreatedAt = now.UTC()
	snapshot.Metadata = cloneStringMap(snapshot.Metadata)
	s.snapshots[snapshot.ID] = snapshot
	environment.SnapshotID = snapshot.ID
	environment.UpdatedAt = now.UTC()
	s.environments[environment.ID] = environment
	snapshot.Metadata = cloneStringMap(snapshot.Metadata)
	return snapshot, nil
}

func (s *MemoryEnvironmentStore) GetSnapshot(_ context.Context, id string) (contracts.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.snapshots[id]
	if !ok {
		return contracts.Snapshot{}, ports.ErrEnvironmentNotFound
	}
	snapshot.Metadata = cloneStringMap(snapshot.Metadata)
	return snapshot, nil
}

func (s *MemoryEnvironmentStore) CommitWorkspaceVersion(_ context.Context, version contracts.WorkspaceVersion) (contracts.WorkspaceVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.environments[version.EnvironmentID]; !ok {
		return contracts.WorkspaceVersion{}, ports.ErrEnvironmentNotFound
	}
	versions := s.workspaceVersions[version.EnvironmentID]
	for _, existing := range versions {
		if existing.TerminalTurnID == version.TerminalTurnID {
			return existing, nil
		}
	}
	version.ContractVersion = contracts.ContractVersionV1
	version.LogicalVersion = int64(len(versions) + 1)
	if !version.Dirty && len(versions) > 0 && versions[len(versions)-1].SourceGeneration == version.SourceGeneration {
		version.SnapshotID = versions[len(versions)-1].SnapshotID
		version.SnapshotLogicalVersion = versions[len(versions)-1].SnapshotLogicalVersion
	}
	if version.Dirty {
		version.SnapshotLogicalVersion = version.LogicalVersion
	}
	s.workspaceVersions[version.EnvironmentID] = append(versions, version)
	return version, nil
}

func (s *MemoryEnvironmentStore) GetCurrentWorkspaceVersion(_ context.Context, environmentID string) (*contracts.WorkspaceVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.environments[environmentID]; !ok {
		return nil, ports.ErrEnvironmentNotFound
	}
	versions := s.workspaceVersions[environmentID]
	if len(versions) == 0 {
		return nil, nil
	}
	version := versions[len(versions)-1]
	return &version, nil
}

func (s *MemoryEnvironmentStore) GetWorkspaceVersion(_ context.Context, environmentID string, logicalVersion int64) (contracts.WorkspaceVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.workspaceVersions[environmentID]
	if logicalVersion < 1 || logicalVersion > int64(len(versions)) {
		return contracts.WorkspaceVersion{}, ports.ErrEnvironmentNotFound
	}
	return versions[logicalVersion-1], nil
}

func (s *MemoryEnvironmentStore) SaveArtifact(_ context.Context, artifact contracts.Artifact, now time.Time) (contracts.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[artifact.EnvironmentID]
	if !ok {
		return contracts.Artifact{}, ports.ErrEnvironmentNotFound
	}
	if environment.CurrentGeneration != artifact.Generation {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	artifact.ContractVersion = contracts.ContractVersionV1
	artifact.CreatedAt = now.UTC()
	artifact.Metadata = cloneStringMap(artifact.Metadata)
	s.artifacts[artifact.ID] = artifact
	artifact.Metadata = cloneStringMap(artifact.Metadata)
	return artifact, nil
}

func (s *MemoryEnvironmentStore) ListLifecycleCandidates(_ context.Context, now time.Time, limit int) ([]contracts.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]contracts.Environment, 0, len(s.environments))
	for _, environment := range s.environments {
		if s.hasActiveLeaseLocked(environment.ID, now.UTC()) {
			continue
		}
		items = append(items, cloneEnvironment(environment))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].LastActivityAt.Equal(items[j].LastActivityAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].LastActivityAt.Before(items[j].LastActivityAt)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *MemoryEnvironmentStore) PurgeEnvironment(_ context.Context, environmentID string, generation int64, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[environmentID]
	if !ok {
		return ports.ErrEnvironmentNotFound
	}
	if environment.CurrentGeneration != generation {
		return ports.ErrGenerationFenced
	}
	delete(s.environmentKeys, environmentNaturalKey(environment))
	delete(s.workspaces, environment.WorkspaceID)
	delete(s.environments, environmentID)
	for id, instance := range s.instances {
		if instance.EnvironmentID == environmentID {
			delete(s.instances, id)
		}
	}
	for id, lease := range s.leases {
		if lease.EnvironmentID == environmentID {
			delete(s.leases, id)
		}
	}
	for id, snapshot := range s.snapshots {
		if snapshot.EnvironmentID == environmentID {
			delete(s.snapshots, id)
		}
	}
	for id, artifact := range s.artifacts {
		if artifact.EnvironmentID == environmentID {
			delete(s.artifacts, id)
		}
	}
	delete(s.workspaceVersions, environmentID)
	return nil
}

func (s *MemoryEnvironmentStore) currentGenerationLocked(environmentID, instanceID string, generation int64) (contracts.Environment, contracts.Instance, error) {
	environment, ok := s.environments[environmentID]
	if !ok {
		return contracts.Environment{}, contracts.Instance{}, ports.ErrEnvironmentNotFound
	}
	instance, instanceExists := s.instances[instanceID]
	if !instanceExists || environment.CurrentGeneration != generation ||
		environment.CurrentInstanceID != instanceID || instance.Generation != generation {
		return contracts.Environment{}, contracts.Instance{}, ports.ErrGenerationFenced
	}
	return environment, instance, nil
}

func (s *MemoryEnvironmentStore) fenceEnvironmentLeasesLocked(environmentID string, generation int64, now time.Time) {
	for id, lease := range s.leases {
		if lease.EnvironmentID == environmentID && lease.Generation < generation && lease.State == contracts.LeaseStateActive {
			lease.State = contracts.LeaseStateExpired
			lease.UpdatedAt = now.UTC()
			s.leases[id] = lease
		}
	}
}

func (s *MemoryEnvironmentStore) hasActiveLeaseLocked(environmentID string, now time.Time) bool {
	for _, lease := range s.leases {
		if lease.EnvironmentID == environmentID && lease.State == contracts.LeaseStateActive && lease.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}

func cloneEnvironment(environment contracts.Environment) contracts.Environment {
	environment.ExposedPorts = append([]contracts.ExposedPort(nil), environment.ExposedPorts...)
	environment.Metadata = cloneStringMap(environment.Metadata)
	return environment
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
