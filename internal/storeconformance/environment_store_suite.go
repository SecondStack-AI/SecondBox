// Package storeconformance defines reusable EnvironmentStore contract checks.
package storeconformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/pkg/contracts"
)

// EnvironmentStoreFactory creates an isolated EnvironmentStore for each conformance scenario.
type EnvironmentStoreFactory func(*testing.T) ports.EnvironmentStore

// RunEnvironmentStoreConformance verifies durable intent, fencing, evidence, and retention behavior.
func RunEnvironmentStoreConformance(t *testing.T, factory EnvironmentStoreFactory) {
	t.Helper()
	t.Run("catalog resolve and missing resources", func(t *testing.T) {
		environmentStore := factory(t)
		ctx := context.Background()
		if err := environmentStore.Ping(ctx); err != nil {
			t.Fatalf("Ping() error = %v", err)
		}
		assertEnvironmentStoreCatalog(t, ctx, environmentStore)
		assertMissingEnvironmentStoreResources(t, ctx, environmentStore)

		now := conformanceTime()
		input := conformanceEnvironmentInput("resolve", now)
		resolved, created, err := environmentStore.ResolveEnvironment(ctx, input)
		if err != nil {
			t.Fatalf("ResolveEnvironment() error = %v", err)
		}
		if !created {
			t.Fatal("ResolveEnvironment() created = false, want true")
		}
		assertEnvironmentIdentity(t, resolved, input.Environment)

		replacement := conformanceEnvironmentInput("replacement", now.Add(time.Minute))
		replacement.Environment.TenantRef = input.Environment.TenantRef
		replacement.Environment.SubjectRef = input.Environment.SubjectRef
		replacement.Environment.EnvironmentKey = input.Environment.EnvironmentKey
		reused, created, err := environmentStore.ResolveEnvironment(ctx, replacement)
		if err != nil {
			t.Fatalf("ResolveEnvironment() reuse error = %v", err)
		}
		if created || reused.ID != input.Environment.ID {
			t.Fatalf("ResolveEnvironment() reuse = (%q, %t), want (%q, false)", reused.ID, created, input.Environment.ID)
		}

		storedEnvironment, err := environmentStore.GetEnvironment(ctx, input.Environment.ID)
		if err != nil {
			t.Fatalf("GetEnvironment() error = %v", err)
		}
		assertEnvironmentIdentity(t, storedEnvironment, input.Environment)
		storedWorkspace, err := environmentStore.GetWorkspace(ctx, input.Workspace.ID)
		if err != nil {
			t.Fatalf("GetWorkspace() error = %v", err)
		}
		if storedWorkspace.ID != input.Workspace.ID || storedWorkspace.ContractVersion != contracts.ContractVersionV1 {
			t.Fatalf("GetWorkspace() = %#v, want workspace %q", storedWorkspace, input.Workspace.ID)
		}
		retainedCount, err := environmentStore.CountRetainedWorkspaces(ctx)
		if err != nil {
			t.Fatalf("CountRetainedWorkspaces() error = %v", err)
		}
		if retainedCount != 1 {
			t.Fatalf("CountRetainedWorkspaces() = %d, want 1", retainedCount)
		}
		usage, err := environmentStore.GetWorkspaceUsage(ctx, input.Environment.TenantRef, input.Environment.SubjectRef)
		if err != nil {
			t.Fatalf("GetWorkspaceUsage() error = %v", err)
		}
		if usage.ContractVersion != contracts.ContractVersionV1 ||
			usage.EnvironmentCount != 1 || usage.QuotaBytes != 8<<30 || usage.UsageBytes != 0 {
			t.Fatalf("GetWorkspaceUsage() = %#v, want one empty agent-standard workspace", usage)
		}
	})

	t.Run("concurrent natural key resolve", func(t *testing.T) {
		environmentStore := factory(t)
		ctx := context.Background()
		now := conformanceTime()
		const contenders = 8
		var waitGroup sync.WaitGroup
		var resultMutex sync.Mutex
		createdCount := 0
		resolvedIDs := map[string]struct{}{}
		errorsSeen := make([]error, 0)
		for index := 0; index < contenders; index++ {
			waitGroup.Add(1)
			go func(index int) {
				defer waitGroup.Done()
				input := conformanceEnvironmentInput(fmt.Sprintf("contender-%d", index), now)
				input.Environment.TenantRef = "tenant-concurrent"
				input.Environment.SubjectRef = "subject-concurrent"
				input.Environment.EnvironmentKey = "shared-key"
				resolved, created, err := environmentStore.ResolveEnvironment(ctx, input)
				resultMutex.Lock()
				defer resultMutex.Unlock()
				if err != nil {
					errorsSeen = append(errorsSeen, err)
					return
				}
				if created {
					createdCount++
				}
				resolvedIDs[resolved.ID] = struct{}{}
			}(index)
		}
		waitGroup.Wait()
		if len(errorsSeen) != 0 {
			t.Fatalf("concurrent ResolveEnvironment() errors = %v", errorsSeen)
		}
		if createdCount != 1 || len(resolvedIDs) != 1 {
			t.Fatalf("concurrent ResolveEnvironment() created %d Environments with IDs %v, want one", createdCount, resolvedIDs)
		}
		retainedCount, err := environmentStore.CountRetainedWorkspaces(ctx)
		if err != nil {
			t.Fatalf("CountRetainedWorkspaces() error = %v", err)
		}
		if retainedCount != 1 {
			t.Fatalf("CountRetainedWorkspaces() = %d after concurrent resolve, want 1", retainedCount)
		}
	})

	t.Run("generation instance and lease fencing", func(t *testing.T) {
		environmentStore := factory(t)
		ctx := context.Background()
		now := conformanceTime()
		input := resolveConformanceEnvironment(t, ctx, environmentStore, "lifecycle", now)

		started, err := environmentStore.BeginStart(ctx, input.Environment.ID, 0, "instance-1", now.Add(time.Minute))
		if err != nil {
			t.Fatalf("BeginStart() error = %v", err)
		}
		if !started.Created || started.Environment.CurrentGeneration != 1 ||
			started.Instance.Generation != 1 || started.Instance.State != contracts.InstanceStatePreparing {
			t.Fatalf("BeginStart() = %#v, want preparing generation 1", started)
		}
		reused, err := environmentStore.BeginStart(ctx, input.Environment.ID, 1, "instance-unused", now.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("BeginStart() reuse error = %v", err)
		}
		if reused.Created || reused.Instance.ID != started.Instance.ID {
			t.Fatalf("BeginStart() reuse = %#v, want existing Instance %q", reused, started.Instance.ID)
		}
		if _, err := environmentStore.MarkInstanceReady(ctx, input.Environment.ID, started.Instance.ID, 0, "backend-stale", now); !errors.Is(err, ports.ErrGenerationFenced) {
			t.Fatalf("MarkInstanceReady() stale generation error = %v, want %v", err, ports.ErrGenerationFenced)
		}
		ready, err := environmentStore.MarkInstanceReady(
			ctx, input.Environment.ID, started.Instance.ID, 1, "backend:opaque", now.Add(3*time.Minute),
		)
		if err != nil {
			t.Fatalf("MarkInstanceReady() error = %v", err)
		}
		if ready.State != contracts.EnvironmentStateReady {
			t.Fatalf("MarkInstanceReady() state = %q, want ready", ready.State)
		}
		currentInstance, err := environmentStore.GetCurrentInstance(ctx, input.Environment.ID)
		if err != nil {
			t.Fatalf("GetCurrentInstance() error = %v", err)
		}
		if currentInstance == nil || currentInstance.ID != started.Instance.ID ||
			currentInstance.ContractVersion != contracts.ContractVersionV1 {
			t.Fatalf("GetCurrentInstance() = %#v, want %q", currentInstance, started.Instance.ID)
		}
		instances, err := environmentStore.ListInstances(ctx, input.Environment.ID)
		if err != nil {
			t.Fatalf("ListInstances() error = %v", err)
		}
		if len(instances) != 1 || instances[0].Generation != 1 {
			t.Fatalf("ListInstances() = %#v, want generation 1", instances)
		}
		if err := environmentStore.TouchEnvironment(ctx, input.Environment.ID, 0, now); !errors.Is(err, ports.ErrGenerationFenced) {
			t.Fatalf("TouchEnvironment() stale generation error = %v, want %v", err, ports.ErrGenerationFenced)
		}
		if err := environmentStore.TouchEnvironment(ctx, input.Environment.ID, 1, now.Add(4*time.Minute)); err != nil {
			t.Fatalf("TouchEnvironment() error = %v", err)
		}

		lease := contracts.Lease{
			ID: "lease-active", EnvironmentID: input.Environment.ID, Generation: 1,
			HolderRef: "wake:1", ExpiresAt: now.Add(30 * time.Minute),
		}
		lease, err = environmentStore.CreateLease(ctx, lease, now.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("CreateLease() error = %v", err)
		}
		if lease.ContractVersion != contracts.ContractVersionV1 || lease.State != contracts.LeaseStateActive {
			t.Fatalf("CreateLease() = %#v, want versioned active Lease", lease)
		}
		storedLease, err := environmentStore.GetLease(ctx, lease.ID)
		if err != nil {
			t.Fatalf("GetLease() error = %v", err)
		}
		if storedLease.State != contracts.LeaseStateActive {
			t.Fatalf("GetLease() state = %q, want active", storedLease.State)
		}
		active, err := environmentStore.HasActiveLease(ctx, input.Environment.ID, now.Add(6*time.Minute))
		if err != nil || !active {
			t.Fatalf("HasActiveLease() = (%t, %v), want (true, nil)", active, err)
		}
		renewed, err := environmentStore.RenewLease(ctx, lease.ID, now.Add(time.Hour), now.Add(7*time.Minute))
		if err != nil {
			t.Fatalf("RenewLease() error = %v", err)
		}
		if !renewed.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("RenewLease() expiry = %s, want %s", renewed.ExpiresAt, now.Add(time.Hour))
		}
		released, err := environmentStore.ReleaseLease(ctx, lease.ID, now.Add(8*time.Minute))
		if err != nil {
			t.Fatalf("ReleaseLease() error = %v", err)
		}
		if released.State != contracts.LeaseStateReleased {
			t.Fatalf("ReleaseLease() state = %q, want released", released.State)
		}

		expiredLease, err := environmentStore.CreateLease(ctx, contracts.Lease{
			ID: "lease-expired", EnvironmentID: input.Environment.ID, Generation: 1,
			HolderRef: "wake:expired", ExpiresAt: now.Add(9 * time.Minute),
		}, now.Add(8*time.Minute))
		if err != nil {
			t.Fatalf("CreateLease() expired fixture error = %v", err)
		}
		if _, err := environmentStore.RenewLease(
			ctx, expiredLease.ID, now.Add(time.Hour), now.Add(10*time.Minute),
		); !errors.Is(err, ports.ErrLeaseExpired) {
			t.Fatalf("RenewLease() expired error = %v, want %v", err, ports.ErrLeaseExpired)
		}

		stopLease, err := environmentStore.CreateLease(ctx, contracts.Lease{
			ID: "lease-stop", EnvironmentID: input.Environment.ID, Generation: 1,
			HolderRef: "wake:stop", ExpiresAt: now.Add(time.Hour),
		}, now.Add(11*time.Minute))
		if err != nil {
			t.Fatalf("CreateLease() stop fixture error = %v", err)
		}
		stoppingEnvironment, stoppingInstance, err := environmentStore.BeginStop(
			ctx, input.Environment.ID, 1, now.Add(12*time.Minute),
		)
		if err != nil {
			t.Fatalf("BeginStop() error = %v", err)
		}
		if stoppingEnvironment.State != contracts.EnvironmentStateStopping ||
			stoppingInstance == nil || stoppingInstance.State != contracts.InstanceStateStopping {
			t.Fatalf("BeginStop() = (%#v, %#v), want stopping Environment and Instance", stoppingEnvironment, stoppingInstance)
		}
		fencedLease, err := environmentStore.GetLease(ctx, stopLease.ID)
		if err != nil {
			t.Fatalf("GetLease() after stop error = %v", err)
		}
		if fencedLease.State != contracts.LeaseStateExpired {
			t.Fatalf("GetLease() after stop state = %q, want expired", fencedLease.State)
		}
		stopped, err := environmentStore.CompleteStop(
			ctx, input.Environment.ID, started.Instance.ID, 1, contracts.InstanceStateStopped, now.Add(13*time.Minute),
		)
		if err != nil {
			t.Fatalf("CompleteStop() error = %v", err)
		}
		if stopped.State != contracts.EnvironmentStateStopped || stopped.CurrentInstanceID != "" {
			t.Fatalf("CompleteStop() = %#v, want stopped without current Instance", stopped)
		}
		currentInstance, err = environmentStore.GetCurrentInstance(ctx, input.Environment.ID)
		if err != nil || currentInstance != nil {
			t.Fatalf("GetCurrentInstance() after stop = (%#v, %v), want (nil, nil)", currentInstance, err)
		}

		replacement, err := environmentStore.BeginStart(
			ctx, input.Environment.ID, 1, "instance-2", now.Add(14*time.Minute),
		)
		if err != nil {
			t.Fatalf("BeginStart() replacement error = %v", err)
		}
		if replacement.Environment.CurrentGeneration != 2 {
			t.Fatalf("BeginStart() replacement generation = %d, want 2", replacement.Environment.CurrentGeneration)
		}
		if _, err := environmentStore.MarkInstanceReady(
			ctx, input.Environment.ID, replacement.Instance.ID, 2, "backend:replacement", now.Add(15*time.Minute),
		); err != nil {
			t.Fatalf("MarkInstanceReady() replacement error = %v", err)
		}
		if _, err := environmentStore.MarkInstanceLost(
			ctx, input.Environment.ID, started.Instance.ID, 1, now.Add(16*time.Minute),
		); !errors.Is(err, ports.ErrGenerationFenced) {
			t.Fatalf("MarkInstanceLost() stale generation error = %v, want %v", err, ports.ErrGenerationFenced)
		}
		lost, err := environmentStore.MarkInstanceLost(
			ctx, input.Environment.ID, replacement.Instance.ID, 2, now.Add(17*time.Minute),
		)
		if err != nil {
			t.Fatalf("MarkInstanceLost() error = %v", err)
		}
		if lost.State != contracts.EnvironmentStateLost || lost.CurrentInstanceID != "" {
			t.Fatalf("MarkInstanceLost() = %#v, want lost without current Instance", lost)
		}

		failureGeneration, err := environmentStore.BeginStart(
			ctx, input.Environment.ID, 2, "instance-3", now.Add(18*time.Minute),
		)
		if err != nil {
			t.Fatalf("BeginStart() failure fixture error = %v", err)
		}
		if err := environmentStore.MarkInstanceFailed(
			ctx, input.Environment.ID, failureGeneration.Instance.ID, 3, "host-start-failed", now.Add(19*time.Minute),
		); err != nil {
			t.Fatalf("MarkInstanceFailed() error = %v", err)
		}
		failed, err := environmentStore.GetEnvironment(ctx, input.Environment.ID)
		if err != nil {
			t.Fatalf("GetEnvironment() failed state error = %v", err)
		}
		if failed.State != contracts.EnvironmentStateFailed || failed.DesiredState != contracts.DesiredStateStopped {
			t.Fatalf("MarkInstanceFailed() persisted Environment = %#v, want failed and stopped intent", failed)
		}
		instances, err = environmentStore.ListInstances(ctx, input.Environment.ID)
		if err != nil {
			t.Fatalf("ListInstances() after replacements error = %v", err)
		}
		if len(instances) != 3 || instances[2].FailureCode != "host-start-failed" {
			t.Fatalf("ListInstances() after replacements = %#v, want three generations and failure evidence", instances)
		}
	})

	t.Run("immutable snapshots artifacts and workspace versions", func(t *testing.T) {
		environmentStore := factory(t)
		ctx := context.Background()
		now := conformanceTime()
		input := resolveConformanceEnvironment(t, ctx, environmentStore, "evidence", now)
		started, err := environmentStore.BeginStart(ctx, input.Environment.ID, 0, "instance-evidence", now.Add(time.Minute))
		if err != nil {
			t.Fatalf("BeginStart() error = %v", err)
		}
		if _, err := environmentStore.MarkInstanceReady(
			ctx, input.Environment.ID, started.Instance.ID, 1, "backend:evidence", now.Add(2*time.Minute),
		); err != nil {
			t.Fatalf("MarkInstanceReady() error = %v", err)
		}
		if _, err := environmentStore.GetCurrentWorkspaceVersion(ctx, input.Environment.ID); err != nil {
			t.Fatalf("GetCurrentWorkspaceVersion() empty error = %v", err)
		}

		snapshot := contracts.Snapshot{
			ID: "snapshot-1", EnvironmentID: input.Environment.ID, WorkspaceID: input.Workspace.ID,
			Generation: 1, OpaqueRef: "snapshot:opaque", ContentHash: "sha256:snapshot",
			SizeBytes: 4096, Metadata: map[string]string{"kind": "checkpoint"},
		}
		staleSnapshot := snapshot
		staleSnapshot.ID = "snapshot-stale"
		staleSnapshot.Generation = 0
		if _, err := environmentStore.SaveSnapshot(ctx, staleSnapshot, now); !errors.Is(err, ports.ErrGenerationFenced) {
			t.Fatalf("SaveSnapshot() stale generation error = %v, want %v", err, ports.ErrGenerationFenced)
		}
		snapshot, err = environmentStore.SaveSnapshot(ctx, snapshot, now.Add(3*time.Minute))
		if err != nil {
			t.Fatalf("SaveSnapshot() error = %v", err)
		}
		if snapshot.ContractVersion != contracts.ContractVersionV1 {
			t.Fatalf("SaveSnapshot() contract version = %q, want %q", snapshot.ContractVersion, contracts.ContractVersionV1)
		}
		snapshot.Metadata["kind"] = "mutated-after-save"
		storedSnapshot, err := environmentStore.GetSnapshot(ctx, snapshot.ID)
		if err != nil {
			t.Fatalf("GetSnapshot() error = %v", err)
		}
		if storedSnapshot.ID != snapshot.ID || storedSnapshot.Metadata["kind"] != "checkpoint" {
			t.Fatalf("GetSnapshot() = %#v, want immutable checkpoint evidence", storedSnapshot)
		}
		usage, err := environmentStore.GetWorkspaceUsage(ctx, input.Environment.TenantRef, input.Environment.SubjectRef)
		if err != nil {
			t.Fatalf("GetWorkspaceUsage() after snapshot error = %v", err)
		}
		if usage.UsageBytes != snapshot.SizeBytes {
			t.Fatalf("GetWorkspaceUsage() bytes = %d, want %d", usage.UsageBytes, snapshot.SizeBytes)
		}

		artifact := contracts.Artifact{
			ID: "artifact-1", EnvironmentID: input.Environment.ID, Generation: 1,
			Name: "result.json", MimeType: "application/json", SizeBytes: 42,
			SHA256: "sha256:artifact", OpaqueRef: "artifact:opaque",
			Metadata: map[string]string{"kind": "result"},
		}
		staleArtifact := artifact
		staleArtifact.ID = "artifact-stale"
		staleArtifact.Generation = 0
		if _, err := environmentStore.SaveArtifact(ctx, staleArtifact, now); !errors.Is(err, ports.ErrGenerationFenced) {
			t.Fatalf("SaveArtifact() stale generation error = %v, want %v", err, ports.ErrGenerationFenced)
		}
		artifact, err = environmentStore.SaveArtifact(ctx, artifact, now.Add(4*time.Minute))
		if err != nil {
			t.Fatalf("SaveArtifact() error = %v", err)
		}
		if artifact.ContractVersion != contracts.ContractVersionV1 {
			t.Fatalf("SaveArtifact() contract version = %q, want %q", artifact.ContractVersion, contracts.ContractVersionV1)
		}

		firstVersion := contracts.WorkspaceVersion{
			EnvironmentID: input.Environment.ID, SourceGeneration: 1, TerminalTurnID: "turn-1",
			TerminalStatus: contracts.WorkspaceTerminalCompleted, WorkspacePresent: true,
			Dirty: true, ContentHash: "sha256:workspace-1", SnapshotID: snapshot.ID,
			CreatedAt: now.Add(5 * time.Minute),
		}
		firstVersion, err = environmentStore.CommitWorkspaceVersion(ctx, firstVersion)
		if err != nil {
			t.Fatalf("CommitWorkspaceVersion() first error = %v", err)
		}
		if firstVersion.LogicalVersion != 1 || firstVersion.SnapshotLogicalVersion != 1 {
			t.Fatalf("CommitWorkspaceVersion() first = %#v, want logical and snapshot version 1", firstVersion)
		}
		duplicate := firstVersion
		duplicate.ContentHash = "sha256:must-not-replace"
		duplicate, err = environmentStore.CommitWorkspaceVersion(ctx, duplicate)
		if err != nil {
			t.Fatalf("CommitWorkspaceVersion() duplicate error = %v", err)
		}
		if duplicate.ContentHash != firstVersion.ContentHash || duplicate.LogicalVersion != 1 {
			t.Fatalf("CommitWorkspaceVersion() duplicate = %#v, want original first version", duplicate)
		}
		secondVersion, err := environmentStore.CommitWorkspaceVersion(ctx, contracts.WorkspaceVersion{
			EnvironmentID: input.Environment.ID, SourceGeneration: 1, TerminalTurnID: "turn-2",
			TerminalStatus: contracts.WorkspaceTerminalCompleted, WorkspacePresent: true,
			Dirty: false, ContentHash: firstVersion.ContentHash, CreatedAt: now.Add(6 * time.Minute),
		})
		if err != nil {
			t.Fatalf("CommitWorkspaceVersion() second error = %v", err)
		}
		if secondVersion.LogicalVersion != 2 || secondVersion.SnapshotID != snapshot.ID ||
			secondVersion.SnapshotLogicalVersion != 1 {
			t.Fatalf("CommitWorkspaceVersion() second = %#v, want inherited snapshot at logical version 2", secondVersion)
		}
		currentVersion, err := environmentStore.GetCurrentWorkspaceVersion(ctx, input.Environment.ID)
		if err != nil {
			t.Fatalf("GetCurrentWorkspaceVersion() error = %v", err)
		}
		if currentVersion == nil || currentVersion.LogicalVersion != 2 {
			t.Fatalf("GetCurrentWorkspaceVersion() = %#v, want logical version 2", currentVersion)
		}
		storedVersion, err := environmentStore.GetWorkspaceVersion(ctx, input.Environment.ID, 1)
		if err != nil {
			t.Fatalf("GetWorkspaceVersion() error = %v", err)
		}
		if storedVersion.TerminalTurnID != firstVersion.TerminalTurnID {
			t.Fatalf("GetWorkspaceVersion() = %#v, want turn %q", storedVersion, firstVersion.TerminalTurnID)
		}
	})

	t.Run("lifecycle candidates and purge", func(t *testing.T) {
		environmentStore := factory(t)
		ctx := context.Background()
		now := conformanceTime()
		activeInput := resolveConformanceEnvironment(t, ctx, environmentStore, "active", now)
		idleInput := resolveConformanceEnvironment(t, ctx, environmentStore, "idle", now.Add(time.Minute))
		started, err := environmentStore.BeginStart(ctx, activeInput.Environment.ID, 0, "instance-active", now.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("BeginStart() active fixture error = %v", err)
		}
		if _, err := environmentStore.MarkInstanceReady(
			ctx, activeInput.Environment.ID, started.Instance.ID, 1, "backend:active", now.Add(3*time.Minute),
		); err != nil {
			t.Fatalf("MarkInstanceReady() active fixture error = %v", err)
		}
		if _, err := environmentStore.CreateLease(ctx, contracts.Lease{
			ID: "lease-candidate", EnvironmentID: activeInput.Environment.ID, Generation: 1,
			HolderRef: "wake:candidate", ExpiresAt: now.Add(time.Hour),
		}, now.Add(4*time.Minute)); err != nil {
			t.Fatalf("CreateLease() candidate fixture error = %v", err)
		}
		candidates, err := environmentStore.ListLifecycleCandidates(ctx, now.Add(5*time.Minute), 10)
		if err != nil {
			t.Fatalf("ListLifecycleCandidates() error = %v", err)
		}
		if len(candidates) != 1 || candidates[0].ID != idleInput.Environment.ID {
			t.Fatalf("ListLifecycleCandidates() = %#v, want only idle Environment %q", candidates, idleInput.Environment.ID)
		}
		if err := environmentStore.PurgeEnvironment(
			ctx, idleInput.Environment.ID, 99, now.Add(6*time.Minute),
		); !errors.Is(err, ports.ErrGenerationFenced) {
			t.Fatalf("PurgeEnvironment() stale generation error = %v, want %v", err, ports.ErrGenerationFenced)
		}
		if err := environmentStore.PurgeEnvironment(
			ctx, idleInput.Environment.ID, 0, now.Add(7*time.Minute),
		); err != nil {
			t.Fatalf("PurgeEnvironment() error = %v", err)
		}
		if _, err := environmentStore.GetEnvironment(ctx, idleInput.Environment.ID); !errors.Is(err, ports.ErrEnvironmentNotFound) {
			t.Fatalf("GetEnvironment() after purge error = %v, want %v", err, ports.ErrEnvironmentNotFound)
		}
		if _, err := environmentStore.GetWorkspace(ctx, idleInput.Workspace.ID); !errors.Is(err, ports.ErrEnvironmentNotFound) {
			t.Fatalf("GetWorkspace() after purge error = %v, want %v", err, ports.ErrEnvironmentNotFound)
		}
		retainedCount, err := environmentStore.CountRetainedWorkspaces(ctx)
		if err != nil {
			t.Fatalf("CountRetainedWorkspaces() after purge error = %v", err)
		}
		if retainedCount != 1 {
			t.Fatalf("CountRetainedWorkspaces() after purge = %d, want 1", retainedCount)
		}
	})
}

