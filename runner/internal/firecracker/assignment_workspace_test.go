package firecracker

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

type replayWorkspaceStore struct {
	workspacestore.WorkspaceStore
	receipt      workspacestore.Receipt
	createCalled bool
}

func (store *replayWorkspaceStore) ReplayCreate(
	context.Context,
	workspacestore.CreateWorkspaceRequest,
) (workspacestore.Receipt, bool, error) {
	return store.receipt, true, nil
}

func (store *replayWorkspaceStore) Create(
	context.Context,
	workspacestore.CreateWorkspaceRequest,
) (workspacestore.Receipt, error) {
	store.createCalled = true
	return workspacestore.Receipt{}, errors.New("unexpected Workspace create after replay")
}

func TestLocalWorkspaceCreateReplayBypassesStoragePressureAdmission(t *testing.T) {
	recordedAt := time.Now().UTC()
	store := &replayWorkspaceStore{receipt: workspacestore.Receipt{
		Kind:          workspacestore.ReceiptWorkspaceCreate,
		OperationID:   "operation-replay",
		WorkspaceID:   "workspace-replay",
		Generation:    1,
		CapacityBytes: 64 << 20,
		RecordedAt:    recordedAt,
	}}
	controller, err := newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		&mutableStoragePressureProbe{sample: storagePressureSample{
			Backend: "ext4", TotalBytes: 100 << 20, UsedBytes: 95 << 20,
		}},
		func(context.Context, string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	backend := &AssignmentBackend{
		manager:         &Manager{workspaceStore: store},
		storagePressure: controller,
	}
	evidence, err := backend.ExecuteLocalWorkspace(t.Context(), &runnerprotocol.LocalWorkspaceCommand{
		Kind:                 runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		OperationId:          "operation-replay",
		WorkspaceId:          "workspace-replay",
		FencingToken:         []byte("fencing-token"),
		LogicalCapacityBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.createCalled || controller.ReservedBytes() != 0 {
		t.Fatalf("replay performed allocation: create=%t reserved=%d", store.createCalled, controller.ReservedBytes())
	}
	if evidence.Generation != 1 || evidence.LogicalCapacity != 64<<20 || !evidence.ReceiptRecordedAt.Equal(recordedAt) {
		t.Fatalf("replay evidence = %+v", evidence)
	}
}

func TestLocalWorkspaceFailureMapsStableTerminalKinds(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want runnerprotocol.LocalWorkspaceTerminalKind
	}{
		{
			name: "absent local data", err: workspacestore.ErrWorkspaceNotFound,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_LOCAL_DATA_ABSENT,
		},
		{
			name: "active writer", err: workspacestore.ErrActiveWriter,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_ACTIVE_WRITER,
		},
		{
			name: "stale generation", err: workspacestore.ErrStaleGeneration,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_GENERATION,
		},
		{
			name: "stale fence", err: workspacestore.ErrStaleFence,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_FENCE,
		},
		{
			name: "unsupported reflink store", err: workspacestore.ErrStorageIncompatible,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STORAGE_INCOMPATIBLE,
		},
		{
			name: "insufficient space", err: syscall.ENOSPC,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_INSUFFICIENT_SPACE,
		},
		{
			name: "corrupt receipt", err: workspacestore.ErrCorruptState,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CORRUPT_RECEIPT,
		},
		{
			name: "conflicting replay", err: workspacestore.ErrConflictingReplay,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CONFLICTING_REPLAY,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var typed interface {
				LocalWorkspaceTerminal() runnerprotocol.LocalWorkspaceTerminalKind
			}
			if err := localWorkspaceFailure(testCase.err); !errors.As(err, &typed) {
				t.Fatalf("failure %v is not typed", err)
			} else if got := typed.LocalWorkspaceTerminal(); got != testCase.want {
				t.Fatalf("terminal = %v, want %v", got, testCase.want)
			}
		})
	}
}
