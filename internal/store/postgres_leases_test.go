package store

import (
	"errors"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestLeaseAcquireRefusesSecondActiveAuthorityAndAllowsAfterRelease(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	_, sandboxID := seedLocalWorkspace(t, store, "lease-exclusive", now)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET state='ready',desired_state='running' WHERE id=$1`,
		sandboxID,
	); err != nil {
		t.Fatal(err)
	}
	firstInput := ports.LeaseInput{
		Lease:     contracts.Lease{ID: "lease-exclusive-first"},
		TenantRef: "tenant-local", SubjectRef: "subject-local",
		SandboxID: sandboxID, Generation: 3,
		ExpiresAt: now.Add(time.Minute), Now: now,
		IdempotencyKey: "lease-exclusive-first", RequestHash: "first",
		IdempotencyEnds: now.Add(time.Hour),
	}
	first, err := store.AcquireLease(t.Context(), firstInput)
	if err != nil || first.State != contracts.LeaseStateActive {
		t.Fatalf("first Lease = %#v, %v", first, err)
	}
	replayed, err := store.AcquireLease(t.Context(), firstInput)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("replayed Lease = %#v, %v", replayed, err)
	}
	secondInput := firstInput
	secondInput.Lease.ID = "lease-exclusive-second"
	secondInput.IdempotencyKey = "lease-exclusive-second"
	secondInput.RequestHash = "second"
	if _, err := store.AcquireLease(
		t.Context(),
		secondInput,
	); !errors.Is(err, ports.ErrLeaseAlreadyActive) {
		t.Fatalf("second active Lease error = %v", err)
	}
	released, err := store.ReleaseLease(t.Context(), ports.LeaseInput{
		Lease: first, TenantRef: firstInput.TenantRef, SubjectRef: firstInput.SubjectRef,
		SandboxID: sandboxID, Generation: 3, Now: now.Add(time.Second),
		IdempotencyKey: "lease-exclusive-release", RequestHash: "release",
		IdempotencyEnds: now.Add(time.Hour),
	})
	if err != nil || released.State != contracts.LeaseStateReleased {
		t.Fatalf("released Lease = %#v, %v", released, err)
	}
	secondInput.Now = now.Add(2 * time.Second)
	secondInput.ExpiresAt = secondInput.Now.Add(time.Minute)
	second, err := store.AcquireLease(t.Context(), secondInput)
	if err != nil || second.ID != secondInput.Lease.ID ||
		second.State != contracts.LeaseStateActive {
		t.Fatalf("second Lease after release = %#v, %v", second, err)
	}
}

func TestLeaseAcquireTakeoverAtomicallyFencesPriorAuthority(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	_, sandboxID := seedLocalWorkspace(t, store, "lease-takeover", now)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET state='ready',desired_state='running' WHERE id=$1`,
		sandboxID,
	); err != nil {
		t.Fatal(err)
	}
	firstInput := ports.LeaseInput{
		Lease: contracts.Lease{ID: "lease-takeover-first"}, TenantRef: "tenant-local",
		SubjectRef: "subject-local", SandboxID: sandboxID, Generation: 3,
		ExpiresAt: now.Add(time.Minute), Now: now,
		IdempotencyKey: "lease-takeover-first", RequestHash: "first",
		IdempotencyEnds: now.Add(time.Hour),
	}
	if _, err := store.AcquireLease(t.Context(), firstInput); err != nil {
		t.Fatal(err)
	}
	secondInput := firstInput
	secondInput.Lease.ID = "lease-takeover-second"
	secondInput.IdempotencyKey = "lease-takeover-second"
	secondInput.RequestHash = "second"
	secondInput.ReplaceActive = true
	secondInput.Now = now.Add(time.Second)
	secondInput.ExpiresAt = secondInput.Now.Add(time.Minute)
	second, err := store.AcquireLease(t.Context(), secondInput)
	if err != nil || second.State != contracts.LeaseStateActive {
		t.Fatalf("takeover Lease = %#v, %v", second, err)
	}
	first, err := store.GetLeaseByID(
		t.Context(), firstInput.TenantRef, firstInput.SubjectRef, firstInput.Lease.ID,
	)
	if err != nil || first.State != contracts.LeaseStateFenced {
		t.Fatalf("prior Lease after takeover = %#v, %v", first, err)
	}
}
