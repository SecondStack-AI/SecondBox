package workspacestore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestQualifiedFilesystemProvidesRealCopyOnWriteIsolation(t *testing.T) {
	root := t.TempDir()
	if parent := strings.TrimSpace(
		os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM"),
	); parent != "" {
		if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent ||
			parent == string(filepath.Separator) {
			t.Fatalf("qualification filesystem %q is not a clean non-root absolute path", parent)
		}
		var err error
		root, err = os.MkdirTemp(parent, ".secondbox-workspacestore-qualification-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("remove WorkspaceStore qualification root: %v", err)
			}
		})
	}
	store, err := New(t.Context(), Config{
		Root:                         root,
		TemplateCapacityBytes:        minimumExt4Bytes,
		FormatterKind:                FormatterMicrosandboxHelper,
		MicrosandboxHelperExecutable: strings.TrimSpace(os.Getenv("SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE")),
	})
	if err != nil {
		if os.Getenv("SECONDBOX_REQUIRE_WORKSPACESTORE_LINUX") == "1" {
			t.Fatalf("qualified Linux WorkspaceStore is required: %v", err)
		}
		if errors.Is(err, ErrStorageIncompatible) {
			t.Skipf("qualification filesystem does not support FICLONE: %v", err)
		}
		t.Skipf("qualification prerequisites are unavailable: %v", err)
	}
	const (
		workspaceID = "qualification-workspace"
		snapshotID  = "qualification-snapshot"
		capacity    = int64(minimumExt4Bytes)
		testOffset  = capacity - 1
	)
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation:      testMutation("qualification-create", workspaceID),
		CapacityBytes: capacity,
	}); err != nil {
		t.Fatalf("create qualified Workspace: %v", err)
	}
	attachment, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(root, "qualified-compute-link.ext4")
	if err := attachment.LinkInto(linkedPath); err != nil {
		t.Fatalf("link qualified attachment into compute-private root: %v", err)
	}
	linkedInfo, err := os.Stat(linkedPath)
	if err != nil {
		t.Fatal(err)
	}
	descriptorInfo, err := attachment.Descriptor().Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(linkedInfo, descriptorInfo) {
		t.Fatal("compute-private link does not identify the held Workspace descriptor")
	}
	if _, err := attachment.Descriptor().WriteAt([]byte{0x41}, testOffset); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("qualification-snapshot", workspaceID),
		SnapshotID:         snapshotID,
		ExpectedGeneration: 1,
	}); err != nil {
		t.Fatalf("create qualified Snapshot: %v", err)
	}
	activeInfo, err := os.Stat(
		store.versionPath(workspaceID, generationImageName(1, "create")),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshotInfo, err := os.Stat(store.snapshotImagePath(snapshotID))
	if err != nil {
		t.Fatal(err)
	}
	activeStat, activeOK := activeInfo.Sys().(*syscall.Stat_t)
	snapshotStat, snapshotOK := snapshotInfo.Sys().(*syscall.Stat_t)
	if !activeOK || !snapshotOK ||
		int64(activeStat.Blocks)*512 >= capacity ||
		int64(snapshotStat.Blocks)*512 >= capacity {
		t.Fatalf(
			"qualified reflinks are not sparse: active=%#v Snapshot=%#v",
			activeInfo.Sys(),
			snapshotInfo.Sys(),
		)
	}
	attachment, err = store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Descriptor().WriteAt([]byte{0x42}, testOffset); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.Open(store.snapshotImagePath(snapshotID))
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	actual := make([]byte, 1)
	if _, err := snapshot.ReadAt(actual, testOffset); err != nil {
		t.Fatal(err)
	}
	if actual[0] != 0x41 {
		t.Fatalf("Snapshot byte changed through active image mutation: %#x", actual[0])
	}
	restore := PrepareRestoreRequest{
		Mutation:           testMutation("qualification-restore", workspaceID),
		SnapshotID:         snapshotID,
		ExpectedGeneration: 1,
		NextGeneration:     2,
	}
	if _, err := store.PrepareRestore(t.Context(), restore); err != nil {
		t.Fatalf("prepare qualified restore: %v", err)
	}
	if _, err := store.SwapRestore(t.Context(), SwapRestoreRequest(restore)); err != nil {
		t.Fatalf("swap qualified restore: %v", err)
	}
	if _, err := store.FinalizeRestore(
		t.Context(),
		RestoreMutation{Mutation: restore.Mutation},
	); err != nil {
		t.Fatalf("finalize qualified restore: %v", err)
	}
	attachment, err = store.Open(t.Context(), workspaceID, 2)
	if err != nil {
		t.Fatal(err)
	}
	restored := make([]byte, 1)
	if _, err := attachment.Descriptor().ReadAt(restored, testOffset); err != nil {
		t.Fatal(err)
	}
	if restored[0] != 0x41 {
		t.Fatalf("restored byte = %#x, want Snapshot state 0x41", restored[0])
	}
	if _, err := attachment.Descriptor().WriteAt([]byte{0x43}, testOffset); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.ReadAt(actual, testOffset); err != nil {
		t.Fatal(err)
	}
	if actual[0] != 0x41 {
		t.Fatalf("Snapshot byte changed through restored-image mutation: %#x", actual[0])
	}

	targetRoot := root + "-relocation-target"
	t.Cleanup(func() { _ = os.RemoveAll(targetRoot) })
	target, err := New(t.Context(), Config{
		Root: targetRoot, TemplateCapacityBytes: capacity,
		FormatterKind:                FormatterMicrosandboxHelper,
		MicrosandboxHelperExecutable: strings.TrimSpace(os.Getenv("SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE")),
	})
	if err != nil {
		t.Fatalf("initialize qualified relocation target: %v", err)
	}
	export, err := store.OpenRelocationExport(t.Context(), RelocationExportRequest{
		Mutation: testMutation("qualification-relocation", workspaceID), ExpectedGeneration: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer export.Close()
	importer, err := target.BeginRelocationImport(t.Context(), RelocationImportRequest{
		Mutation:   testMutation("qualification-relocation", workspaceID),
		Generation: 2, CapacityBytes: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	var offset uint64
	for {
		count, readErr := export.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			if err := importer.WriteChunk(offset, chunk); err != nil {
				t.Fatal(err)
			}
			_, _ = hash.Write(chunk)
			offset += uint64(count)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	if _, err := importer.Complete(offset, "sha256:"+hex.EncodeToString(hash.Sum(nil))); err != nil {
		t.Fatal(err)
	}
	relocated, err := target.Open(t.Context(), workspaceID, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer relocated.Close()
	relocatedByte := []byte{0}
	if _, err := relocated.Descriptor().ReadAt(relocatedByte, testOffset); err != nil {
		t.Fatal(err)
	}
	if relocatedByte[0] != 0x43 {
		t.Fatalf("relocated byte = %#x, want 0x43", relocatedByte[0])
	}
	relocatedInfo, err := relocated.Descriptor().Stat()
	if err != nil {
		t.Fatal(err)
	}
	relocatedStat, ok := relocatedInfo.Sys().(*syscall.Stat_t)
	if !ok || int64(relocatedStat.Blocks)*512 >= capacity {
		t.Fatalf("qualified relocation did not preserve sparse allocation: %#v", relocatedInfo.Sys())
	}
}

func TestQualifiedTwoRunnerRootsAreDistinctAndIsolated(t *testing.T) {
	parentA := strings.TrimSpace(os.Getenv("SECONDBOX_MULTIRUNNER_FILESYSTEM_A"))
	parentB := strings.TrimSpace(os.Getenv("SECONDBOX_MULTIRUNNER_FILESYSTEM_B"))
	runnerA := strings.TrimSpace(os.Getenv("SECONDBOX_MULTIRUNNER_RUNNER_A_ID"))
	runnerB := strings.TrimSpace(os.Getenv("SECONDBOX_MULTIRUNNER_RUNNER_B_ID"))
	required := os.Getenv("SECONDBOX_REQUIRE_QUALIFIED_MULTIRUNNER") == "1"
	if parentA == "" || parentB == "" || runnerA == "" || runnerB == "" {
		if required {
			t.Fatal("qualified multi-runner test requires both stable runner IDs and filesystem roots")
		}
		t.Skip("qualified multi-runner filesystem fixture is not configured")
	}
	if runnerA == runnerB {
		t.Fatal("qualified multi-runner fixture uses the same stable runner identity twice")
	}
	for name, parent := range map[string]string{"A": parentA, "B": parentB} {
		if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent ||
			parent == string(filepath.Separator) {
			t.Fatalf("runner %s qualification filesystem %q is not a clean non-root absolute path", name, parent)
		}
		if err := os.MkdirAll(parent, privateDirectoryMode); err != nil {
			t.Fatalf("create runner %s qualification filesystem: %v", name, err)
		}
	}
	resolvedA, err := filepath.EvalSymlinks(parentA)
	if err != nil {
		t.Fatal(err)
	}
	resolvedB, err := filepath.EvalSymlinks(parentB)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedA == resolvedB {
		t.Fatal("qualified multi-runner fixture resolves both workspace roots to the same path")
	}
	rootA, err := os.MkdirTemp(parentA, ".secondbox-multirunner-a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(rootA); err != nil {
			t.Errorf("remove runner A qualification root: %v", err)
		}
	})
	rootB, err := os.MkdirTemp(parentB, ".secondbox-multirunner-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(rootB); err != nil {
			t.Errorf("remove runner B qualification root: %v", err)
		}
	})
	storeA, err := New(t.Context(), Config{
		Root:                         rootA,
		TemplateCapacityBytes:        minimumExt4Bytes,
		FormatterKind:                FormatterMicrosandboxHelper,
		MicrosandboxHelperExecutable: strings.TrimSpace(os.Getenv("SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE")),
	})
	if err != nil {
		t.Fatalf("initialize runner %q WorkspaceStore: %v", runnerA, err)
	}
	storeB, err := New(t.Context(), Config{
		Root:                         rootB,
		TemplateCapacityBytes:        minimumExt4Bytes,
		FormatterKind:                FormatterMicrosandboxHelper,
		MicrosandboxHelperExecutable: strings.TrimSpace(os.Getenv("SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE")),
	})
	if err != nil {
		t.Fatalf("initialize runner %q WorkspaceStore: %v", runnerB, err)
	}
	const capacity = int64(minimumExt4Bytes)
	if _, err := storeA.Create(t.Context(), CreateWorkspaceRequest{
		Mutation:      testMutation("qualification-create-a", "workspace-a"),
		CapacityBytes: capacity,
	}); err != nil {
		t.Fatalf("create runner A Workspace: %v", err)
	}
	if _, err := storeB.Create(t.Context(), CreateWorkspaceRequest{
		Mutation:      testMutation("qualification-create-b", "workspace-b"),
		CapacityBytes: capacity,
	}); err != nil {
		t.Fatalf("create runner B Workspace: %v", err)
	}
	reportA, err := storeA.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reportB, err := storeB.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(reportA.Workspaces) != 1 ||
		reportA.Workspaces[0].WorkspaceID != "workspace-a" ||
		len(reportB.Workspaces) != 1 ||
		reportB.Workspaces[0].WorkspaceID != "workspace-b" {
		t.Fatalf("qualified roots are not isolated: A=%#v B=%#v", reportA, reportB)
	}
}
