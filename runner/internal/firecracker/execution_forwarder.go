package firecracker

import (
	"context"
	"net/netip"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

type instanceExecutionForwarder interface {
	ListenerAddress() netip.AddrPort
	Done() <-chan struct{}
	Wait() error
	Revoke()
	Close(context.Context) error
}

func (b *AssignmentBackend) executionNetworkForAssignment(assignment *runnerprotocol.AssignmentCommand) (*runtimemanager.AttributedExecutionNetwork, error) {
	execution, fence := assignment.AttributedExecution, assignment.Fence
	if execution == nil {
		return nil, nil
	}
	socket, err := b.manager.cfg.NetworkPolicyEgressContexts.AttributedGatewaySocket(assignment.EgressContext, execution.Gateway)
	if err != nil {
		return nil, err
	}
	options, err := b.manager.networkPolicyCompileOptions(assignment.EgressContext, true)
	if err != nil {
		return nil, err
	}
	return &runtimemanager.AttributedExecutionNetwork{
		GatewaySocket: socket, MaximumConnections: int(execution.MaximumConnections), CompileOptions: options,
		Attribution: egressattribution.ExecutionAttribution{
			TenantRef: execution.TenantRef, SubjectRef: execution.SubjectRef,
			SandboxID: fence.SandboxId, InstanceID: fence.InstanceId, AssignmentID: fence.AssignmentId,
			Generation: int64(fence.SandboxGeneration), AuthorizationRef: execution.AuthorizationRef,
			ExpiresAt: time.UnixMilli(int64(execution.ExpiresAtUnixMs)),
		},
	}, nil
}

func closeExecutionForwarder(forwarder instanceExecutionForwarder) error {
	if forwarder == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return forwarder.Close(ctx)
}
