package lifecycle

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestReconcilerConsumesDurableClaimAndCommitsOneTransition(t *testing.T) {
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState: contracts.SandboxStateStopped,
		DesiredState:  contracts.SandboxDesiredStateRunning,
	}}
	effects := &fakeEffectExecutor{}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
		Effects: effects,
	}
	decision, found, err := reconciler.RunOnce(
		t.Context(), time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		ports.LifecycleWakeTriggerNotify,
	)
	if err != nil || !found || decision.Action != ActionStartInstance ||
		effects.action != ActionStartInstance || store.action != "" {
		t.Fatalf(
			"reconciliation = %#v, %t, %v, effect %q, database-only commit %q",
			decision, found, err, effects.action, store.action,
		)
	}
}

func TestReconcilerWaitsForStoppedWorkspaceCreationEvidence(t *testing.T) {
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState: contracts.SandboxStateCreating,
		DesiredState:  contracts.SandboxDesiredStateStopped,
	}}
	effects := &fakeEffectExecutor{}
	reconciler := Reconciler{
		Store: store, Effects: effects, WorkerID: "worker-1",
		ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(
		t.Context(), time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		ports.LifecycleWakeTriggerNotify,
	)
	if err != nil || !found || decision.Action != ActionWait ||
		store.action != string(ActionWait) || effects.action != "" {
		t.Fatalf(
			"reconciliation = %#v, %t, %v, effect %q, database commit %q",
			decision, found, err, effects.action, store.action,
		)
	}
}

func TestReconcilerDefersWorkspaceEffectContention(t *testing.T) {
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState: contracts.SandboxStateStopped,
		DesiredState:  contracts.SandboxDesiredStateRunning,
	}}
	effects := &fakeEffectExecutor{err: ports.ErrWorkspaceMutation}
	reconciler := Reconciler{
		Store: store, Effects: effects, WorkerID: "worker-1",
		ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(
		t.Context(), time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		ports.LifecycleWakeTriggerNotify,
	)
	if err != nil || !found || decision.Action != ActionStartInstance ||
		store.action != string(ActionWait) {
		t.Fatalf(
			"contention reconciliation = %#v, %t, %v, database commit %q",
			decision, found, err, store.action,
		)
	}
}

// Losing a serialization race is an ordinary outcome of concurrency under
// serializable isolation, not a fault. Failing here ends the reconciler, and
// ending the reconciler shuts the server down: a burst of concurrent placements
// would take the whole control plane with it.
func TestReconcilerDefersSerializationContentionInsteadOfFailing(t *testing.T) {
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState: contracts.SandboxStateStopped,
		DesiredState:  contracts.SandboxDesiredStateRunning,
	}}
	effects := &fakeEffectExecutor{
		err: fmt.Errorf("placement: %w", ports.ErrSerializationContention),
	}
	reconciler := Reconciler{
		Store: store, Effects: effects, WorkerID: "worker-1",
		ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(
		t.Context(), time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		ports.LifecycleWakeTriggerNotify,
	)
	if err != nil {
		t.Fatalf("serialization contention ended reconciliation: %v", err)
	}
	if !found || decision.Action != ActionStartInstance ||
		store.action != string(ActionWait) {
		t.Fatalf(
			"contention reconciliation = %#v, %t, database commit %q",
			decision, found, store.action,
		)
	}
}

