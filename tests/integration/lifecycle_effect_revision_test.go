package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

// Exercise the actual claim, effect transaction, and HTTP representation while
// Runner evidence is outstanding. Quarantine deliberately models contradictory
// persisted evidence: a succeeded delete whose Sandbox was never finalized.
func TestUnchangedEffectWaitHoldsPublicRevision(t *testing.T) {
	for _, kind := range []string{"stop", "delete", "delete-quarantine"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			sandboxID, action := prepareWaitingEffect(t, fixture, kind)
			if kind == "delete-quarantine" {
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.lifecycle_effects SET state='succeeded' WHERE sandbox_id=$1`, sandboxID); err != nil {
					t.Fatal(err)
				}
			}
			revision, etag := quiescenceSandboxETag(t, fixture, sandboxID)
			updatedAt := sandboxUpdatedAt(t, fixture, sandboxID)
			for pass := 0; pass < deferralPasses; pass++ {
				fixture.runLifecycle(t, sandboxID, action)
				gotRevision, gotETag := quiescenceSandboxETag(t, fixture, sandboxID)
				if gotRevision != revision || gotETag != etag || !sandboxUpdatedAt(t, fixture, sandboxID).Equal(updatedAt) {
					t.Fatalf("pass %d: revision %d -> %d, ETag %q -> %q, updated_at %s -> %s", pass, revision, gotRevision, etag, gotETag, updatedAt, sandboxUpdatedAt(t, fixture, sandboxID))
				}
				var owner string
				var expires *time.Time
				if err := fixture.pool.QueryRow(t.Context(), `SELECT reconcile_owner,reconcile_claim_expires_at FROM secondbox.sandboxes WHERE id=$1`, sandboxID).Scan(&owner, &expires); err != nil {
					t.Fatal(err)
				}
				if owner != "" || expires != nil {
					t.Fatalf("claim retained: %q %v", owner, expires)
				}
				if _, scheduled := fixture.sandboxDueAt(t, sandboxID); !scheduled {
					t.Fatal("effect wait parked reconciliation")
				}
			}
			if kind == "delete-quarantine" {
				return
			}
			// Expiry is a real transition: persist another command and advance revision.
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.lifecycle_effects SET effect_deadline=$2 WHERE sandbox_id=$1 AND state='queued'`, sandboxID, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			fixture.runLifecycle(t, sandboxID, action)
			retryRevision, retryETag := quiescenceSandboxETag(t, fixture, sandboxID)
			var retries int
			if err := fixture.pool.QueryRow(t.Context(), `SELECT retry_count FROM secondbox.lifecycle_effects WHERE sandbox_id=$1 AND state='queued'`, sandboxID).Scan(&retries); err != nil {
				t.Fatal(err)
			}
			if retries != 1 || retryRevision != revision+1 || retryETag == etag || !sandboxUpdatedAt(t, fixture, sandboxID).After(updatedAt) {
				t.Fatalf("retry count=%d revision=%d (was %d) ETag=%q", retries, retryRevision, revision, retryETag)
			}
			if kind == "stop" {
				fixture.completeFence(t, sandboxID)
				fixture.completeGenerationAdvance(t, sandboxID)
				fixture.runLifecycle(t, sandboxID, lifecycle.ActionFinishStop)
			} else {
				fixture.completeWorkspaceDelete(t, sandboxID)
			}
			finalRevision, _ := quiescenceSandboxETag(t, fixture, sandboxID)
			if finalRevision <= retryRevision {
				t.Fatalf("completion held revision %d", finalRevision)
			}
		})
	}
}

