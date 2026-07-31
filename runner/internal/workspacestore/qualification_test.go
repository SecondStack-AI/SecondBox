package workspacestore

import (
	"errors"
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
		Root:                  root,
		TemplateCapacityBytes: minimumExt4Bytes,
	})
	if err != nil {
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
	if _, err := attachment.Image().WriteAt([]byte{0x41}, testOffset); err != nil {
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
	if _, err := attachment.Image().WriteAt([]byte{0x42}, testOffset); err != nil {
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
	if _, err := store.SwapRestore(t.Context(), SwapRestoreRequest{
		Mutation:           restore.Mutation,
		SnapshotID:         restore.SnapshotID,
		ExpectedGeneration: restore.ExpectedGeneration,
		NextGeneration:     restore.NextGeneration,
	}); err != nil {
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
	if _, err := attachment.Image().ReadAt(restored, testOffset); err != nil {
		t.Fatal(err)
	}
	if restored[0] != 0x41 {
		t.Fatalf("restored byte = %#x, want Snapshot state 0x41", restored[0])
	}
	if _, err := attachment.Image().WriteAt([]byte{0x43}, testOffset); err != nil {
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
		Root:                  rootA,
		TemplateCapacityBytes: minimumExt4Bytes,
	})
	if err != nil {
		t.Fatalf("initialize runner %q WorkspaceStore: %v", runnerA, err)
	}
	storeB, err := New(t.Context(), Config{
		Root:                  rootB,
		TemplateCapacityBytes: minimumExt4Bytes,
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
