package workspacestore

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"
)

func TestWorkspaceStorageObservationRealReflink(t *testing.T) {
	parent := os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM")
	if parent == "" {
		t.Skip("real reflink qualification filesystem must be explicit")
	}
	root, err := os.MkdirTemp(parent, "secondbox-storage-observation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	const capacity = int64(2 * 1024 * 1024 * 1024)
	store, err := New(t.Context(), Config{Root: root, FormatterKind: FormatterMke2fs, TemplateCapacityBytes: capacity})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{Mutation: testMutation("create-real", "real"), CapacityBytes: capacity}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Open(t.Context(), "real", 1)
	if err != nil {
		t.Fatal(err)
	}
	freshInfo, err := attachment.Descriptor().Stat()
	if err != nil {
		t.Fatal(err)
	}
	fresh := store.observeWorkspaceStorage("real")
	if fresh.AllocatedBytes == nil || *fresh.AllocatedBytes != freshInfo.Sys().(*syscall.Stat_t).Blocks*512 {
		t.Fatalf("fresh allocated observation differs from st_blocks: %+v", fresh)
	}
	if runtime.GOOS == "linux" {
		if fresh.ExclusiveBytes == nil {
			if fresh.ExclusiveReason != "exclusive_extents_encoded" {
				t.Fatalf("fresh exclusive observation: %+v", fresh)
			}
			t.Logf("fresh 2 GiB Workspace: allocatedBytes=%d exclusiveReason=%s", *fresh.AllocatedBytes, fresh.ExclusiveReason)
		} else {
			if *fresh.ExclusiveBytes > 1024*1024 {
				t.Fatalf("fresh reflink has too much exclusive allocation: %+v", fresh)
			}
			t.Logf("fresh 2 GiB Workspace: allocatedBytes=%d exclusiveBytes=%d", *fresh.AllocatedBytes, *fresh.ExclusiveBytes)
		}
	}
	if _, err := attachment.Descriptor().WriteAt([]byte("retained"), capacity-4096); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Descriptor().Sync(); err != nil {
		t.Fatal(err)
	}
	info, err := attachment.Descriptor().Stat()
	if err != nil {
		t.Fatal(err)
	}
	observed := store.observeWorkspaceStorage("real")
	if observed.AllocatedBytes == nil || *observed.AllocatedBytes != info.Sys().(*syscall.Stat_t).Blocks*512 || *observed.AllocatedBytes >= capacity {
		t.Fatalf("real sparse observation: %+v", observed)
	}
	if runtime.GOOS == "linux" {
		if observed.ExclusiveBytes == nil {
			if observed.ExclusiveReason != "exclusive_extents_encoded" {
				t.Fatalf("written exclusive observation: %+v", observed)
			}
		} else if fresh.ExclusiveBytes != nil && *observed.ExclusiveBytes < *fresh.ExclusiveBytes+4096 {
			t.Fatalf("write did not grow exclusive allocation: %+v", observed)
		}
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{Mutation: testMutation("snapshot-real", "real"), SnapshotID: "real-snapshot", ExpectedGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	shared := store.observeWorkspaceStorage("real")
	if shared.AllocatedBytes == nil || *shared.AllocatedBytes != *observed.AllocatedBytes {
		t.Fatalf("reflink changes are not unique-byte accounting: %+v", shared)
	}
	if runtime.GOOS == "linux" && (shared.ExclusiveBytes == nil || *shared.ExclusiveBytes != 0) {
		t.Fatalf("Snapshot must share all current image extents: %+v", shared)
	}
}

func TestWorkspaceStorageObservationPreservesActiveWriterAndSparseBytes(t *testing.T) {
	store, _, _ := newFakeStore(t)
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{Mutation: testMutation("create-observed", "observed"), CapacityBytes: minimumExt4Bytes}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Open(t.Context(), "observed", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Descriptor().WriteAt([]byte("retained files"), minimumExt4Bytes-4096); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Descriptor().Sync(); err != nil {
		t.Fatal(err)
	}
	info, err := attachment.Descriptor().Stat()
	if err != nil {
		t.Fatal(err)
	}
	want := info.Sys().(*syscall.Stat_t).Blocks * 512
	observations, err := store.ObserveStorage(t.Context(), 1)
	if err != nil || len(observations) != 1 {
		t.Fatalf("observation = %+v, %v", observations, err)
	}
	got := observations[0]
	if got.AllocatedBytes == nil || *got.AllocatedBytes != want || got.Generation != 1 || got.Reason != "" || got.ObservedAt.IsZero() {
		t.Fatalf("active observation = %+v, want allocated %d", got, want)
	}
	if _, err := store.Open(t.Context(), "observed", 1); !errors.Is(err, ErrActiveWriter) {
		t.Fatalf("observation released writer: %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	stopped := store.observeWorkspaceStorage("observed")
	if stopped.AllocatedBytes == nil || *stopped.AllocatedBytes != want {
		t.Fatalf("stopped observation = %+v", stopped)
	}
	if _, err := store.DeleteWorkspace(t.Context(), DeleteWorkspaceRequest{Mutation: testMutation("delete-observed", "observed"), ExpectedGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	missing := store.observeWorkspaceStorage("observed")
	if missing.Reason != "missing" || missing.AllocatedBytes != nil {
		t.Fatalf("deleted observation = %+v", missing)
	}
	for range 3 {
		if _, err := store.ObserveStorage(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceStorageObservationReportsMissingImageWithoutZero(t *testing.T) {
	store, _, _ := newFakeStore(t)
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{Mutation: testMutation("create-missing", "missing"), CapacityBytes: minimumExt4Bytes}); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.readCurrentManifest("missing")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.versionPath("missing", manifest.Image)); err != nil {
		t.Fatal(err)
	}
	observations, err := store.ObserveStorage(t.Context(), 1)
	if err != nil || len(observations) != 1 || observations[0].Reason != "missing" || observations[0].AllocatedBytes != nil {
		t.Fatalf("missing image = %+v, %v", observations, err)
	}
}
