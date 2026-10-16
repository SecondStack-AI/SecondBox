//go:build linux

package workspacestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func qualifiedMke2fsStore(t *testing.T, capacity int64) *Store {
	t.Helper()
	parent := strings.TrimSpace(os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM"))
	if parent == "" {
		if os.Getenv("SECONDBOX_REQUIRE_WORKSPACESTORE_LINUX") == "1" {
			t.Fatal("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM is required")
		}
		t.Skip("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM names no reflink filesystem")
	}
	root, err := os.MkdirTemp(parent, ".secondbox-workspacestore-reclaim-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove WorkspaceStore reclaim root: %v", err)
		}
	})
	store, err := New(t.Context(), Config{
		Root: root, TemplateCapacityBytes: capacity, FormatterKind: FormatterMke2fs,
	})
	if err != nil {
		if errors.Is(err, ErrStorageIncompatible) && os.Getenv("SECONDBOX_REQUIRE_WORKSPACESTORE_LINUX") != "1" {
			t.Skipf("qualification filesystem does not support FICLONE: %v", err)
		}
		t.Fatalf("initialize qualified mke2fs WorkspaceStore: %v", err)
	}
	return store
}

func debugfsWriteFile(t *testing.T, image string, file ext4TestFile) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, file.data, 0o600); err != nil {
		t.Fatal(err)
	}
	debugfsWrite(t, image, "write "+source+" "+file.name)
}

