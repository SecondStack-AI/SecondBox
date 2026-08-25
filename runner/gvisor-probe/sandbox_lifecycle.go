package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	bootDeadline     = 60 * time.Second
	teardownDeadline = 30 * time.Second
	gracefulDeadline = 30 * time.Second
)

// proofSandboxLifecycle proves the supervision model the backend design
// depends on: bundle boot with exit-code propagation, graceful stop through
// signal forwarding, parent-death teardown of the whole sandbox tree, the
// forced-kill path, and stale-state reconciliation.
func proofSandboxLifecycle(env *probeEnv) error {
	base := filepath.Join(env.workDir, "sandbox-lifecycle")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}

	if err := subproofBootExit(env, base); err != nil {
		return fmt.Errorf("boot-exit: %w", err)
	}
	if err := subproofGracefulStop(env, base); err != nil {
		return fmt.Errorf("graceful-stop: %w", err)
	}
	if err := subproofParentDeath(env, base); err != nil {
		return fmt.Errorf("parent-death: %w", err)
	}
	if err := subproofForcedKill(env, base); err != nil {
		return fmt.Errorf("forced-kill: %w", err)
	}
	return nil
}

// subproofBootExit boots a run-to-completion sandbox, requires the marker the
// guest wrote through a bind mount, and proves guest exit codes propagate
// through runsc to the supervising parent.
func subproofBootExit(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "boot")
	if err != nil {
		return err
	}
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs: []string{"hello", guestMarkerPath("boot")},
		Binds:     []bindMount{{Source: area.markerDir, Destination: guestMarkerMount, ReadOnly: false}},
	}); err != nil {
		return err
	}
	start := time.Now()
	command := env.runscRun(area, "run")
	if err := command.Run(); err != nil {
		return fmt.Errorf("runsc run: %w", err)
	}
	bootMillis := time.Since(start).Milliseconds()
	marker, err := os.ReadFile(filepath.Join(area.markerDir, "boot"))
	if err != nil {
		return fmt.Errorf("marker missing after exit: %w", err)
	}
	if !strings.Contains(string(marker), "hello") {
		return fmt.Errorf("marker content unexpected: %q", marker)
	}

	// A second sandbox exits nonzero; the supervisor must observe that code.
	exitArea, err := newProofArea(env, base, "exit-code")
	if err != nil {
		return err
	}
	if err := writeBundle(exitArea.bundleDir, env.guestPath, bundleConfig{
		GuestArgs: []string{"fail", guestMarkerPath("exit-code")},
		Binds:     []bindMount{{Source: exitArea.markerDir, Destination: guestMarkerMount, ReadOnly: false}},
	}); err != nil {
		return err
	}
	exitCommand := env.runscRun(exitArea, "run")
	runErr := exitCommand.Run()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != guestFailExitCode {
		return fmt.Errorf("expected guest exit code %d, got %v", guestFailExitCode, runErr)
	}

	emit(env.stdout, "sandbox-boot-exit", "passed",
		"boot_millis="+strconv.FormatInt(bootMillis, 10),
		"marker=ok",
		"propagated_exit_code="+strconv.Itoa(exitErr.ExitCode()))
	return nil
}

// subproofGracefulStop proves SIGTERM to the supervised runsc process is
// forwarded to the guest, which exits cleanly within the deadline.
func subproofGracefulStop(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "graceful")
	if err != nil {
		return err
	}
	command, err := startStaySandbox(env, area)
	if err != nil {
		return err
	}
	defer reapArea(env, area, command)

	start := time.Now()
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal runsc: %w", err)
	}
	if err := waitCommand(command, gracefulDeadline); err != nil {
		return fmt.Errorf("guest did not stop after SIGTERM: %w", err)
	}
	stopMillis := time.Since(start).Milliseconds()
	if err := waitProcessesGone(area.containerID, teardownDeadline); err != nil {
		return err
	}
	emit(env.stdout, "sandbox-graceful-stop", "passed",
		"stop_millis="+strconv.FormatInt(stopMillis, 10),
		"survivors=0")
	return nil
}