func prepareWaitingEffect(t *testing.T, fixture *teardownFixture, kind string) (string, lifecycle.Action) {
	t.Helper()
	if kind == "stop" {
		sandboxID, _ := fixture.createReadySandbox(t)
		current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.controlPlane.StopSandbox(t.Context(), fixture.principal, sandboxID, "effect-stop-"+sandboxID, current.Revision); err != nil {
			t.Fatal(err)
		}
		fixture.runLifecycle(t, sandboxID, lifecycle.ActionDrain)
		fixture.runLifecycle(t, sandboxID, lifecycle.ActionStopInstance)
		return sandboxID, lifecycle.ActionStopInstance
	}
	sandboxID := stopSandboxToRest(t, fixture)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlPlane.DeleteSandbox(t.Context(), fixture.principal, sandboxID, "effect-delete-"+sandboxID, current.Revision); err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionDelete)
	return sandboxID, lifecycle.ActionDelete
}

// A wait branch may share its transaction with recovery work. Retaining the
// ETag there would hide a public transition added by startup-failure recovery.
func TestEffectWaitRecoveryAdvancesPublicRevision(t *testing.T) {
	for _, kind := range []string{"stop-failed", "stop-mutation", "delete-mutation"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			effectKind := "stop"
			if kind == "delete-mutation" {
				effectKind = "delete"
			}
			sandboxID, action := prepareWaitingEffect(t, fixture, effectKind)
			if kind == "stop-failed" {
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET state='failed',revision=revision+1 WHERE id=$1`, sandboxID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',mutation_expected_generation=NULL,mutation_target_generation=NULL,mutation_state='' WHERE sandbox_id=$1`, sandboxID); err != nil {
					t.Fatal(err)
				}
			}
			revision, etag := quiescenceSandboxETag(t, fixture, sandboxID)
			updatedAt := sandboxUpdatedAt(t, fixture, sandboxID)
			fixture.runLifecycle(t, sandboxID, action)
			recoveredRevision, recoveredETag := quiescenceSandboxETag(t, fixture, sandboxID)
			if recoveredRevision != revision+1 || recoveredETag == etag || !sandboxUpdatedAt(t, fixture, sandboxID).After(updatedAt) {
				t.Fatalf("recovery revision %d -> %d ETag %q -> %q", revision, recoveredRevision, etag, recoveredETag)
			}
			current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
			if err != nil {
				t.Fatal(err)
			}
			if effectKind == "stop" && current.State != "stopping" {
				t.Fatalf("recovery left state %q", current.State)
			}
			fixture.runLifecycle(t, sandboxID, action)
			heldRevision, heldETag := quiescenceSandboxETag(t, fixture, sandboxID)
			if heldRevision != recoveredRevision || heldETag != recoveredETag {
				t.Fatal("repeat recovery wait advanced revision")
			}
		})
	}
}

