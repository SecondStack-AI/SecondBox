package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A start is pinned to its home Runner, so placement must not wait on any other
// Runner row in the pool. Holding a sibling Runner's row lock models a
// concurrent placement on that Runner.
func TestSchedulerPlacementDoesNotLockSiblingRunners(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	sibling := fixture.holdRow(t, `SELECT id FROM secondbox.runners WHERE id=$1 FOR UPDATE`, fixture.siblingRunnerID)
	defer sibling.Rollback(context.Background())
	fixture.scheduleWithin(t, 5*time.Second)
}

// Placing a Sandbox already admitted to run does not change quota usage, so it
// must not wait for admission holding the Tenant quota ledger.
func TestSchedulerPlacementOfAdmittedStartSkipsQuotaLedger(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	ledger := fixture.holdRow(t, `SELECT tenant_ref FROM secondbox.tenant_quotas WHERE tenant_ref=$1 FOR UPDATE`, "task4-project")
	defer ledger.Rollback(context.Background())
	fixture.scheduleWithin(t, 5*time.Second)
}

// Restarting a failed Sandbox increases quota usage, so its placement must
// serialize with admission on the Tenant quota ledger.
func TestSchedulerPlacementOfFailedRestartWaitsForQuotaLedger(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET state='failed' WHERE id=$1`, fixture.request.SandboxID,
	); err != nil {
		t.Fatal(err)
	}
	ledger := fixture.holdRow(t, `SELECT tenant_ref FROM secondbox.tenant_quotas WHERE tenant_ref=$1 FOR UPDATE`, "task4-project")
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := fixture.scheduler.Schedule(ctx, fixture.request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed-Sandbox restart placement with the quota ledger held = %v, want it to wait", err)
	}
	if err := ledger.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.scheduleWithin(t, 5*time.Second)
}

type schedulerLockFixture struct {
	pool             *pgxpool.Pool
	stateStore       *runnercontrol.PostgresStateStore
	scheduler        *scheduler.PostgresStore
	request          scheduler.ScheduleRequest
	now              time.Time
	poolName         string
	homeRunnerID     string
	homeConnectionID string
	homeIdentity     runnercontrol.RunnerIdentity
	siblingRunnerID  string
}

func newSchedulerLockFixture(t *testing.T) schedulerLockFixture {
	t.Helper()
	fixture := schedulerLockFixture{
		now:      time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		poolName: task4ID("home-lock-pool"),
	}
	task4InsertRunnerPool(t, fixture.poolName, fixture.now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, fixture.now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, fixture.now)
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	fixture.stateStore = stateStore
	register := func() (string, string, runnercontrol.RunnerIdentity) {
		runnerID := task4ID("runner")
		connectionID := task4ID("connection")
		issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := stateStore.OpenConnection(t.Context(), issued.Identity, connectionID, 1, fixture.now); err != nil {
			t.Fatal(err)
		}
		if _, err := stateStore.RecordRegistration(t.Context(), task4Registration(runnerID, connectionID, fixture.poolName), fixture.now); err != nil {
			t.Fatal(err)
		}
		heartbeat := task4Heartbeat(runnerID, connectionID, "heartbeat-2", 2, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
		if _, err := stateStore.RecordHeartbeat(t.Context(), heartbeat, fixture.now); err != nil {
			t.Fatal(err)
		}
		task4CompleteWorkspaceReconciliation(t, stateStore, runnerID, connectionID, 3, nil, fixture.now)
		return runnerID, connectionID, issued.Identity
	}
	fixture.homeRunnerID, fixture.homeConnectionID, fixture.homeIdentity = register()
	fixture.siblingRunnerID, _, _ = register()

	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fixture.pool = pool
	fixedNow := fixture.now
	fixture.scheduler, err = scheduler.NewPostgresStore(t.Context(), scheduler.PostgresStoreConfig{
		DatabaseURL: integrationDatabaseURL,
		Now:         func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fixture.scheduler.Close)
	fixture.request = fixture.newSandboxRequest(t)
	return fixture
}

// newSandboxRequest inserts another startable Sandbox homed on the fixture's
// home Runner and returns its placement request.
func (fixture schedulerLockFixture) newSandboxRequest(t *testing.T) scheduler.ScheduleRequest {
	t.Helper()
	now := fixture.now
	sandboxID := task4ID("sandbox")
	profileRevisionID := task4ID("profile-revision")
	workspaceID := task4InsertSchedulableSandbox(t, sandboxID, profileRevisionID, fixture.homeRunnerID, now)
	assignmentID := task4ID("assignment")
	instanceID := task4ID("instance")
	fencingToken := []byte("01234567890123456789012345678901")
	requirements := []string{"local-workspace", "network-policy"}
	return scheduler.ScheduleRequest{
		AssignmentID: assignmentID, AssignmentCommandID: task4ID("assignment-command"),
		InstanceID: instanceID, SandboxID: sandboxID, ProfileRevisionID: profileRevisionID,
		WorkspaceID: workspaceID, StartMutationID: task4ID("workspace-start"),
		Requirements: scheduler.Requirements{
			PoolName: fixture.poolName, Architecture: "amd64", RequiredCapabilities: requirements,
			Capacity: scheduler.Capacity{
				VCPUCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30,
				Instances: 1, Operations: 1,
			},
		},
		AssignmentCommand: &runnerv1.AssignmentCommand{
			WorkspaceId: workspaceID,
			Fence: &runnerv1.AssignmentFence{
				AssignmentId: assignmentID, SandboxId: sandboxID, InstanceId: instanceID,
				SandboxGeneration: 1, FencingToken: fencingToken,
			},
			ProfileRevisionId: profileRevisionID,
			Requirements: &runnerv1.ProfileRequirements{
				VcpuCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30,
				Architecture: "amd64", RequiredCapabilities: requirements,
				MaximumOperationMs: 60_000, MaximumOutputBytes: 1 << 20,
			},
			DeadlineUnixMs: uint64(now.Add(2 * time.Minute).UnixMilli()),
			Correlation: &runnerv1.Correlation{
				RequestId: task4ID("request"), OperationId: task4ID("operation"),
				SandboxId: sandboxID, SandboxGeneration: 1,
			},
		},
		FencingToken:   fencingToken,
		ClaimExpiresAt: now.Add(time.Minute), OperationDeadline: now.Add(2 * time.Minute),
		RetryLimit: 2, SerializationRetryLimit: 3,
		HeartbeatTimeout: 30 * time.Second, Now: now,
		EffectStartedAt: now, PlanReadyAt: now,
		LifecycleClaimOwner: "lifecycle-worker-test",
	}
}

func (fixture schedulerLockFixture) holdRow(t *testing.T, query string, id string) pgx.Tx {
	t.Helper()
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(t.Context(), query, id); err != nil {
		t.Fatal(err)
	}
	return tx
}

func (fixture schedulerLockFixture) scheduleWithin(t *testing.T, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	assignment, created, err := fixture.scheduler.Schedule(ctx, fixture.request)
	if err != nil {
		t.Fatalf("placement waited on a lock it does not need: %v", err)
	}
	if !created || assignment.RunnerID != fixture.homeRunnerID {
		t.Fatalf("placement = created %t runner %q, want new Assignment on home Runner %q",
			created, assignment.RunnerID, fixture.homeRunnerID)
	}
}
