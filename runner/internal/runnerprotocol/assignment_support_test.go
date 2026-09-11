package runnerv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestAttributedExecutionAssignmentReplay(t *testing.T) {
	fence := &AssignmentFence{AssignmentId: "assignment"}
	binding := &AttributedExecution{TenantRef: "tenant", SubjectRef: "subject", AuthorizationRef: "authorization", ExpiresAtUnixMs: 1234, Gateway: "gateway", MaximumConnections: 10}
	assignment := &AssignmentCommand{Fence: fence, AttributedExecution: proto.CloneOf(binding)}
	if !SameAssignmentIdentity(fence, "", binding, assignment) {
		t.Fatal("unchanged attribution did not replay")
	}
	for name, change := range map[string]func(*AssignmentCommand){
		"mode":      func(a *AssignmentCommand) { a.AttributedExecution = nil },
		"tenant":    func(a *AssignmentCommand) { a.AttributedExecution.TenantRef = "other" },
		"subject":   func(a *AssignmentCommand) { a.AttributedExecution.SubjectRef = "other" },
		"reference": func(a *AssignmentCommand) { a.AttributedExecution.AuthorizationRef = "other" },
		"expiry":    func(a *AssignmentCommand) { a.AttributedExecution.ExpiresAtUnixMs++ },
		"gateway":   func(a *AssignmentCommand) { a.AttributedExecution.Gateway = "other" },
		"limit":     func(a *AssignmentCommand) { a.AttributedExecution.MaximumConnections++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := proto.CloneOf(assignment)
			change(changed)
			if SameAssignmentIdentity(fence, "", binding, changed) {
				t.Fatal("changed attribution replayed")
			}
		})
	}
	if SameAssignmentIdentity(fence, "", nil, assignment) {
		t.Fatal("ordinary assignment gained attribution")
	}
}

func TestAttributedExecutionCapabilityRequired(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		for _, capability := range []bool{false, true} {
			assignment := &AssignmentCommand{Requirements: &ProfileRequirements{}}
			if attributed {
				assignment.AttributedExecution = &AttributedExecution{}
			}
			if capability {
				assignment.Requirements.RequiredCapabilities = []string{"attributed-execution"}
			}
			err := ValidateAttributedExecutionCapability(assignment)
			if (err == nil) != (attributed == capability) {
				t.Fatalf("attributed=%v capability=%v: %v", attributed, capability, err)
			}
		}
	}
}

func TestAssignmentIdentityAndRecoveredSummary(t *testing.T) {
	fence := &AssignmentFence{
		AssignmentId: "assignment-a", SandboxId: "sandbox-a", InstanceId: "instance-a",
		SandboxGeneration: 7, FencingToken: []byte("fencing-token"),
	}
	assignment := &AssignmentCommand{
		Fence: fence, EgressContext: "tenant-a",
		Requirements: &ProfileRequirements{RequiresTenantEgressContext: true},
	}
	if !SameAssignmentIdentity(fence, "tenant-a", nil, assignment) {
		t.Fatal("identical assignment identity did not replay")
	}
	assignment.Requirements.RequiresTenantEgressContext = false
	if SameAssignmentIdentity(fence, "tenant-a", nil, assignment) {
		t.Fatal("inconsistent context-required identity replayed")
	}
	assignment.Requirements.RequiresTenantEgressContext = true
	assignment.EgressContext = "tenant-b"
	if SameAssignmentIdentity(fence, "tenant-a", nil, assignment) {
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
