package firecracker

import (
	"context"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/egressforwarder"
)

func (m *Manager) cleanupExecutionListenerRules(ctx context.Context, tapName string) error {
	return egressforwarder.RemoveExecutionListenerRules(ctx, m.cfg.NetworkPolicyNFTPath, []string{tapName})
}

func startAttributedExecForwarder(ctx context.Context, config AttributedExecWindowConfig, attribution egressattribution.ExecutionAttribution) (attributedExecForwarder, error) {
	return egressforwarder.StartExecutionForwarder(ctx, egressforwarder.ExecutionForwarderConfig{
		NFTPath: config.NFTPath, GatewaySocket: config.Gateway.Socket,
		MaximumConnections: config.Gateway.MaximumConnections,
		Policy:             config.Listener, Attribution: attribution,
	})
}
