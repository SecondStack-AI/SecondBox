//go:build linux

package gvisor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for the runner binary's supervisor
// dispatch, so StartMountSupervisor can re-execute this process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == MountSupervisorInvocation {
		if err := RunMountSupervisor(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 3 && os.Args[1] == netTargetsInvocation {
		if err := runNetworkTargets(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const qualificationEnvironment = "SECONDBOX_GVISOR_QUALIFICATION"

// requireAttachmentQualification gates the real-host attachment suite: it
// needs root for loop devices and ext4 mounts and is driven by the
// test-gvisor recipe, which fails clearly when prerequisites are absent.
func requireAttachmentQualification(t *testing.T) {
	t.Helper()
	if os.Getenv(qualificationEnvironment) == "" {
		t.Skipf("%s is not set", qualificationEnvironment)
	}
	if os.Geteuid() != 0 {
		t.Fatalf("%s is set but the suite is not running as root", qualificationEnvironment)
	}
	for _, tool := range []string{"mkfs.ext4", "e2fsck", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("attachment qualification requires %s", tool)
		}
	}
}

const qualificationUUID = "31e40cd4-5f5a-4b54-a06e-0123456789ab"

func createQualificationImage(t *testing.T, sizeBytes int64) (string, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workspace.img")
	image, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := image.Truncate(sizeBytes); err != nil {
		t.Fatal(err)
	}
	format := exec.Command("mkfs.ext4", "-F", "-q", "-U", qualificationUUID, path)
	if output, err := format.CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, bytes.TrimSpace(output))
	}
	t.Cleanup(func() { _ = image.Close() })
	return path, image
}

func debugfsContent(t *testing.T, imagePath, filePath string) string {
	t.Helper()
	command := exec.Command("debugfs", "-R", "cat "+filePath, imagePath)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("debugfs cat %s: %v", filePath, err)
	}
	return string(output)
}

// TestAttachmentHoldRoundTripPreservesContentAndIdentity proves the
// supervised attach/detach cycle: pre-seeded content survives, the read/write
// probe runs and cleans up, identity never changes, and the strict release
// order leaves a clean image.
func TestAttachmentHoldRoundTripPreservesContentAndIdentity(t *testing.T) {
	requireAttachmentQualification(t)
	imagePath, image := createQualificationImage(t, 128<<20)

	// Pre-seed a marker through a direct host mount before any supervisor.
	seedMount := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(seedMount, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := exec.Command("mount", "-o", "loop", imagePath, seedMount)
	if output, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed mount: %v: %s", err, bytes.TrimSpace(output))
	}
	if err := os.WriteFile(filepath.Join(seedMount, "seed-marker"), []byte("seeded-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("umount", seedMount).CombinedOutput(); err != nil {
		t.Fatalf("seed umount: %v: %s", err, bytes.TrimSpace(output))
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writerLock := qualificationWriterLock(t)
	handles, err := StartMountSupervisor(self, MountSupervisorPlan{
		Mountpoint:    filepath.Join(t.TempDir(), "mnt"),
		ExpectedUUID:  qualificationUUID,
		CapacityBytes: 128 << 20,
		Hold:          true,
	}, image, writerLock)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := handles.ReadStatusLine()
	if err != nil {
		t.Fatalf("read ready status: %v", err)
	}
	if ready.Kind != "ready" || ready.Fields["rw_probe"] != "ok" || ready.Fields["loop_device"] == "" {
		t.Fatalf("ready status = %#v", ready)
	}
	if _, err := handles.Control.Write([]byte{controlTerminate}); err != nil {
		t.Fatal(err)
	}
	detached, err := handles.ReadStatusLine()
	if err != nil {
		t.Fatalf("read detached status: %v", err)
	}
	if detached.Kind != "detached" {
		t.Fatalf("detached status = %#v", detached)
	}
	if err := handles.Command.Wait(); err != nil {
		t.Fatalf("supervisor exit: %v", err)
	}
	if err := handles.CloseParentSide(); err != nil {
		t.Fatal(err)
	}

	if content := debugfsContent(t, imagePath, "/seed-marker"); !strings.Contains(content, "seeded-content") {
		t.Fatalf("seed marker after detach = %q", content)
	}
	check := exec.Command("e2fsck", "-fn", imagePath)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("e2fsck after detach: %v: %s", err, bytes.TrimSpace(output))
	}
	candidates, err := scanStaleLoops("/sys/block", "/dev", filepath.Dir(imagePath))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("loop devices survived clean detach: %#v", candidates)
	}
}

