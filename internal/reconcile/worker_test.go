package reconcile

import (
	"context"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestAssignmentWorkerFencesExpiredStartupWithExactAuthority(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	store := &assignmentWorkerStore{
		claim: Claim{
			AssignmentID: "asn-worker", SandboxID: "sbx-worker", InstanceID: "ins-worker",
			RunnerID: "run-worker", FencingToken: []byte("worker-fencing-token"),
			Correlation: &runnerv1.Correlation{
				RequestId: "req-worker", OperationId: "op-worker", SandboxId: "sbx-worker",
				InstanceId: "ins-worker", SandboxGeneration: 9,
				AssignmentId: "asn-worker", RunnerId: "run-worker",
			},
			Revision: 4,
			State: AssignmentState{
				State: "starting", Generation: 9, Deadline: now.Add(-time.Millisecond),
				RetryLimit: 1,
			},
		},
		found: true,
	}
	worker := AssignmentWorker{
		Store: store, WorkerID: "assignment-worker", ClaimDuration: time.Minute,
		PollInterval: time.Second, CommandDeadline: 30 * time.Second,
		HeartbeatTimeout: time.Minute,
		NewCommandID: func(string) string {
			return "cmd-fence-worker"
		},
	}
	decision, found, err := worker.RunOnce(t.Context(), now)
	if err != nil || !found || decision.Action != ActionFence {
		t.Fatalf("worker decision = %#v, %t, %v", decision, found, err)
	}
	command := store.fenceCommand
	if command == nil || command.MessageId != "cmd-fence-worker" ||
		command.Fence == nil || command.Fence.AssignmentId != "asn-worker" ||
		command.Fence.SandboxId != "sbx-worker" ||
		command.Fence.InstanceId != "ins-worker" ||
		command.Fence.SandboxGeneration != 9 ||
		string(command.Fence.FencingToken) != "worker-fencing-token" ||
		command.Correlation.GetRequestId() != "req-worker" ||
		command.Correlation.GetOperationId() != "op-worker" ||
		command.Correlation.GetRunnerId() != "run-worker" ||
		command.DeadlineUnixMs != uint64(now.Add(30*time.Second).UnixMilli()) {
		t.Fatalf("worker fence command = %#v", command)
	}
}

func TestAssignmentWorkerSeparatesStartupTimeoutFromRunnerLoss(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	startupTimeout := AssignmentState{
		State: "fenced", Generation: 4, FenceProofDigest: "sha256:startup-fence",
		FailureClass: FailureStartupTimeout,
	}
	if decision := assignmentWorkerDecision(startupTimeout, now); decision.Action != ActionFailTerminal {
		t.Fatalf("proved startup timeout decision = %#v", decision)
	}
	runnerLoss := AssignmentState{
		State: "fenced", Generation: 4, FenceProofDigest: "sha256:runner-loss-fence",
		FailureClass: FailureFencing,
	}
	if decision := assignmentWorkerDecision(runnerLoss, now); decision.Action != ActionAdvanceGeneration ||
		!decision.MayReassign || decision.NextGeneration != 5 {
		t.Fatalf("proved Runner loss decision = %#v", decision)
	}
	ordinaryStop := AssignmentState{
		State: "fenced", Generation: 4, FenceProofDigest: "sha256:ordinary-stop",
	}
	if decision := assignmentWorkerDecision(ordinaryStop, now); decision.Action != ActionWait {
		t.Fatalf("ordinary lifecycle stop decision = %#v", decision)
	}
}

type assignmentWorkerStore struct {
	claim        Claim
	found        bool
	fenceCommand *runnerv1.FenceCommand
}

func (store *assignmentWorkerStore) MarkExpiredRunners(
	context.Context,
	time.Time,
	time.Time,
) (int64, error) {
	return 0, nil
}

func (store *assignmentWorkerStore) ClaimNext(
	context.Context,
	string,
	time.Time,
	time.Time,
) (Claim, bool, error) {
	return store.claim, store.found, nil
}

func (store *assignmentWorkerStore) ApplyDecision(
	_ context.Context,
	_ Claim,
	_ Decision,
	command *runnerv1.FenceCommand,
	_ time.Time,
	_ time.Time,
) error {
	store.fenceCommand = command
	return nil
}

func (store *assignmentWorkerStore) AdvanceFencedGeneration(
	context.Context,
	string,
	int64,
	time.Time,
) (int64, error) {
	return 0, nil
}
