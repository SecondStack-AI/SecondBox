// Package lifecycle computes durable desired-state reconciliation actions.
package lifecycle

import (
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type Action string

const (
	ActionWait          Action = "wait"
	ActionStartInstance Action = "start_instance"
	ActionMarkReady     Action = "mark_ready"
	ActionDrain         Action = "drain"
	ActionStopInstance  Action = "stop_instance"
	ActionFinishStop    Action = "finish_stop"
	ActionDelete        Action = "delete"
	ActionFail          Action = "fail"
)

// View contains only durable inputs needed for one idempotent decision.
type View struct {
	Observed                  string
	Desired                   string
	StopEffectState           string
	GuestLiveness             string
	InstanceTerminationReason string
	IntentTerminationReason   string
	HasInstance               bool
	ActiveSessions            int64
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
			return Decision{Action: ActionDelete}
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
			return Decision{Action: ActionStopInstance}
		case contracts.SandboxStateStopping:
			return decideStopping(view)
		}
	}
	if view.Desired == contracts.SandboxDesiredStateStopped {
		switch view.Observed {
		case contracts.SandboxStateStopped, contracts.SandboxStateFailed:
			return Decision{Action: ActionWait}
		case contracts.SandboxStateCreating:
			return Decision{Action: ActionWait}
		case contracts.SandboxStateReady, contracts.SandboxStateStarting:
			return Decision{Action: ActionDrain, TerminationReason: requestedTerminationReason(view)}
		case contracts.SandboxStateDraining:
			if drainBarrierActive(view, now) {
				return Decision{Action: ActionWait}
			}
			return Decision{Action: ActionStopInstance}
		case contracts.SandboxStateStopping:
			return decideStopping(view)
		}
	}
	if view.Desired == contracts.SandboxDesiredStateRunning {
		switch view.Observed {
		case contracts.SandboxStateCreating:
			return Decision{Action: ActionWait}
		case contracts.SandboxStateStopped:
			return Decision{Action: ActionStartInstance}
		case contracts.SandboxStateStarting:
			if view.GuestLiveness == contracts.GuestLivenessReady {
				return Decision{Action: ActionMarkReady}
			}
			if view.HasInstance {
				return Decision{Action: ActionWait}
			}
			return Decision{Action: ActionStartInstance}
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
			return Decision{Action: ActionStopInstance}
		case contracts.SandboxStateStopping:
			return decideStopping(view)
		case contracts.SandboxStateFailed:
			return Decision{Action: ActionStartInstance}
		}
	}
	return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
}

func decideStopping(view View) Decision {
	if view.StopEffectState == "runner_failed" {
		return Decision{Action: ActionFail, TerminationReason: contracts.TerminationReasonInternalFailure}
	}
	computeIsTerminal := !view.HasInstance ||
		view.GuestLiveness == contracts.GuestLivenessStopped ||
		view.GuestLiveness == contracts.GuestLivenessLost
	if !computeIsTerminal {
		return Decision{Action: ActionStopInstance}
	}
	if view.StopEffectState == "runner_succeeded" {
		return Decision{Action: ActionFinishStop}
	}
	return Decision{Action: ActionWait}
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
