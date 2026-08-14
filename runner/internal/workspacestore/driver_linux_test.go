//go:build linux

package workspacestore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxFormatterCompositionIsExplicit(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"mke2fs", "tune2fs", "e2fsck", "microsandbox-helper"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)

	if _, err := newLinuxDriver("", ""); err == nil || !strings.Contains(err.Error(), "formatter kind") {
		t.Fatalf("missing formatter kind error = %v", err)
	}
	if _, err := newLinuxDriver(FormatterMke2fs, ""); err != nil {
		t.Fatalf("compose Firecracker formatter without Microsandbox helper: %v", err)
	}
	if _, err := newLinuxDriver(
		FormatterMicrosandboxHelper,
		filepath.Join(bin, "microsandbox-helper"),
	); err != nil {
		t.Fatalf("compose Microsandbox helper formatter: %v", err)
	}
}

func TestLinuxAttachmentIsOpaqueAndCarriesPortableIdentity(t *testing.T) {
	store, _, _ := newFakeStore(t)
	const workspaceID = "attachment-identity"
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("attachment-create", workspaceID), CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Close()
	if attachment.Descriptor().Name() != "workspace" {
		t.Fatalf("attachment leaked backing path %q", attachment.Descriptor().Name())
	}
	if attachment.StableBlockID() != "workspace" || attachment.CapacityBytes() != minimumExt4Bytes ||
		attachment.FilesystemUUID() != deterministicUUID(workspaceID) ||
		attachment.ChildDescriptorPath(4) != "/proc/self/fd/4" ||
		attachment.ChildDescriptorPath(2) != "" {
		t.Fatalf("attachment metadata is incomplete")
	}
}

func TestLinuxFlockExcludesOtherProcessesAndReleasesOnCrash(t *testing.T) {
	store, _, _ := newFakeStore(t)
	const workspaceID = "cross-process-lock"
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("cross-process-create", workspaceID), CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(store.locksRoot(), workspaceID+".lock")
	command := workspaceLockProbeCommand(t, lockPath, "probe")
	err = command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("other process acquired active writer lock: %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	command = workspaceLockProbeCommand(t, lockPath, "crash")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("crashing lock owner: %v: %s", err, output)
	}
	attachment, err = store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("lock was not released by process exit: %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
}

func workspaceLockProbeCommand(t *testing.T, lockPath, mode string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestWorkspaceLockProbeProcess$")
	command.Env = append(os.Environ(),
		"SECONDBOX_WORKSPACESTORE_LOCK_PROBE=1",
		"SECONDBOX_WORKSPACESTORE_LOCK_PATH="+lockPath,
		"SECONDBOX_WORKSPACESTORE_LOCK_MODE="+mode,
	)
	return command
}

func TestWorkspaceLockProbeProcess(t *testing.T) {
	if os.Getenv("SECONDBOX_WORKSPACESTORE_LOCK_PROBE") != "1" {
		t.Skip("subprocess only")
	}
	file, err := os.OpenFile(os.Getenv("SECONDBOX_WORKSPACESTORE_LOCK_PATH"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := platformTryLock(file); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Fatal(err)
		}
		os.Exit(42)
	}
	if os.Getenv("SECONDBOX_WORKSPACESTORE_LOCK_MODE") != "crash" {
		t.Fatal("unexpected lock acquisition")
	}
	os.Exit(0)
}
