package rowlock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestQuotaUsagePredicateMatchesTrigger(t *testing.T) {
	pool := openRowlockTestPool(t)
	states := []string{
		contracts.SandboxStateCreating, contracts.SandboxStateStopped, contracts.SandboxStateStarting,
		contracts.SandboxStateReady, contracts.SandboxStateDraining, contracts.SandboxStateStopping,
		contracts.SandboxStateFailed, contracts.SandboxStateDeleting, contracts.SandboxStateDeleted,
	}
	desiredStates := []string{
		contracts.SandboxDesiredStateRunning, contracts.SandboxDesiredStateStopped,
		contracts.SandboxDesiredStateDeleted,
	}
	for _, state := range states {
		for _, desired := range desiredStates {
			var active bool
			if err := pool.QueryRow(t.Context(),
				`SELECT secondbox.sandbox_quota_active($1,$2)`, state, desired,
			).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if got := QuotaUsageActive(state, desired); got != active {
				t.Errorf("QuotaUsageActive(%q, %q) = %t, trigger predicate = %t", state, desired, got, active)
			}
		}
	}
}

func TestNonIncreasingTransitionDoesNotWaitForQuotaLedgers(t *testing.T) {
	pool := openRowlockTestPool(t)
	fixture := seedRowlockFixture(t, pool, fmt.Sprintf("ledgerless-%d", time.Now().UnixNano()))
	holdTenantQuotaLedger(t, pool, fixture.tenantRef)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	locked, err := SandboxWorkspaceForTransition(ctx, tx, fixture.sandboxID, NonIncreasingTransition)
	if err != nil {
		t.Fatalf("non-increasing transition waited for the quota ledgers: %v", err)
	}
	if locked.SandboxState != contracts.SandboxStateStopped || locked.Workspace.ID != fixture.workspaceID {
		t.Fatalf("locked Sandbox = %#v", locked)
	}
	// Rewriting the state it already has changes no quota usage, so the
	// trigger does not reach for the held ledger either.
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET state='stopped',desired_state='deleted' WHERE id=$1`,
		fixture.sandboxID,
	); err != nil {
		t.Fatalf("non-increasing Sandbox write waited for the quota ledgers: %v", err)
	}
	assertRowLockUnavailable(t, pool, "secondbox.sandboxes", fixture.sandboxID)
	assertRowLockUnavailable(t, pool, "secondbox.workspaces", fixture.workspaceID)
}

func TestLedgerlessSandboxRejectsQuotaIncrease(t *testing.T) {
	pool := openRowlockTestPool(t)
	fixture := seedRowlockFixture(t, pool, fmt.Sprintf("ledgerless-increase-%d", time.Now().UnixNano()))
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := SandboxWorkspaceForTransition(t.Context(), tx, fixture.sandboxID, NonIncreasingTransition); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET desired_state='running' WHERE id=$1`, fixture.sandboxID)
	if err == nil || !strings.Contains(err.Error(), "increases quota usage without its quota ledger lock") {
		t.Fatalf("quota increase on a ledgerless Sandbox error = %v", err)
	}
}

func TestIncreasingTransitionTakesQuotaLedgersBeforeSandbox(t *testing.T) {
	pool := openRowlockTestPool(t)
	fixture := seedRowlockFixture(t, pool, fmt.Sprintf("ledger-first-%d", time.Now().UnixNano()))
	blocker := holdTenantQuotaLedger(t, pool, fixture.tenantRef)

	mutation, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Rollback(t.Context())
	var mutationPID int32
	if err := mutation.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&mutationPID); err != nil {
		t.Fatal(err)
	}
	startRunning := func(state, desired string) bool {
		return QuotaUsageIncreases(state, desired, state, contracts.SandboxDesiredStateRunning)
	}
	result := make(chan error, 1)
	go func() {
		_, lockErr := SandboxWorkspaceForTransition(t.Context(), mutation, fixture.sandboxID, startRunning)
		if lockErr == nil {
			_, lockErr = mutation.Exec(t.Context(), `
				UPDATE secondbox.sandboxes SET desired_state='running' WHERE id=$1`, fixture.sandboxID)
		}
		result <- lockErr
	}()
	waitForBackendLock(t, pool, mutationPID)
	var sandboxID string
	if err := pool.QueryRow(t.Context(), `
		SELECT id FROM secondbox.sandboxes WHERE id=$1 FOR UPDATE NOWAIT`, fixture.sandboxID,
	).Scan(&sandboxID); err != nil {
		t.Fatalf("increasing transition locked the Sandbox before its quota ledgers: %v", err)
	}
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestTransitionReportsQuotaLedgerRequiredWhenLockedStateIncreases(t *testing.T) {
	pool := openRowlockTestPool(t)
	fixture := seedRowlockFixture(t, pool, fmt.Sprintf("ledger-required-%d", time.Now().UnixNano()))
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	// The first evaluation models an unlocked read taken before a concurrent
	// writer moved the Sandbox into a state the transition would increase.
	evaluations := 0
	increases := func(string, string) bool {
		evaluations++
		return evaluations > 1
	}
	if _, err := SandboxWorkspaceForTransition(t.Context(), tx, fixture.sandboxID, increases); !errors.Is(err, ErrQuotaLedgerRequired) {
		t.Fatalf("stale unlocked quota decision error = %v", err)
	}
}

func holdTenantQuotaLedger(t *testing.T, pool *pgxpool.Pool, tenantRef string) pgx.Tx {
	t.Helper()
	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if err := blocker.QueryRow(t.Context(), `
		SELECT tenant_ref FROM secondbox.tenant_quotas
		WHERE tenant_ref=$1 FOR UPDATE`, tenantRef,
	).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	return blocker
}
