package workspacestore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
)

type fakeFormatter struct {
	mu       sync.Mutex
	uuids    []string
	setUUIDs []string
	err      error
	setErr   error
}

func (formatter *fakeFormatter) Format(_ context.Context, path string, uuid string) error {
	formatter.mu.Lock()
	defer formatter.mu.Unlock()
	if formatter.err != nil {
		return formatter.err
	}
	formatter.uuids = append(formatter.uuids, uuid)
	return writeFakeExt4Identity(path, uuid)
}

func (formatter *fakeFormatter) SetUUID(_ context.Context, path string, uuid string) error {
	formatter.mu.Lock()
	defer formatter.mu.Unlock()
	if formatter.setErr != nil {
		return formatter.setErr
	}
	formatter.setUUIDs = append(formatter.setUUIDs, uuid)
	return writeFakeExt4Identity(path, uuid)
}

func writeFakeExt4Identity(path string, uuid string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte{0x53, 0xef}, ext4MagicOffset); err != nil {
		return err
	}
	decoded, err := decodeUUID(uuid)
	if err != nil {
		return err
	}
	if _, err := file.WriteAt(decoded, ext4UUIDOffset); err != nil {
		return err
	}
	return file.Sync()
}

type fakeCloner struct {
	mu    sync.Mutex
	calls int
	err   error
}

func testMutation(operationID string, workspaceID string) Mutation {
	return Mutation{
		OperationID: operationID, WorkspaceID: workspaceID,
		FencingToken: []byte("01234567890123456789012345678901"),
	}
}

func (cloner *fakeCloner) Clone(destination *os.File, source *os.File) error {
	cloner.mu.Lock()
	defer cloner.mu.Unlock()
	cloner.calls++
	if cloner.err != nil {
		return cloner.err
	}
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if err := destination.Truncate(info.Size()); err != nil {
		return err
	}
	copyBytes := info.Size()
	if copyBytes > 8192 {
		copyBytes = 8192
	}
	content := make([]byte, copyBytes)
	if _, err := source.ReadAt(content, 0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(content) > 0 {
		if _, err := destination.WriteAt(content, 0); err != nil {
			return err
		}
	}
	return nil
}

func TestWorkspaceTemplatePrewarmReusesOneImmutableFilesystem(t *testing.T) {
	const capacity = int64(minimumExt4Bytes)
	root := t.TempDir()
	cloner := &fakeCloner{}
	formatter := &fakeFormatter{}
	store, err := newStore(Config{
		Root:                  root,
		TemplateCapacityBytes: capacity,
	}, cloner, formatter)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	templatePath := store.ext4TemplatePath(capacity)
	if err := validateExt4Template(
		templatePath,
		capacity,
		deterministicTemplateUUID(capacity),
	); err != nil {
		t.Fatalf("validate prewarmed template: %v", err)
	}
	if len(formatter.uuids) != 1 || len(formatter.setUUIDs) != 0 {
		t.Fatalf("prewarm formatter activity = format %#v, rewrite %#v", formatter.uuids, formatter.setUUIDs)
	}

	for index, workspaceID := range []string{"workspace-template-a", "workspace-template-b"} {
		if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
			Mutation:      testMutation("operation-template-"+strconv.Itoa(index), workspaceID),
			CapacityBytes: capacity,
		}); err != nil {
			t.Fatalf("create %s: %v", workspaceID, err)
		}
		if err := store.validateImage(
			workspaceID,
			generationImageName(1, "create"),
			capacity,
		); err != nil {
			t.Fatalf("validate %s identity: %v", workspaceID, err)
		}
	}
	if len(formatter.uuids) != 1 || len(formatter.setUUIDs) != 2 {
		t.Fatalf("template reuse formatter activity = format %#v, rewrite %#v", formatter.uuids, formatter.setUUIDs)
	}
	if cloner.calls != 3 {
		t.Fatalf("clone calls after probe and two template children = %d, want 3", cloner.calls)
	}

	first, err := store.Open(t.Context(), "workspace-template-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Descriptor().WriteAt([]byte{0x7f}, capacity-1); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Open(t.Context(), "workspace-template-b", 1)
	if err != nil {
		t.Fatal(err)
	}
	actual := []byte{0xff}
	if _, err := second.Descriptor().ReadAt(actual, capacity-1); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if actual[0] != 0 {
		t.Fatalf("template children share mutations: got %#x", actual[0])
	}
	if err := validateExt4Template(
		templatePath,
		capacity,
		deterministicTemplateUUID(capacity),
	); err != nil {
		t.Fatalf("template changed after child UUID rewrites: %v", err)
	}

	restarted, err := newStore(Config{
		Root:                  root,
		TemplateCapacityBytes: capacity,
	}, cloner, formatter)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.initialize(t.Context()); err != nil {
		t.Fatalf("reuse template after restart: %v", err)
	}
	if len(formatter.uuids) != 1 {
		t.Fatalf("restart reformatted template: %#v", formatter.uuids)
	}

	if err := os.Chmod(templatePath, writableImageMode); err != nil {
		t.Fatal(err)
	}
	template, err := os.OpenFile(templatePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := template.WriteAt(make([]byte, 16), ext4UUIDOffset); err != nil {
		t.Fatal(err)
	}
	if err := template.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := template.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(templatePath, snapshotImageMode); err != nil {
		t.Fatal(err)
	}
	corruptRestart, err := newStore(Config{
		Root:                  root,
		TemplateCapacityBytes: capacity,
	}, cloner, formatter)
	if err != nil {
		t.Fatal(err)
	}
	if err := corruptRestart.initialize(t.Context()); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("corrupt template restart error = %v", err)
	}
}

