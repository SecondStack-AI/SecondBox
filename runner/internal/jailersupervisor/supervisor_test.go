package jailersupervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if handled, err := RunInvocation(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSupervisorWaitsForAndReapsOrphanedDescendant(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	jailerPath := filepath.Join(dir, "fake-jailer")
	script := "#!/bin/sh\nsleep 0.25 &\nprintf '%s\\n' \"$!\" > \"$1\"\nexit 0\n"
	if err := os.WriteFile(jailerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	supervisorEnvironment, err := CommandEnvironment(jailerPath, []string{pidPath})
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], InvocationArgument)
	command.Env = append(os.Environ(), supervisorEnvironment)
	started := time.Now()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("supervisor: %v\n%s", err, output)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("supervisor exited after %s without waiting for orphaned descendant", elapsed)
	}

	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Kill(pid, 0)
	if err == nil || !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("orphaned descendant pid %d remains after supervisor exit: %v", pid, err)
	}
}

func TestSupervisorCommandEnvironmentRejectsMissingExecutable(t *testing.T) {
	if _, err := CommandEnvironment(" ", nil); err == nil {
		t.Fatal("missing jailer executable was accepted")
	}
}
