package microvm

import (
	"context"
	"fmt"
	"strings"
)

// ProbePrivilegedLauncher performs the same versioned protocol handshake used
// by manager startup without constructing a runtime manager.
func ProbePrivilegedLauncher(ctx context.Context, socketPath string) error {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return fmt.Errorf("privileged launcher socket is not configured")
	}
	return newPrivilegedLauncherClient(socketPath).Ping(ctx)
}
