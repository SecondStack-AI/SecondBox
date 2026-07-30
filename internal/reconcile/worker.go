package reconcile

import (
	"context"
	"errors"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"google.golang.org/protobuf/proto"
)

// WorkerStore owns durable Assignment claims, commands, and generation advancement.
type WorkerStore interface {
	MarkExpiredRunners(context.Context, time.Time, time.Time) (int64, error)
	ClaimNext(context.Context, string, time.Time, time.Time) (Claim, bool, error)
	ApplyDecision(context.Context, Claim, Decision, *runnerv1.FenceCommand, time.Time, time.Time) error
	AdvanceFencedGeneration(context.Context, string, int64, time.Time) (int64, error)
}

// AssignmentWorker reconciles deadline and Runner-loss evidence without process-local ownership.
type AssignmentWorker struct {
	Store            WorkerStore
	WorkerID         string
	ClaimDuration    time.Duration
	PollInterval     time.Duration
	CommandDeadline  time.Duration
	HeartbeatTimeout time.Duration
	NewCommandID     func(string) string
}

// RunOnce performs at most one revision-fenced Assignment transition.
func (worker AssignmentWorker) RunOnce(
	ctx context.Context,
	now time.Time,
) (Decision, bool, error) {
	if worker.Store == nil || worker.WorkerID == "" ||
		worker.ClaimDuration <= 0 || worker.PollInterval <= 0 ||
		worker.CommandDeadline <= 0 || worker.HeartbeatTimeout <= 0 ||
		worker.NewCommandID == nil {
		return Decision{}, false, errors.New("SecondBox Assignment worker dependencies and bounds are required")
	}
	now = now.UTC()
	if _, err := worker.Store.MarkExpiredRunners(
		ctx, now.Add(-worker.HeartbeatTimeout), now,
	); err != nil {
		return Decision{}, false, err
	}
	claim, found, err := worker.Store.ClaimNext(
		ctx, worker.WorkerID, now.Add(worker.ClaimDuration), now,
	)
	if err != nil || !found {
		return Decision{}, found, err
	}
	decision := assignmentWorkerDecision(claim.State, now)
	if decision.Action == ActionAdvanceGeneration {
		_, err := worker.Store.AdvanceFencedGeneration(
			ctx, claim.AssignmentID, claim.Revision, now,
		)
		if errors.Is(err, ports.ErrWorkspaceMutation) {
			waitErr := worker.Store.ApplyDecision(
				ctx,
				claim,
				Decision{Action: ActionWait},
				nil,
				now.Add(worker.PollInterval),
				now,
			)
			return decision, true, waitErr
		}
		return decision, true, err
	}
	var fenceCommand *runnerv1.FenceCommand
	if decision.Action == ActionFence {
		if claim.Correlation == nil {
			return Decision{}, true, errors.New("SecondBox Assignment fence correlation is required")
		}
		reason := runnerv1.FenceReason_FENCE_REASON_ASSIGNMENT_REPLACED
		if claim.State.State == "uncertain" || claim.State.FailureClass == FailureFencing {
			reason = runnerv1.FenceReason_FENCE_REASON_GENERATION_ADVANCED
		}
		fenceCommand = &runnerv1.FenceCommand{
			MessageId: worker.NewCommandID("assignment-fence"),
			Fence: &runnerv1.AssignmentFence{
				AssignmentId: claim.AssignmentID,
				SandboxId:    claim.SandboxID, InstanceId: claim.InstanceID,
				SandboxGeneration: uint64(claim.State.Generation),
				FencingToken:      append([]byte(nil), claim.FencingToken...),
			},
			Reason:         reason,
			DeadlineUnixMs: uint64(now.Add(worker.CommandDeadline).UnixMilli()),
			Correlation:    proto.Clone(claim.Correlation).(*runnerv1.Correlation),
		}
	}
	err = worker.Store.ApplyDecision(
		ctx, claim, decision, fenceCommand,
		now.Add(worker.PollInterval), now,
	)
	return decision, true, err
}

func assignmentWorkerDecision(state AssignmentState, now time.Time) Decision {
	if state.State == "uncertain" {
		return DecideRunnerLoss(state, now)
	}
	if state.State == "fenced" && state.FenceProofDigest != "" {
		if state.FailureClass == FailureFencing {
			return DecideRunnerLoss(state, now)
		}
		if state.FailureClass == FailureStartupTimeout {
			return Decision{Action: ActionFailTerminal}
		}
		return Decision{Action: ActionWait}
	}
	return DecideAssignment(state, now)
}