func TestConcurrentWorkspaceCreatesCoalesceLazyTemplateGeneration(t *testing.T) {
	store, cloner, formatter := newFakeStore(t)
	const createCount = 8
	var wait sync.WaitGroup
	errorsByIndex := make([]error, createCount)
	for index := range createCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			workspaceID := "workspace-concurrent-" + strconv.Itoa(index)
			_, errorsByIndex[index] = store.Create(t.Context(), CreateWorkspaceRequest{
				Mutation:      testMutation("operation-concurrent-"+strconv.Itoa(index), workspaceID),
				CapacityBytes: minimumExt4Bytes,
			})
		}()
	}
	wait.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("concurrent create %d: %v", index, err)
		}
	}
	if len(formatter.uuids) != 1 {
		t.Fatalf("concurrent creates formatted %d templates, want 1", len(formatter.uuids))
	}
	if len(formatter.setUUIDs) != createCount {
		t.Fatalf("concurrent creates rewrote %d UUIDs, want %d", len(formatter.setUUIDs), createCount)
	}
	if cloner.calls != createCount+1 {
		t.Fatalf("concurrent clone calls = %d, want %d", cloner.calls, createCount+1)
	}
}

func newFakeStore(t *testing.T) (*Store, *fakeCloner, *fakeFormatter) {
	t.Helper()
	cloner := &fakeCloner{}
	formatter := &fakeFormatter{}
	store, err := newStore(Config{Root: t.TempDir()}, cloner, formatter)
	if err != nil {
		t.Fatalf("new WorkspaceStore: %v", err)
	}
	if err := store.initialize(t.Context()); err != nil {
		t.Fatalf("initialize WorkspaceStore: %v", err)
	}
	return store, cloner, formatter
}