// TestAttachmentSupervisorCrashAutoclearsAndRecovers proves the crash path:
// a force-killed supervisor leaks no loop device (autoclear), the image
// recovers through journal replay, and startup reconciliation finds nothing
// left to clean.
func TestAttachmentSupervisorCrashAutoclearsAndRecovers(t *testing.T) {
	requireAttachmentQualification(t)
	imagePath, image := createQualificationImage(t, 128<<20)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writerLock := qualificationWriterLock(t)
	handles, err := StartMountSupervisor(self, MountSupervisorPlan{
		Mountpoint:    filepath.Join(t.TempDir(), "mnt"),
		ExpectedUUID:  qualificationUUID,
		CapacityBytes: 128 << 20,
		Hold:          true,
	}, image, writerLock)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := handles.ReadStatusLine(); err != nil || ready.Kind != "ready" {
		t.Fatalf("ready = %#v err=%v", ready, err)
	}
	if err := syscall.Kill(-handles.Command.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = handles.Command.Wait()
	_ = handles.CloseParentSide()

	deadline := time.Now().Add(10 * time.Second)
	for {
		candidates, err := scanStaleLoops("/sys/block", "/dev", filepath.Dir(imagePath))
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("loop devices survived supervisor crash: %#v", candidates)
		}
		time.Sleep(100 * time.Millisecond)
	}
	repair := exec.Command("e2fsck", "-fp", imagePath)
	if output, err := repair.CombinedOutput(); err != nil {
		// e2fsck exit 1 means errors were corrected; journal replay after a
		// force-killed writer is exactly that.
		exitErr, isExit := err.(*exec.ExitError)
		if !isExit || exitErr.ExitCode() > 1 {
			t.Fatalf("e2fsck after crash: %v: %s", err, bytes.TrimSpace(output))
		}
	}
	reconciled, err := ReconcileStaleLoops(filepath.Dir(imagePath))
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 0 {
		t.Fatalf("reconciliation found devices autoclear should have removed: %#v", reconciled)
	}
}

// TestAttachmentRejectsWrongIdentity proves declared-identity validation
// fails the attachment before any mount exists.
func TestAttachmentRejectsWrongIdentity(t *testing.T) {
	requireAttachmentQualification(t)
	_, image := createQualificationImage(t, 128<<20)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writerLock := qualificationWriterLock(t)
	handles, err := StartMountSupervisor(self, MountSupervisorPlan{
		Mountpoint:    filepath.Join(t.TempDir(), "mnt"),
		ExpectedUUID:  "00000000-0000-0000-0000-000000000000",
		CapacityBytes: 128 << 20,
		Hold:          true,
	}, image, writerLock)
	if err != nil {
		t.Fatal(err)
	}
	status, err := handles.ReadStatusLine()
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status.Kind != "terminal" || status.Fields["outcome"] != "supervisor-failure" ||
		!strings.Contains(status.Fields["detail"], "UUID") {
		t.Fatalf("wrong-identity status = %#v", status)
	}
	_ = handles.Command.Wait()
	_ = handles.CloseParentSide()
}

// qualificationWriterLock stands in for the attachment's writer-lock
// descriptor: the supervisor only holds it, so any open file proves the
// inheritance plumbing.
func qualificationWriterLock(t *testing.T) *os.File {
	t.Helper()
	lock, err := os.CreateTemp(t.TempDir(), "writer-lock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}
