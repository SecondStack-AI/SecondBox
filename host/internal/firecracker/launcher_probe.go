package microvm

import (
	"context"
	"fmt"
	"strings"
	"time"
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

// WaitForPrivilegedLauncher retries the versioned protocol handshake until the
// launcher is ready or ctx expires. Systemd may start its ExecStartPost probe
// before the launcher has created its Unix socket.
func WaitForPrivilegedLauncher(ctx context.Context, socketPath string, retryInterval time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if retryInterval <= 0 {
		retryInterval = 100 * time.Millisecond
	}
	var lastErr error
	for {
		if err := ProbePrivilegedLauncher(ctx, socketPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for privileged launcher readiness: %w (last error: %v)", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}
