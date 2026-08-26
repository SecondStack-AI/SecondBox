//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// probePlatform proves the real environment: the pinned runsc reports its
// expected release, the sentry platform can boot and tear down a trivial
// sandbox with no KVM requirement, loop devices are usable, and startup
// reconciliation has cleared any stale leftovers. Only success is cached: a
// transient failure retries on the next readiness pass instead of pinning
// the runner unready until restart.
func (backend *AssignmentBackend) probePlatform(ctx context.Context) error {
	backend.platformProbeMu.Lock()
	defer backend.platformProbeMu.Unlock()
	if backend.platformProbed {
		return nil
	}
	if err := backend.runPlatformProbe(ctx); err != nil {
		return err
	}
	backend.platformProbed = true
	return nil
}

func (backend *AssignmentBackend) runPlatformProbe(ctx context.Context) error {
	version, err := exec.CommandContext(ctx, backend.config.RunscPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("SecondBox gVisor runsc version probe: %w", err)
	}
	release := strings.TrimPrefix(backend.config.manifest.HelperBuildID, "runsc-")
	if !strings.Contains(string(version), release) {
		return fmt.Errorf("SecondBox gVisor runsc reports a different release than the pinned materialization")
	}
	loopControl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("SecondBox gVisor loop devices are unavailable: %w", err)
	}
	_ = loopControl.Close()
	if _, err := ReconcileStaleLoops(backend.config.WorkspaceRoot); err != nil {
		return fmt.Errorf("SecondBox gVisor stale attachment reconciliation: %w", err)
	}
	if err := reconcileStaleCgroups(backend.config.NetworkProfile); err != nil {
		return fmt.Errorf("SecondBox gVisor stale cgroup reconciliation: %w", err)
	}
	if err := reconcileStaleNetworks(ctx, backend.config.NetworkProfile); err != nil {
		return fmt.Errorf("SecondBox gVisor stale network reconciliation: %w", err)
	}
	if err := reconcileStaleRuntimeDirectories(backend.config.RuntimeDir); err != nil {
		return fmt.Errorf("SecondBox gVisor stale runtime reconciliation: %w", err)
	}
	if err := ensureHostNetworkPlumbing(ctx, dnsAddressForProfile(backend.config.NetworkProfile)); err != nil {
		return fmt.Errorf("SecondBox gVisor host network plumbing: %w", err)
	}
	if err := backend.enforcer.Ready(ctx); err != nil {
		return fmt.Errorf("SecondBox gVisor network policy enforcement: %w", err)
	}
	stateRoot, err := os.MkdirTemp(backend.config.RuntimeDir, "readiness-")
	if err != nil {
		return fmt.Errorf("SecondBox gVisor readiness state root: %w", err)
	}
	defer func() { _ = os.RemoveAll(stateRoot) }()
	probe := exec.CommandContext(ctx, backend.config.RunscPath,
		"--root", stateRoot, "--network=none", "--platform=systrap", "do", "/bin/true")
	probe.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if output, err := probe.CombinedOutput(); err != nil {
		return fmt.Errorf("SecondBox gVisor sentry platform probe: %v: %s", err, bytes.TrimSpace(output))
	}
	return nil
}
