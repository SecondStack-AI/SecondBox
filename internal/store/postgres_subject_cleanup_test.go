package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var adminTestAuditSequence atomic.Int64

func TestSubjectCloseIsAtomicIdempotentAndCleanupHasOneIdentity(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("subject-cleanup-tenant", now)
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	subject := managementTestSubject(tenant.Ref, "subject-cleanup", now)
	if _, err := controlPlaneStore.CreateSubject(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	application, err := controlPlaneStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID: "subject-cleanup-authority", TenantRef: tenant.Ref, SubjectRef: subject.Ref,
		State: contracts.AuthorityStateActive, Scopes: []string{"sandbox:read"},
		ProfileGrants: []string{"coding"}, Metadata: map[string]string{},
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	closeErrors := make(chan error, 1)
	createErrors := make(chan error, 1)
	go func() {
		defer wait.Done()
		<-start
		_, _, closeErr := controlPlaneStore.CloseManagedSubject(
			t.Context(), tenant.Ref, subject.Ref, 1, now.Add(time.Second),
			adminTestInput(tenant.Ref, subject.Ref, "subject.close", "close-race", now.Add(time.Second)),
		)
		closeErrors <- closeErr
	}()
	go func() {
		defer wait.Done()
		<-start
		_, _, createErr := controlPlaneStore.CreateManagedApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
			ID: "subject-cleanup-racing-authority", TenantRef: tenant.Ref, SubjectRef: subject.Ref,
			State: contracts.AuthorityStateActive, Scopes: []string{"sandbox:read"},
			ProfileGrants: []string{"coding"}, Metadata: map[string]string{},
			ExpiresAt: pointerTime(now.Add(30 * time.Minute)), Revision: 1,
			CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
		}, adminTestInput(tenant.Ref, subject.Ref, "application_authority.create", "create-race", now.Add(time.Second)))
		createErrors <- createErr
	}()
	close(start)
	wait.Wait()
	if err := <-closeErrors; err != nil {
		t.Fatal(err)
	}
	createErr := <-createErrors
	if createErr != nil && !errors.Is(createErr, ports.ErrGrantEscalationDenied) {
		t.Fatalf("concurrent authority create error = %v", createErr)
	}
	var active int64
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.application_authorities
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state='active'`,
		tenant.Ref, subject.Ref,
	).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active authorities after close = %d", active)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), application.BearerToken, now.Add(2*time.Second),
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("closed Subject authority authentication = %v", err)
	}

	closed, _, err := controlPlaneStore.CloseManagedSubject(
		t.Context(), tenant.Ref, subject.Ref, 1, now.Add(2*time.Second),
		adminTestInput(tenant.Ref, subject.Ref, "subject.close", "close-repeat", now.Add(2*time.Second)),
	)
	if err != nil || closed.State != contracts.SubjectStateClosed || closed.Revision != 2 {
		t.Fatalf("repeated close = %#v error=%v", closed, err)
	}
	operation := contracts.Operation{
		ID: "operation-subject-cleanup", Kind: "subject_cleanup",
		State: contracts.OperationStatePending, RequestID: "request-subject-cleanup",
		CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second),
	}
	first, _, err := controlPlaneStore.CreateManagedSubjectCleanup(
		t.Context(), tenant.Ref, subject.Ref, operation, closed.Revision,
		now.Add(3*time.Second), adminTestInput(tenant.Ref, subject.Ref, "subject.cleanup", "cleanup-first", now.Add(3*time.Second)),
	)
	if err != nil {
		t.Fatal(err)
	}
	secondCandidate := operation
	secondCandidate.ID = "operation-subject-cleanup-duplicate"
	second, result, err := controlPlaneStore.CreateManagedSubjectCleanup(
		t.Context(), tenant.Ref, subject.Ref, secondCandidate, closed.Revision,
		now.Add(4*time.Second), adminTestInput(tenant.Ref, subject.Ref, "subject.cleanup", "cleanup-second", now.Add(4*time.Second)),
	)
	if err != nil || second.ID != first.ID || !result.Replayed {
		t.Fatalf("cleanup identity replay = %#v result=%#v error=%v", second, result, err)
	}
	inspected, err := controlPlaneStore.GetTenantOperation(t.Context(), tenant.Ref, first.ID)
	if err != nil || inspected.ID != first.ID || inspected.SubjectRef != subject.Ref {
		t.Fatalf("tenant Operation inspection = %#v error=%v", inspected, err)
	}
}

func adminTestInput(tenantRef, subjectRef, operation, key string, now time.Time) ports.AdminIdempotencyInput {
	return ports.AdminIdempotencyInput{
		TenantRef: tenantRef, SubjectRef: subjectRef, Operation: operation,
		TargetID: subjectRef, Key: key, RequestHash: key + "-hash",
		Now: now, Ends: now.Add(time.Hour), AuditEvent: adminTestAudit(key, now),
	}
}

func adminTestAudit(key string, now time.Time) *contracts.AuditEvent {
	return &contracts.AuditEvent{
		ID: fmt.Sprintf("audit-%s-%d", key, adminTestAuditSequence.Add(1)), ActorKind: "test", ActorID: "store-test",
		Action: "store.test", ResourceKind: "test", ResourceID: key,
		Outcome: "succeeded", RequestID: "request-" + key,
		Details: map[string]string{}, CreatedAt: now,
	}
}

func pointerTime(value time.Time) *time.Time { return &value }
