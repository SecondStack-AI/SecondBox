package integration_test

import (
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEveryUsefulSessionKindSuppressesIdleReclamationWhileGuestHeartbeatDoesNot(
	t *testing.T,
) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "useful-session-idle",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-useful-session-idle",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "useful-session-idle-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seedRelayReadyAssignment(t, sandbox, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation,
		"useful-session-idle-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, WorkerID: "useful-session-idle-worker",
		ClaimDuration: time.Minute, PollInterval: time.Second,
	}

	for _, kind := range []string{
		contracts.ActivitySessionKindExec,
		contracts.ActivitySessionKindFile,
		contracts.ActivitySessionKindPTY,
		contracts.ActivitySessionKindPort,
	} {
		t.Run(kind, func(t *testing.T) {
			session, err := controlPlane.OpenActivitySession(
				t.Context(), principal, sandbox.ID, sandbox.Generation, lease.ID, kind,
			)
			if err != nil {
				t.Fatal(err)
			}
			makeSandboxIdleAndDue(t, pool, sandbox.ID, now)
			decision, found, err := reconciler.RunOnce(t.Context(), now)
			if err != nil || !found {
				t.Fatalf("active %s lifecycle reconciliation = %#v, %t, %v", kind, decision, found, err)
			}
			if decision.Action != lifecycle.ActionWait {
				t.Fatalf("active %s session allowed idle reclamation: %#v", kind, decision)
			}
			if _, err := controlPlane.CloseActivitySession(
				t.Context(), principal, sandbox.ID, sandbox.Generation, session.ID,
			); err != nil {
				t.Fatal(err)
			}
		})
	}

	makeSandboxIdleAndDue(t, pool, sandbox.ID, now)
	beforeHeartbeat, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.ReportGuestLiveness(
		t.Context(), principal, sandbox.ID, sandbox.Generation, contracts.GuestLivenessReady,
	); err != nil {
		t.Fatal(err)
	}
	afterHeartbeat, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHeartbeat.LastActivityAt == nil || afterHeartbeat.LastActivityAt == nil ||
		!afterHeartbeat.LastActivityAt.Equal(*beforeHeartbeat.LastActivityAt) {
		t.Fatalf(
			"guest heartbeat changed useful activity: before=%v after=%v",
			beforeHeartbeat.LastActivityAt, afterHeartbeat.LastActivityAt,
		)
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now)
	if err != nil || !found {
		t.Fatalf("heartbeat-only lifecycle reconciliation = %#v, %t, %v", decision, found, err)
	}
	if decision.Action != lifecycle.ActionDrain ||
		decision.TerminationReason != contracts.TerminationReasonIdleTimeout {
		t.Fatalf("heartbeat suppressed idle reclamation: %#v", decision)
	}
}

func makeSandboxIdleAndDue(
	t *testing.T,
	pool *pgxpool.Pool,
	sandboxID string,
	now time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET last_activity_at=$2,next_reconcile_at=$3,updated_at=$3
		WHERE id=$1`,
		sandboxID, now.Add(-301*time.Second), now,
	); err != nil {
		t.Fatal(err)
	}
}
