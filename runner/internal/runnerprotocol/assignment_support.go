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
func SameAssignmentIdentity(fence *AssignmentFence, egressContext string, permission *AttributedExecutionPermission, assignment *AssignmentCommand) bool {
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
		proto.Equal(permission, assignment.AttributedExecutionPermission) &&
		(egressContext != "") == requiresEgressContext
}

// ValidateAttributedExecutionPermission requires the permission exactly when
// the per-exec attribution capability is required, and bounds its values.
func ValidateAttributedExecutionPermission(assignment *AssignmentCommand) error {
	if assignment == nil || assignment.Requirements == nil {
		return fmt.Errorf("attributed execution permission requires assignment requirements")
	}
	permission := assignment.AttributedExecutionPermission
	required := slices.Contains(assignment.Requirements.RequiredCapabilities, "per-exec-attribution")
	if required != (permission != nil) {
		return fmt.Errorf("attributed execution permission and required capability disagree")
	}
	if permission == nil {
		return nil
	}
	if permission.Gateway == "" || permission.MaximumConnections < 1 || permission.MaximumConnections > 4096 ||
		!assignment.Requirements.RequiresTenantEgressContext || assignment.EgressContext == "" {
		return fmt.Errorf("attributed execution permission requires a gateway, 1 to 4096 connections, and a pinned egress context")
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
