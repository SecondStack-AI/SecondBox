package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// A stop or delete intent can commit between the lifecycle claim's start plan
// and its placement. Releasing the claim must keep that intent due now rather
// than defer it to the Assignment deadline, which applies only while the
// Sandbox is still wanted running.
func TestPlacementKeepsIntentThatRacedItsClaimDue(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	running := fixture.request
	deleted := fixture.newSandboxRequest(t)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET reconcile_owner=$2,reconcile_claim_expires_at=$3,next_reconcile_at=$4
		WHERE id=ANY($1::text[])`,
		[]string{running.SandboxID, deleted.SandboxID}, running.LifecycleClaimOwner,
		fixture.now.Add(time.Minute), fixture.now.Add(-time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET desired_state='deleted' WHERE id=$1`, deleted.SandboxID,
	); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		name    string
		request scheduleRequestWithin
		want    time.Time
	}{
		{"running", scheduleRequestWithin{fixture, running}, running.OperationDeadline},
		{"deleted", scheduleRequestWithin{fixture, deleted}, fixture.now},
	} {
		request.request.schedule(t)
		var owner string
		var nextReconcileAt time.Time
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT COALESCE(reconcile_owner,''),next_reconcile_at FROM secondbox.sandboxes WHERE id=$1`,
			request.request.request.SandboxID,
		).Scan(&owner, &nextReconcileAt); err != nil {
			t.Fatal(err)
		}
		if owner != "" || !nextReconcileAt.Equal(request.want) {
			t.Fatalf("%s Sandbox after placement: owner=%q next_reconcile_at=%s, want released claim due %s",
				request.name, owner, nextReconcileAt, request.want)
		}
	}
}

// Assignment events lock their Sandboxes without the quota ledgers, so a batch
// must take them in the same identity order as other multi-Sandbox writers,
// whatever order the Runner reported them in.
func TestAssignmentEventBatchLocksSandboxesInIdentityOrder(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	requests := []scheduleRequestWithin{
		{fixture, fixture.request},
		{fixture, fixture.newSandboxRequest(t)},
	}
	fences := map[string]*runnerv1.AssignmentFence{}
	correlations := map[string]*runnerv1.Correlation{}
	for _, request := range requests {
		request.schedule(t)
	}
	for range requests {
		delivery, found, err := fixture.stateStore.ClaimCommand(
			t.Context(), fixture.homeRunnerID, fixture.homeConnectionID, fixture.now,
		)
		if err != nil || !found || delivery.Message.GetAssignment() == nil {
			t.Fatalf("Assignment command delivery found=%t error=%v", found, err)
		}
		if err := fixture.stateStore.MarkCommandDelivered(t.Context(), delivery, fixture.homeConnectionID, fixture.now); err != nil {
			t.Fatal(err)
		}
		assignment := delivery.Message.GetAssignment()
		fences[assignment.Fence.SandboxId] = assignment.Fence
		correlations[assignment.Fence.SandboxId] = assignment.Correlation
	}
	earlier, later := requests[0].request.SandboxID, requests[1].request.SandboxID
	if later < earlier {
		earlier, later = later, earlier
	}
	var records []runnercontrol.EventPersistenceRecord
	for index, sandboxID := range []string{later, earlier} {
		records = append(records, runnercontrol.EventPersistenceRecord{
			ReceivedAt: fixture.now,
			Event: runnercontrol.Event{
				Kind: runnercontrol.EventAssignment, RunnerID: fixture.homeRunnerID,
				ConnectionID: fixture.homeConnectionID,
				Message: &runnerv1.RunnerToControlPlane{
					Message: &runnerv1.RunnerToControlPlane_AssignmentProgress{
						AssignmentProgress: &runnerv1.AssignmentProgress{
							MessageId: fmt.Sprintf("progress-order-%d", index), Sequence: uint64(4 + index),
							Fence:            proto.Clone(fences[sandboxID]).(*runnerv1.AssignmentFence),
							Stage:            runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_RUNNER_ADMISSION,
							ObservedAtUnixMs: uint64(fixture.now.UnixMilli()),
							ObservedAtUnixNs: uint64(fixture.now.UnixNano()),
							Correlation:      proto.Clone(correlations[sandboxID]).(*runnerv1.Correlation),
						},
					},
				},
			},
		})
	}

	blocker := fixture.holdRow(t, `SELECT id FROM secondbox.sandboxes WHERE id=$1 FOR UPDATE`, earlier)
	result := make(chan error, 1)
	go func() { result <- fixture.stateStore.RecordEvents(t.Context(), records) }()
	waitForSandboxRowLockWaiter(t, fixture.pool)
	probe, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, probeErr := probe.Exec(t.Context(), `SELECT id FROM secondbox.sandboxes WHERE id=$1 FOR UPDATE NOWAIT`, later)
	_ = probe.Rollback(context.Background())
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if probeErr != nil {
		<-result
		t.Fatalf("Assignment batch locked a later Sandbox while waiting for an earlier one: %v", probeErr)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

type scheduleRequestWithin struct {
	fixture schedulerLockFixture
	request scheduler.ScheduleRequest
}

func (placement scheduleRequestWithin) schedule(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, created, err := placement.fixture.scheduler.Schedule(ctx, placement.request); err != nil || !created {
		t.Fatalf("placement of %s created=%t error=%v", placement.request.SandboxID, created, err)
	}
}

// waitForSandboxRowLockWaiter waits until some session blocks on a Sandbox row lock.
func waitForSandboxRowLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE wait_event_type='Lock' AND query LIKE '%FROM secondbox.sandboxes%FOR UPDATE%'
			)`,
		).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no session waited on a Sandbox row lock")
}
