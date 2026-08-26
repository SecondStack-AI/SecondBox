package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func execCommandInNewMountNamespace(binary string, arguments ...string) *exec.Cmd {
	command := exec.Command(binary, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	return command
}

// proofPerformance records the Task 0H observations: thirty cold-start
// samples (start of runsc run to the guest's first observable write) and
// bounded workspace throughput through the gofer with directFS enabled and
// disabled. Observations only; the spike sets no performance gate.
func proofPerformance(env *probeEnv) error {
	base := filepath.Join(env.workDir, "performance")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	if err := subproofColdStarts(env, base); err != nil {
		return fmt.Errorf("cold-starts: %w", err)
	}
	if env.rootless {
		emit(env.stdout, "perf-workspace-io", "skipped", "reason=rootless_development_mode")
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	for _, directFS := range []bool{true, false} {
		command := execCommandInNewMountNamespace(self,
			"-runsc", env.runscPath,
			"-guest", env.guestPath,
			"-internal-perf-work", filepath.Join(base, fmt.Sprintf("io-directfs-%t", directFS)),
			"-internal-perf-directfs="+strconv.FormatBool(directFS),
		)
		command.Stdout = env.stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("workspace-io directfs=%t: %w", directFS, err)
		}
	}
	return nil
}

const coldStartSampleCount = 30

func subproofColdStarts(env *probeEnv, base string) error {
	samples := make([]int64, 0, coldStartSampleCount)
	for iteration := 0; iteration < coldStartSampleCount; iteration++ {
		area, err := newProofArea(env, base, fmt.Sprintf("cold-%02d", iteration))
		if err != nil {
			return err
		}
		if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
			GuestArgs: []string{"stay", guestMarkerPath("boot")},
			Binds:     []bindMount{{Source: area.markerDir, Destination: guestMarkerMount, ReadOnly: false}},
		}); err != nil {
			return err
		}
		command := env.runscRun(area, "run")
		start := time.Now()
		if err := command.Start(); err != nil {
			return fmt.Errorf("start sample %d: %w", iteration, err)
		}
		markerPath := filepath.Join(area.markerDir, "boot")
		if err := waitForFileFast(markerPath, bootDeadline); err != nil {
			reapArea(env, area, command)
			return fmt.Errorf("sample %d never booted: %w", iteration, err)
		}
		samples = append(samples, time.Since(start).Milliseconds())
		reapArea(env, area, command)
	}
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 := sorted[len(sorted)/2]
	p95 := sorted[(len(sorted)*95)/100]
	rendered := make([]string, len(samples))
	for i, sample := range samples {
		rendered[i] = strconv.FormatInt(sample, 10)
	}
	emit(env.stdout, "perf-cold-start", "passed",
		"samples="+strconv.Itoa(len(samples)),
		"p50_millis="+strconv.FormatInt(p50, 10),
		"p95_millis="+strconv.FormatInt(p95, 10),
		"min_millis="+strconv.FormatInt(sorted[0], 10),
		"max_millis="+strconv.FormatInt(sorted[len(sorted)-1], 10),
		"all_millis="+strings.Join(rendered, ","))
	return nil
}

// waitForFileFast polls on a tight cadence so boot samples carry little
// measurement quantization.
func waitForFileFast(path string, deadline time.Duration) error {
	expiry := time.Now().Add(deadline)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(expiry) {
			return fmt.Errorf("timed out waiting for %s", path)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// runPerfIOChild executes inside an unshared mount namespace: it attaches a
// fresh workspace image exactly as the workspace proof does and runs the
// guest iobench through runsc with the requested directFS mode.
func runPerfIOChild(runscPath, guestPath, base string, directFS bool) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private: %w", err)
	}
	env := &probeEnv{
		runscPath: runscPath,
		guestPath: guestPath,
		workDir:   base,
		stdout:    os.Stdout,
	}
	area, err := newProofArea(env, base, "io")
	if err != nil {
		return err
	}
	imagePath := filepath.Join(base, "io", "workspace.img")
	image, err := createExt4Image(imagePath, 512<<20, workspaceUUID)
	if err != nil {
		return err
	}
	defer image.Close()
	attachment, err := attachLoop(image)
	if err != nil {
		return err
	}
	mountPoint := filepath.Join(base, "io", "mnt")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	if err := syscall.Mount(attachment, mountPoint, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s: %w", attachment, err)
	}
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs: []string{"iobench", "/workspace/unused"},
		Binds:     []bindMount{{Source: mountPoint, Destination: "/workspace", ReadOnly: false}},
	}); err != nil {
		return err
	}
	command := env.runscRun(area, "run", "--directfs="+strconv.FormatBool(directFS))
	var stdout bytes.Buffer
	command.Stdout = &stdout
	defer reapArea(env, area, nil)
	if err := command.Run(); err != nil {
		return fmt.Errorf("iobench run: %w", err)
	}
	if err := detachClean(mountPoint, attachment); err != nil {
		return err
	}
	report := strings.TrimSpace(stdout.String())
	if !strings.Contains(report, "write_mib_s=") {
		return fmt.Errorf("iobench report missing: %q", report)
	}
	emit(env.stdout, "perf-workspace-io", "passed",
		"directfs="+strconv.FormatBool(directFS),
		strings.Fields(report)[0],
		strings.Fields(report)[1],
		strings.Fields(report)[2])
	return nil
}