func TestEffectWaitClaimFences(t *testing.T) {
	for _, kind := range []string{"stop", "delete"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			sandboxID, action := prepareWaitingEffect(t, fixture, kind)
			fixture.deferOtherSandboxes(t, sandboxID)
			now := time.Now().Add(time.Second)
			claim := claimWaitingEffect(t, fixture, sandboxID, "effect-worker-a", now)
			if _, found, err := fixture.store.ClaimLifecycle(t.Context(), "effect-worker-b", now, time.Minute, ports.LifecycleWakeTriggerDeadline); err != nil || found {
				t.Fatalf("live claim admitted second worker: found=%t err=%v", found, err)
			}
			execute := func(claim ports.LifecycleReconcileClaim) error {
				return fixture.reconciler.Effects.ExecuteLifecycleEffect(t.Context(), claim, lifecycle.Decision{Action: action}, now, now.Add(time.Second))
			}
			if err := execute(claim); err != nil {
				t.Fatal(err)
			}
			if err := execute(claim); !errors.Is(err, ports.ErrRevisionConflict) {
				t.Fatalf("consumed claim replay = %v", err)
			}
			now = now.Add(2 * time.Second)
			successor := claimWaitingEffect(t, fixture, sandboxID, "effect-worker-b", now)
			if successor.Revision != claim.Revision {
				t.Fatal("wait changed claim revision")
			}
			if err := execute(claim); !errors.Is(err, ports.ErrRevisionConflict) {
				t.Fatalf("old owner replay = %v", err)
			}
			// Expire a live claim and take it over while retaining the public revision.
			now = now.Add(2 * time.Minute)
			takeover := claimWaitingEffect(t, fixture, sandboxID, "effect-worker-c", now)
			if err := execute(successor); !errors.Is(err, ports.ErrRevisionConflict) {
				t.Fatalf("superseded claimant = %v", err)
			}
			// Expiry will retry, so a replay after that durable transition is fenced by
			// revision as well as ownership, even if the same worker claims again.
			if err := execute(takeover); err != nil {
				t.Fatal(err)
			}
			now = now.Add(2 * time.Second)
			fresh := claimWaitingEffect(t, fixture, sandboxID, "effect-worker-c", now)
			if fresh.Revision <= takeover.Revision {
				t.Fatal("deadline retry did not advance revision")
			}
			if err := execute(takeover); !errors.Is(err, ports.ErrRevisionConflict) {
				t.Fatalf("old revision replay by same worker = %v", err)
			}
			if err := execute(fresh); err != nil {
				t.Fatal(err)
			}
			if kind == "stop" {
				fixture.completeFence(t, sandboxID)
				fixture.completeGenerationAdvance(t, sandboxID)
				fixture.runLifecycle(t, sandboxID, lifecycle.ActionFinishStop)
			} else {
				fixture.completeWorkspaceDelete(t, sandboxID)
			}
			if err := execute(fresh); !errors.Is(err, ports.ErrRevisionConflict) {
				t.Fatalf("old generation/completed effect replay = %v", err)
			}
		})
	}
}

func claimWaitingEffect(t *testing.T, fixture *teardownFixture, sandboxID, owner string, now time.Time) ports.LifecycleReconcileClaim {
	t.Helper()
	claim, found, err := fixture.store.ClaimLifecycle(t.Context(), owner, now, time.Minute, ports.LifecycleWakeTriggerDeadline)
	if err != nil || !found || claim.SandboxID != sandboxID {
		t.Fatalf("claim %s = %+v found=%t err=%v", owner, claim, found, err)
	}
	return claim
}

// A runner's effect-row update and the wait decision must serialize. Verify
// PostgreSQL actually blocks the broker, then observes the newly expired
// deadline and retries instead of committing a wait from an unlocked read.
func TestEffectWaitLocksEffectBeforeDeciding(t *testing.T) {
	for _, kind := range []string{"stop", "delete"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			sandboxID, action := prepareWaitingEffect(t, fixture, kind)
			fixture.deferOtherSandboxes(t, sandboxID)
			now := time.Now().Add(time.Second)
			claim := claimWaitingEffect(t, fixture, sandboxID, "effect-lock-worker", now)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := fixture.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var blocker uint32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM secondbox.lifecycle_effects WHERE sandbox_id=$1 AND state='queued' FOR UPDATE`, sandboxID).Scan(&blocker); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				result <- fixture.reconciler.Effects.ExecuteLifecycleEffect(ctx, claim, lifecycle.Decision{Action: action}, now, now.Add(time.Second))
			}()
			for {
				var blocked bool
				if err := fixture.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("effect transaction did not wait for row lock: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE secondbox.lifecycle_effects SET effect_deadline=$2 WHERE sandbox_id=$1 AND state='queued'`, sandboxID, now.Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			var retryCount int
			if err := fixture.pool.QueryRow(ctx, `SELECT retry_count FROM secondbox.lifecycle_effects WHERE sandbox_id=$1 AND state='queued'`, sandboxID).Scan(&retryCount); err != nil {
				t.Fatal(err)
			}
			revision, _ := quiescenceSandboxETag(t, fixture, sandboxID)
			if retryCount != 1 || revision != claim.Revision+1 {
				t.Fatalf("locked effect update missed: retries=%d revision=%d", retryCount, revision)
			}
		})
	}
}
