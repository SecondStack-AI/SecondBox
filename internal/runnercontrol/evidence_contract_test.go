package runnercontrol

import (
	"testing"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestRunnerEvidenceContractHasOnlyFixedClassificationAndCorrelationFields(t *testing.T) {
	fields := (&runnerv1.Evidence{}).ProtoReflect().Descriptor().Fields()
	want := []string{
		"message_id",
		"sequence",
		"event",
		"outcome",
		"terminal_kind",
		"observed_at_unix_ms",
		"correlation",
	}
	if fields.Len() != len(want) {
		t.Fatalf("Evidence fields = %d, want %d", fields.Len(), len(want))
	}
	for index, name := range want {
		if got := string(fields.Get(index).Name()); got != name {
			t.Fatalf("Evidence field %d = %q, want %q", index, got, name)
		}
	}
}

func TestRunnerOperationCorrelationRejectsMissingAndMismatchedAuthority(t *testing.T) {
	fence := &runnerv1.AssignmentFence{
		AssignmentId: "assignment-1", SandboxId: "sandbox-1", InstanceId: "instance-1",
		SandboxGeneration: 7,
	}
	correlation := &runnerv1.Correlation{
		RequestId: "request-1", OperationId: "operation-1", SandboxId: "sandbox-1",
		InstanceId: "instance-1", SandboxGeneration: 7,
		AssignmentId: "assignment-1", RunnerId: "runner-1",
	}
	if err := validateOperationCorrelation("runner-1", fence, correlation); err != nil {
		t.Fatalf("complete operation correlation: %v", err)
	}
	if err := validateOperationCorrelation("runner-1", fence, nil); err == nil {
		t.Fatal("missing operation correlation was accepted")
	}
	correlation.RunnerId = "runner-2"
	if err := validateOperationCorrelation("runner-1", fence, correlation); err == nil {
		t.Fatal("mismatched Runner correlation was accepted")
	}
}