func TestWorkspaceLifecycleIsIdempotentAndCrashRecoverable(t *testing.T) {
	store, cloner, formatter := newFakeStore(t)
	const (
		workspaceID = "workspace-one"
		snapshotID  = "snapshot-one"
		capacity    = int64(minimumExt4Bytes)
	)
	create := CreateWorkspaceRequest{
		Mutation:      testMutation("operation-create", workspaceID),
		CapacityBytes: capacity,
	}
	created, err := store.Create(t.Context(), create)
	if err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	if created.Generation != 1 || created.CapacityBytes != capacity {
		t.Fatalf("create receipt = %#v", created)
	}
	if len(formatter.uuids) != 1 || formatter.uuids[0] != deterministicTemplateUUID(capacity) {
		t.Fatalf("formatter UUIDs = %#v", formatter.uuids)
	}
	if len(formatter.setUUIDs) != 1 || formatter.setUUIDs[0] != deterministicUUID(workspaceID) {
		t.Fatalf("rewritten UUIDs = %#v", formatter.setUUIDs)
	}
	imageInfo, err := os.Stat(store.versionPath(workspaceID, generationImageName(1, "create")))
	if err != nil {
		t.Fatalf("stat sparse image: %v", err)
	}
	if stat, ok := imageInfo.Sys().(*syscall.Stat_t); !ok ||
		int64(stat.Blocks)*512 >= imageInfo.Size() {
		t.Fatalf("formatted image is not sparse: %#v", imageInfo.Sys())
	}
	if replayed, err := store.Create(t.Context(), create); err != nil || replayed != created {
		t.Fatalf("replayed create = %#v, %v", replayed, err)
	}
	conflicting := create
	conflicting.CapacityBytes += 4096
	if _, err := store.Create(t.Context(), conflicting); !errors.Is(err, ErrConflictingReplay) {
		t.Fatalf("conflicting create error = %v", err)
	}
	staleFence := create
	staleFence.FencingToken = []byte("abcdefghijklmnopqrstuvwxyz012345")
	if _, err := store.Create(t.Context(), staleFence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale-fence create error = %v", err)
	}

	attachment, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("open Workspace: %v", err)
	}
	if attachment.Handle().WorkspaceID() != workspaceID ||
		attachment.Handle().Generation() != 1 {
		t.Fatalf("opaque attachment = %#v", attachment.Handle())
	}
	if _, err := attachment.Descriptor().WriteAt([]byte("A"), 4096); err != nil {
		t.Fatalf("write state A: %v", err)
	}
	activeReport, err := store.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("reconcile active Workspace writer: %v", err)
	}
	if len(activeReport.Workspaces) != 1 ||
		!activeReport.Workspaces[0].ActiveWriter ||
		len(activeReport.Receipts) != 1 ||
		activeReport.Receipts[0].Kind != ReceiptWorkspaceCreate {
		t.Fatalf("active Workspace reconciliation = %#v", activeReport)
	}
	snapshotCreate := CreateSnapshotRequest{
		Mutation:           testMutation("operation-snapshot", workspaceID),
		SnapshotID:         snapshotID,
		ExpectedGeneration: 1,
	}
	if _, err := store.CreateSnapshot(t.Context(), snapshotCreate); !errors.Is(err, ErrActiveWriter) {
		t.Fatalf("Snapshot with active writer error = %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("close Workspace: %v", err)
	}
	snapshotReceipt, err := store.CreateSnapshot(t.Context(), snapshotCreate)
	if err != nil {
		t.Fatalf("create Snapshot: %v", err)
	}
	if snapshotReceipt.SnapshotID != snapshotID || cloner.calls < 2 {
		t.Fatalf("Snapshot receipt = %#v, clone calls = %d", snapshotReceipt, cloner.calls)
	}
	if mode := mustStat(t, store.snapshotImagePath(snapshotID)).Mode().Perm(); mode != snapshotImageMode {
		t.Fatalf("Snapshot mode = %o", mode)
	}

	attachment, err = store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("reopen Workspace: %v", err)
	}
	if _, err := attachment.Descriptor().WriteAt([]byte("B"), 4096); err != nil {
		t.Fatalf("write state B: %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("close state B Workspace: %v", err)
	}
	advance := AdvanceGenerationRequest{
		Mutation:           testMutation("operation-stop", workspaceID),
		ExpectedGeneration: 1,
		NextGeneration:     2,
	}
	if _, err := store.AdvanceGeneration(t.Context(), advance); err != nil {
		t.Fatalf("advance generation: %v", err)
	}
	if err := os.Remove(store.receiptPath(advance.Mutation, ReceiptGenerationAdvance)); err != nil {
		t.Fatalf("remove advance receipt for crash simulation: %v", err)
	}
	if receipt, err := store.AdvanceGeneration(t.Context(), advance); err != nil ||
		receipt.Generation != 2 {
		t.Fatalf("recover advanced manifest = %#v, %v", receipt, err)
	}

	prepare := PrepareRestoreRequest{
		Mutation:           testMutation("operation-restore", workspaceID),
		SnapshotID:         snapshotID,
		ExpectedGeneration: 2,
		NextGeneration:     3,
	}
	if _, err := store.PrepareRestore(t.Context(), prepare); err != nil {
		t.Fatalf("prepare restore: %v", err)
	}
	if cloner.calls != 4 {
		t.Fatalf(
			"WorkspaceStore clone calls after probe, template, Snapshot, and restore = %d, want 4",
			cloner.calls,
		)
	}
	if _, err := store.DeleteSnapshot(t.Context(), DeleteSnapshotRequest{
		Mutation:   testMutation("operation-delete-in-use", workspaceID),
		SnapshotID: snapshotID,
	}); !errors.Is(err, ErrSnapshotInUse) {
		t.Fatalf("delete in-use Snapshot error = %v", err)
	}
	swap := SwapRestoreRequest(prepare)
	if _, err := store.SwapRestore(t.Context(), swap); err != nil {
		t.Fatalf("swap restore: %v", err)
	}
	if err := os.Remove(store.receiptPath(swap.Mutation, ReceiptRestoreSwap)); err != nil {
		t.Fatalf("remove swap receipt for crash simulation: %v", err)
	}
	if receipt, err := store.SwapRestore(t.Context(), swap); err != nil ||
		receipt.Generation != 3 {
		t.Fatalf("recover swapped manifest = %#v, %v", receipt, err)
	}
	if _, err := store.AbortRestore(
		t.Context(),
		RestoreMutation{Mutation: prepare.Mutation},
	); !errors.Is(err, ErrRestorePending) {
		t.Fatalf("abort after durable swap error = %v", err)
	}
	if _, err := os.Stat(store.rollbackPath(workspaceID, prepare.OperationID)); err != nil {
		t.Fatalf("swapped restore rollback evidence was not preserved: %v", err)
	}
	finalize := RestoreMutation{Mutation: prepare.Mutation}
	if _, err := store.FinalizeRestore(t.Context(), finalize); err != nil {
		t.Fatalf("finalize restore: %v", err)
	}
	if err := os.Remove(store.receiptPath(finalize.Mutation, ReceiptRestoreFinalize)); err != nil {
		t.Fatalf("remove finalize receipt for crash simulation: %v", err)
	}
	if receipt, err := store.FinalizeRestore(t.Context(), finalize); err != nil ||
		receipt.Generation != 3 {
		t.Fatalf("recover finalized restore = %#v, %v", receipt, err)
	}

	attachment, err = store.Open(t.Context(), workspaceID, 3)
	if err != nil {
		t.Fatalf("open restored Workspace: %v", err)
	}
	state := make([]byte, 1)
	if _, err := attachment.Descriptor().ReadAt(state, 4096); err != nil {
		t.Fatalf("read restored state: %v", err)
	}
	if string(state) != "A" {
		t.Fatalf("restored state = %q, want A", state)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("close restored Workspace: %v", err)
	}
	if _, err := os.Stat(store.versionPath(workspaceID, generationImageName(1, "create"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-restore image still exists: %v", err)
	}
	snapshots, err := os.ReadDir(store.snapshotsRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Name() != snapshotID {
		t.Fatalf("restore created implicit safety Snapshots: %#v", snapshots)
	}
	for _, directory := range []string{
		store.stagedDir(workspaceID),
		store.rollbackDir(workspaceID),
	} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("finalized restore retained temporary state in %q: %#v", directory, entries)
		}
	}

	restarted, err := newStore(Config{Root: store.root}, cloner, formatter)
	if err != nil {
		t.Fatalf("reconstruct WorkspaceStore: %v", err)
	}
	if err := restarted.initialize(t.Context()); err != nil {
		t.Fatalf("restart WorkspaceStore: %v", err)
	}
	report, err := restarted.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("reconcile WorkspaceStore: %v", err)
	}
	if len(report.Workspaces) != 1 || report.Workspaces[0].Generation != 3 ||
		report.Workspaces[0].RestorePending {
		t.Fatalf("reconcile report = %#v", report)
	}
	if len(report.Receipts) < 6 {
		t.Fatalf("reconciled receipts = %#v", report.Receipts)
	}

	deleteSnapshot := DeleteSnapshotRequest{
		Mutation:   testMutation("operation-delete-snapshot", workspaceID),
		SnapshotID: snapshotID,
	}
	if _, err := restarted.DeleteSnapshot(t.Context(), deleteSnapshot); err != nil {
		t.Fatalf("delete Snapshot: %v", err)
	}
	if _, err := restarted.DeleteSnapshot(t.Context(), deleteSnapshot); err != nil {
		t.Fatalf("replay delete Snapshot: %v", err)
	}
	deleteWorkspace := DeleteWorkspaceRequest{
		Mutation:           testMutation("operation-delete-workspace", workspaceID),
		ExpectedGeneration: 3,
	}
	if _, err := restarted.DeleteWorkspace(t.Context(), deleteWorkspace); err != nil {
		t.Fatalf("delete Workspace: %v", err)
	}
	if _, err := restarted.DeleteWorkspace(t.Context(), deleteWorkspace); err != nil {
		t.Fatalf("replay delete Workspace: %v", err)
	}
	if _, err := restarted.Inspect(t.Context(), workspaceID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("inspect deleted Workspace error = %v", err)
	}
	report, err = restarted.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("reconcile deleted Workspace: %v", err)
	}
	if len(report.Workspaces) != 0 ||
		len(report.Receipts) != 1 ||
		report.Receipts[0].Kind != ReceiptWorkspaceDelete ||
		report.Receipts[0].OperationID != deleteWorkspace.OperationID {
		t.Fatalf("deleted Workspace reconciliation = %#v", report)
	}
}

