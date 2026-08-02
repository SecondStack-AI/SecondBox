package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

// ReconcileStore owns durable claims and compare-and-swap transition commits.
type ReconcileStore interface {
	ClaimLifecycle(ctx context.Context, workerID string, now time.Time, claimDuration time.Duration) (ports.LifecycleReconcileClaim, bool, error)
	ApplyLifecycleAction(ctx context.Context, claim ports.LifecycleReconcileClaim, action, terminationReason string, now, nextReconcileAt time.Time) error
}

// EffectExecutor performs one durable runner or object-store effect.
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
}

// RunOnce claims and commits at most one due Sandbox transition.
func (reconciler Reconciler) RunOnce(ctx context.Context, now time.Time) (Decision, bool, error) {
	if reconciler.Store == nil || reconciler.WorkerID == "" ||
		reconciler.ClaimDuration <= 0 || reconciler.PollInterval <= 0 {
		return Decision{}, false, errors.New("SecondBox lifecycle reconciler dependencies and bounds are required")
	}
	claim, found, err := reconciler.Store.ClaimLifecycle(
		ctx, reconciler.WorkerID, now.UTC(), reconciler.ClaimDuration,
	)
	if err != nil || !found {
		return Decision{}, found, err
	}
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
	decision := Decide(view, now.UTC())
	if actionRequiresEffect(decision.Action) {
		if reconciler.Effects == nil {
			return Decision{}, true, errors.New("SecondBox lifecycle runner effect executor is required")
		}
		if err := reconciler.Effects.ExecuteLifecycleEffect(
			ctx, claim, decision, now.UTC(), now.UTC().Add(reconciler.PollInterval),
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
					now.UTC(),
					now.UTC().Add(reconciler.PollInterval),
				); waitErr != nil {
					return Decision{}, true, fmt.Errorf(
						"SecondBox lifecycle effect contention deferral failed: %w",
						waitErr,
					)
				}
				return decision, true, nil
			}
			return Decision{}, true, fmt.Errorf("SecondBox lifecycle effect failed: %w", err)
		}
		return decision, true, nil
	}
	if err := reconciler.Store.ApplyLifecycleAction(
		ctx, claim, string(decision.Action), decision.TerminationReason,
		now.UTC(), now.UTC().Add(reconciler.PollInterval),
	); err != nil {
		return Decision{}, true, fmt.Errorf("SecondBox lifecycle decision commit failed: %w", err)
	}
	return decision, true, nil
}

func actionRequiresEffect(action Action) bool {
	switch action {
	case ActionStartInstance, ActionStopInstance, ActionDelete:
		return true
	default:
		return false
	}
}
