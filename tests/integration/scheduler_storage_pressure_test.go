package integration_test

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// A connected home Runner that reports storage admission denial, or a failed
// storage probe, must receive no new Assignment: the start is deferred as an
// unavailable home, not sent to a Runner that would refuse it. Only a measured
// healthy report re-admits the Runner, including after a reconnect.
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

	// A failed probe replaces the denial but proves nothing about recovery; the
	// Runner refuses admission while it cannot measure.
	reportPressure(5, contracts.StoragePressureStatusUnavailable, fixture.now.Add(time.Second))
	if _, _, err := fixture.scheduler.Schedule(t.Context(), fixture.request); !errors.Is(err, scheduler.ErrHomeRunnerUnavailable) {
		t.Fatalf("placement on unmeasured home = %v, want ErrHomeRunnerUnavailable", err)
	}

	reportPressure(6, "healthy", fixture.now.Add(2*time.Second))
	fixture.scheduleWithin(t, 5*time.Second)

	reportPressure(7, contracts.StoragePressureStatusAdmissionDenied, fixture.now.Add(3*time.Second))
	connectionID := task4ID("connection")
	if err := fixture.stateStore.OpenConnection(
		t.Context(), fixture.homeIdentity, connectionID, 1, fixture.now.Add(4*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.stateStore.RecordRegistration(
		t.Context(),
		task4Registration(fixture.homeRunnerID, connectionID, fixture.poolName),
		fixture.now.Add(4*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if status := readPressure(); status == nil || *status != contracts.StoragePressureStatusAdmissionDenied {
		t.Fatalf("storage pressure after re-registration = %v, want retained admission denial", status)
	}
	fixture.homeConnectionID = connectionID
	// A heartbeat without a completed probe must not erase the refusal either.
	unmeasured := task4Heartbeat(
		fixture.homeRunnerID, connectionID, task4ID("heartbeat"), 2, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
	)
	unmeasured.StoragePressure = nil
	if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), unmeasured, fixture.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if status := readPressure(); status == nil || *status != contracts.StoragePressureStatusAdmissionDenied {
		t.Fatalf("storage pressure after unmeasured heartbeat = %v, want retained admission denial", status)
	}
	reportPressure(3, "healthy", fixture.now.Add(6*time.Second))
	if status := readPressure(); status == nil || *status != "healthy" {
		t.Fatalf("storage pressure after measured recovery = %v, want healthy", status)
	}
}

func TestSchedulerWaitsForPressureAfterRegistration(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	connectionID := task4ID("connection")
	if err := fixture.stateStore.OpenConnection(t.Context(), fixture.homeIdentity, connectionID, 1, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.stateStore.RecordRegistration(t.Context(), task4Registration(fixture.homeRunnerID, connectionID, fixture.poolName), fixture.now); err != nil {
		t.Fatal(err)
	}
	assertBlocked := func() {
		t.Helper()
		if _, _, err := fixture.scheduler.Schedule(t.Context(), fixture.request); !errors.Is(err, scheduler.ErrHomeRunnerUnavailable) {
			t.Fatalf("placement before pressure-bearing heartbeat = %v", err)
		}
	}
	assertBlocked()
	heartbeat := task4Heartbeat(fixture.homeRunnerID, connectionID, task4ID("heartbeat"), 2, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
	heartbeat.StoragePressure = nil
	if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), heartbeat, fixture.now); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	heartbeat.MessageId, heartbeat.Sequence = task4ID("heartbeat"), 3
	heartbeat.StoragePressure = &runnerv1.StoragePressureObservation{Status: "healthy", ObservedAtUnixMs: uint64(fixture.now.UnixMilli())}
	if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), heartbeat, fixture.now); err != nil {
		t.Fatal(err)
	}
	fixture.scheduleWithin(t, 5*time.Second)
}

func TestSchedulerAcceptsRecoveryAfterRunnerClockRollback(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	heartbeat := task4Heartbeat(fixture.homeRunnerID, fixture.homeConnectionID, task4ID("heartbeat"), 4, runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
	heartbeat.StoragePressure = &runnerv1.StoragePressureObservation{Status: "admission_denied", ObservedAtUnixMs: uint64(fixture.now.UnixMilli())}
	if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), heartbeat, fixture.now); err != nil {
		t.Fatal(err)
	}
	connectionID := task4ID("connection")
	if err := fixture.stateStore.OpenConnection(t.Context(), fixture.homeIdentity, connectionID, 1, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.stateStore.RecordRegistration(t.Context(), task4Registration(fixture.homeRunnerID, connectionID, fixture.poolName), fixture.now); err != nil {
		t.Fatal(err)
	}
	heartbeat.ConnectionId, heartbeat.MessageId, heartbeat.Sequence = connectionID, task4ID("heartbeat"), 2
	heartbeat.StoragePressure = &runnerv1.StoragePressureObservation{Status: "healthy", ObservedAtUnixMs: uint64(fixture.now.Add(-time.Hour).UnixMilli())}
	if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), heartbeat, fixture.now); err != nil {
		t.Fatal(err)
	}
	fixture.scheduleWithin(t, 5*time.Second)
	heartbeat.ConnectionId, heartbeat.MessageId, heartbeat.Sequence = fixture.homeConnectionID, task4ID("heartbeat"), 5
	heartbeat.StoragePressure.Status = "admission_denied"
	if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), heartbeat, fixture.now); err == nil {
		t.Fatal("superseded connection overwrote the recovery")
	}
}
