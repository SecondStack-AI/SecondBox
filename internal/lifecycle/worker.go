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
	ClaimLifecycle(context.Context, string, time.Time, time.Duration) (ports.LifecycleReconcileClaim, bool, error)
	ApplyLifecycleAction(context.Context, ports.LifecycleReconcileClaim, string, string, time.Time, time.Time) error
}

// EffectExecutor performs one durable runner or object-store effect.
type EffectExecutor interface {
	ExecuteLifecycleEffect(
		context.Context,
		ports.LifecycleReconcileClaim,
		Decision,
		time.Time,
		time.Time,
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
		MaterializationState: claim.MaterializationState,
		CheckpointState:      claim.CheckpointState, StopEffectState: claim.StopEffectState,
		GuestLiveness:             claim.GuestLiveness,
		InstanceTerminationReason: claim.InstanceTerminationReason,
		IntentTerminationReason:   claim.IntentTerminationReason,
		HasInstance:               claim.HasInstance,
		ActiveSessions:            claim.ActiveSessions, CheckpointOnStop: claim.CheckpointOnStop,
		ForceCheckpoint: claim.ForceCheckpoint,
		DrainGrace:      time.Duration(claim.DrainGraceSeconds) * time.Second,
		IdleTimeout:     time.Duration(claim.IdleSeconds) * time.Second,
		MaximumDuration: time.Duration(claim.MaximumDurationSeconds) * time.Second,
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
	case ActionMaterialize, ActionStartInstance, ActionCheckpoint, ActionStopInstance:
		return true
	default:
		return false
	}
}
