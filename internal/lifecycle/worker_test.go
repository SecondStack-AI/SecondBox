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
	decision, found, err := reconciler.RunOnce(t.Context(), now)
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
	decision, found, err := reconciler.RunOnce(t.Context(), now)
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
	decision, found, err := reconciler.RunOnce(t.Context(), now)
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
	decision, found, err := reconciler.RunOnce(t.Context(), now)
	if err != nil || !found || decision.Action != ActionWait {
		t.Fatalf("reconciliation = %#v, %t, %v", decision, found, err)
	}
	want := now.Add(time.Second)
	if !store.nextReconcileAt.Equal(want) {
		t.Fatalf("next reconciliation = %s, want poll deadline %s", store.nextReconcileAt, want)
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
	found, err := reconciler.RunBatch(t.Context(), func() time.Time { return now })
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
	context.Context,
	string,
	time.Time,
	time.Duration,
) (ports.LifecycleReconcileClaim, bool, error) {
	return store.claim, true, nil
}

func (store *fakeReconcileStore) ClaimLifecycleBatch(
	_ context.Context,
	_ string,
	_ time.Time,
	_ time.Duration,
	batchSize int,
) ([]ports.LifecycleReconcileClaim, error) {
	store.batchSize = batchSize
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
