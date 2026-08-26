package microsandbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"syscall"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

type localWorkspaceError struct {
	terminal runnerprotocol.LocalWorkspaceTerminalKind
	cause    error
}

func (failure localWorkspaceError) Error() string { return failure.cause.Error() }
func (failure localWorkspaceError) Unwrap() error { return failure.cause }
func (failure localWorkspaceError) LocalWorkspaceTerminal() runnerprotocol.LocalWorkspaceTerminalKind {
	return failure.terminal
}

// ExecuteLocalWorkspace exposes the provider-neutral WorkspaceStore protocol
// without allowing the helper or control plane to resolve a local path.
func (backend *AssignmentBackend) ExecuteLocalWorkspace(
	ctx context.Context,
	command *runnerprotocol.LocalWorkspaceCommand,
) (runnercontrol.LocalWorkspaceEvidence, error) {
	if backend == nil || backend.config.WorkspaceStore == nil || command == nil {
		return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(
			fmt.Errorf("SecondBox Microsandbox local-workspace command is unavailable"),
		)
	}
	mutation := workspacestore.Mutation{
		OperationID: command.OperationId, WorkspaceID: command.WorkspaceId,
		FencingToken: append([]byte(nil), command.FencingToken...),
	}
	store := backend.config.WorkspaceStore
	var receipt workspacestore.Receipt
	var err error
	switch command.Kind {
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE:
		if command.LogicalCapacityBytes > math.MaxInt64 {
			err = fmt.Errorf("SecondBox Microsandbox local-workspace capacity exceeds Runner bounds")
			break
		}
		receipt, err = store.Create(ctx, workspacestore.CreateWorkspaceRequest{
			Mutation: mutation, CapacityBytes: int64(command.LogicalCapacityBytes),
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT:
		if command.LogicalCapacityBytes > math.MaxInt64 {
			err = fmt.Errorf("SecondBox Microsandbox local-workspace capacity exceeds Runner bounds")
			break
		}
		receipt, err = store.CloneFromSnapshot(ctx, workspacestore.CloneWorkspaceRequest{
			Mutation: mutation, SourceSnapshot: command.SnapshotId,
			CapacityBytes: int64(command.LogicalCapacityBytes),
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_INSPECT:
		var inspection workspacestore.WorkspaceInspection
		inspection, err = store.Inspect(ctx, command.WorkspaceId)
		if err == nil {
			return runnercontrol.LocalWorkspaceEvidence{
				Generation: inspection.Generation, LogicalCapacity: uint64(inspection.CapacityBytes),
				Inventory: []*runnerprotocol.LocalWorkspaceInventoryItem{{
					WorkspaceId: inspection.WorkspaceID, Generation: inspection.Generation,
					LogicalCapacityBytes: uint64(inspection.CapacityBytes), Formatted: inspection.Formatted,
					RestorePending: inspection.RestorePending, RelocationSealed: inspection.RelocationSealed,
				}},
			}, nil
		}
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE:
		receipt, err = store.DeleteWorkspace(ctx, workspacestore.DeleteWorkspaceRequest{
			Mutation: mutation, ExpectedGeneration: command.ExpectedGeneration,
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE:
		receipt, err = store.AbortRelocation(ctx, workspacestore.RelocationExportRequest{
			Mutation: mutation, ExpectedGeneration: command.ExpectedGeneration,
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION:
		receipt, err = store.AdvanceGeneration(ctx, workspacestore.AdvanceGenerationRequest{
			Mutation: mutation, ExpectedGeneration: command.ExpectedGeneration,
			NextGeneration: command.NextGeneration,
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE:
		receipt, err = store.CreateSnapshot(ctx, workspacestore.CreateSnapshotRequest{
			Mutation: mutation, SnapshotID: command.SnapshotId,
			ExpectedGeneration: command.ExpectedGeneration,
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE:
		receipt, err = store.DeleteSnapshot(ctx, workspacestore.DeleteSnapshotRequest{
			Mutation: mutation, SnapshotID: command.SnapshotId,
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE:
		receipt, err = store.PrepareRestore(ctx, workspacestore.PrepareRestoreRequest{
			Mutation: mutation, SnapshotID: command.SnapshotId,
			ExpectedGeneration: command.ExpectedGeneration, NextGeneration: command.NextGeneration,
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP:
		receipt, err = store.SwapRestore(ctx, workspacestore.SwapRestoreRequest{
			Mutation: mutation, SnapshotID: command.SnapshotId,
			ExpectedGeneration: command.ExpectedGeneration, NextGeneration: command.NextGeneration,
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE:
		receipt, err = store.FinalizeRestore(ctx, workspacestore.RestoreMutation{Mutation: mutation})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		receipt, err = store.AbortRestore(ctx, workspacestore.RestoreMutation{Mutation: mutation})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE:
		return backend.reconcileWorkspace(ctx)
	default:
		err = fmt.Errorf("SecondBox Microsandbox local-workspace command kind is unsupported")
	}
	if err != nil {
		return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(err)
	}
	return workspaceReceiptEvidence(receipt), nil
}

func (backend *AssignmentBackend) reconcileWorkspace(ctx context.Context) (runnercontrol.LocalWorkspaceEvidence, error) {
	report, err := backend.config.WorkspaceStore.Reconcile(ctx)
	if err != nil {
		return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(err)
	}
	evidence := runnercontrol.LocalWorkspaceEvidence{}
	for _, workspace := range report.Workspaces {
		evidence.Inventory = append(evidence.Inventory, &runnerprotocol.LocalWorkspaceInventoryItem{
			WorkspaceId: workspace.WorkspaceID, Generation: workspace.Generation,
			LogicalCapacityBytes: uint64(workspace.CapacityBytes), Formatted: workspace.Formatted,
			RestorePending: workspace.RestorePending, ActiveWriter: workspace.ActiveWriter,
			RelocationSealed: workspace.RelocationSealed,
		})
	}
	for _, receipt := range report.Receipts {
		kind, kindErr := workspaceReceiptKind(receipt.Kind)
		if kindErr != nil {
			return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(kindErr)
		}
		evidence.Receipts = append(evidence.Receipts, &runnerprotocol.LocalWorkspaceReceiptItem{
			Kind: kind, OperationId: receipt.OperationID, WorkspaceId: receipt.WorkspaceID,
			SnapshotId: receipt.SnapshotID, PreviousGeneration: receipt.PreviousGeneration,
			Generation: receipt.Generation, LogicalCapacityBytes: uint64(receipt.CapacityBytes),
			ReceiptRecordedAtUnixMs: uint64(receipt.RecordedAt.UTC().UnixMilli()),
		})
	}
	return evidence, nil
}

type workspaceRelocationExport struct {
	workspacestore.RelocationExport
}

func (export workspaceRelocationExport) Evidence() runnercontrol.LocalWorkspaceEvidence {
	return workspaceReceiptEvidence(export.Receipt())
}

type workspaceRelocationImport struct {
	workspacestore.RelocationImport
}

func (relocation workspaceRelocationImport) Complete(size uint64, checksum string) (runnercontrol.LocalWorkspaceEvidence, error) {
	receipt, err := relocation.RelocationImport.Complete(size, checksum)
	return workspaceReceiptEvidence(receipt), err
}

func (relocation workspaceRelocationImport) CompletedEvidence() (runnercontrol.LocalWorkspaceEvidence, bool) {
	receipt, complete := relocation.CompletedReceipt()
	return workspaceReceiptEvidence(receipt), complete
}

func (backend *AssignmentBackend) OpenWorkspaceRelocationExport(
	ctx context.Context,
	command *runnerprotocol.LocalWorkspaceCommand,
) (runnercontrol.WorkspaceRelocationExport, error) {
	if backend == nil || backend.config.WorkspaceStore == nil || command == nil {
		return nil, localWorkspaceFailure(fmt.Errorf("SecondBox Microsandbox Workspace relocation export is unavailable"))
	}
	export, err := backend.config.WorkspaceStore.OpenRelocationExport(ctx, workspacestore.RelocationExportRequest{
		Mutation: workspacestore.Mutation{
			OperationID: command.OperationId, WorkspaceID: command.WorkspaceId,
			FencingToken: append([]byte(nil), command.FencingToken...),
		},
		ExpectedGeneration: command.ExpectedGeneration,
	})
	if err != nil {
		return nil, localWorkspaceFailure(err)
	}
	return workspaceRelocationExport{RelocationExport: export}, nil
}

func (backend *AssignmentBackend) BeginWorkspaceRelocationImport(
	ctx context.Context,
	frame *runnerprotocol.WorkspaceTransferFrame,
) (runnercontrol.WorkspaceRelocationImport, error) {
	if backend == nil || backend.config.WorkspaceStore == nil || frame == nil ||
		frame.GetOpen() == nil || frame.GetOpen().LogicalCapacityBytes > math.MaxInt64 {
		return nil, localWorkspaceFailure(fmt.Errorf("SecondBox Microsandbox Workspace relocation import is unavailable"))
	}
	importer, err := backend.config.WorkspaceStore.BeginRelocationImport(ctx, workspacestore.RelocationImportRequest{
		Mutation: workspacestore.Mutation{
			OperationID: frame.OperationId, WorkspaceID: frame.WorkspaceId,
			FencingToken: append([]byte(nil), frame.GetOpen().FencingToken...),
		},
		Generation: frame.Generation, CapacityBytes: int64(frame.GetOpen().LogicalCapacityBytes),
	})
	if err != nil {
		return nil, localWorkspaceFailure(err)
	}
	return workspaceRelocationImport{RelocationImport: importer}, nil
}

func workspaceReceiptEvidence(receipt workspacestore.Receipt) runnercontrol.LocalWorkspaceEvidence {
	return runnercontrol.LocalWorkspaceEvidence{
		PreviousGeneration: receipt.PreviousGeneration, Generation: receipt.Generation,
		LogicalCapacity: uint64(receipt.CapacityBytes), ReceiptRecordedAt: receipt.RecordedAt,
		Checksum: receipt.Checksum,
	}
}

func workspaceReceiptKind(kind string) (runnerprotocol.LocalWorkspaceCommandKind, error) {
	mapping := map[string]runnerprotocol.LocalWorkspaceCommandKind{
		workspacestore.ReceiptWorkspaceCreate:   runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		workspacestore.ReceiptWorkspaceClone:    runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT,
		workspacestore.ReceiptWorkspaceDelete:   runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE,
		workspacestore.ReceiptGenerationAdvance: runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
		workspacestore.ReceiptSnapshotCreate:    runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
		workspacestore.ReceiptSnapshotDelete:    runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE,
		workspacestore.ReceiptRestorePrepare:    runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		workspacestore.ReceiptRestoreSwap:       runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		workspacestore.ReceiptRestoreFinalize:   runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		workspacestore.ReceiptRestoreAbort:      runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT,
		workspacestore.ReceiptRelocationExport:  runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT,
		workspacestore.ReceiptRelocationImport:  runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_IMPORT,
		workspacestore.ReceiptRelocationAbort:   runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE,
	}
	value, ok := mapping[kind]
	if !ok {
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_UNSPECIFIED,
			fmt.Errorf("SecondBox Microsandbox WorkspaceStore receipt kind %q is unsupported", kind)
	}
	return value, nil
}

func localWorkspaceFailure(err error) error {
	terminal := runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED
	switch {
	case errors.Is(err, workspacestore.ErrWorkspaceNotFound), errors.Is(err, workspacestore.ErrSnapshotNotFound):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_LOCAL_DATA_ABSENT
	case errors.Is(err, workspacestore.ErrActiveWriter), errors.Is(err, workspacestore.ErrRelocationSealed):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_ACTIVE_WRITER
	case errors.Is(err, workspacestore.ErrStaleGeneration):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_GENERATION
	case errors.Is(err, workspacestore.ErrStaleFence):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_FENCE
	case errors.Is(err, workspacestore.ErrStorageIncompatible), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOTTY), errors.Is(err, syscall.EXDEV):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STORAGE_INCOMPATIBLE
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_INSUFFICIENT_SPACE
	case errors.Is(err, workspacestore.ErrCorruptState):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CORRUPT_RECEIPT
	case errors.Is(err, workspacestore.ErrConflictingReplay):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CONFLICTING_REPLAY
	case errors.Is(err, workspacestore.ErrSnapshotInUse):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SNAPSHOT_IN_USE
	case errors.Is(err, workspacestore.ErrRestorePending):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RESTORE_PENDING
	}
	return localWorkspaceError{terminal: terminal, cause: err}
}

var _ runnercontrol.LocalWorkspaceBackend = (*AssignmentBackend)(nil)
var _ runnercontrol.WorkspaceRelocationBackend = (*AssignmentBackend)(nil)