func assertEnvironmentStoreCatalog(t *testing.T, ctx context.Context, environmentStore ports.EnvironmentStore) {
	t.Helper()
	resourceClasses, err := environmentStore.ListResourceClasses(ctx)
	if err != nil {
		t.Fatalf("ListResourceClasses() error = %v", err)
	}
	for _, id := range []string{contracts.ResourceClassAgentStandard, contracts.ResourceClassCodingStandard} {
		if !containsResourceClass(resourceClasses, id) {
			t.Errorf("ListResourceClasses() is missing %q: %#v", id, resourceClasses)
		}
		resourceClass, err := environmentStore.GetResourceClass(ctx, id)
		if err != nil {
			t.Fatalf("GetResourceClass(%q) error = %v", id, err)
		}
		if resourceClass.ID != id || resourceClass.ContractVersion != contracts.ContractVersionV1 {
			t.Errorf("GetResourceClass(%q) = %#v", id, resourceClass)
		}
	}
	policies, err := environmentStore.ListLifecyclePolicies(ctx)
	if err != nil {
		t.Fatalf("ListLifecyclePolicies() error = %v", err)
	}
	for _, id := range []string{contracts.LifecyclePolicyAgentCompartment, contracts.LifecyclePolicyCodingEnvironment} {
		if !containsLifecyclePolicy(policies, id) {
			t.Errorf("ListLifecyclePolicies() is missing %q: %#v", id, policies)
		}
		policy, err := environmentStore.GetLifecyclePolicy(ctx, id)
		if err != nil {
			t.Fatalf("GetLifecyclePolicy(%q) error = %v", id, err)
		}
		if policy.ID != id || policy.ContractVersion != contracts.ContractVersionV1 {
			t.Errorf("GetLifecyclePolicy(%q) = %#v", id, policy)
		}
	}
}