// subproofParentDeath launches the sandbox under an intermediate launcher
// process carrying the parent-death signal, kills only the launcher, and
// proves the entire sandbox tree exits, then reconciles the stale record.
func subproofParentDeath(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "parent-death")
	if err != nil {
		return err
	}
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs: []string{"stay", guestMarkerPath("parent-death")},
		Binds:     []bindMount{{Source: area.markerDir, Destination: guestMarkerMount, ReadOnly: false}},
	}); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	arguments := []string{
		"-runsc", env.runscPath,
		"-internal-launch-state-root", area.stateRoot,
		"-internal-launch-bundle", area.bundleDir,
		"-internal-launch-id", area.containerID,
	}
	if env.rootless {
		arguments = append(arguments, "-rootless")
	}
	launcher := exec.Command(self, arguments...)
	launcher.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	launcher.Stdout = os.Stderr
	launcher.Stderr = os.Stderr
	if err := launcher.Start(); err != nil {
		return fmt.Errorf("start launcher: %w", err)
	}
	defer reapArea(env, area, launcher)

	if err := waitForFile(filepath.Join(area.markerDir, "parent-death"), bootDeadline); err != nil {
		return fmt.Errorf("sandbox under launcher did not boot: %w", err)
	}
	start := time.Now()
	if err := launcher.Process.Kill(); err != nil {
		return fmt.Errorf("kill launcher: %w", err)
	}
	_ = launcher.Wait()
	if err := waitProcessesGone(area.containerID, teardownDeadline); err != nil {
		return fmt.Errorf("sandbox survived launcher death: %w", err)
	}
	teardownMillis := time.Since(start).Milliseconds()

	// The state directory retains a stale record; startup reconciliation must
	// be able to clear it and observe that it is gone.
	if err := env.runscAdmin(area, "delete", "-force", area.containerID).Run(); err != nil {
		return fmt.Errorf("delete stale record: %w", err)
	}
	if err := env.runscAdmin(area, "state", area.containerID).Run(); err == nil {
		return fmt.Errorf("stale record still present after delete")
	}
	emit(env.stdout, "sandbox-parent-death", "passed",
		"teardown_millis="+strconv.FormatInt(teardownMillis, 10),
		"survivors=0",
		"stale_record=reconciled")
	return nil
}

// subproofForcedKill records the deadline path separately: a process-group
// SIGKILL of a live sandbox followed by forced state reconciliation.
func subproofForcedKill(env *probeEnv, base string) error {
	area, err := newProofArea(env, base, "forced")
	if err != nil {
		return err
	}
	command, err := startStaySandbox(env, area)
	if err != nil {
		return err
	}
	defer reapArea(env, area, command)

	start := time.Now()
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill process group: %w", err)
	}
	_ = command.Wait()
	if err := waitProcessesGone(area.containerID, teardownDeadline); err != nil {
		return fmt.Errorf("sandbox survived process-group kill: %w", err)
	}
	killMillis := time.Since(start).Milliseconds()
	if err := env.runscAdmin(area, "delete", "-force", area.containerID).Run(); err != nil {
		return fmt.Errorf("delete stale record: %w", err)
	}
	emit(env.stdout, "sandbox-forced-kill", "passed",
		"kill_millis="+strconv.FormatInt(killMillis, 10),
		"survivors=0",
		"stale_record=reconciled")
	return nil
}

