package runnerv1

import "testing"

func TestAssignmentIdentityAndRecoveredSummary(t *testing.T) {
	fence := &AssignmentFence{
		AssignmentId: "assignment-a", SandboxId: "sandbox-a", InstanceId: "instance-a",
		SandboxGeneration: 7, FencingToken: []byte("fencing-token"),
	}
	assignment := &AssignmentCommand{
		Fence: fence, EgressContext: "tenant-a",
		Requirements: &ProfileRequirements{RequiresTenantEgressContext: true},
	}
	if !SameAssignmentIdentity(fence, "tenant-a", assignment) {
		t.Fatal("identical assignment identity did not replay")
	}
	assignment.Requirements.RequiresTenantEgressContext = false
	if SameAssignmentIdentity(fence, "tenant-a", assignment) {
		t.Fatal("inconsistent context-required identity replayed")
	}
	assignment.Requirements.RequiresTenantEgressContext = true
	assignment.EgressContext = "tenant-b"
	if SameAssignmentIdentity(fence, "tenant-a", assignment) {
		t.Fatal("changed context identity replayed")
	}

	summary := RecoveredAssignmentSummary(fence, "tenant-a")
	if summary == nil || summary.AssignmentId != fence.AssignmentId || summary.EgressContext != "tenant-a" {
		t.Fatalf("recovered summary = %#v", summary)
	}
	summary.FencingToken[0] = 'X'
	if fence.FencingToken[0] == 'X' {
		t.Fatal("recovered summary aliases the active fencing token")
	}
	if RecoveredAssignmentSummary(nil, "") != nil {
		t.Fatal("nil fence produced a recovered summary")
	}
}