func TestCloneWorkspaceFromSnapshotCreatesIndependentGenerationOne(t *testing.T) {
	store, cloner, formatter := newFakeStore(t)
	const (
		sourceWorkspaceID = "workspace-clone-source"
		targetWorkspaceID = "workspace-clone-target"
		snapshotID        = "snapshot-clone-source"
		capacity          = int64(minimumExt4Bytes)
	)
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation:      testMutation("operation-create-source", sourceWorkspaceID),
		CapacityBytes: capacity,
	}); err != nil {
		t.Fatalf("create source Workspace: %v", err)
	}
	source, err := store.Open(t.Context(), sourceWorkspaceID, 1)
	if err != nil {
		t.Fatalf("open source Workspace: %v", err)
	}
	if _, err := source.Descriptor().WriteAt([]byte("portable-workspace"), 4096); err != nil {
		t.Fatalf("write source Workspace: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close source Workspace: %v", err)
	}
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("operation-snapshot-source", sourceWorkspaceID),
		SnapshotID:         snapshotID,
		ExpectedGeneration: 1,
	}); err != nil {
		t.Fatalf("create source Snapshot: %v", err)
	}

	request := CloneWorkspaceRequest{
		Mutation:       testMutation("operation-clone-target", targetWorkspaceID),
		SourceSnapshot: snapshotID,
		CapacityBytes:  capacity,
	}
	created, err := store.CloneFromSnapshot(t.Context(), request)
	if err != nil {
		t.Fatalf("clone target Workspace: %v", err)
	}
	if created.Generation != 1 || created.CapacityBytes != capacity {
		t.Fatalf("clone receipt = %#v", created)
	}
	if replayed, err := store.CloneFromSnapshot(t.Context(), request); err != nil || replayed != created {
		t.Fatalf("replayed clone = %#v, %v", replayed, err)
	}
	target, err := store.Open(t.Context(), targetWorkspaceID, 1)
	if err != nil {
		t.Fatalf("open target Workspace: %v", err)
	}
	defer target.Close()
	got := make([]byte, len("portable-workspace"))
	if _, err := target.Descriptor().ReadAt(got, 4096); err != nil {
		t.Fatalf("read target Workspace: %v", err)
	}
	if string(got) != "portable-workspace" {
		t.Fatalf("target Workspace content = %q", got)
	}
	if len(formatter.uuids) != 1 {
		t.Fatalf("clone formatted a second filesystem: UUIDs = %#v", formatter.uuids)
	}
	if len(formatter.setUUIDs) != 2 ||
		formatter.setUUIDs[1] != deterministicUUID(targetWorkspaceID) {
		t.Fatalf("clone UUID rewrites = %#v", formatter.setUUIDs)
	}
	if cloner.calls != 4 {
		t.Fatalf("clone calls after probe, template, Snapshot, and target clone = %d", cloner.calls)
	}
}

