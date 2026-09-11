//go:build !linux

package firecracker

import (
	"context"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func (m *Manager) cleanupExecutionListenerRules(context.Context, string) error {
	return fmt.Errorf("Firecracker attributed execution cleanup requires Linux host networking")
}

func (h *instanceHostReservation) startExecutionForwarder(context.Context, *runtimemanager.AttributedExecutionNetwork) (*networkpolicy.CompiledPolicy, error) {
	return nil, fmt.Errorf("Firecracker attributed execution requires Linux host networking")
}
