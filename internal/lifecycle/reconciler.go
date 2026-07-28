// Package lifecycle computes durable desired-state reconciliation actions.
package lifecycle

import (
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type Action string

const (
	ActionWait                Action = "wait"
	ActionFinishCreateStopped Action = "finish_create_stopped"
	ActionMaterialize         Action = "materialize"
	ActionStartInstance       Action = "start_instance"
	ActionMarkReady           Action = "mark_ready"
	ActionDrain               Action = "drain"
	ActionCheckpoint          Action = "checkpoint"
	ActionStopInstance        Action = "stop_instance"
	ActionFinishStop          Action = "finish_stop"
	ActionDelete              Action = "delete"
	ActionFinishDelete        Action = "finish_delete"
	ActionFail                Action = "fail"
)

// View contains only durable inputs needed for one idempotent decision.
type View struct {
	Observed                  string
	Desired                   string
	MaterializationState      string
	CheckpointState           string
	StopEffectState           string
	GuestLiveness             string
	InstanceTerminationReason string
	IntentTerminationReason   string
	HasInstance               bool
	ActiveSessions            int64
	CheckpointOnStop          bool
	ForceCheckpoint           bool
	ReadyAt                   time.Time
	LastUsefulActivityAt      time.Time
	DrainStartedAt            time.Time
	DrainGrace                time.Duration
	IdleTimeout               time.Duration
	MaximumDuration           time.Duration
}

// Decision is the single next action and any stable termination reason it establishes.
type Decision struct {
	Action            Action
	TerminationReason string
}

// Decide computes one restart-safe transition without performing side effects.
func Decide(view View, now time.Time) Decision {
	if view.Desired == contracts.SandboxDesiredStateDeleted {
		switch view.Observed {
		case contracts.SandboxStateDeleted:
			return Decision{Action: ActionWait}
		case contracts.SandboxStateStopped, contracts.SandboxStateFailed:
			return Decision{Action: ActionDelete}
		case contracts.SandboxStateDeleting:
			return Decision{Action: ActionFinishDelete}
		case contracts.SandboxStateCreating:
			if !view.HasInstance {
				return Decision{Action: ActionDelete}
			}
			return Decision{Action: ActionDrain, TerminationReason: requestedTerminationReason(view)}
		case contracts.SandboxStateReady, contracts.SandboxStateStarting:
			return Decision{Action: ActionDrain, TerminationReason: requestedTerminationReason(view)}
		case contracts.SandboxStateDraining:
			if drainBarrierActive(view, now) {
				return Decision{Action: ActionWait}
			}
			if checkpointRequestedAndRunnable(view) {
				return Decision{Action: ActionCheckpoint}
			}
			return Decision{Action: ActionStopInstance}
		case contracts.SandboxStateCheckpointing:
			if view.CheckpointState == contracts.ObjectStateIntegrityFailed {
				if view.HasInstance {
					return Decision{Action: ActionStopInstance}
				}
				return Decision{Action: ActionDelete}
			}
			if view.CheckpointState == contracts.ObjectStatePublished {
				if view.HasInstance {
					return Decision{Action: ActionStopInstance}
				}
				return Decision{Action: ActionDelete}
			}
			return Decision{Action: ActionCheckpoint}
		case contracts.SandboxStateStopping:
			if view.StopEffectState == "runner_failed" {
				return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
			}
			if view.GuestLiveness == contracts.GuestLivenessStopped || !view.HasInstance {
				return Decision{Action: ActionDelete}
			}
			return Decision{Action: ActionStopInstance}
		}
	}
	if view.Desired == contracts.SandboxDesiredStateStopped {
		switch view.Observed {
		case contracts.SandboxStateStopped, contracts.SandboxStateFailed:
			return Decision{Action: ActionWait}
		case contracts.SandboxStateCreating:
			return Decision{Action: ActionFinishCreateStopped}
		case contracts.SandboxStateReady, contracts.SandboxStateStarting:
			return Decision{Action: ActionDrain, TerminationReason: requestedTerminationReason(view)}
		case contracts.SandboxStateDraining:
			if drainBarrierActive(view, now) {
				return Decision{Action: ActionWait}
			}
			if checkpointRequestedAndRunnable(view) {
				return Decision{Action: ActionCheckpoint}
			}
			return Decision{Action: ActionStopInstance}
		case contracts.SandboxStateCheckpointing:
			if view.CheckpointState == contracts.ObjectStateIntegrityFailed {
				return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
			}
			if view.CheckpointState == contracts.ObjectStatePublished {
				if view.HasInstance {
					return Decision{Action: ActionStopInstance}
				}
				return Decision{Action: ActionFinishStop}
			}
			return Decision{Action: ActionCheckpoint}
		case contracts.SandboxStateStopping:
			if view.StopEffectState == "runner_failed" {
				return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
			}
			if view.GuestLiveness == contracts.GuestLivenessStopped || !view.HasInstance {
				return Decision{Action: ActionFinishStop}
			}
			return Decision{Action: ActionStopInstance}
		}
	}
	if view.Desired == contracts.SandboxDesiredStateRunning {
		switch view.Observed {
		case contracts.SandboxStateCreating, contracts.SandboxStateStopped:
			return Decision{Action: ActionMaterialize}
		case contracts.SandboxStateStarting:
			if view.MaterializationState == contracts.MaterializationStateReady &&
				view.GuestLiveness == contracts.GuestLivenessReady {
				return Decision{Action: ActionMarkReady}
			}
			if view.MaterializationState == contracts.MaterializationStatePreparing ||
				view.MaterializationState == contracts.MaterializationStateReady {
				return Decision{Action: ActionWait}
			}
			return Decision{Action: ActionMaterialize}
		case contracts.SandboxStateReady:
			if view.GuestLiveness == contracts.GuestLivenessStopped {
				reason := view.InstanceTerminationReason
				if !ValidTerminationReason(reason) {
					reason = contracts.TerminationReasonInternalFailure
				}
				return Decision{Action: ActionDrain, TerminationReason: reason}
			}
			if view.GuestLiveness == contracts.GuestLivenessLost {
				return Decision{Action: ActionDrain, TerminationReason: contracts.TerminationReasonGuestAgentLost}
			}
			if view.MaximumDuration > 0 && !view.ReadyAt.IsZero() && !now.Before(view.ReadyAt.Add(view.MaximumDuration)) {
				return Decision{Action: ActionDrain, TerminationReason: contracts.TerminationReasonMaximumDuration}
			}
			if view.ActiveSessions == 0 && view.IdleTimeout > 0 && !view.LastUsefulActivityAt.IsZero() &&
				!now.Before(view.LastUsefulActivityAt.Add(view.IdleTimeout)) {
				return Decision{Action: ActionDrain, TerminationReason: contracts.TerminationReasonIdleTimeout}
			}
			return Decision{Action: ActionWait}
		case contracts.SandboxStateDraining:
			if drainBarrierActive(view, now) {
				return Decision{Action: ActionWait}
			}
			if checkpointRequestedAndRunnable(view) {
				return Decision{Action: ActionCheckpoint}
			}
			return Decision{Action: ActionStopInstance}
		case contracts.SandboxStateCheckpointing:
			if view.CheckpointState == contracts.ObjectStateIntegrityFailed {
				return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
			}
			if view.CheckpointState == contracts.ObjectStatePublished {
				if view.HasInstance {
					return Decision{Action: ActionStopInstance}
				}
				return Decision{Action: ActionFinishStop}
			}
			return Decision{Action: ActionCheckpoint}
		case contracts.SandboxStateStopping:
			if view.StopEffectState == "runner_failed" {
				return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
			}
			if view.GuestLiveness == contracts.GuestLivenessStopped || !view.HasInstance {
				return Decision{Action: ActionFinishStop}
			}
			return Decision{Action: ActionStopInstance}
		case contracts.SandboxStateFailed:
			return Decision{Action: ActionMaterialize}
		}
	}
	return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
}

func requestedTerminationReason(view View) string {
	if ValidTerminationReason(view.IntentTerminationReason) {
		return view.IntentTerminationReason
	}
	return contracts.TerminationReasonRequestedStop
}

func drainBarrierActive(view View, now time.Time) bool {
	if view.ActiveSessions == 0 {
		return false
	}
	return view.DrainStartedAt.IsZero() || view.DrainGrace <= 0 ||
		now.Before(view.DrainStartedAt.Add(view.DrainGrace))
}

func checkpointRequestedAndRunnable(view View) bool {
	return (view.CheckpointOnStop || view.ForceCheckpoint) &&
		view.HasInstance && view.GuestLiveness == contracts.GuestLivenessReady
}

// ValidTerminationReason recognizes the complete stable v1 reason vocabulary.
func ValidTerminationReason(reason string) bool {
	switch reason {
	case contracts.TerminationReasonRequestedDrain,
		contracts.TerminationReasonRequestedStop,
		contracts.TerminationReasonIdleTimeout,
		contracts.TerminationReasonMaximumDuration,
		contracts.TerminationReasonGuestShutdown,
		contracts.TerminationReasonResourceExhaustion,
		contracts.TerminationReasonGuestAgentLost,
		contracts.TerminationReasonRunnerLost,
		contracts.TerminationReasonStartupFailed,
		contracts.TerminationReasonFenced,
		contracts.TerminationReasonInternalFailure:
		return true
	default:
		return false
	}
}