func assertMissingEnvironmentStoreResources(t *testing.T, ctx context.Context, environmentStore ports.EnvironmentStore) {
	t.Helper()
	if _, err := environmentStore.GetEnvironment(ctx, "environment-missing"); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("GetEnvironment() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.GetWorkspace(ctx, "workspace-missing"); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("GetWorkspace() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.GetCurrentInstance(ctx, "environment-missing"); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("GetCurrentInstance() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.ListInstances(ctx, "environment-missing"); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("ListInstances() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.HasActiveLease(ctx, "environment-missing", conformanceTime()); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("HasActiveLease() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.GetLease(ctx, "lease-missing"); !errors.Is(err, ports.ErrLeaseNotFound) {
		t.Errorf("GetLease() missing error = %v, want %v", err, ports.ErrLeaseNotFound)
	}
	if _, err := environmentStore.GetSnapshot(ctx, "snapshot-missing"); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("GetSnapshot() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.GetCurrentWorkspaceVersion(ctx, "environment-missing"); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("GetCurrentWorkspaceVersion() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.GetWorkspaceVersion(ctx, "environment-missing", 1); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("GetWorkspaceVersion() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
	if _, err := environmentStore.BeginStart(
		ctx, "environment-missing", 0, "instance-missing", conformanceTime(),
	); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Errorf("BeginStart() missing error = %v, want %v", err, ports.ErrEnvironmentNotFound)
	}
}

func resolveConformanceEnvironment(
	t *testing.T,
	ctx context.Context,
	environmentStore ports.EnvironmentStore,
	suffix string,
	now time.Time,
) ports.ResolveEnvironmentInput {
	t.Helper()
	input := conformanceEnvironmentInput(suffix, now)
	_, created, err := environmentStore.ResolveEnvironment(ctx, input)
	if err != nil {
		t.Fatalf("ResolveEnvironment() fixture error = %v", err)
	}
	if !created {
		t.Fatalf("ResolveEnvironment() fixture %q was unexpectedly reused", suffix)
	}
	return input
}

func conformanceEnvironmentInput(suffix string, now time.Time) ports.ResolveEnvironmentInput {
	environmentID := "environment-" + suffix
	workspaceID := "workspace-" + suffix
	return ports.ResolveEnvironmentInput{
		Environment: contracts.Environment{
			ContractVersion:   contracts.ContractVersionV1,
			ID:                environmentID,
			TenantRef:         "tenant-" + suffix,
			SubjectRef:        "subject-" + suffix,
			EnvironmentKey:    "environment-key-" + suffix,
			WorkspaceID:       workspaceID,
			ImageRef:          "image@sha256:" + suffix,
			ToolchainRef:      "toolchain:" + suffix,
			ResourceClassID:   contracts.ResourceClassAgentStandard,
			LifecyclePolicyID: contracts.LifecyclePolicyAgentCompartment,
			DesiredState:      contracts.DesiredStateStopped,
			State:             contracts.EnvironmentStateStopped,
			ExposedPorts:      []contracts.ExposedPort{},
			Metadata:          map[string]string{"suite": "environment-store-conformance"},
			LastActivityAt:    now.UTC(),
			CreatedAt:         now.UTC(),
			UpdatedAt:         now.UTC(),
		},
		Workspace: contracts.Workspace{
			ContractVersion: contracts.ContractVersionV1,
			ID:              workspaceID,
			TenantRef:       "tenant-" + suffix,
			SubjectRef:      "subject-" + suffix,
			StorageRef:      "workspace:opaque:" + suffix,
			Generation:      1,
			RetainUntil:     now.Add(24 * time.Hour).UTC(),
			CreatedAt:       now.UTC(),
			UpdatedAt:       now.UTC(),
		},
	}
}

func conformanceTime() time.Time {
	return time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
}

func assertEnvironmentIdentity(t *testing.T, actual, expected contracts.Environment) {
	t.Helper()
	if actual.ContractVersion != contracts.ContractVersionV1 ||
		actual.ID != expected.ID || actual.WorkspaceID != expected.WorkspaceID ||
		actual.TenantRef != expected.TenantRef || actual.SubjectRef != expected.SubjectRef ||
		actual.EnvironmentKey != expected.EnvironmentKey {
		t.Fatalf("Environment identity = %#v, want %#v", actual, expected)
	}
}

func containsResourceClass(items []contracts.ResourceClass, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func containsLifecyclePolicy(items []contracts.LifecyclePolicy, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
