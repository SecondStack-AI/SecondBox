package store

import (
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestExpiredLifecycleIdempotencyDoesNotReplay(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	_, sandboxID := seedLocalWorkspace(t, controlPlaneStore, "lifecycle-expiry", now)
	input := ports.LifecycleIntentInput{
		Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"},
		SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateStopped,
		Operation: localTestOperation("operation-lifecycle-expiry-first", "stop", now),
		Now:       now, ExpectedRevision: 1, IdempotencyKey: "lifecycle-expiry",
		RequestHash: "first", IdempotencyEnds: now.Add(time.Hour),
	}
	first, err := controlPlaneStore.SetSandboxDesiredState(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Operation = localTestOperation("operation-lifecycle-expiry-second", "stop", now.Add(2*time.Hour))
	input.Now = now.Add(2 * time.Hour)
	input.ExpectedRevision = 2
	input.RequestHash = "second"
	input.IdempotencyEnds = input.Now.Add(time.Hour)
	second, err := controlPlaneStore.SetSandboxDesiredState(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.ID != input.Operation.ID {
		t.Fatalf("expired lifecycle replay returned %q after %q", second.ID, first.ID)
	}
}

func TestExpiredLeaseIdempotencyDoesNotReplay(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC)
	_, sandboxID := seedLocalWorkspace(t, controlPlaneStore, "lease-expiry", now)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET state='ready',desired_state='running' WHERE id=$1`, sandboxID); err != nil {
		t.Fatal(err)
	}
	input := ports.LeaseInput{
		Lease: contracts.Lease{ID: "lease-expiry-first"}, TenantRef: "tenant-local",
		SubjectRef: "subject-local", SandboxID: sandboxID, Generation: 3,
		ExpiresAt: now.Add(30 * time.Minute), Now: now, IdempotencyKey: "lease-expiry",
		RequestHash: "first", IdempotencyEnds: now.Add(time.Hour),
	}
	first, err := controlPlaneStore.AcquireLease(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.leases SET state='released' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	input.Lease.ID = "lease-expiry-second"
	input.Now = now.Add(2 * time.Hour)
	input.ExpiresAt = input.Now.Add(30 * time.Minute)
	input.RequestHash = "second"
	input.IdempotencyEnds = input.Now.Add(time.Hour)
	second, err := controlPlaneStore.AcquireLease(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.ID != input.Lease.ID {
		t.Fatalf("expired Lease replay returned %q after %q", second.ID, first.ID)
	}
}

func TestExpiredSnapshotIdempotencyDoesNotReplay(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, controlPlaneStore, "snapshot-expiry", now)
	seedLocalWorkspacePolicyAndRunner(t, controlPlaneStore, now)
	input := localSnapshotCreateInput(sandboxID, "snapshot-expiry-first", "snapshot-expiry", now)
	first, err := controlPlaneStore.CreateSnapshot(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',
		    mutation_expected_generation=NULL,mutation_target_generation=NULL,mutation_state=''
		WHERE id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	input.Snapshot.ID = "snapshot-expiry-second"
	input.Snapshot.Name = "snapshot-expiry-second"
	input.Snapshot.CreatedAt = now.Add(2 * time.Hour)
	retainUntil := input.Snapshot.CreatedAt.Add(24 * time.Hour)
	input.Snapshot.RetainUntil = &retainUntil
	input.Operation = localTestOperation("operation-snapshot-expiry-second", "snapshot_create", input.Snapshot.CreatedAt)
	input.EffectID = "effect-snapshot-expiry-second"
	input.CommandID = "command-snapshot-expiry-second"
	input.RequestHash = "second"
	input.IdempotencyEnds = input.Snapshot.CreatedAt.Add(time.Hour)
	input.ExpectedRevision = 2
	second, err := controlPlaneStore.CreateSnapshot(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.ID != input.Operation.ID {
		t.Fatalf("expired Snapshot replay returned %q after %q", second.ID, first.ID)
	}
}
