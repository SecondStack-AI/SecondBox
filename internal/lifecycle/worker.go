package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// ReconcileStore owns durable claims and compare-and-swap transition commits.
// The wake trigger is attribution evidence carried into the claim transaction;
// it never changes which work the claim query finds.
//
// A zero nextReconcileAt is the parked schedule: the commit carries no durable
// deadline at all, so the Sandbox leaves the reconciliation work set until an
// external event schedules it again.
type ReconcileStore interface {
	ClaimLifecycle(ctx context.Context, workerID string, now time.Time, claimDuration time.Duration, wakeTrigger ports.LifecycleWakeTrigger) (ports.LifecycleReconcileClaim, bool, error)
	ApplyLifecycleAction(ctx context.Context, claim ports.LifecycleReconcileClaim, action, terminationReason string, now, nextReconcileAt time.Time) error
}

// BatchReconcileStore claims an ordered cohort under the same worker fence.
type BatchReconcileStore interface {
	ClaimLifecycleBatch(ctx context.Context, workerID string, now time.Time, claimDuration time.Duration, batchSize int, wakeTrigger ports.LifecycleWakeTrigger) ([]ports.LifecycleReconcileClaim, error)
}

// EffectExecutor performs one durable Runner effect.
type EffectExecutor interface {
	ExecuteLifecycleEffect(
		ctx context.Context,
		claim ports.LifecycleReconcileClaim,
		decision Decision,
		now time.Time,
		nextReconcileAt time.Time,
	) error
}

// Reconciler consumes durable desired state one idempotent transition at a time.
type Reconciler struct {
	Store         ReconcileStore
	Effects       EffectExecutor
	WorkerID      string
	ClaimDuration time.Duration
	PollInterval  time.Duration
	BatchSize     int
}

// RunOnce claims and commits at most one due Sandbox transition.
func (reconciler Reconciler) RunOnce(
	ctx context.Context,
	now time.Time,
	wakeTrigger ports.LifecycleWakeTrigger,
) (Decision, bool, error) {
	if reconciler.Store == nil || reconciler.WorkerID == "" ||
		reconciler.ClaimDuration <= 0 || reconciler.PollInterval <= 0 {
		return Decision{}, false, errors.New("SecondBox lifecycle reconciler dependencies and bounds are required")
	}
	claim, found, err := reconciler.Store.ClaimLifecycle(
		ctx, reconciler.WorkerID, now.UTC(), reconciler.ClaimDuration, wakeTrigger,
	)
	if err != nil || !found {
		return Decision{}, found, err
	}
	decision, err := reconciler.reconcileClaim(ctx, claim, now.UTC())
	return decision, true, err
}

