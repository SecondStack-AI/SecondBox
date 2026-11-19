package workspacestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

func TestWorkspaceArchiveRestoresSparseImageOnAnotherStore(t *testing.T) {
	source, _, _ := newFakeStore(t)
	target, _, _ := newFakeStore(t)
	const workspaceID = "archive-workspace"
	if _, err := source.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("create-archive", workspaceID), CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := source.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{1 << 20, minimumExt4Bytes - 4096} {
		if _, err := attachment.Descriptor().WriteAt([]byte("uncommitted workspace journal"), offset); err != nil {
			t.Fatal(err)
		}
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	request := RelocationExportRequest{Mutation: testMutation("archive-export", workspaceID), ExpectedGeneration: 1}
	descriptor, err := source.ExportWorkspaceArchive(t.Context(), request, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ArchiveBytes != int64(archive.Len()) || archive.Len() > 1<<20 {
		t.Fatalf("sparse archive descriptor=%+v bytes=%d", descriptor, archive.Len())
	}
	if _, err := source.Open(t.Context(), workspaceID, 1); !errors.Is(err, ErrRelocationSealed) {
		t.Fatalf("archive source must remain sealed, got %v", err)
	}
	receipt, err := target.ImportWorkspaceArchive(t.Context(), WorkspaceArchiveImportRequest{
		RelocationImportRequest: RelocationImportRequest{
			Mutation: testMutation("archive-import", workspaceID), Generation: 1, CapacityBytes: minimumExt4Bytes,
		},
		Archive: descriptor,
	}, bytes.NewReader(archive.Bytes()))
	if err != nil || receipt.Checksum != descriptor.ImageChecksum {
		t.Fatalf("restore receipt=%+v error=%v", receipt, err)
	}
	restored, err := target.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(restored.Descriptor(), 0, minimumExt4Bytes)); err != nil {
		t.Fatal(err)
	}
	if actual := "sha256:" + hex.EncodeToString(hash.Sum(nil)); actual != descriptor.ImageChecksum {
		t.Fatalf("restored disk checksum=%s expected=%s", actual, descriptor.ImageChecksum)
	}
}

func TestWorkspaceArchiveRejectsCorruptionAndWrongIdentityBeforePublish(t *testing.T) {
	source, _, _ := newFakeStore(t)
	const workspaceID = "archive-validation"
	if _, err := source.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("create-validation", workspaceID), CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	descriptor, err := source.ExportWorkspaceArchive(t.Context(), RelocationExportRequest{
		Mutation: testMutation("export-validation", workspaceID), ExpectedGeneration: 1,
	}, &archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"truncated", "changed", "wrong-workspace", "wrong-generation", "trailing-bytes"} {
		t.Run(scenario, func(t *testing.T) {
			target, _, _ := newFakeStore(t)
			content := append([]byte(nil), archive.Bytes()...)
			request := WorkspaceArchiveImportRequest{
				RelocationImportRequest: RelocationImportRequest{
					Mutation: testMutation("import-validation", workspaceID), Generation: 1, CapacityBytes: minimumExt4Bytes,
				}, Archive: descriptor,
			}
			switch scenario {
			case "truncated":
				content = content[:len(content)-4]
			case "changed":
				content[len(content)/2] ^= 1
			case "wrong-workspace":
				request.WorkspaceID = "other-workspace"
			case "wrong-generation":
				request.Generation++
			case "trailing-bytes":
				content = append(content, 0)
			}
			if _, err := target.ImportWorkspaceArchive(t.Context(), request, bytes.NewReader(content)); err == nil {
				t.Fatal("invalid archive accepted")
			}
			if _, err := target.Open(t.Context(), request.WorkspaceID, request.Generation); !errors.Is(err, ErrWorkspaceNotFound) {
				t.Fatalf("failed restore published a workspace: %v", err)
			}
		})
	}
}

func TestWorkspaceArchiveRefusesActiveWriter(t *testing.T) {
	store, _, _ := newFakeStore(t)
	const workspaceID = "archive-busy"
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("create-busy", workspaceID), CapacityBytes: minimumExt4Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Close()
	if _, err := store.ExportWorkspaceArchive(t.Context(), RelocationExportRequest{
		Mutation: testMutation("archive-busy", workspaceID), ExpectedGeneration: 1,
	}, io.Discard); !errors.Is(err, ErrActiveWriter) {
		t.Fatalf("archive active writer error=%v", err)
	}
}

func TestWorkspaceArchiveSameHomeRestoreSurvivesExactOldCleanupReplay(t *testing.T) {
	store, _, _ := newFakeStore(t)
	const workspaceID = "archive-return-home"
	if _, err := store.Create(t.Context(), CreateWorkspaceRequest{Mutation: testMutation("return-create", workspaceID), CapacityBytes: minimumExt4Bytes}); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("restored original workspace data")
	if _, err := writer.Descriptor().WriteAt(marker, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	descriptor, err := store.ExportWorkspaceArchive(t.Context(), RelocationExportRequest{Mutation: testMutation("return-export", workspaceID), ExpectedGeneration: 1}, &archive)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := DeleteWorkspaceRequest{Mutation: testMutation("return-cleanup", workspaceID), ExpectedGeneration: 1, RelocationSource: true}
	deleted, err := store.DeleteWorkspace(t.Context(), cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportWorkspaceArchive(t.Context(), WorkspaceArchiveImportRequest{RelocationImportRequest: RelocationImportRequest{Mutation: testMutation("return-import", workspaceID), Generation: 1, CapacityBytes: minimumExt4Bytes}, Archive: descriptor}, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.DeleteWorkspace(t.Context(), cleanup)
	if err != nil || replayed != deleted {
		t.Fatalf("old cleanup receipt replay=%#v expected=%#v error=%v", replayed, deleted, err)
	}
	// A later tiering cycle must retain the first cleanup's durable replay
	// tombstone even when it removes and restores this generation again.
	var secondArchive bytes.Buffer
	secondDescriptor, err := store.ExportWorkspaceArchive(t.Context(), RelocationExportRequest{Mutation: testMutation("return-export-second", workspaceID), ExpectedGeneration: 1}, &secondArchive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteWorkspace(t.Context(), DeleteWorkspaceRequest{Mutation: testMutation("return-cleanup-second", workspaceID), ExpectedGeneration: 1, RelocationSource: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportWorkspaceArchive(t.Context(), WorkspaceArchiveImportRequest{RelocationImportRequest: RelocationImportRequest{Mutation: testMutation("return-import-second", workspaceID), Generation: 1, CapacityBytes: minimumExt4Bytes}, Archive: secondDescriptor}, bytes.NewReader(secondArchive.Bytes())); err != nil {
		t.Fatal(err)
	}
	replayed, err = store.DeleteWorkspace(t.Context(), cleanup)
	if err != nil || replayed != deleted {
		t.Fatalf("old cleanup tombstone lost across tiering cycles: replay=%#v original=%#v error=%v", replayed, deleted, err)
	}
	restored, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("old cleanup deleted restored workspace: %v", err)
	}
	defer restored.Close()
	actual := make([]byte, len(marker))
	if _, err := restored.Descriptor().ReadAt(actual, 1<<20); err != nil || !bytes.Equal(actual, marker) {
		t.Fatalf("restored data after cleanup replay=%q error=%v", actual, err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteWorkspace(t.Context(), DeleteWorkspaceRequest{Mutation: testMutation("return-final-delete", workspaceID), ExpectedGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	report, err := store.Reconcile(t.Context())
	if err != nil || len(report.Workspaces) != 0 || len(report.Receipts) != 1 || report.Receipts[0].OperationID != "return-final-delete" {
		t.Fatalf("final deletion retained obsolete tiering receipts: report=%#v error=%v", report, err)
	}
}
