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

type fakeReconcileStore struct {
	claim  ports.LifecycleReconcileClaim
	action string
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