// RunBatch claims a bounded cohort and executes its effects sequentially.
func (reconciler Reconciler) RunBatch(
	ctx context.Context,
	clock func() time.Time,
	wakeTrigger ports.LifecycleWakeTrigger,
) (bool, error) {
	if reconciler.Store == nil || reconciler.WorkerID == "" ||
		reconciler.ClaimDuration <= 0 || reconciler.PollInterval <= 0 ||
		reconciler.BatchSize <= 0 || clock == nil {
		return false, errors.New("SecondBox lifecycle batch reconciler dependencies and bounds are required")
	}
	if reconciler.BatchSize == 1 {
		_, found, err := reconciler.RunOnce(ctx, clock(), wakeTrigger)
		return found, err
	}
	store, ok := reconciler.Store.(BatchReconcileStore)
	if !ok {
		return false, errors.New("SecondBox lifecycle batch claim store is required")
	}
	claims, err := store.ClaimLifecycleBatch(
		ctx, reconciler.WorkerID, clock().UTC(), reconciler.ClaimDuration,
		reconciler.BatchSize, wakeTrigger,
	)
	if err != nil || len(claims) == 0 {
		return false, err
	}
	if len(claims) > reconciler.BatchSize {
		return false, errors.New("SecondBox lifecycle batch claim exceeded its bound")
	}
	for _, claim := range claims {
		if _, err := reconciler.reconcileClaim(ctx, claim, clock().UTC()); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (reconciler Reconciler) reconcileClaim(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	now time.Time,
) (Decision, error) {
	view := View{
		Observed: claim.ObservedState, Desired: claim.DesiredState,
		StopEffectState:           claim.StopEffectState,
		GuestLiveness:             claim.GuestLiveness,
		InstanceTerminationReason: claim.InstanceTerminationReason,
		IntentTerminationReason:   claim.IntentTerminationReason,
		HasInstance:               claim.HasInstance,
		ActiveSessions:            claim.ActiveSessions,
		DrainGrace:                time.Duration(claim.DrainGraceSeconds) * time.Second,
		IdleTimeout:               time.Duration(claim.IdleSeconds) * time.Second,
		MaximumDuration:           time.Duration(claim.MaximumDurationSeconds) * time.Second,
	}
	if claim.ReadyAt != nil {
		view.ReadyAt = claim.ReadyAt.UTC()
	}
	if claim.LastActivityAt != nil {
		view.LastUsefulActivityAt = claim.LastActivityAt.UTC()
	}
	if claim.DrainStartedAt != nil {
		view.DrainStartedAt = claim.DrainStartedAt.UTC()
	}
	now = now.UTC()
	decision := Decide(view, now)
	nextReconcileAt := nextLifecycleReconcileAt(view, decision, now, reconciler.PollInterval)
	if actionRequiresEffect(decision.Action) {
		if reconciler.Effects == nil {
			return Decision{}, errors.New("SecondBox lifecycle runner effect executor is required")
		}
		if err := reconciler.Effects.ExecuteLifecycleEffect(
			ctx, claim, decision, now, now.Add(reconciler.PollInterval),
		); err != nil {
			// Losing a serialization race is an ordinary outcome of concurrency,
			// not a fault, so it defers exactly like Workspace contention does.
			// Failing here ends the reconciler, and ending the reconciler stops
			// the server: a burst of concurrent placements would take the whole
			// control plane down.
			if errors.Is(err, ports.ErrWorkspaceMutation) ||
				errors.Is(err, ports.ErrSerializationContention) {
				if waitErr := reconciler.Store.ApplyLifecycleAction(
					ctx,
					claim,
					string(ActionWait),
					"",
					now,
					now.Add(reconciler.PollInterval),
				); waitErr != nil {
					return Decision{}, fmt.Errorf(
						"SecondBox lifecycle effect contention deferral failed: %w",
						waitErr,
					)
				}
				return decision, nil
			}
			return Decision{}, fmt.Errorf("SecondBox lifecycle effect failed: %w", err)
		}
		return decision, nil
	}
	if err := reconciler.Store.ApplyLifecycleAction(
		ctx, claim, string(decision.Action), decision.TerminationReason,
		now, nextReconcileAt,
	); err != nil {
		return Decision{}, fmt.Errorf("SecondBox lifecycle decision commit failed: %w", err)
	}
	return decision, nil
}

// nextLifecycleReconcileAt keeps transitional Sandboxes on the bounded poll
// interval, but lets a healthy ready Sandbox sleep until durable policy can
// actually change its decision. Runner terminal evidence and desired-state
// writes explicitly wake the Sandbox by moving next_reconcile_at to now.
//
// A Sandbox the commit leaves at rest carries no deadline at all and returns
// the zero time, which the store commits as a parked schedule.
func nextLifecycleReconcileAt(
	view View,
	decision Decision,
	now time.Time,
	pollInterval time.Duration,
) time.Time {
	fallback := now.Add(pollInterval)
	if successorIsImmediatelyAvailable(view, decision) {
		return now
	}
	if committedStateIsAtRest(view, decision) {
		return time.Time{}
	}
	if decision.Action != ActionWait ||
		view.Observed != contracts.SandboxStateReady ||
		view.Desired != contracts.SandboxDesiredStateRunning ||
		view.GuestLiveness != contracts.GuestLivenessReady {
		return fallback
	}

	var maximumDeadline time.Time
	if view.MaximumDuration > 0 && !view.ReadyAt.IsZero() {
		maximumDeadline = view.ReadyAt.Add(view.MaximumDuration)
	}
	if view.ActiveSessions > 0 {
		return earlierFutureDeadline(fallback, maximumDeadline, now)
	}

	var idleDeadline time.Time
	if idleSince := IdleSince(view); view.IdleTimeout > 0 && !idleSince.IsZero() {
		idleDeadline = idleSince.Add(view.IdleTimeout)
	}
	deadline := earlierFutureDeadline(idleDeadline, maximumDeadline, now)
	if deadline.IsZero() {
		return fallback
	}
	return deadline
}

// successorIsImmediatelyAvailable reports whether the state a decision commits
// determines its own successor with no further evidence. Such a transition is
// scheduled at `now`, which both leaves the Sandbox due for the worker's next
// claim and satisfies the sandboxes_notify_due predicate, so the successor runs
// without waiting out a recovery poll interval that occupies no work.
//
// Only these two transitions qualify, and the restriction is what keeps the
// worker from spinning:
//
//   - Every action requiring a Runner effect must wait for the
//     Runner's acknowledgement, so it keeps the bounded poll.
//   - ActionWait is the steady state. Waking a waiting Sandbox immediately
//     would make an idle deployment reconcile in a tight loop, so a wait is
//     never immediate and an idle Sandbox still sleeps to its own deadline.
//   - Both successors named here are effect actions or a wait, never another
//     immediate transition, so one state change can advance at most one extra
//     hop before the schedule is bounded again.
func successorIsImmediatelyAvailable(view View, decision Decision) bool {
	switch decision.Action {
	case ActionDrain:
		// Drain commits `draining`, whose successor is stop_instance for every
		// desired state — unless an active session holds the drain barrier
		// open, and then the barrier's own grace period is what must elapse.
		return view.ActiveSessions == 0
	case ActionFinishStop:
		// Finish-stop commits `stopped`. A Sandbox still wanted deleted deletes
		// and one wanted running starts again; only a Sandbox that is already
		// in its desired state has nothing waiting on this commit, and that one
		// parks instead of being scheduled at all.
		return view.Desired != contracts.SandboxDesiredStateStopped
	default:
		return false
	}
}

// committedStateIsAtRest reports whether the state a decision commits is a
// durable rest state: the Sandbox already holds its desired state and carries
// no deadline whose expiry could produce a different decision. No amount of
// elapsed time changes what the reconciler would decide, so scheduling another
// pass buys nothing and costs a claim transaction and a commit per interval,
// forever, for every Sandbox in the population.
//
// Such a commit parks the Sandbox instead. Only an external event schedules it
// again, and every event that can change its decision writes the schedule in
// the same transaction that changes the durable state:
//
//   - A lifecycle intent (start, stop, drain, delete) commits desired_state and
//     next_reconcile_at together in SetSandboxDesiredState, which also satisfies
//     the sandboxes_notify_due predicate and wakes the worker at once.
//   - Runner evidence is fenced to the Sandbox's current Instance and
//     generation, and a Sandbox at rest holds neither, so no Runner event
//     applies to one.
//   - Snapshot, relocation, metadata, activity, and Lease writes change no input
//     to Decide for a Sandbox at rest, so none of them needs a reconciliation
//     pass and none schedules one.
//
// The two idle policy deadlines, the idle timeout and the maximum duration, are
// running concerns measured from readiness. Neither applies to a Sandbox that
// holds no Instance, which is why a rest state carries no deadline.
func committedStateIsAtRest(view View, decision Decision) bool {
	switch decision.Action {
	case ActionWait:
		return stateIsAtRest(view.Desired, view.Observed)
	case ActionFinishStop:
		// Finish-stop commits `stopped`; wanting anything else has a successor.
		return stateIsAtRest(view.Desired, contracts.SandboxStateStopped)
	default:
		return false
	}
}

// stateIsAtRest names the durable pairs in Decide's matrix that reconcile to
// ActionWait for every clock. Adding a pair here must be matched by a wake path
// that covers every event able to change that pair's decision.
func stateIsAtRest(desired, observed string) bool {
	switch desired {
	case contracts.SandboxDesiredStateStopped:
		return observed == contracts.SandboxStateStopped ||
			observed == contracts.SandboxStateFailed
	case contracts.SandboxDesiredStateDeleted:
		return observed == contracts.SandboxStateDeleted
	default:
		return false
	}
}

func earlierFutureDeadline(left, right, now time.Time) time.Time {
	if !left.After(now) {
		left = time.Time{}
	}
	if !right.After(now) {
		right = time.Time{}
	}
	if left.IsZero() || (!right.IsZero() && right.Before(left)) {
		return right
	}
	return left
}

func actionRequiresEffect(action Action) bool {
	switch action {
	case ActionStartInstance, ActionStopInstance, ActionDelete:
		return true
	default:
		return false
	}
}
