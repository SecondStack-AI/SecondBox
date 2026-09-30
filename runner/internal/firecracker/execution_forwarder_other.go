//go:build !linux

package firecracker

import (
	"context"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
)

func (m *Manager) cleanupExecutionListenerRules(context.Context, string) error {
	return fmt.Errorf("Firecracker attributed execution cleanup requires Linux host networking")
}

func startAttributedExecForwarder(context.Context, AttributedExecWindowConfig, egressattribution.ExecutionAttribution) (attributedExecForwarder, error) {
	return nil, fmt.Errorf("SecondBox attributed execution requires Linux host networking")
}