func TestReconcilerSchedulesHealthyReadySandboxAtIdleDeadline(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	readyAt := now.Add(-10 * time.Minute)
	lastActivityAt := now.Add(-2 * time.Minute)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState:          contracts.SandboxStateReady,
		DesiredState:           contracts.SandboxDesiredStateRunning,
		GuestLiveness:          contracts.GuestLivenessReady,
		ReadyAt:                &readyAt,
		LastActivityAt:         &lastActivityAt,
		IdleSeconds:            600,
		MaximumDurationSeconds: 3600,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionWait {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	want := lastActivityAt.Add(10 * time.Minute)
	if !store.nextReconcileAt.Equal(want) {
		t.Fatalf("next reconciliation = %s, want idle deadline %s", store.nextReconcileAt, want)
	}
}

func TestReconcilerSchedulesHealthyReadySandboxAtMaximumDeadline(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	readyAt := now.Add(-50 * time.Minute)
	lastActivityAt := now
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState:          contracts.SandboxStateReady,
		DesiredState:           contracts.SandboxDesiredStateRunning,
		GuestLiveness:          contracts.GuestLivenessReady,
		ReadyAt:                &readyAt,
		LastActivityAt:         &lastActivityAt,
		IdleSeconds:            3600,
		MaximumDurationSeconds: 3600,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionWait {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	want := readyAt.Add(time.Hour)
	if !store.nextReconcileAt.Equal(want) {
		t.Fatalf("next reconciliation = %s, want maximum deadline %s", store.nextReconcileAt, want)
	}
}

func TestReconcilerPollsHealthyReadySandboxWhileSessionIsActive(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	readyAt := now.Add(-10 * time.Minute)
	lastActivityAt := now.Add(-2 * time.Minute)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState:          contracts.SandboxStateReady,
		DesiredState:           contracts.SandboxDesiredStateRunning,
		GuestLiveness:          contracts.GuestLivenessReady,
		ReadyAt:                &readyAt,
		LastActivityAt:         &lastActivityAt,
		ActiveSessions:         1,
		IdleSeconds:            60,
		MaximumDurationSeconds: 3600,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionWait {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	want := now.Add(time.Second)
	if !store.nextReconcileAt.Equal(want) {
		t.Fatalf("next reconciliation = %s, want poll deadline %s", store.nextReconcileAt, want)
	}
}

func TestReconcilerKeepsTransitionalWaitOnPollInterval(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState: contracts.SandboxStateStarting,
		DesiredState:  contracts.SandboxDesiredStateRunning,
		GuestLiveness: contracts.GuestLivenessStarting,
		HasInstance:   true,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionWait {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	want := now.Add(time.Second)
	if !store.nextReconcileAt.Equal(want) {
		t.Fatalf("next reconciliation = %s, want poll deadline %s", store.nextReconcileAt, want)
	}
}

// The drain and finish-stop commits determine their own successor, so they
// leave the Sandbox due at once instead of paying a recovery poll interval that
// occupies no work. These four tests fix that boundary: the two transitions
// that qualify, and the two conditions under which each does not.
func TestReconcilerLeavesDrainCommitImmediatelyDue(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState: contracts.SandboxStateReady,
		DesiredState:  contracts.SandboxDesiredStateDeleted,
		GuestLiveness: contracts.GuestLivenessReady,
		HasInstance:   true,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionDrain {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	if !store.nextReconcileAt.Equal(now) {
		t.Fatalf(
			"next reconciliation = %s, want the drain commit clock %s",
			store.nextReconcileAt, now,
		)
	}
}

func TestReconcilerKeepsDrainOnPollIntervalWhileASessionIsActive(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState:     contracts.SandboxStateReady,
		DesiredState:      contracts.SandboxDesiredStateDeleted,
		GuestLiveness:     contracts.GuestLivenessReady,
		HasInstance:       true,
		ActiveSessions:    1,
		DrainGraceSeconds: 30,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionDrain {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	want := now.Add(time.Second)
	if !store.nextReconcileAt.Equal(want) {
		t.Fatalf(
			"next reconciliation = %s, want the poll deadline %s while the drain barrier holds",
			store.nextReconcileAt, want,
		)
	}
}

func TestReconcilerLeavesFinishStopImmediatelyDueWhenDeletionIsWanted(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState:   contracts.SandboxStateStopping,
		DesiredState:    contracts.SandboxDesiredStateDeleted,
		StopEffectState: "runner_succeeded",
		GuestLiveness:   contracts.GuestLivenessStopped,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionFinishStop {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	if !store.nextReconcileAt.Equal(now) {
		t.Fatalf(
			"next reconciliation = %s, want the finish-stop commit clock %s",
			store.nextReconcileAt, now,
		)
	}
}

func TestReconcilerParksFinishStopWhenStoppedIsWanted(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState:   contracts.SandboxStateStopping,
		DesiredState:    contracts.SandboxDesiredStateStopped,
		StopEffectState: "runner_succeeded",
		GuestLiveness:   contracts.GuestLivenessStopped,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionFinishStop {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	if !store.nextReconcileAt.IsZero() {
		t.Fatalf(
			"next reconciliation = %s, want a parked schedule: finish-stop commits the desired state",
			store.nextReconcileAt,
		)
	}
}

// TestReconcilerParksSandboxesAtRest fixes the boundary of the rest matrix. A
// Sandbox holding its desired state with no deadline that can change the
// decision leaves the reconciliation work set entirely; anything with an
// outstanding transition or an unmet desired state keeps its schedule.
func TestReconcilerParksSandboxesAtRest(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name       string
		observed   string
		desired    string
		wantAction Action
		wantParked bool
	}{
		{
			name: "stopped Sandbox wanted stopped", observed: contracts.SandboxStateStopped,
			desired:    contracts.SandboxDesiredStateStopped,
			wantAction: ActionWait, wantParked: true,
		},
		{
			name: "failed Sandbox wanted stopped", observed: contracts.SandboxStateFailed,
			desired:    contracts.SandboxDesiredStateStopped,
			wantAction: ActionWait, wantParked: true,
		},
		{
			name: "deleted Sandbox wanted deleted", observed: contracts.SandboxStateDeleted,
			desired:    contracts.SandboxDesiredStateDeleted,
			wantAction: ActionWait, wantParked: true,
		},
		{
			name: "creating Sandbox wanted stopped", observed: contracts.SandboxStateCreating,
			desired:    contracts.SandboxDesiredStateStopped,
			wantAction: ActionWait, wantParked: false,
		},
		{
			name: "stopped Sandbox wanted running", observed: contracts.SandboxStateStopped,
			desired:    contracts.SandboxDesiredStateRunning,
			wantAction: ActionStartInstance, wantParked: false,
		},
		{
			name: "stopped Sandbox wanted deleted", observed: contracts.SandboxStateStopped,
			desired:    contracts.SandboxDesiredStateDeleted,
			wantAction: ActionDelete, wantParked: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
				SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
				ObservedState: testCase.observed, DesiredState: testCase.desired,
			}}
			effects := &fakeEffectExecutor{}
			reconciler := Reconciler{
				Store: store, Effects: effects, WorkerID: "worker-1",
				ClaimDuration: time.Minute, PollInterval: time.Second,
			}
			decision, found, err := reconciler.RunOnce(
				t.Context(), now, ports.LifecycleWakeTriggerNotify,
			)
			if err != nil || !found || decision.Action != testCase.wantAction {
				t.Fatalf(
					"reconciliation = %#v, %t, %v, want %s",
					decision, found, err, testCase.wantAction,
				)
			}
			if !actionRequiresEffect(testCase.wantAction) {
				if parked := store.nextReconcileAt.IsZero(); parked != testCase.wantParked {
					t.Fatalf(
						"next reconciliation = %s parked=%t, want parked=%t",
						store.nextReconcileAt, parked, testCase.wantParked,
					)
				}
				return
			}
			// An effect action never reaches the parking decision: it commits
			// its own schedule and waits for the Runner's acknowledgement.
			if store.action != "" {
				t.Fatalf("effect action committed a database-only decision %q", store.action)
			}
		})
	}
}

func TestReconcilerProcessesClaimBatchSequentially(t *testing.T) {
	store := &fakeReconcileStore{batchClaims: []ports.LifecycleReconcileClaim{
		{
			SandboxID: "sbx-batch-1", WorkerID: "worker-batch", Revision: 2,
			ObservedState: contracts.SandboxStateCreating,
			DesiredState:  contracts.SandboxDesiredStateStopped,
		},
		{
			SandboxID: "sbx-batch-2", WorkerID: "worker-batch", Revision: 4,
			ObservedState: contracts.SandboxStateCreating,
			DesiredState:  contracts.SandboxDesiredStateStopped,
		},
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-batch", ClaimDuration: time.Minute,
		PollInterval: time.Second, BatchSize: 2,
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	found, err := reconciler.RunBatch(
		t.Context(), func() time.Time { return now },
		ports.LifecycleWakeTriggerNotify,
	)
	if err != nil || !found {
		t.Fatalf("batch reconciliation found=%t error=%v", found, err)
	}
	if store.batchSize != 2 || len(store.appliedSandboxIDs) != 2 ||
		store.appliedSandboxIDs[0] != "sbx-batch-1" ||
		store.appliedSandboxIDs[1] != "sbx-batch-2" {
		t.Fatalf(
			"batch size=%d applied=%v",
			store.batchSize, store.appliedSandboxIDs,
		)
	}
}

type fakeReconcileStore struct {
	claim             ports.LifecycleReconcileClaim
	batchClaims       []ports.LifecycleReconcileClaim
	batchSize         int
	action            string
	appliedSandboxIDs []string
	nextReconcileAt   time.Time
	wakeTrigger       ports.LifecycleWakeTrigger
}

type fakeEffectExecutor struct {
	action Action
	err    error
}

func (executor *fakeEffectExecutor) ExecuteLifecycleEffect(
	_ context.Context,
	_ ports.LifecycleReconcileClaim,
	decision Decision,
	_ time.Time,
	_ time.Time,
) error {
	executor.action = decision.Action
	return executor.err
}

func (store *fakeReconcileStore) ClaimLifecycle(
	_ context.Context,
	_ string,
	_ time.Time,
	_ time.Duration,
	wakeTrigger ports.LifecycleWakeTrigger,
) (ports.LifecycleReconcileClaim, bool, error) {
	store.wakeTrigger = wakeTrigger
	return store.claim, true, nil
}

func (store *fakeReconcileStore) ClaimLifecycleBatch(
	_ context.Context,
	_ string,
	_ time.Time,
	_ time.Duration,
	batchSize int,
	wakeTrigger ports.LifecycleWakeTrigger,
) ([]ports.LifecycleReconcileClaim, error) {
	store.batchSize = batchSize
	store.wakeTrigger = wakeTrigger
	return append([]ports.LifecycleReconcileClaim(nil), store.batchClaims...), nil
}

func (store *fakeReconcileStore) ApplyLifecycleAction(
	_ context.Context,
	claim ports.LifecycleReconcileClaim,
	action string,
	_ string,
	_ time.Time,
	nextReconcileAt time.Time,
) error {
	store.action = action
	store.appliedSandboxIDs = append(store.appliedSandboxIDs, claim.SandboxID)
	store.nextReconcileAt = nextReconcileAt
	return nil
}

// The wake-up a restart schedules must agree with the decision the next
// reconciliation makes, so it is measured from the same readiness rather than
// from the stale activity the previous generation left behind.
func TestReconcilerSchedulesRestartedSandboxFromItsOwnReadiness(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	readyAt := now
	lastActivityAt := now.Add(-72 * time.Minute)
	store := &fakeReconcileStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-1", WorkerID: "worker-1", Revision: 3,
		ObservedState:          contracts.SandboxStateReady,
		DesiredState:           contracts.SandboxDesiredStateRunning,
		GuestLiveness:          contracts.GuestLivenessReady,
		ReadyAt:                &readyAt,
		LastActivityAt:         &lastActivityAt,
		IdleSeconds:            60,
		MaximumDurationSeconds: 3600,
	}}
	reconciler := Reconciler{
		Store: store, WorkerID: "worker-1", ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now, ports.LifecycleWakeTriggerNotify)
	if err != nil || !found || decision.Action != ActionWait {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	want := readyAt.Add(time.Minute)
	if !store.nextReconcileAt.Equal(want) {
		t.Fatalf("next reconciliation = %s, want idle deadline %s", store.nextReconcileAt, want)
	}
}
