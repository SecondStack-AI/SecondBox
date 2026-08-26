//go:build darwin

package workspacestore

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinDriverUsesAPFSCloneLocksAndDescriptorIdentity(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.ext4")
	source, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.Truncate(minimumExt4Bytes); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteAt([]byte("source"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := source.Chmod(0o400); err != nil {
		t.Fatal(err)
	}
	destinationPath := filepath.Join(root, "destination.ext4")
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	driver := darwinDriver{}
	if err := driver.Clone(destination, source); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.WriteAt([]byte("clone!"), 0); err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len("source"))
	if _, err := source.ReadAt(actual, 0); err != nil {
		t.Fatal(err)
	}
	if string(actual) != "source" {
		t.Fatalf("source mutated through APFS clone: %q", actual)
	}
	if err := source.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []*os.File{source, destination} {
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int64(stat.Blocks)*512 >= minimumExt4Bytes {
			t.Fatalf("APFS clone is not sparse: %#v", info.Sys())
		}
	}

	attachment, err := platformOpenAttachment(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Close()
	if attachment.Name() != "workspace" || attachment.Fd() < 10 {
		t.Fatalf("unexpected opaque attachment: name=%q fd=%d", attachment.Name(), attachment.Fd())
	}
	linkedPath := filepath.Join(root, "linked.ext4")
	if err := platformLinkDescriptor(attachment, linkedPath); err != nil {
		t.Fatal(err)
	}
	linkedInfo, err := os.Stat(linkedPath)
	if err != nil {
		t.Fatal(err)
	}
	attachmentInfo, err := attachment.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(linkedInfo, attachmentInfo) {
		t.Fatal("Darwin descriptor link changed attachment identity")
	}
	if platformChildDescriptorPath(4) != "/dev/fd/4" || platformChildDescriptorPath(2) != "" {
		t.Fatal("Darwin child descriptor path is invalid")
	}

	lockA, err := os.OpenFile(filepath.Join(root, "writer.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockA.Close()
	lockB, err := os.OpenFile(lockA.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lockB.Close()
	if err := platformTryLock(lockA); err != nil {
		t.Fatal(err)
	}
	if err := platformTryLock(lockB); !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("second writer lock = %v", err)
	}
	if err := lockA.Close(); err != nil {
		t.Fatal(err)
	}
	if err := platformTryLock(lockB); err != nil {
		t.Fatalf("writer lock did not release when its last descriptor closed: %v", err)
	}
	if err := platformSyncDirectory(root); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinDriverRejectsFirecrackerFormatter(t *testing.T) {
	if _, err := newPlatformDriver(FormatterMke2fs, ""); err == nil {
		t.Fatal("Darwin WorkspaceStore accepted Firecracker formatter")
	}
}

func TestDarwinSparseCompactionPreservesBytes(t *testing.T) {
	file, err := os.OpenFile(
		filepath.Join(t.TempDir(), "allocated.ext4"),
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zeros := make([]byte, 1<<20)
	for offset := int64(0); offset < minimumExt4Bytes; offset += int64(len(zeros)) {
		if _, err := file.WriteAt(zeros, offset); err != nil {
			t.Fatal(err)
		}
	}
	markers := map[int64]byte{0: 0x42, minimumExt4Bytes - 1: 0x24}
	for offset, marker := range markers {
		if _, err := file.WriteAt([]byte{marker}, offset); err != nil {
			t.Fatal(err)
		}
	}
	if err := compactDarwinSparseFile(file, minimumExt4Bytes); err != nil {
		t.Fatal(err)
	}
	for offset, expected := range markers {
		actual := []byte{0}
		if _, err := file.ReadAt(actual, offset); err != nil {
			t.Fatal(err)
		}
		if actual[0] != expected {
			t.Fatalf("byte at %d = %#x, want %#x", offset, actual[0], expected)
		}
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Blocks)*512 >= minimumExt4Bytes {
		t.Fatalf("APFS sparse compaction did not release zero extents: %#v", info.Sys())
	}
}
