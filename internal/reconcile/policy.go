// Package reconcile defines restart-safe assignment recovery decisions.
package reconcile

import "time"

type Action string

const (
	ActionWait              Action = "wait"
	ActionRetry             Action = "retry"
	ActionFence             Action = "fence"
	ActionAdvanceGeneration Action = "advance_generation"
	ActionFailTerminal      Action = "fail_terminal"
)

type FailureClass string

const (
	FailureNone           FailureClass = ""
	FailureTransient      FailureClass = "transient"
	FailureCompatibility  FailureClass = "compatibility"
	FailureAdmission      FailureClass = "admission"
	FailureFencing        FailureClass = "fencing"
	FailureIntegrity      FailureClass = "integrity"
	FailureAuthorization  FailureClass = "authorization"
	FailureStartupTimeout FailureClass = "startup_timeout"
)

// AssignmentState is durable evidence used to compute one reconciliation action.
type AssignmentState struct {
	State            string
	Generation       int64
	FenceProofDigest string
	FailureClass     FailureClass
	RetryCount       int
	RetryLimit       int
	Deadline         time.Time
}

// Decision is one idempotent action. Runner loss can advance authority only
// through the pinned home runner's durable local Workspace receipt.
type Decision struct {
	Action         Action
	NextGeneration int64
}

// DecideRunnerLoss fences uncertain compute before queuing a local generation advance.
func DecideRunnerLoss(state AssignmentState, now time.Time) Decision {
	if state.State == "fenced" && state.FenceProofDigest != "" {
		return Decision{
			Action:         ActionAdvanceGeneration,
			NextGeneration: state.Generation + 1,
		}
	}
	return Decision{Action: ActionFence}
}

// DecideAssignment classifies bounded retry, deadlines, and terminal failures.
func DecideAssignment(state AssignmentState, now time.Time) Decision {
	if state.State == "ready" {
		return Decision{Action: ActionWait}
	}
	if state.State == "fencing" {
		if state.Deadline.IsZero() || now.Before(state.Deadline) {
			return Decision{Action: ActionWait}
		}
		if state.RetryCount < state.RetryLimit {
			return Decision{Action: ActionFence}
		}
		return Decision{Action: ActionFailTerminal}
	}
	if !state.Deadline.IsZero() && !now.Before(state.Deadline) {
		return Decision{Action: ActionFence}
	}
	if state.State != "failed" {
		return Decision{Action: ActionWait}
	}
	if state.FailureClass == FailureTransient && state.RetryCount < state.RetryLimit {
		return Decision{Action: ActionRetry}
	}
	return Decision{Action: ActionFailTerminal}
}
