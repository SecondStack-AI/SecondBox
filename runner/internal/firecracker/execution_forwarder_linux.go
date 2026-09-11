package firecracker

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/SecondStack-AI/SecondBox/runner/internal/egressforwarder"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func (m *Manager) cleanupExecutionListenerRules(ctx context.Context, tapName string) error {
	return egressforwarder.RemoveExecutionListenerRules(ctx, m.cfg.NetworkPolicyNFTPath, []string{tapName})
}

func (h *instanceHostReservation) startExecutionForwarder(ctx context.Context, network *runtimemanager.AttributedExecutionNetwork) (*networkpolicy.CompiledPolicy, error) {
	if h.tapName == "" {
		return nil, fmt.Errorf("Firecracker attributed execution requires TAP networking")
	}
	forwarder, err := egressforwarder.StartExecutionForwarder(ctx, egressforwarder.ExecutionForwarderConfig{
		NFTPath: h.manager.cfg.NetworkPolicyNFTPath, GatewaySocket: network.GatewaySocket,
		Attribution: network.Attribution, MaximumConnections: network.MaximumConnections,
		Policy: egressforwarder.ExecutionListenerPolicy{
			InstanceID: network.Attribution.InstanceID, GuestInterface: h.tapName,
			BridgeInterface: h.manager.cfg.MicroVMBridgeName,
			GuestAddress:    netip.MustParseAddr(h.guestIP),
			ListenerAddress: netip.AddrPortFrom(bridgeAddress(h.manager.cfg.MicroVMBridgeCIDR), 0),
		},
	})
	if err != nil {
		return nil, err
	}
	h.executionForwarder = forwarder
	return networkpolicy.CompileExecutionListener(forwarder.ListenerAddress(), network.CompileOptions)
}
