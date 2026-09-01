package runnerv1

import "bytes"

// SameAssignmentIdentity compares the durable replay identity shared by every
// compute backend. The context-required bit is derived from the context name;
// RunnerProtocolService rejects commands where those values disagree.
func SameAssignmentIdentity(fence *AssignmentFence, egressContext string, assignment *AssignmentCommand) bool {
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
		(egressContext != "") == requiresEgressContext
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
