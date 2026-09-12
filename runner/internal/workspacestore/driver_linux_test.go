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

// TestWriterFenceSurvivesParentCloseWhileChildHoldsDescriptor proves the
// exact fence a compute backend relies on: a child inheriting the real
// Attachment's writer-lock descriptor keeps the exclusive Workspace fence
// after the parent attachment closes, so no replacement attachment succeeds
// until the child exits.
func TestWriterFenceSurvivesParentCloseWhileChildHoldsDescriptor(t *testing.T) {
	store, _, _ := newFakeStore(t)
	const workspaceID = "fence-inheritance"
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("fence-create", workspaceID), CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("cat")
	child.Stdin = stdin
	child.ExtraFiles = []*os.File{attachment.LockDescriptor()}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(t.Context(), workspaceID, 1); !errors.Is(err, ErrActiveWriter) {
		_ = stdinWriter.Close()
		_ = child.Wait()
		t.Fatalf("writer fence released while the child still held its inherited descriptor: %v", err)
	}
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("writer fence did not release after the last inherited descriptor closed: %v", err)
	}
	_ = replacement.Close()
}

// A Snapshot of a Workspace that a guest mounted carries a mount time newer
// than its last check, which makes tune2fs refuse a UUID rewrite. The clone
// path must check the exclusively owned copy and then rewrite.
func TestLinuxSetUUIDRewritesMountedSinceCheckFilesystem(t *testing.T) {
	for _, tool := range []string{"mke2fs", "tune2fs", "e2fsck", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	driver, err := newLinuxDriver(FormatterMke2fs, "")
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(t.TempDir(), "workspace.ext4")
	if output, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-O", "metadata_csum,^metadata_csum_seed", image, "16M").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs: %v: %s", err, output)
	}
	const rewritten = "3f0c3a1e-6c2d-4c1a-9c2e-0f1a2b3c4d5e"
	for _, test := range []struct {
		name         string
		mountedAfter bool
	}{{"fresh", false}, {"mounted since check", true}} {
		t.Run(test.name, func(t *testing.T) {
			if test.mountedAfter {
				// s_mtime is the last mount time; 2033 is later than any check time this test observes.
				if output, err := exec.Command("debugfs", "-w", "-R", "set_super_value mtime 2000000000", image).CombinedOutput(); err != nil {
					t.Fatalf("debugfs: %v: %s", err, output)
				}
				if output, err := exec.Command("tune2fs", "-U", rewritten, image).CombinedOutput(); err == nil || !strings.Contains(string(output), tune2fsFreshCheckRequired) {
					t.Fatalf("fixture does not reproduce the refusal: %v: %s", err, output)
				}
			}
			workspace, err := os.OpenFile(image, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer workspace.Close()
			if err := driver.SetUUID(t.Context(), workspace, rewritten); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("tune2fs", "-l", image).CombinedOutput()
			if err != nil || !strings.Contains(string(output), "Filesystem UUID:          "+rewritten) {
				t.Fatalf("UUID not rewritten: %v: %s", err, output)
			}
		})
	}
}
