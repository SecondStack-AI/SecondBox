package reconcile

import (
	"testing"
	"time"
)

func TestRunnerLossRequiresFenceProofBeforeReplacement(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	decision := DecideRunnerLoss(AssignmentState{
		State: "uncertain", Generation: 7, FenceProofDigest: "",
		RetryCount: 0, Deadline: now.Add(time.Minute),
	}, now)
	if decision.Action != ActionFence || decision.MayReassign {
		t.Fatalf("loss decision = %#v, want fence without reassignment", decision)
	}

	decision = DecideRunnerLoss(AssignmentState{
		State: "fenced", Generation: 7, FenceProofDigest: "sha256:proof",
		RetryCount: 0, Deadline: now.Add(time.Minute),
	}, now)
	if decision.Action != ActionAdvanceGeneration || !decision.MayReassign || decision.NextGeneration != 8 {
		t.Fatalf("proved fence decision = %#v, want generation 8 reassignment", decision)
	}
}

func TestRetryClassificationIsBoundedAndDeadlineAware(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		state AssignmentState
		want  Action
	}{
		{
			name:  "transient within bound",
			state: AssignmentState{State: "failed", FailureClass: FailureTransient, RetryCount: 1, RetryLimit: 3, Deadline: now.Add(time.Minute)},
			want:  ActionRetry,
		},
		{
			name:  "transient exhausted",
			state: AssignmentState{State: "failed", FailureClass: FailureTransient, RetryCount: 3, RetryLimit: 3, Deadline: now.Add(time.Minute)},
			want:  ActionFailTerminal,
		},
		{
			name:  "deadline elapsed",
			state: AssignmentState{State: "starting", FailureClass: FailureTransient, RetryCount: 0, RetryLimit: 3, Deadline: now.Add(-time.Millisecond)},
			want:  ActionFence,
		},
		{
			name:  "fencing waits before deadline",
			state: AssignmentState{State: "fencing", FailureClass: FailureFencing, RetryCount: 0, RetryLimit: 1, Deadline: now.Add(time.Minute)},
			want:  ActionWait,
		},
		{
			name:  "fencing deadline retries within bound",
			state: AssignmentState{State: "fencing", FailureClass: FailureFencing, RetryCount: 0, RetryLimit: 1, Deadline: now.Add(-time.Millisecond)},
			want:  ActionFence,
		},
		{
			name:  "fencing deadline fails at bound",
			state: AssignmentState{State: "fencing", FailureClass: FailureFencing, RetryCount: 1, RetryLimit: 1, Deadline: now.Add(-time.Millisecond)},
			want:  ActionFailTerminal,
		},
		{
			name:  "compatibility terminal",
			state: AssignmentState{State: "failed", FailureClass: FailureCompatibility, RetryCount: 0, RetryLimit: 3, Deadline: now.Add(time.Minute)},
			want:  ActionFailTerminal,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := DecideAssignment(test.state, now).Action; got != test.want {
				t.Fatalf("action = %q, want %q", got, test.want)
			}
		})
	}
}
