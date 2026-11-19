package workspacestore

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

func TestWorkspaceReconcileBoundsReceiptEvidenceAcrossRepeatedTieringWithoutPruningReplay(t *testing.T) {
	store, _, _ := newFakeStore(t)
	const workspaceID = "bounded-tiering-receipts"
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{Mutation: testMutation("bounded-create", workspaceID), CapacityBytes: minimumExt4Bytes}); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("survives repeated tiering and old cleanup replay")
	if _, err := writer.Descriptor().WriteAt(marker, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var oldCleanup DeleteWorkspaceRequest
	var original Receipt
	const cycles = 12
	for cycle := range cycles {
		var archive bytes.Buffer
		descriptor, err := store.ExportWorkspaceArchive(t.Context(), RelocationExportRequest{Mutation: testMutation(fmt.Sprintf("bounded-export-%02d", cycle), workspaceID), ExpectedGeneration: 1}, &archive)
		if err != nil {
			t.Fatal(err)
		}
		cleanup := DeleteWorkspaceRequest{Mutation: testMutation(fmt.Sprintf("bounded-cleanup-%02d", cycle), workspaceID), ExpectedGeneration: 1, RelocationSource: true}
		deleted, err := store.DeleteWorkspace(t.Context(), cleanup)
		if err != nil {
			t.Fatal(err)
		}
		if cycle == 0 {
			oldCleanup, original = cleanup, deleted
		}
		if _, err := store.ImportWorkspaceArchive(t.Context(), WorkspaceArchiveImportRequest{RelocationImportRequest: RelocationImportRequest{Mutation: testMutation(fmt.Sprintf("bounded-import-%02d", cycle), workspaceID), Generation: 1, CapacityBytes: minimumExt4Bytes}, Archive: descriptor}, bytes.NewReader(archive.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	advance := AdvanceGenerationRequest{Mutation: testMutation("bounded-current-stop", workspaceID), ExpectedGeneration: 1, NextGeneration: 2}
	current, err := store.AdvanceGeneration(t.Context(), advance)
	if err != nil {
		t.Fatal(err)
	}
	// History far exceeds this fixture's report bound. Nil requests advertise no
	// receipts; one exact active mutation still recovers its durable evidence.
	empty, err := store.Reconcile(t.Context(), nil)
	if err != nil || len(empty.Workspaces) != 1 || len(empty.Receipts) != 0 {
		t.Fatalf("historical receipts entered inventory: %#v error=%v", empty, err)
	}
	identity := ReceiptIdentity{WorkspaceID: workspaceID, OperationID: advance.OperationID, Kind: ReceiptGenerationAdvance}
	report, err := store.Reconcile(t.Context(), []ReceiptIdentity{identity, identity, {WorkspaceID: workspaceID, OperationID: "never-applied", Kind: ReceiptWorkspaceCreate}})
	if err != nil || len(report.Workspaces) != 1 || report.Workspaces[0].Generation != 2 || len(report.Receipts) != 1 || report.Receipts[0] != current {
		t.Fatalf("bounded active receipt evidence=%#v error=%v", report, err)
	}
	if _, err := os.Stat(store.receiptPath(oldCleanup.Mutation, ReceiptWorkspaceDelete)); err != nil {
		t.Fatalf("inventory removed an old cleanup tombstone: %v", err)
	}
	replayed, err := store.DeleteWorkspace(t.Context(), oldCleanup)
	if err != nil || replayed != original {
		t.Fatalf("old cleanup lost exact replay: %#v error=%v", replayed, err)
	}
	restored, err := store.Open(t.Context(), workspaceID, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	actual := make([]byte, len(marker))
	if _, err := restored.Descriptor().ReadAt(actual, 1<<20); err != nil || !bytes.Equal(actual, marker) {
		t.Fatalf("old cleanup deleted current bytes: %q error=%v", actual, err)
	}
}

func TestWorkspaceReconcileRejectsUnsafeReceiptSelectorsWithoutScanningHistory(t *testing.T) {
	store, _, _ := newFakeStore(t)
	for _, identity := range []ReceiptIdentity{{WorkspaceID: "../escape", OperationID: "known", Kind: ReceiptWorkspaceDelete}, {WorkspaceID: "known", OperationID: "../escape", Kind: ReceiptWorkspaceDelete}, {WorkspaceID: "known", OperationID: "known", Kind: "../escape"}} {
		if _, err := store.Reconcile(t.Context(), []ReceiptIdentity{identity}); err == nil {
			t.Fatalf("unsafe selector accepted: %#v", identity)
		}
	}
}
