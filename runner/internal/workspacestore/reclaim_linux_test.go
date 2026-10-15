//go:build linux

package workspacestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if before.AllocatedBytes == nil || before.ExclusiveBytes == nil ||
		after.AllocatedBytes == nil || after.ExclusiveBytes == nil || after.Generation != 3 {
		t.Fatalf("storage observations are incomplete: before=%+v after=%+v", before, after)
	}
	t.Logf(
		"generation advance reclaim: allocatedBytes %d -> %d, exclusiveBytes %d -> %d",
		*before.AllocatedBytes, *after.AllocatedBytes, *before.ExclusiveBytes, *after.ExclusiveBytes,
	)
	if released := *before.AllocatedBytes - *after.AllocatedBytes; released < 46<<20 {
		t.Fatalf("allocated bytes fell by %d, want the 48 MiB of freed blocks", released)
	}
	if released := *before.ExclusiveBytes - *after.ExclusiveBytes; released < 30<<20 {
		t.Fatalf("exclusive bytes fell by %d, want the 32 MiB exclusive scratch copies", released)
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
