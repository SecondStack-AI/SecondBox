package workspacestore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWorkspaceRelocationSealsTransfersImportsAndDeletesSource(t *testing.T) {
	const (
		workspaceID = "workspace-relocation"
		operationID = "operation-relocation"
		capacity    = int64(minimumExt4Bytes)
	)
	source, _, _ := newFakeStore(t)
	target, _, _ := newFakeStore(t)
	if _, err := source.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("operation-create-source", workspaceID), CapacityBytes: capacity,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := source.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("SecondBox relocation preserves Workspace bytes")
	if _, err := attachment.Descriptor().WriteAt(marker, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	request := RelocationExportRequest{
		Mutation: testMutation(operationID, workspaceID), ExpectedGeneration: 1,
	}
	export, err := source.OpenRelocationExport(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Open(t.Context(), workspaceID, 1); !errors.Is(err, ErrActiveWriter) {
		t.Fatalf("open source during transfer error = %v", err)
	}
	if err := export.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Open(t.Context(), workspaceID, 1); !errors.Is(err, ErrRelocationSealed) {
		t.Fatalf("open sealed source error = %v", err)
	}
	export, err = source.OpenRelocationExport(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	importer, err := target.BeginRelocationImport(t.Context(), RelocationImportRequest{
		Mutation: testMutation(operationID, workspaceID), Generation: 1, CapacityBytes: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var offset uint64
	for {
		read, readErr := export.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			if err := importer.WriteChunk(offset, chunk); err != nil {
				t.Fatal(err)
			}
			if _, err := hash.Write(chunk); err != nil {
				t.Fatal(err)
			}
			offset += uint64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	checksum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	importReceipt, err := importer.Complete(offset, checksum)
	if err != nil {
		t.Fatal(err)
	}
	if importReceipt.Checksum != checksum || importReceipt.RecordedAt.IsZero() {
		t.Fatalf("import receipt = %#v", importReceipt)
	}
	replayedImport, err := target.BeginRelocationImport(t.Context(), RelocationImportRequest{
		Mutation: testMutation(operationID, workspaceID), Generation: 1, CapacityBytes: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayedReceipt, completed := replayedImport.CompletedReceipt(); !completed || replayedReceipt.Checksum != checksum {
		t.Fatalf("replayed import receipt = %#v, completed = %t", replayedReceipt, completed)
	}
	if err := export.Close(); err != nil {
		t.Fatal(err)
	}
	targetAttachment, err := target.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len(marker))
	if _, err := targetAttachment.Descriptor().ReadAt(actual, 1<<20); err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(marker) {
		t.Fatalf("relocated marker = %q", actual)
	}
	targetInfo, err := targetAttachment.Descriptor().Stat()
	if err != nil {
		t.Fatal(err)
	}
	targetStat, ok := targetInfo.Sys().(*syscall.Stat_t)
	if !ok || int64(targetStat.Blocks)*512 >= capacity {
		t.Fatalf("relocated Workspace lost sparse allocation: %#v", targetInfo.Sys())
	}
	if err := targetAttachment.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DeleteWorkspace(t.Context(), DeleteWorkspaceRequest{
		Mutation: testMutation("operation-delete-relocated-source", workspaceID), ExpectedGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Inspect(t.Context(), workspaceID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("inspect deleted source error = %v", err)
	}
}

func TestWorkspaceRelocationFailureKeepsOriginalSourceStartable(t *testing.T) {
	const (
		workspaceID = "workspace-relocation-abort"
		operationID = "operation-relocation-abort"
		capacity    = int64(minimumExt4Bytes)
	)
	source, _, _ := newFakeStore(t)
	target, _, _ := newFakeStore(t)
	if _, err := source.Create(t.Context(), CreateWorkspaceRequest{
		Mutation: testMutation("operation-create-abort-source", workspaceID), CapacityBytes: capacity,
	}); err != nil {
		t.Fatal(err)
	}
	request := RelocationExportRequest{
		Mutation: testMutation(operationID, workspaceID), ExpectedGeneration: 1,
	}
	export, err := source.OpenRelocationExport(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	importer, err := target.BeginRelocationImport(t.Context(), RelocationImportRequest{
		Mutation: testMutation(operationID, workspaceID), Generation: 1, CapacityBytes: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64<<10)
	var offset uint64
	for {
		read, readErr := export.Read(buffer)
		if read > 0 {
			if err := importer.WriteChunk(offset, buffer[:read]); err != nil {
				t.Fatal(err)
			}
			offset += uint64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	if _, err := importer.Complete(offset, "sha256:"+string(make([]byte, 64))); err == nil {
		t.Fatal("checksum mismatch succeeded")
	}
	if err := importer.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := export.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AbortRelocation(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	attachment, err := source.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("open original source after abort: %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Inspect(t.Context(), workspaceID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("inspect failed target import error = %v", err)
	}
}

func TestWorkspaceRelocationImportReceiptFailureRemovesPublishedTarget(t *testing.T) {
	const (
		workspaceID = "workspace-relocation-receipt-failure"
		operationID = "operation-relocation-receipt-failure"
		capacity    = int64(minimumExt4Bytes)
	)
	source, _, _ := newFakeStore(t)
	target, _, _ := newFakeStore(t)
	if _, err := source.Create(t.Context(), CreateWorkspaceRequest{
		Mutation:      testMutation("operation-create-receipt-failure", workspaceID),
		CapacityBytes: capacity,
	}); err != nil {
		t.Fatal(err)
	}
	mutation := testMutation(operationID, workspaceID)
	exportRequest := RelocationExportRequest{
		Mutation: mutation, ExpectedGeneration: 1,
	}
	export, err := source.OpenRelocationExport(t.Context(), exportRequest)
	if err != nil {
		t.Fatal(err)
	}
	importer, err := target.BeginRelocationImport(t.Context(), RelocationImportRequest{
		Mutation: mutation, Generation: 1, CapacityBytes: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var offset uint64
	for {
		read, readErr := export.Read(buffer)
		if read > 0 {
			if err := importer.WriteChunk(offset, buffer[:read]); err != nil {
				t.Fatal(err)
			}
			if _, err := hash.Write(buffer[:read]); err != nil {
				t.Fatal(err)
			}
			offset += uint64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	receiptDirectory := filepath.Dir(target.receiptPath(mutation, ReceiptRelocationImport))
	if err := os.Remove(receiptDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptDirectory, []byte("block receipt persistence"), 0o600); err != nil {
		t.Fatal(err)
	}
	checksum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if _, err := importer.Complete(offset, checksum); err == nil {
		t.Fatal("relocation import succeeded without a durable receipt directory")
	}
	if err := importer.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Inspect(t.Context(), workspaceID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("inspect target after receipt failure error = %v", err)
	}
	if err := export.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AbortRelocation(t.Context(), exportRequest); err != nil {
		t.Fatal(err)
	}
	attachment, err := source.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("open source after target receipt failure: %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
}
