package runnerv1

import (
	"bytes"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"
)

// SameAssignmentIdentity compares the durable replay identity shared by every
// compute backend. The context-required bit is derived from the context name;
// RunnerProtocolService rejects commands where those values disagree.
func SameAssignmentIdentity(fence *AssignmentFence, egressContext string, executionBinding *AttributedExecution, assignment *AssignmentCommand) bool {
	if fence == nil || assignment == nil || assignment.Fence == nil {
		return false
	}
	requiresEgressContext := assignment.Requirements != nil && assignment.Requirements.RequiresTenantEgressContext
	return fence.AssignmentId == assignment.Fence.AssignmentId &&
		fence.SandboxId == assignment.Fence.SandboxId &&
		fence.InstanceId == assignment.Fence.InstanceId &&
		fence.SandboxGeneration == assignment.Fence.SandboxGeneration &&
		bytes.Equal(fence.FencingToken, assignment.Fence.FencingToken) &&
		egressContext == assignment.EgressContext &&
		proto.Equal(executionBinding, assignment.AttributedExecution) &&
		(egressContext != "") == requiresEgressContext
}

func ValidateAttributedExecutionCapability(assignment *AssignmentCommand) error {
	if assignment == nil || assignment.Requirements == nil {
		return fmt.Errorf("attributed execution capability requires assignment requirements")
	}
	required := slices.Contains(assignment.Requirements.RequiredCapabilities, "attributed-execution")
	if required != (assignment.AttributedExecution != nil) {
		return fmt.Errorf("attributed execution binding and required capability disagree")
	}
	return nil
}

// RecoveredAssignmentSummary clones one backend's durable assignment identity
// into the heartbeat protocol representation. A nil fence has no identity.
func RecoveredAssignmentSummary(fence *AssignmentFence, egressContext string) *ActiveAssignmentSummary {
	if fence == nil {
		return nil
	}
	return &ActiveAssignmentSummary{
		AssignmentId: fence.AssignmentId, SandboxId: fence.SandboxId,
		InstanceId: fence.InstanceId, SandboxGeneration: fence.SandboxGeneration,
		FencingToken: bytes.Clone(fence.FencingToken), EgressContext: egressContext,
	}
}