// runLauncher is the intermediate supervisor for the parent-death proof. It
// starts one sandbox with the parent-death signal armed and then waits; when
// this process is killed, the signal must take the sandbox down.
func runLauncher(runscPath string, rootless bool, stateRoot, bundleDir, containerID string) error {
	if runscPath == "" || bundleDir == "" || containerID == "" {
		return fmt.Errorf("launcher requires -runsc, -internal-launch-bundle, and -internal-launch-id")
	}
	arguments := runscBaseArguments(stateRoot, rootless)
	arguments = append(arguments, "run", "-bundle", bundleDir, containerID)
	command := exec.Command(runscPath, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	return command.Wait()
}

type proofArea struct {
	bundleDir   string
	markerDir   string
	stateRoot   string
	containerID string
}

const (
	guestMarkerMount  = "/probe-host"
	guestFailExitCode = 7
)

func guestMarkerPath(name string) string {
	return guestMarkerMount + "/" + name
}

func newProofArea(env *probeEnv, base, name string) (*proofArea, error) {
	area := &proofArea{
		bundleDir:   filepath.Join(base, name, "bundle"),
		markerDir:   filepath.Join(base, name, "marker"),
		stateRoot:   filepath.Join(base, name, "state"),
		containerID: fmt.Sprintf("secondbox-gvisor-probe-%s-%d", name, os.Getpid()),
	}
	for _, directory := range []string{area.bundleDir, area.markerDir, area.stateRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	return area, nil
}

func runscBaseArguments(stateRoot string, rootless bool) []string {
	arguments := []string{"--root", stateRoot, "--network=none", "--platform=systrap"}
	if rootless {
		arguments = append(arguments, "--rootless")
	}
	return arguments
}

// runscRun builds the supervised foreground run command for one proof area.
func (env *probeEnv) runscRun(area *proofArea, verb string) *exec.Cmd {
	arguments := runscBaseArguments(area.stateRoot, env.rootless)
	arguments = append(arguments, verb, "-bundle", area.bundleDir, area.containerID)
	command := exec.Command(env.runscPath, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	return command
}

// runscAdmin builds a state-management command (state, delete) for one area.
func (env *probeEnv) runscAdmin(area *proofArea, verbAndArguments ...string) *exec.Cmd {
	arguments := runscBaseArguments(area.stateRoot, env.rootless)
	arguments = append(arguments, verbAndArguments...)
	command := exec.Command(env.runscPath, arguments...)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	return command
}

func startStaySandbox(env *probeEnv, area *proofArea) (*exec.Cmd, error) {
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		GuestArgs: []string{"stay", guestMarkerPath("stay")},
		Binds:     []bindMount{{Source: area.markerDir, Destination: guestMarkerMount, ReadOnly: false}},
	}); err != nil {
		return nil, err
	}
	command := env.runscRun(area, "run")
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start runsc run: %w", err)
	}
	if err := waitForFile(filepath.Join(area.markerDir, "stay"), bootDeadline); err != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		return nil, fmt.Errorf("sandbox did not boot: %w", err)
	}
	return command, nil
}

// reapArea force-kills any leftover sandbox processes and clears state so a
// failed sub-proof cannot leak into the next one.
func reapArea(env *probeEnv, area *proofArea, command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
	}
	_ = env.runscAdmin(area, "delete", "-force", area.containerID).Run()
}

func waitForFile(path string, deadline time.Duration) error {
	expiry := time.Now().Add(deadline)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(expiry) {
			return fmt.Errorf("timed out waiting for %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitCommand(command *exec.Cmd, deadline time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err == nil || errors.As(err, &exitErr) {
			return nil
		}
		return err
	case <-time.After(deadline):
		return fmt.Errorf("timed out after %s", deadline)
	}
}

// waitProcessesGone polls /proc until no process command line references the
// container ID, proving sentry and gofer both exited.
func waitProcessesGone(containerID string, deadline time.Duration) error {
	expiry := time.Now().Add(deadline)
	for {
		survivors := processesReferencing(containerID)
		if len(survivors) == 0 {
			return nil
		}
		if time.Now().After(expiry) {
			return fmt.Errorf("processes still reference %s: %v", containerID, survivors)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func processesReferencing(token string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		commandLine, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.Contains(string(commandLine), token) {
			pids = append(pids, pid)
		}
	}
	return pids
}