func TestRunnerRestartReplaysEveryLocalWorkspaceOperation(t *testing.T) {
	const (
		workspaceID = "workspace-restart"
		snapshotID  = "snapshot-restart"
		capacity    = int64(minimumExt4Bytes)
	)
	createRequest := CreateWorkspaceRequest{
		Mutation:      testMutation("operation-create", workspaceID),
		CapacityBytes: capacity,
	}
	createWorkspace := func(t *testing.T, store *Store) {
		t.Helper()
		if _, err := store.Create(t.Context(), createRequest); err != nil {
			t.Fatalf("create Workspace prerequisite: %v", err)
		}
	}
	createSnapshot := func(t *testing.T, store *Store) CreateSnapshotRequest {
		t.Helper()
		request := CreateSnapshotRequest{
			Mutation:           testMutation("operation-snapshot-create", workspaceID),
			SnapshotID:         snapshotID,
			ExpectedGeneration: 1,
		}
		if _, err := store.CreateSnapshot(t.Context(), request); err != nil {
			t.Fatalf("create Snapshot prerequisite: %v", err)
		}
		return request
	}
	advanceGeneration := func(t *testing.T, store *Store) AdvanceGenerationRequest {
		t.Helper()
		request := AdvanceGenerationRequest{
			Mutation:           testMutation("operation-advance", workspaceID),
			ExpectedGeneration: 1,
			NextGeneration:     2,
		}
		if _, err := store.AdvanceGeneration(t.Context(), request); err != nil {
			t.Fatalf("advance generation prerequisite: %v", err)
		}
		return request
	}
	prepareRestore := func(t *testing.T, store *Store) PrepareRestoreRequest {
		t.Helper()
		createSnapshot(t, store)
		advanceGeneration(t, store)
		request := PrepareRestoreRequest{
			Mutation:           testMutation("operation-restore", workspaceID),
			SnapshotID:         snapshotID,
			ExpectedGeneration: 2,
			NextGeneration:     3,
		}
		if _, err := store.PrepareRestore(t.Context(), request); err != nil {
			t.Fatalf("prepare restore prerequisite: %v", err)
		}
		return request
	}
	restart := func(t *testing.T, store *Store, cloner *fakeCloner, formatter *fakeFormatter) *Store {
		t.Helper()
		restarted, err := newStore(Config{Root: store.root}, cloner, formatter)
		if err != nil {
			t.Fatalf("reconstruct WorkspaceStore: %v", err)
		}
		if err := restarted.initialize(t.Context()); err != nil {
			t.Fatalf("restart WorkspaceStore: %v", err)
		}
		return restarted
	}
	assertReplay := func(t *testing.T, first Receipt, replayed Receipt, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("replay after Runner restart: %v", err)
		}
		if replayed != first {
			t.Fatalf("receipt changed across Runner restart:\nfirst  %#v\nreplay %#v", first, replayed)
		}
	}

	t.Run("create", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		first, err := store.Create(t.Context(), createRequest)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.Create(t.Context(), createRequest)
		assertReplay(t, first, replayed, err)
	})
	t.Run("inspect", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		restarted := restart(t, store, cloner, formatter)
		inspection, err := restarted.Inspect(t.Context(), workspaceID)
		if err != nil || inspection.Generation != 1 ||
			inspection.CapacityBytes != capacity || !inspection.Formatted {
			t.Fatalf("inspect after Runner restart = %#v, %v", inspection, err)
		}
	})
	t.Run("generation advance", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		request := AdvanceGenerationRequest{
			Mutation:           testMutation("operation-advance", workspaceID),
			ExpectedGeneration: 1,
			NextGeneration:     2,
		}
		first, err := store.AdvanceGeneration(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.AdvanceGeneration(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("Snapshot create", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		request := CreateSnapshotRequest{
			Mutation:           testMutation("operation-snapshot-create", workspaceID),
			SnapshotID:         snapshotID,
			ExpectedGeneration: 1,
		}
		first, err := store.CreateSnapshot(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.CreateSnapshot(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("Snapshot delete", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		createSnapshot(t, store)
		request := DeleteSnapshotRequest{
			Mutation:   testMutation("operation-snapshot-delete", workspaceID),
			SnapshotID: snapshotID,
		}
		first, err := store.DeleteSnapshot(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.DeleteSnapshot(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("restore prepare", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		createSnapshot(t, store)
		advanceGeneration(t, store)
		request := PrepareRestoreRequest{
			Mutation:           testMutation("operation-restore", workspaceID),
			SnapshotID:         snapshotID,
			ExpectedGeneration: 2,
			NextGeneration:     3,
		}
		first, err := store.PrepareRestore(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.PrepareRestore(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("restore swap", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		prepare := prepareRestore(t, store)
		request := SwapRestoreRequest(prepare)
		first, err := store.SwapRestore(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.SwapRestore(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("restore finalize", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		prepare := prepareRestore(t, store)
		if _, err := store.SwapRestore(t.Context(), SwapRestoreRequest(prepare)); err != nil {
			t.Fatal(err)
		}
		request := RestoreMutation{Mutation: prepare.Mutation}
		first, err := store.FinalizeRestore(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.FinalizeRestore(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("restore abort", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		prepare := prepareRestore(t, store)
		request := RestoreMutation{Mutation: prepare.Mutation}
		first, err := store.AbortRestore(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.AbortRestore(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("Workspace delete", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		request := DeleteWorkspaceRequest{
			Mutation:           testMutation("operation-workspace-delete", workspaceID),
			ExpectedGeneration: 1,
		}
		first, err := store.DeleteWorkspace(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		restarted := restart(t, store, cloner, formatter)
		replayed, err := restarted.DeleteWorkspace(t.Context(), request)
		assertReplay(t, first, replayed, err)
	})
	t.Run("reconcile", func(t *testing.T) {
		store, cloner, formatter := newFakeStore(t)
		createWorkspace(t, store)
		restarted := restart(t, store, cloner, formatter)
		report, err := restarted.Reconcile(t.Context())
		if err != nil || len(report.Workspaces) != 1 ||
			report.Workspaces[0].WorkspaceID != workspaceID ||
			len(report.Receipts) != 1 ||
			report.Receipts[0].Kind != ReceiptWorkspaceCreate {
			t.Fatalf("reconcile after Runner restart = %#v, %v", report, err)
		}
	})
}

func TestWorkspaceStoreRejectsPathsUnsupportedCloneAndDiskFull(t *testing.T) {
	store, cloner, formatter := newFakeStore(t)
	for _, id := range []string{"", ".", "..", "../escape", "nested/path", "with space"} {
		if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
			Mutation:      testMutation("operation", id),
			CapacityBytes: minimumExt4Bytes,
		}); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Workspace ID %q error = %v", id, err)
		}
	}
	uuidFailure := CreateWorkspaceRequest{
		Mutation:      testMutation("uuid-failure", "workspace-uuid-failure"),
		CapacityBytes: minimumExt4Bytes,
	}
	formatter.setErr = io.ErrUnexpectedEOF
	if _, err := store.Create(t.Context(), uuidFailure); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("UUID rewrite error = %v", err)
	}
	if _, err := os.Stat(store.versionPath(
		uuidFailure.WorkspaceID,
		generationImageName(1, "create"),
	)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed UUID rewrite published a Workspace image: %v", err)
	}
	formatter.setErr = nil
	if _, err := store.Create(t.Context(), uuidFailure); err != nil {
		t.Fatalf("retry UUID rewrite: %v", err)
	}
	create := CreateWorkspaceRequest{
		Mutation:      testMutation("create", "workspace"),
		CapacityBytes: minimumExt4Bytes,
	}
	if _, err := store.Create(t.Context(), create); err != nil {
		t.Fatalf("create Workspace: %v", err)
	}
	cloner.err = syscall.EOPNOTSUPP
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("unsupported", "workspace"),
		SnapshotID:         "unsupported",
		ExpectedGeneration: 1,
	}); !errors.Is(err, syscall.EOPNOTSUPP) || !errors.Is(err, ErrStorageIncompatible) {
		t.Fatalf("unsupported FICLONE error = %v", err)
	}
	if _, err := os.Stat(store.snapshotImagePath("unsupported")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported FICLONE published an image: %v", err)
	}
	cloner.err = nil
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("restore-source", "workspace"),
		SnapshotID:         "restore-source",
		ExpectedGeneration: 1,
	}); err != nil {
		t.Fatalf("create restore source Snapshot: %v", err)
	}
	cloner.err = syscall.EOPNOTSUPP
	if _, err := store.PrepareRestore(t.Context(), PrepareRestoreRequest{
		Mutation:           testMutation("unsupported-restore", "workspace"),
		SnapshotID:         "restore-source",
		ExpectedGeneration: 1,
		NextGeneration:     2,
	}); !errors.Is(err, syscall.EOPNOTSUPP) || !errors.Is(err, ErrStorageIncompatible) {
		t.Fatalf("unsupported restore FICLONE error = %v", err)
	}
	if _, err := os.Stat(
		store.stagedImagePath("workspace", "unsupported-restore"),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported restore FICLONE published a staged image: %v", err)
	}
	cloner.err = syscall.ENOSPC
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("disk-full", "workspace"),
		SnapshotID:         "disk-full",
		ExpectedGeneration: 1,
	}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("disk-full FICLONE error = %v", err)
	}
}

func TestWorkspaceCreateRecoversWhenReceiptDirectoryPreparationFails(t *testing.T) {
	store, _, _ := newFakeStore(t)
	request := CreateWorkspaceRequest{
		Mutation:      testMutation("receipt-directory-failure", "workspace-receipt-directory-failure"),
		CapacityBytes: minimumExt4Bytes,
	}
	workspaceReceiptPath := filepath.Join(store.receiptsRoot(), request.WorkspaceID)
	if err := os.Mkdir(workspaceReceiptPath, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), request); err == nil {
		t.Fatal("Workspace create succeeded with an invalid receipt directory")
	}
	inspection, err := store.Inspect(t.Context(), request.WorkspaceID)
	if err != nil || inspection.Generation != 1 {
		t.Fatalf("durable Workspace after receipt preparation failure = %#v, %v", inspection, err)
	}
	if _, err := os.Stat(store.receiptPath(request.Mutation, ReceiptWorkspaceCreate)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed receipt preparation published success evidence: %v", err)
	}
	if err := os.Chmod(workspaceReceiptPath, privateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	report, err := store.Reconcile(t.Context())
	if err != nil || len(report.Workspaces) != 1 || len(report.Receipts) != 0 {
		t.Fatalf("reconcile non-authoritative receipt directory = %#v, %v", report, err)
	}
	receipt, err := store.Create(t.Context(), request)
	if err != nil || receipt.Generation != 1 {
		t.Fatalf("recover Workspace create after receipt preparation failure = %#v, %v", receipt, err)
	}
}

func TestWorkspaceStoreProbeFailsClosed(t *testing.T) {
	root := t.TempDir()
	store, err := newStore(
		Config{Root: root},
		&fakeCloner{err: syscall.EOPNOTSUPP},
		&fakeFormatter{},
	)
	if err != nil {
		t.Fatalf("new WorkspaceStore: %v", err)
	}
	if err := store.initialize(t.Context()); !errors.Is(err, ErrStorageIncompatible) {
		t.Fatalf("unsupported root error = %v", err)
	}
	for _, root := range []string{"relative", "/", filepath.Clean(root) + string(os.PathSeparator) + "."} {
		if _, err := newStore(Config{Root: root}, &fakeCloner{}, &fakeFormatter{}); err == nil {
			t.Fatalf("invalid root %q was accepted", root)
		}
	}
}

func TestAbortRestoreLeavesCurrentImageUnchanged(t *testing.T) {
	store, _, _ := newFakeStore(t)
	create := CreateWorkspaceRequest{
		Mutation:      testMutation("create", "workspace"),
		CapacityBytes: minimumExt4Bytes,
	}
	if _, err := store.Create(t.Context(), create); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("snapshot", "workspace"),
		SnapshotID:         "snapshot",
		ExpectedGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	prepare := PrepareRestoreRequest{
		Mutation:           testMutation("restore", "workspace"),
		SnapshotID:         "snapshot",
		ExpectedGeneration: 1,
		NextGeneration:     2,
	}
	if _, err := store.PrepareRestore(t.Context(), prepare); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AbortRestore(
		t.Context(),
		RestoreMutation{Mutation: prepare.Mutation},
	); err != nil {
		t.Fatalf("abort restore: %v", err)
	}
	inspection, err := store.Inspect(t.Context(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Generation != 1 || inspection.RestorePending {
		t.Fatalf("post-abort inspection = %#v", inspection)
	}
}

func TestRestoreRecoversEveryLocalPublishAndReceiptBoundary(t *testing.T) {
	store, cloner, _ := newFakeStore(t)
	const (
		workspaceID = "workspace-boundaries"
		snapshotID  = "snapshot-boundaries"
	)
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation:      testMutation("create-boundaries", workspaceID),
		CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("snapshot-boundaries", workspaceID),
		SnapshotID:         snapshotID,
		ExpectedGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	prepare := PrepareRestoreRequest{
		Mutation:           testMutation("restore-boundaries", workspaceID),
		SnapshotID:         snapshotID,
		ExpectedGeneration: 1,
		NextGeneration:     2,
	}
	if _, err := store.PrepareRestore(t.Context(), prepare); err != nil {
		t.Fatal(err)
	}
	cloneCallsAfterPrepare := cloner.calls
	if err := os.Remove(
		store.receiptPath(prepare.Mutation, ReceiptRestorePrepare),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(
		store.stagedManifestPath(workspaceID, prepare.OperationID),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareRestore(t.Context(), prepare); err != nil {
		t.Fatalf("recover staged image published before manifest/receipt: %v", err)
	}
	if cloner.calls != cloneCallsAfterPrepare {
		t.Fatalf(
			"prepare recovery recloned an image: calls=%d want=%d",
			cloner.calls,
			cloneCallsAfterPrepare,
		)
	}
	staged, err := store.readStagedRestore(workspaceID, prepare.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := store.versionPath(workspaceID, staged.Image)
	if err := os.Rename(
		store.stagedImagePath(workspaceID, prepare.OperationID),
		targetPath,
	); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(store.stagedDir(workspaceID)); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(store.versionsDir(workspaceID)); err != nil {
		t.Fatal(err)
	}
	swap := SwapRestoreRequest{
		Mutation:           prepare.Mutation,
		SnapshotID:         snapshotID,
		ExpectedGeneration: 1,
		NextGeneration:     2,
	}
	if _, err := store.SwapRestore(t.Context(), swap); err != nil {
		t.Fatalf("recover version rename before rollback/current manifest: %v", err)
	}
	if err := os.Remove(
		store.receiptPath(swap.Mutation, ReceiptRestoreSwap),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SwapRestore(t.Context(), swap); err != nil {
		t.Fatalf("recover current-manifest publish before swap receipt: %v", err)
	}
	rollback, err := store.readRollback(workspaceID, prepare.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	previousPath, err := store.validatedVersionPath(
		workspaceID,
		rollback.PreviousImage,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(previousPath); err != nil {
		t.Fatal(err)
	}
	finalize := RestoreMutation{Mutation: prepare.Mutation}
	if _, err := store.FinalizeRestore(t.Context(), finalize); err != nil {
		t.Fatalf("recover partial finalize deletion: %v", err)
	}
	if err := os.Remove(
		store.receiptPath(finalize.Mutation, ReceiptRestoreFinalize),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeRestore(t.Context(), finalize); err != nil {
		t.Fatalf("recover finalized cleanup before receipt: %v", err)
	}
	inspection, err := store.Inspect(t.Context(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Generation != 2 || inspection.RestorePending {
		t.Fatalf("recovered restore inspection = %#v", inspection)
	}
}

func TestPrepareRestoreClassifiesMissingSnapshotImageAsLocalDataAbsent(t *testing.T) {
	store, _, _ := newFakeStore(t)
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation:      testMutation("create-missing-snapshot", "workspace-missing-snapshot"),
		CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Mutation:           testMutation("snapshot-missing-image", "workspace-missing-snapshot"),
		SnapshotID:         "snapshot-missing-image",
		ExpectedGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.snapshotImagePath("snapshot-missing-image")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareRestore(t.Context(), PrepareRestoreRequest{
		Mutation:           testMutation("restore-missing-image", "workspace-missing-snapshot"),
		SnapshotID:         "snapshot-missing-image",
		ExpectedGeneration: 1,
		NextGeneration:     2,
	}); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("missing Snapshot image error = %v", err)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return info
}
