package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// proofCgroupLimits proves cgroup v2 enforcement of the two bounds a backend
// assignment must honour: an integer-vCPU CPU quota and a hard memory limit.
// Limits are declared through the OCI spec so runsc owns cgroup placement,
// which is exactly the mechanism the production backend will use.
func proofCgroupLimits(env *probeEnv) error {
	if env.rootless {
		// Development-only escape: cgroup writes need root. The qualification
		// recipe never passes -rootless, so this cannot skip the real gate.
		emit(env.stdout, "cgroup-limits", "skipped", "reason=rootless_development_mode")
		return nil
	}
	base := filepath.Join(env.workDir, "cgroup-limits")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	if err := subproofCPUQuota(env, base); err != nil {
		return fmt.Errorf("cpu-quota: %w", err)
	}
	if err := subproofMemoryLimit(env, base); err != nil {
		return fmt.Errorf("memory-limit: %w", err)
	}
	return nil
}

const (
	cgroupMountPoint = "/sys/fs/cgroup"
	// The spin guest runs four workers for three seconds under a one-CPU
	// quota. Unlimited it accumulates ~12 CPU-seconds; enforced it stays
	// near 3. The bound leaves headroom for sentry overhead and sampling lag.
	cpuUsageBoundMicros   = 6_000_000
	cpuUsageMinimumMicros = 1_500_000
	memoryLimitBytes      = 128 << 20
)

func subproofCPUQuota(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "cpu")
	if err != nil {
		return err
	}
	cgroupsPath := "/secondbox-gvisor-probe/" + area.containerID
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs:   []string{"spin", guestMarkerPath("cpu")},
		Binds:       []bindMount{{Source: area.markerDir, Destination: guestMarkerMount, ReadOnly: false}},
		CgroupsPath: cgroupsPath,
		Resources: &ociResources{
			CPU: &ociCPU{Quota: 100_000, Period: 100_000},
		},
	}); err != nil {
		return err
	}
	command := env.runscRun(area, "run")
	if err := command.Start(); err != nil {
		return fmt.Errorf("start runsc run: %w", err)
	}
	defer reapArea(env, area, command)

	if err := waitForFile(filepath.Join(area.markerDir, "cpu"), bootDeadline); err != nil {
		return fmt.Errorf("spin sandbox did not boot: %w", err)
	}
	// Sample the container cgroup while the sandbox runs; runsc removes the
	// cgroup on exit, so the last successful sample is the evidence.
	usage := int64(-1)
	stopSampling := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			value, err := readCPUUsageMicros(cgroupsPath)
			if err == nil {
				usage = value
			}
			select {
			case <-stopSampling:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	waitErr := waitCommand(command, teardownDeadline+spinWallDuration)
	close(stopSampling)
	<-sampled
	if waitErr != nil {
		return fmt.Errorf("spin sandbox did not finish: %w", waitErr)
	}
	if usage < 0 {
		return fmt.Errorf("cgroup cpu.stat was never readable under %s", cgroupsPath)
	}
	if usage < cpuUsageMinimumMicros {
		return fmt.Errorf("cpu usage %d microseconds is implausibly low; spin did not run", usage)
	}
	if usage > cpuUsageBoundMicros {
		return fmt.Errorf("cpu usage %d microseconds exceeds the one-CPU quota bound %d",
			usage, cpuUsageBoundMicros)
	}
	emit(env.stdout, "cgroup-cpu-quota", "passed",
		"usage_micros="+strconv.FormatInt(usage, 10),
		"bound_micros="+strconv.FormatInt(cpuUsageBoundMicros, 10),
		"workers=4",
		"quota_cpus=1")
	return nil
}

const spinWallDuration = 10 * time.Second

func subproofMemoryLimit(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "memory")
	if err != nil {
		return err
	}
	cgroupsPath := "/secondbox-gvisor-probe/" + area.containerID
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs:   []string{"hog", guestMarkerPath("memory")},
		Binds:       []bindMount{{Source: area.markerDir, Destination: guestMarkerMount, ReadOnly: false}},
		CgroupsPath: cgroupsPath,
		Resources: &ociResources{
			Memory: &ociMemory{Limit: memoryLimitBytes},
		},
	}); err != nil {
		return err
	}
	command := env.runscRun(area, "run")
	if err := command.Start(); err != nil {
		return fmt.Errorf("start runsc run: %w", err)
	}
	defer reapArea(env, area, command)

	if err := waitForFile(filepath.Join(area.markerDir, "memory"), bootDeadline); err != nil {
		return fmt.Errorf("hog sandbox did not boot: %w", err)
	}
	runErr := awaitExit(command, teardownDeadline+spinWallDuration)
	if runErr == nil {
		return fmt.Errorf("hog completed a 1 GiB allocation under a %d-byte limit", memoryLimitBytes)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return fmt.Errorf("hog sandbox failed without an exit status: %w", runErr)
	}
	emit(env.stdout, "cgroup-memory-limit", "passed",
		"limit_bytes="+strconv.Itoa(memoryLimitBytes),
		"outcome="+sanitizeValue(exitErr.String()))
	return nil
}

// awaitExit waits for the supervised command and preserves its exit error,
// unlike waitCommand which treats any exit as success.
func awaitExit(command *exec.Cmd, deadline time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(deadline):
		return fmt.Errorf("timed out after %s", deadline)
	}
}

// readCPUUsageMicros reads usage_usec from the container cgroup, following
// one level of runsc-created child cgroups if the leaf holds the tasks.
func readCPUUsageMicros(cgroupsPath string) (int64, error) {
	roots := []string{filepath.Join(cgroupMountPoint, cgroupsPath)}
	if children, err := filepath.Glob(filepath.Join(cgroupMountPoint, cgroupsPath, "*")); err == nil {
		roots = append(roots, children...)
	}
	var lastErr error = os.ErrNotExist
	best := int64(-1)
	for _, root := range roots {
		content, err := os.ReadFile(filepath.Join(root, "cpu.stat"))
		if err != nil {
			lastErr = err
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "usage_usec" {
				value, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil && value > best {
					best = value
				}
			}
		}
	}
	if best >= 0 {
		return best, nil
	}
	return 0, lastErr
}
