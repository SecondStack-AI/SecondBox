//go:build linux

package gvisor

import (
	"context"
	"net/netip"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/egressforwarder"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func (backend *AssignmentBackend) startExecutionForwarder(ctx context.Context, assignment *runnerprotocol.AssignmentCommand, network instanceNetwork, options networkpolicy.CompileOptions) (*egressforwarder.ExecutionForwarder, *networkpolicy.CompiledPolicy, error) {
	execution, fence := assignment.AttributedExecution, assignment.Fence
	socket, err := backend.config.NetworkPolicy.EgressContexts.AttributedGatewaySocket(assignment.EgressContext, execution.Gateway)
	if err != nil {
		return nil, nil, err
	}
	forwarder, err := egressforwarder.StartExecutionForwarder(ctx, egressforwarder.ExecutionForwarderConfig{
		NFTPath: backend.nftPath, GatewaySocket: socket, MaximumConnections: int(execution.MaximumConnections),
		Policy: egressforwarder.ExecutionListenerPolicy{
			InstanceID: fence.InstanceId, GuestInterface: network.hostVeth,
			GuestAddress:    netip.MustParseAddr(network.guestAddress),
			ListenerAddress: netip.AddrPortFrom(netip.MustParseAddr(network.hostAddress), 0),
		},
		Attribution: egressattribution.ExecutionAttribution{
			TenantRef: execution.TenantRef, SubjectRef: execution.SubjectRef,
			SandboxID: fence.SandboxId, InstanceID: fence.InstanceId, AssignmentID: fence.AssignmentId,
			Generation: int64(fence.SandboxGeneration), AuthorizationRef: execution.AuthorizationRef,
			ExpiresAt: time.UnixMilli(int64(execution.ExpiresAtUnixMs)),
		},
	})
	if err != nil {
		return nil, nil, err
	}
	compiled, err := networkpolicy.CompileExecutionListener(forwarder.ListenerAddress(), options)
	return forwarder, compiled, err
}
