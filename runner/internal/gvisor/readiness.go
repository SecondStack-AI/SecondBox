//go:build linux

package gvisor

import (
	"bytes"
	"errors"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// probePlatform proves the real environment: the pinned runsc reports its
// expected release, the sentry platform can boot and tear down a trivial
// sandbox with no KVM requirement, and startup reconciliation has cleared any
// stale leftovers. Only success is cached, and only for the immutable
// platform facts: a transient failure retries on the next readiness pass
// instead of pinning the runner unready until restart, while health that can
// degrade after a successful probe - network-policy enforcement and loop
// allocation - re-proves itself on every pass.
func (backend *AssignmentBackend) probePlatform(ctx context.Context) error {
	backend.platformProbeMu.Lock()
	defer backend.platformProbeMu.Unlock()
	if !backend.platformProbed {
		if err := backend.runPlatformProbe(ctx); err != nil {
			return err
		}
		backend.platformProbed = true
	}
	if err := ensureHostNetworkPlumbing(ctx, dnsAddressForProfile(backend.config.NetworkProfile)); err != nil {
		return fmt.Errorf("SecondBox gVisor host network plumbing: %w", err)
	}
	if err := backend.enforcer.Ready(ctx); err != nil {
		return fmt.Errorf("SecondBox gVisor network policy enforcement: %w", err)
	}
	if err := probeLoopAllocation(backend.config.RuntimeDir); err != nil {
		return fmt.Errorf("SecondBox gVisor loop devices are unavailable: %w", err)
	}
	if err := probeCgroupControls(backend.config.NetworkProfile); err != nil {
		return fmt.Errorf("SecondBox gVisor cgroup resource controls: %w", err)
	}
	return nil
}

// probeCgroupControls proves a sandbox cgroup can actually be created with
// writable CPU and memory controls before ResourceLimitsReady is advertised:
// an unprepared host would otherwise register successfully and then fail
// every assignment at launch.
func probeCgroupControls(profile uint32) error {
	probe := filepath.Join("/sys/fs/cgroup", sandboxCgroupParent(), sandboxCgroupDirectory(profile), "readiness-probe")
	if err := os.MkdirAll(probe, 0o755); err != nil {
		return fmt.Errorf("create probe cgroup: %w", err)
	}
	defer func() { _ = os.Remove(probe) }()
	for control, value := range map[string]string{
		"cpu.max":    "max 100000",
		"memory.max": "max",
	} {
		if err := os.WriteFile(filepath.Join(probe, control), []byte(value+"\n"), 0o644); err != nil {
			return fmt.Errorf("write probe %s: %w", control, err)
		}
	}
	return os.Remove(probe)
}

// probeLoopAllocation proves a loop device can actually be allocated, opened,
// and bound, not merely that the control node exists: missing device nodes,
// exhausted allocation, or absent ioctl authority would otherwise pass
// readiness while every assignment start fails. A disposable backing file is
// bound with autoclear and detached immediately.
func probeLoopAllocation(runtimeDir string) error {
	backing, err := os.CreateTemp(runtimeDir, "loop-probe-")
	if err != nil {
		return fmt.Errorf("create loop probe backing file: %w", err)
	}
	defer func() {
		_ = backing.Close()
		_ = os.Remove(backing.Name())
	}()
	if err := backing.Truncate(4096); err != nil {
		return fmt.Errorf("size loop probe backing file: %w", err)
	}
	loopControl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer loopControl.Close()
	index, err := unix.IoctlRetInt(int(loopControl.Fd()), unix.LOOP_CTL_GET_FREE)
	if err != nil {
		return fmt.Errorf("acquire free loop device: %w", err)
	}
	device, err := os.OpenFile(fmt.Sprintf("/dev/loop%d", index), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer device.Close()
	if err := unix.IoctlSetInt(int(device.Fd()), unix.LOOP_SET_FD, int(backing.Fd())); err != nil {
		if errors.Is(err, unix.EBUSY) {
			// Losing the unreserved index to a concurrent start proves
			// allocation works; the probe's job is done.
			return nil
		}
		return fmt.Errorf("bind loop probe backing file: %w", err)
	}
	if err := unix.IoctlSetInt(int(device.Fd()), unix.LOOP_CLR_FD, 0); err != nil {
		return fmt.Errorf("detach loop probe backing file: %w", err)
	}
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
