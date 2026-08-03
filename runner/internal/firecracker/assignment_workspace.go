package firecracker

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

func (failure localWorkspaceError) Error() string {
	return failure.cause.Error()
}

func (failure localWorkspaceError) Unwrap() error {
	return failure.cause
}

func (failure localWorkspaceError) LocalWorkspaceTerminal() runnerprotocol.LocalWorkspaceTerminalKind {
	return failure.terminal
}

// ExecuteLocalWorkspace adapts logical protocol commands to the runner-owned
// WorkspaceStore. No local path or image bytes enter the protocol result.
func (b *AssignmentBackend) ExecuteLocalWorkspace(
	ctx context.Context,
	command *runnerprotocol.LocalWorkspaceCommand,
) (runnercontrol.LocalWorkspaceEvidence, error) {
	if b == nil || b.manager == nil || b.manager.workspaceStore == nil {
		return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(
			fmt.Errorf("SecondBox Firecracker WorkspaceStore is unavailable"),
		)
	}
	if command == nil {
		return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(
			fmt.Errorf("SecondBox Firecracker local-workspace command is required"),
		)
	}
	mutation := workspacestore.Mutation{
		OperationID:  command.OperationId,
		WorkspaceID:  command.WorkspaceId,
		FencingToken: append([]byte(nil), command.FencingToken...),
	}
	var (
		receipt workspacestore.Receipt
		err     error
	)
	switch command.Kind {
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE:
		if command.LogicalCapacityBytes > math.MaxInt64 {
			return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(
				fmt.Errorf("SecondBox Firecracker local-workspace capacity exceeds Runner bounds"),
			)
		}
		receipt, err = b.manager.workspaceStore.Create(ctx, workspacestore.CreateWorkspaceRequest{
			Mutation:      mutation,
			CapacityBytes: int64(command.LogicalCapacityBytes),
		})
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT:
		if command.LogicalCapacityBytes > math.MaxInt64 {
			return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(
				fmt.Errorf("SecondBox Firecracker local-workspace capacity exceeds Runner bounds"),
			)
		}
		receipt, err = b.manager.workspaceStore.CloneFromSnapshot(
			ctx,
			workspacestore.CloneWorkspaceRequest{
				Mutation:       mutation,
				SourceSnapshot: command.SnapshotId,
				CapacityBytes:  int64(command.LogicalCapacityBytes),
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_INSPECT:
		var inspection workspacestore.WorkspaceInspection
		inspection, err = b.manager.workspaceStore.Inspect(ctx, command.WorkspaceId)
		if err == nil {
			return runnercontrol.LocalWorkspaceEvidence{
				Generation:      inspection.Generation,
				LogicalCapacity: uint64(inspection.CapacityBytes),
				Inventory: []*runnerprotocol.LocalWorkspaceInventoryItem{{
					WorkspaceId:          inspection.WorkspaceID,
					Generation:           inspection.Generation,
					LogicalCapacityBytes: uint64(inspection.CapacityBytes),
					Formatted:            inspection.Formatted,
					RestorePending:       inspection.RestorePending,
					RelocationSealed:     inspection.RelocationSealed,
				}},
			}, nil
		}
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE:
		receipt, err = b.manager.workspaceStore.DeleteWorkspace(
			ctx,
			workspacestore.DeleteWorkspaceRequest{
				Mutation:           mutation,
				ExpectedGeneration: command.ExpectedGeneration,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE:
		receipt, err = b.manager.workspaceStore.DeleteWorkspace(
			ctx,
			workspacestore.DeleteWorkspaceRequest{
				Mutation: mutation, ExpectedGeneration: command.ExpectedGeneration,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE:
		receipt, err = b.manager.workspaceStore.AbortRelocation(
			ctx,
			workspacestore.RelocationExportRequest{
				Mutation: mutation, ExpectedGeneration: command.ExpectedGeneration,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION:
		receipt, err = b.manager.workspaceStore.AdvanceGeneration(
			ctx,
			workspacestore.AdvanceGenerationRequest{
				Mutation:           mutation,
				ExpectedGeneration: command.ExpectedGeneration,
				NextGeneration:     command.NextGeneration,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE:
		receipt, err = b.manager.workspaceStore.CreateSnapshot(
			ctx,
			workspacestore.CreateSnapshotRequest{
				Mutation:           mutation,
				SnapshotID:         command.SnapshotId,
				ExpectedGeneration: command.ExpectedGeneration,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE:
		receipt, err = b.manager.workspaceStore.DeleteSnapshot(
			ctx,
			workspacestore.DeleteSnapshotRequest{
				Mutation:   mutation,
				SnapshotID: command.SnapshotId,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE:
		receipt, err = b.manager.workspaceStore.PrepareRestore(
			ctx,
			workspacestore.PrepareRestoreRequest{
				Mutation:           mutation,
				SnapshotID:         command.SnapshotId,
				ExpectedGeneration: command.ExpectedGeneration,
				NextGeneration:     command.NextGeneration,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP:
		receipt, err = b.manager.workspaceStore.SwapRestore(
			ctx,
			workspacestore.SwapRestoreRequest{
				Mutation:           mutation,
				SnapshotID:         command.SnapshotId,
				ExpectedGeneration: command.ExpectedGeneration,
				NextGeneration:     command.NextGeneration,
			},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE:
		receipt, err = b.manager.workspaceStore.FinalizeRestore(
			ctx,
			workspacestore.RestoreMutation{Mutation: mutation},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		receipt, err = b.manager.workspaceStore.AbortRestore(
			ctx,
			workspacestore.RestoreMutation{Mutation: mutation},
		)
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE:
		var report workspacestore.ReconcileReport
		report, err = b.manager.workspaceStore.Reconcile(ctx)
		if err == nil {
			evidence := runnercontrol.LocalWorkspaceEvidence{
				Inventory: make(
					[]*runnerprotocol.LocalWorkspaceInventoryItem,
					0,
					len(report.Workspaces),
				),
				Receipts: make(
					[]*runnerprotocol.LocalWorkspaceReceiptItem,
					0,
					len(report.Receipts),
				),
			}
			for _, workspace := range report.Workspaces {
				evidence.Inventory = append(evidence.Inventory, &runnerprotocol.LocalWorkspaceInventoryItem{
					WorkspaceId:          workspace.WorkspaceID,
					Generation:           workspace.Generation,
					LogicalCapacityBytes: uint64(workspace.CapacityBytes),
					Formatted:            workspace.Formatted,
					RestorePending:       workspace.RestorePending,
					ActiveWriter:         workspace.ActiveWriter,
					RelocationSealed:     workspace.RelocationSealed,
				})
			}
			for _, receipt := range report.Receipts {
				kind, kindErr := localWorkspaceReceiptKind(receipt.Kind)
				if kindErr != nil {
					return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(kindErr)
				}
				evidence.Receipts = append(
					evidence.Receipts,
					&runnerprotocol.LocalWorkspaceReceiptItem{
						Kind:                    kind,
						OperationId:             receipt.OperationID,
						WorkspaceId:             receipt.WorkspaceID,
						SnapshotId:              receipt.SnapshotID,
						PreviousGeneration:      receipt.PreviousGeneration,
						Generation:              receipt.Generation,
						LogicalCapacityBytes:    uint64(receipt.CapacityBytes),
						ReceiptRecordedAtUnixMs: uint64(receipt.RecordedAt.UTC().UnixMilli()),
					},
				)
			}
			return evidence, nil
		}
	default:
		err = fmt.Errorf("SecondBox Firecracker local-workspace command kind is unsupported")
	}
	if err != nil {
		return runnercontrol.LocalWorkspaceEvidence{}, localWorkspaceFailure(err)
	}
	return runnercontrol.LocalWorkspaceEvidence{
		PreviousGeneration: receipt.PreviousGeneration,
		Generation:         receipt.Generation,
		LogicalCapacity:    uint64(receipt.CapacityBytes),
		ReceiptRecordedAt:  receipt.RecordedAt,
	}, nil
}

type workspaceRelocationExport struct {
	workspacestore.RelocationExport
}

func (export workspaceRelocationExport) Evidence() runnercontrol.LocalWorkspaceEvidence {
	receipt := export.Receipt()
	return runnercontrol.LocalWorkspaceEvidence{
		Generation:        receipt.Generation,
		LogicalCapacity:   uint64(receipt.CapacityBytes),
		ReceiptRecordedAt: receipt.RecordedAt,
	}
}

type workspaceRelocationImport struct {
	workspacestore.RelocationImport
}

func (relocation workspaceRelocationImport) Complete(
	size uint64,
	checksum string,
) (runnercontrol.LocalWorkspaceEvidence, error) {
	receipt, err := relocation.RelocationImport.Complete(size, checksum)
	return relocationReceiptEvidence(receipt), err
}

func (relocation workspaceRelocationImport) CompletedEvidence() (
	runnercontrol.LocalWorkspaceEvidence,
	bool,
) {
	receipt, completed := relocation.CompletedReceipt()
	return relocationReceiptEvidence(receipt), completed
}

func relocationReceiptEvidence(receipt workspacestore.Receipt) runnercontrol.LocalWorkspaceEvidence {
	return runnercontrol.LocalWorkspaceEvidence{
		Generation:        receipt.Generation,
		LogicalCapacity:   uint64(receipt.CapacityBytes),
		ReceiptRecordedAt: receipt.RecordedAt,
		Checksum:          receipt.Checksum,
	}
}

// OpenWorkspaceRelocationExport adapts the sealed WorkspaceStore reader.
func (b *AssignmentBackend) OpenWorkspaceRelocationExport(
	ctx context.Context,
	command *runnerprotocol.LocalWorkspaceCommand,
) (runnercontrol.WorkspaceRelocationExport, error) {
	if b == nil || b.manager == nil || b.manager.workspaceStore == nil || command == nil {
		return nil, localWorkspaceFailure(
			fmt.Errorf("SecondBox Firecracker WorkspaceStore relocation export is unavailable"),
		)
	}
	export, err := b.manager.workspaceStore.OpenRelocationExport(
		ctx,
		workspacestore.RelocationExportRequest{
			Mutation: workspacestore.Mutation{
				OperationID:  command.OperationId,
				WorkspaceID:  command.WorkspaceId,
				FencingToken: append([]byte(nil), command.FencingToken...),
			},
			ExpectedGeneration: command.ExpectedGeneration,
		},
	)
	if err != nil {
		return nil, localWorkspaceFailure(err)
	}
	return workspaceRelocationExport{RelocationExport: export}, nil
}

// BeginWorkspaceRelocationImport adapts one control-plane-forwarded target stream.
func (b *AssignmentBackend) BeginWorkspaceRelocationImport(
	ctx context.Context,
	frame *runnerprotocol.WorkspaceTransferFrame,
) (runnercontrol.WorkspaceRelocationImport, error) {
	if b == nil || b.manager == nil || b.manager.workspaceStore == nil ||
		frame == nil || frame.GetOpen() == nil || frame.GetOpen().LogicalCapacityBytes > math.MaxInt64 {
		return nil, localWorkspaceFailure(
			fmt.Errorf("SecondBox Firecracker WorkspaceStore relocation import is unavailable"),
		)
	}
	importer, err := b.manager.workspaceStore.BeginRelocationImport(
		ctx,
		workspacestore.RelocationImportRequest{
			Mutation: workspacestore.Mutation{
				OperationID:  frame.OperationId,
				WorkspaceID:  frame.WorkspaceId,
				FencingToken: append([]byte(nil), frame.GetOpen().FencingToken...),
			},
			Generation:    frame.Generation,
			CapacityBytes: int64(frame.GetOpen().LogicalCapacityBytes),
		},
	)
	if err != nil {
		return nil, localWorkspaceFailure(err)
	}
	return workspaceRelocationImport{RelocationImport: importer}, nil
}

func localWorkspaceReceiptKind(
	kind string,
) (runnerprotocol.LocalWorkspaceCommandKind, error) {
	switch kind {
	case workspacestore.ReceiptWorkspaceCreate:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE, nil
	case workspacestore.ReceiptWorkspaceClone:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT, nil
	case workspacestore.ReceiptWorkspaceDelete:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE, nil
	case workspacestore.ReceiptGenerationAdvance:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION, nil
	case workspacestore.ReceiptSnapshotCreate:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE, nil
	case workspacestore.ReceiptSnapshotDelete:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE, nil
	case workspacestore.ReceiptRestorePrepare:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE, nil
	case workspacestore.ReceiptRestoreSwap:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP, nil
	case workspacestore.ReceiptRestoreFinalize:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE, nil
	case workspacestore.ReceiptRestoreAbort:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT, nil
	case workspacestore.ReceiptRelocationExport:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT, nil
	case workspacestore.ReceiptRelocationImport:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_IMPORT, nil
	case workspacestore.ReceiptRelocationAbort:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE, nil
	default:
		return runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_UNSPECIFIED,
			fmt.Errorf("SecondBox Firecracker WorkspaceStore receipt kind %q is unsupported", kind)
	}
}

func localWorkspaceFailure(err error) error {
	terminal := runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED
	switch {
	case errors.Is(err, workspacestore.ErrWorkspaceNotFound),
		errors.Is(err, workspacestore.ErrSnapshotNotFound):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_LOCAL_DATA_ABSENT
	case errors.Is(err, workspacestore.ErrActiveWriter),
		errors.Is(err, workspacestore.ErrRelocationSealed):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_ACTIVE_WRITER
	case errors.Is(err, workspacestore.ErrStaleGeneration):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_GENERATION
	case errors.Is(err, workspacestore.ErrStaleFence):
		terminal = runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_FENCE
	case errors.Is(err, workspacestore.ErrStorageIncompatible),
		errors.Is(err, syscall.EOPNOTSUPP),
		errors.Is(err, syscall.ENOTTY),
		errors.Is(err, syscall.EXDEV):
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
