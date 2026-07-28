package lifecycle

import (
	"context"
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
	if err != nil || !found || decision.Action != ActionMaterialize ||
		effects.action != ActionMaterialize || store.action != "" {
		t.Fatalf(
			"reconciliation = %#v, %t, %v, effect %q, database-only commit %q",
			decision, found, err, effects.action, store.action,
		)
	}
}

func TestReconcilerCommitsDatabaseOnlyTransitionWithoutEffectBroker(t *testing.T) {
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
	if err != nil || !found || decision.Action != ActionFinishCreateStopped ||
		store.action != string(ActionFinishCreateStopped) || effects.action != "" {
		t.Fatalf(
			"reconciliation = %#v, %t, %v, effect %q, database commit %q",
			decision, found, err, effects.action, store.action,
		)
	}
}

type fakeReconcileStore struct {
	claim  ports.LifecycleReconcileClaim
	action string
}

type fakeEffectExecutor struct {
	action Action
}

func (executor *fakeEffectExecutor) ExecuteLifecycleEffect(
	_ context.Context,
	_ ports.LifecycleReconcileClaim,
	decision Decision,
	_ time.Time,
	_ time.Time,
) error {
	executor.action = decision.Action
	return nil
}

func (store *fakeReconcileStore) ClaimLifecycle(
	context.Context,
	string,
	time.Time,
	time.Duration,
) (ports.LifecycleReconcileClaim, bool, error) {
	return store.claim, true, nil
}

func (store *fakeReconcileStore) ApplyLifecycleAction(
	_ context.Context,
	_ ports.LifecycleReconcileClaim,
	action string,
	_ string,
	_ time.Time,
	_ time.Time,
) error {
	store.action = action
	return nil
}