func TestQualifiedGenerationAdvanceReclaimsFreedBlocksAndKeepsSnapshot(t *testing.T) {
	const (
		workspaceID = "reclaim-workspace"
		snapshotID  = "reclaim-snapshot"
		capacity    = int64(ext4TestImageBytes)
	)
	store := qualifiedMke2fsStore(t, capacity)
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("reclaim-create", workspaceID), CapacityBytes: capacity,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.readCurrentManifest(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.validatedVersionPath(workspaceID, manifest.Image)
	if err != nil {
		t.Fatal(err)
	}
	retained := randomExt4TestFile(t, "retained.bin", 4<<20)
	snapshotted := randomExt4TestFile(t, "snapshotted.bin", 48<<20)
	debugfsWriteFile(t, image, retained)
	debugfsWriteFile(t, image, snapshotted)
	advance := func(operationID string, generation uint64) {
		t.Helper()
		if _, err := store.AdvanceGeneration(t.Context(), AdvanceGenerationRequest{
			Mutation:           testMutation(operationID, workspaceID),
			ExpectedGeneration: generation, NextGeneration: generation + 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	advance("reclaim-stop-1", 1)
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:   testMutation("reclaim-snapshot", workspaceID),
		SnapshotID: snapshotID, ExpectedGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	snapshotImage := store.snapshotImagePath(snapshotID)
	snapshotAllocated := ext4TestAllocatedBytes(t, snapshotImage)

	// The guest deletes the Snapshot's file, then writes and deletes scratch
	// data. ext4 reuses the freed blocks, so the image holds 48 MiB of freed
	// blocks: the scratch writes' exclusive copies and the still-shared rest.
	debugfsWrite(t, image, "rm /snapshotted.bin")
	debugfsWriteFile(t, image, randomExt4TestFile(t, "scratch.bin", 32<<20))
	debugfsWrite(t, image, "rm /scratch.bin")
	before := store.observeWorkspaceStorage(workspaceID)
	advance("reclaim-stop-2", 2)
	after := store.observeWorkspaceStorage(workspaceID)
	if before.AllocatedBytes == nil || after.AllocatedBytes == nil || after.Generation != 3 {
		t.Fatalf("storage observations are incomplete: before=%+v after=%+v", before, after)
	}
	t.Logf("generation advance reclaim: allocatedBytes %d -> %d", *before.AllocatedBytes, *after.AllocatedBytes)
	if released := *before.AllocatedBytes - *after.AllocatedBytes; released < 46<<20 {
		t.Fatalf("allocated bytes fell by %d, want the 48 MiB of freed blocks", released)
	}
	// A compressing Btrfs encodes some extents, and the meter then reports no
	// exclusive bytes at all; the uncompressed (XFS, or Btrfs nocow or
	// nocompress) qualification roots measure them.
	switch {
	case before.ExclusiveBytes != nil && after.ExclusiveBytes != nil:
		t.Logf("generation advance reclaim: exclusiveBytes %d -> %d", *before.ExclusiveBytes, *after.ExclusiveBytes)
		if released := *before.ExclusiveBytes - *after.ExclusiveBytes; released < 30<<20 {
			t.Fatalf("exclusive bytes fell by %d, want the 32 MiB exclusive scratch copies", released)
		}
	case before.ExclusiveReason == "exclusive_extents_encoded" || after.ExclusiveReason == "exclusive_extents_encoded":
		t.Logf("exclusive bytes unavailable on an encoding filesystem: before=%s after=%s", before.ExclusiveReason, after.ExclusiveReason)
	default:
		t.Fatalf("exclusive bytes unavailable: before=%+v after=%+v", before, after)
	}
	requireCleanExt4(t, image)
	if !bytes.Equal(debugfsDump(t, image, "/retained.bin"), retained.data) {
		t.Fatal("retained Workspace file changed")
	}
	if allocated := ext4TestAllocatedBytes(t, snapshotImage); allocated != snapshotAllocated {
		t.Fatalf("Snapshot allocation changed from %d to %d", snapshotAllocated, allocated)
	}
	requireCleanExt4(t, snapshotImage)
	if !bytes.Equal(debugfsDump(t, snapshotImage, "/snapshotted.bin"), snapshotted.data) {
		t.Fatal("Snapshot lost the file the Workspace deleted")
	}

	const cloneID = "reclaim-clone"
	if _, err := store.CloneFromSnapshot(t.Context(), CloneWorkspaceRequest{
		Mutation:       testMutation("reclaim-clone", cloneID),
		SourceSnapshot: snapshotID, CapacityBytes: capacity,
	}); err != nil {
		t.Fatal(err)
	}
	cloneManifest, err := store.readCurrentManifest(cloneID)
	if err != nil {
		t.Fatal(err)
	}
	cloneImage, err := store.validatedVersionPath(cloneID, cloneManifest.Image)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(debugfsDump(t, cloneImage, "/snapshotted.bin"), snapshotted.data) {
		t.Fatal("Workspace cloned from the Snapshot lost its file")
	}
}

// TestQualifiedRangeAllocationCountsUnwrittenExtents covers the host state that
// SEEK_DATA reports as a hole although the meters charge it: an unwritten
// extent, as FALLOC_FL_ZERO_RANGE from a loop device's write-zeroes leaves.
func TestQualifiedRangeAllocationCountsUnwrittenExtents(t *testing.T) {
	parent := strings.TrimSpace(os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM"))
	if parent == "" {
		if os.Getenv("SECONDBOX_REQUIRE_WORKSPACESTORE_LINUX") == "1" {
			t.Fatal("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM is required")
		}
		t.Skip("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM names no reflink filesystem")
	}
	file, err := os.CreateTemp(parent, ".secondbox-workspacestore-unwritten-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(file.Name()) })
	defer file.Close()
	const length = 4 << 20
	if err := unix.Fallocate(int(file.Fd()), 0, 0, length); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Seek(int(file.Fd()), 0, unix.SEEK_DATA); !errors.Is(err, unix.ENXIO) {
		t.Logf("SEEK_DATA on an unwritten extent returned %v; the fixture still checks FIEMAP", err)
	}
	if allocated, err := platformRangeAllocated(file, 1<<20, 1<<20); err != nil || !allocated {
		t.Fatalf("unwritten extent allocation = %v, %v", allocated, err)
	}
	if err := platformPunchHole(file, 1<<20, 1<<20); err != nil {
		t.Fatal(err)
	}
	if allocated, err := platformRangeAllocated(file, 1<<20, 1<<20); err != nil || allocated {
		t.Fatalf("punched range allocation = %v, %v", allocated, err)
	}
	if allocated, err := platformRangeAllocated(file, 2<<20, 1<<20); err != nil || !allocated {
		t.Fatalf("remaining unwritten extent allocation = %v, %v", allocated, err)
	}
}
