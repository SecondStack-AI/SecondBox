package microvm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ProbeDirectFirecracker verifies the executable used by authorized unjailed
// launches is present and still matches the pinned runtime version.
func ProbeDirectFirecracker(ctx context.Context, firecrackerPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := requireExecutable("firecracker", firecrackerPath); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, firecrackerPath, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run firecracker --version: %w", err)
	}
	major, minor, patch, ok := parseFirecrackerVersion(string(out))
	if !ok {
		return fmt.Errorf("parse firecracker --version output %q", strings.TrimSpace(string(out)))
	}
	wantMajor, wantMinor, wantPatch, ok := expectedFirecrackerVersion()
	if !ok {
		return fmt.Errorf("parse embedded firecracker.lock")
	}
	if major != wantMajor || minor != wantMinor || patch != wantPatch {
		return fmt.Errorf("firecracker %d.%d.%d does not match pinned version %d.%d.%d", major, minor, patch, wantMajor, wantMinor, wantPatch)
	}
	return nil
}

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
