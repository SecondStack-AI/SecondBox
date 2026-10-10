package integration_test

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// A connected home Runner that reports storage admission denial must receive
// no new Assignment: the start is deferred as an unavailable home, not sent to
// a Runner that would refuse it. A healthy report re-admits the Runner, and a
// new connection discards the previous connection's pressure evidence.
func TestSchedulerDefersStartWhileHomeRunnerDeniesStorageAdmission(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	reportPressure := func(sequence uint64, status string, observedAt time.Time) {
		t.Helper()
		heartbeat := task4Heartbeat(
			fixture.homeRunnerID, fixture.homeConnectionID,
			task4ID("heartbeat"), sequence, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
		)
		heartbeat.StoragePressure = &runnerv1.StoragePressureObservation{
			Status: status, ObservedAtUnixMs: uint64(observedAt.UnixMilli()),
		}
		if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), heartbeat, observedAt); err != nil {
			t.Fatal(err)
		}
	}
	readPressure := func() *string {
		t.Helper()
		var status *string
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT storage_pressure_json->>'status' FROM secondbox.runners WHERE id=$1`,
			fixture.homeRunnerID,
		).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	reportPressure(4, contracts.StoragePressureStatusAdmissionDenied, fixture.now)
	if _, _, err := fixture.scheduler.Schedule(t.Context(), fixture.request); !errors.Is(err, scheduler.ErrHomeRunnerUnavailable) {
		t.Fatalf("placement on admission-denied home = %v, want ErrHomeRunnerUnavailable", err)
	}
	var state string
	var assignments int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT runner.state,
		       (SELECT count(*) FROM secondbox.assignments WHERE sandbox_id=$2)
		FROM secondbox.runners AS runner WHERE runner.id=$1`,
		fixture.homeRunnerID, fixture.request.SandboxID,
	).Scan(&state, &assignments); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || assignments != 0 {
		t.Fatalf("admission-denied home state=%q assignments=%d, want ready with none", state, assignments)
	}

	reportPressure(5, "healthy", fixture.now.Add(time.Second))
	fixture.scheduleWithin(t, 5*time.Second)

	reportPressure(6, contracts.StoragePressureStatusAdmissionDenied, fixture.now.Add(2*time.Second))
	connectionID := task4ID("connection")
	if err := fixture.stateStore.OpenConnection(
		t.Context(), fixture.homeIdentity, connectionID, 1, fixture.now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.stateStore.RecordRegistration(
		t.Context(),
		task4Registration(fixture.homeRunnerID, connectionID, fixture.poolName),
		fixture.now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if status := readPressure(); status != nil {
		t.Fatalf("storage pressure after re-registration = %q, want none until the new connection reports", *status)
	}
}
