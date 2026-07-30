package main

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestLifecycleReconcilerRunsImmediatelyAndStopsWithContext(t *testing.T) {
	store := &processLifecycleStore{claim: ports.LifecycleReconcileClaim{
		SandboxID: "sbx-process", WorkerID: "worker-process", Revision: 3,
		ObservedState: contracts.SandboxStateCreating,
		DesiredState:  contracts.SandboxDesiredStateStopped,
	}, applied: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	completed := make(chan error, 1)
	go func() {
		completed <- runLifecycleReconciler(ctx, lifecycle.Reconciler{
			Store: store, WorkerID: "worker-process",
			ClaimDuration: time.Minute, PollInterval: time.Hour,
		})
	}()
	select {
	case <-store.applied:
	case <-time.After(time.Second):
		t.Fatal("lifecycle reconciler did not perform its immediate pass")
	}
	cancel()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("lifecycle reconciler shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle reconciler did not stop with process context")
	}
}

func TestLifecycleReconcilerRetriesRevisionContention(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store := &contentionLifecycleStore{cancel: cancel, applyErr: ports.ErrRevisionConflict}
	err := runLifecycleReconciler(ctx, lifecycle.Reconciler{
		Store: store, WorkerID: "worker-contention",
		ClaimDuration: time.Minute, PollInterval: time.Hour,
	})
	if err != nil || store.applyCalls != 1 || store.claimCalls != 2 {
		t.Fatalf(
			"lifecycle contention result = error %v, claims %d, applies %d",
			err, store.claimCalls, store.applyCalls,
		)
	}
}

func TestAssignmentReconcilerRetriesOnlyLostClaims(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store := &contentionAssignmentStore{cancel: cancel, applyErr: reconcile.ErrClaimLost}
	err := runAssignmentReconciler(ctx, reconcile.AssignmentWorker{
		Store: store, WorkerID: "assignment-contention",
		ClaimDuration: time.Minute, PollInterval: time.Hour,
		CommandDeadline: time.Minute, HeartbeatTimeout: time.Minute,
		NewCommandID: func(string) string {
			return "unused-command"
		},
	})
	if err != nil || store.applyCalls != 1 || store.claimCalls != 2 {
		t.Fatalf(
			"Assignment contention result = error %v, claims %d, applies %d",
			err, store.claimCalls, store.applyCalls,
		)
	}
}

func TestReconcileWorkersSurfaceUnexpectedErrors(t *testing.T) {
	unexpected := errors.New("unexpected dependency failure")
	lifecycleStore := &contentionLifecycleStore{
		cancel: func() {}, applyErr: unexpected,
	}
	lifecycleErr := runLifecycleReconciler(t.Context(), lifecycle.Reconciler{
		Store: lifecycleStore, WorkerID: "worker-unexpected",
		ClaimDuration: time.Minute, PollInterval: time.Hour,
	})
	if !errors.Is(lifecycleErr, unexpected) {
		t.Fatalf("unexpected lifecycle error = %v", lifecycleErr)
	}
	assignmentStore := &contentionAssignmentStore{
		cancel: func() {}, applyErr: unexpected,
	}
	assignmentErr := runAssignmentReconciler(t.Context(), reconcile.AssignmentWorker{
		Store: assignmentStore, WorkerID: "assignment-unexpected",
		ClaimDuration: time.Minute, PollInterval: time.Hour,
		CommandDeadline: time.Minute, HeartbeatTimeout: time.Minute,
		NewCommandID: func(string) string {
			return "unused-command"
		},
	})
	if !errors.Is(assignmentErr, unexpected) {
		t.Fatalf("unexpected Assignment error = %v", assignmentErr)
	}
}

type processLifecycleStore struct {
	claim   ports.LifecycleReconcileClaim
	applied chan struct{}
	claimed bool
}

type contentionLifecycleStore struct {
	cancel     context.CancelFunc
	applyErr   error
	claimCalls int
	applyCalls int
}

func (store *contentionLifecycleStore) ClaimLifecycle(
	_ context.Context,
	workerID string,
	_ time.Time,
	_ time.Duration,
) (ports.LifecycleReconcileClaim, bool, error) {
	store.claimCalls++
	if store.claimCalls == 1 {
		return ports.LifecycleReconcileClaim{
			SandboxID: "sbx-contention", WorkerID: workerID, Revision: 2,
			ObservedState: contracts.SandboxStateCreating,
			DesiredState:  contracts.SandboxDesiredStateStopped,
		}, true, nil
	}
	store.cancel()
	return ports.LifecycleReconcileClaim{}, false, context.Canceled
}

func (store *contentionLifecycleStore) ApplyLifecycleAction(
	context.Context,
	ports.LifecycleReconcileClaim,
	string,
	string,
	time.Time,
	time.Time,
) error {
	store.applyCalls++
	return store.applyErr
}

type contentionAssignmentStore struct {
	cancel     context.CancelFunc
	applyErr   error
	claimCalls int
	applyCalls int
}

func (store *contentionAssignmentStore) MarkExpiredRunners(
	context.Context,
	time.Time,
	time.Time,
) (int64, error) {
	return 0, nil
}

func (store *contentionAssignmentStore) ClaimNext(
	_ context.Context,
	workerID string,
	_ time.Time,
	now time.Time,
) (reconcile.Claim, bool, error) {
	store.claimCalls++
	if store.claimCalls == 1 {
		return reconcile.Claim{
			AssignmentID: "asn-contention", SandboxID: "sbx-contention",
			InstanceID: "ins-contention", RunnerID: "run-contention",
			WorkerID: workerID, Revision: 2,
			State: reconcile.AssignmentState{
				State: "assigned", Generation: 1, Deadline: now.Add(time.Minute),
			},
		}, true, nil
	}
	store.cancel()
	return reconcile.Claim{}, false, context.Canceled
}

func (store *contentionAssignmentStore) ApplyDecision(
	context.Context,
	reconcile.Claim,
	reconcile.Decision,
	*runnerv1.FenceCommand,
	time.Time,
	time.Time,
) error {
	store.applyCalls++
	return store.applyErr
}

func (store *contentionAssignmentStore) AdvanceFencedGeneration(
	context.Context,
	string,
	int64,
	time.Time,
) (int64, error) {
	return 0, errors.New("unexpected generation advancement")
}

func (store *processLifecycleStore) ClaimLifecycle(
	context.Context,
	string,
	time.Time,
	time.Duration,
) (ports.LifecycleReconcileClaim, bool, error) {
	if !store.claimed {
		store.claimed = true
		return store.claim, true, nil
	}
	return ports.LifecycleReconcileClaim{}, false, nil
}

func (store *processLifecycleStore) ApplyLifecycleAction(
	_ context.Context,
	_ ports.LifecycleReconcileClaim,
	_ string,
	_ string,
	_ time.Time,
	_ time.Time,
) error {
	close(store.applied)
	return nil
}
