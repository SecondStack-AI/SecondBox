package runnerv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestAttributedExecutionPermissionAssignmentReplay(t *testing.T) {
	fence := &AssignmentFence{AssignmentId: "assignment"}
	permission := &AttributedExecutionPermission{Gateway: "gateway", MaximumConnections: 10}
	assignment := &AssignmentCommand{Fence: fence, AttributedExecutionPermission: proto.CloneOf(permission)}
	if !SameAssignmentIdentity(fence, "", permission, assignment) {
		t.Fatal("unchanged permission did not replay")
	}
	for name, change := range map[string]func(*AssignmentCommand){
		"removed": func(a *AssignmentCommand) { a.AttributedExecutionPermission = nil },
		"gateway": func(a *AssignmentCommand) { a.AttributedExecutionPermission.Gateway = "other" },
		"limit":   func(a *AssignmentCommand) { a.AttributedExecutionPermission.MaximumConnections++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := proto.CloneOf(assignment)
			change(changed)
			if SameAssignmentIdentity(fence, "", permission, changed) {
				t.Fatal("changed permission replayed")
			}
		})
	}
	if SameAssignmentIdentity(fence, "", nil, assignment) {
		t.Fatal("ordinary assignment gained permission")
	}
}

func TestAttributedExecutionPermissionRequiresCapabilityAndContext(t *testing.T) {
	for _, permitted := range []bool{false, true} {
		for _, capability := range []bool{false, true} {
			assignment := &AssignmentCommand{
				Requirements:  &ProfileRequirements{RequiresTenantEgressContext: true},
				EgressContext: "tenant",
			}
			if permitted {
				assignment.AttributedExecutionPermission = &AttributedExecutionPermission{Gateway: "gateway", MaximumConnections: 2}
			}
			if capability {
				assignment.Requirements.RequiredCapabilities = []string{"per-exec-attribution"}
			}
			err := ValidateAttributedExecutionPermission(assignment)
			if (err == nil) != (permitted == capability) {
				t.Fatalf("permitted=%v capability=%v: %v", permitted, capability, err)
			}
		}
	}
	for name, permission := range map[string]*AttributedExecutionPermission{
		"gateway": {MaximumConnections: 2},
		"zero":    {Gateway: "gateway"},
		"excess":  {Gateway: "gateway", MaximumConnections: 4097},
	} {
		assignment := &AssignmentCommand{
			Requirements: &ProfileRequirements{
				RequiresTenantEgressContext: true, RequiredCapabilities: []string{"per-exec-attribution"},
			},
			EgressContext: "tenant", AttributedExecutionPermission: permission,
		}
		if ValidateAttributedExecutionPermission(assignment) == nil {
			t.Fatalf("accepted %s permission", name)
		}
	}
	unpinned := &AssignmentCommand{
		Requirements:                  &ProfileRequirements{RequiredCapabilities: []string{"per-exec-attribution"}},
		AttributedExecutionPermission: &AttributedExecutionPermission{Gateway: "gateway", MaximumConnections: 2},
	}
	if ValidateAttributedExecutionPermission(unpinned) == nil {
		t.Fatal("accepted a permission without a pinned egress context")
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
