package firecracker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

type replayWorkspaceStore struct {
	workspacestore.WorkspaceStore
	receipt      workspacestore.Receipt
	createCalled bool
}

type relocationPressureWorkspaceStore struct {
	workspacestore.WorkspaceStore
	workspacestore.WorkspaceArchiveTransferStore
	replayed     bool
	beginCalled  bool
	beginErr     error
	importCalled bool
}

func (store *relocationPressureWorkspaceStore) ImportWorkspaceArchive(_ context.Context, request workspacestore.WorkspaceArchiveImportRequest, _ io.Reader) (workspacestore.Receipt, error) {
	store.importCalled = true
	return workspacestore.Receipt{Generation: request.Generation, CapacityBytes: request.CapacityBytes, Checksum: request.Archive.ImageChecksum, RecordedAt: time.Now()}, nil
}

func TestPhysicalStorageArchiveImportChecksPressureBeforeDownloadAndAllowsReplay(t *testing.T) {
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{Backend: "ext4", TotalBytes: 100 << 20, UsedBytes: 95 << 20}}
	controller, err := newStoragePressureController(storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90, PhysicalOnly: true}, probe, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	store := &relocationPressureWorkspaceStore{}
	backend := &AssignmentBackend{manager: &Manager{cfg: &config.Config{MicroVMStorageAdmissionMode: "physical"}, workspaceStore: store}, storagePressure: controller}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "archive"); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	command := &runnerprotocol.LocalWorkspaceCommand{
		Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT,
		OperationId: "archive-import", WorkspaceId: "target", ExpectedGeneration: 1, LogicalCapacityBytes: 64 << 20, FencingToken: []byte("fencing-token"),
		ArchiveTransfer: &runnerprotocol.WorkspaceArchiveTransfer{Url: server.URL, ExpiresAtUnixMs: uint64(time.Now().Add(time.Hour).UnixMilli()), ArchiveDescriptor: &runnerprotocol.WorkspaceArchiveDescriptor{ArchiveBytes: 7, ArchiveChecksum: "sha256:" + strings.Repeat("a", 64), ImageChecksum: "sha256:" + strings.Repeat("b", 64)}},
	}
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), command); !errors.Is(err, ErrStoragePressureAdmissionDenied) {
		t.Fatalf("archive pressure denial = %v", err)
	}
	if store.importCalled {
		t.Fatal("archive importer opened under pressure")
	}
	probe.err = errors.New("probe failed")
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), command); !errors.Is(err, ErrStoragePressureProbe) {
		t.Fatalf("archive probe failure = %v", err)
	}
	if store.importCalled {
		t.Fatal("archive importer opened after failed probe")
	}
	store.replayed = true
	evidence, err := backend.ExecuteLocalWorkspace(t.Context(), command)
	if err != nil || !store.importCalled || evidence.Generation != 1 {
		t.Fatalf("completed archive import replay evidence=%+v error=%v", evidence, err)
	}
}

func (store *relocationPressureWorkspaceStore) ReplayRelocationImport(
	context.Context, workspacestore.RelocationImportRequest,
) (workspacestore.Receipt, bool, error) {
	return workspacestore.Receipt{}, store.replayed, nil
}

func (store *relocationPressureWorkspaceStore) BeginRelocationImport(
	context.Context, workspacestore.RelocationImportRequest,
) (workspacestore.RelocationImport, error) {
	store.beginCalled = true
	return nil, store.beginErr
}

func TestPhysicalStorageRelocationImportChecksPressureBeforeAllocation(t *testing.T) {
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{
		Backend: "ext4", TotalBytes: 100 << 20, UsedBytes: 95 << 20,
	}}
	controller, err := newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90, PhysicalOnly: true},
		probe, func(context.Context, string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &relocationPressureWorkspaceStore{beginErr: errors.New("replayed import reached")}
	backend := &AssignmentBackend{
		manager:         &Manager{cfg: &config.Config{MicroVMStorageAdmissionMode: "physical"}, workspaceStore: store},
		storagePressure: controller,
	}
	frame := &runnerprotocol.WorkspaceTransferFrame{
		OperationId: "relocate-target", WorkspaceId: "target", Generation: 1,
		Payload: &runnerprotocol.WorkspaceTransferFrame_Open{Open: &runnerprotocol.WorkspaceTransferOpen{
			LogicalCapacityBytes: 64 << 20, FencingToken: []byte("fencing-token"),
		}},
	}
	if _, err := backend.BeginWorkspaceRelocationImport(t.Context(), frame); !errors.Is(err, ErrStoragePressureAdmissionDenied) {
		t.Fatalf("relocation pressure denial = %v", err)
	}
	if store.beginCalled {
		t.Fatal("relocation importer opened under pressure")
	}
	probe.err = errors.New("probe failed")
	if _, err := backend.BeginWorkspaceRelocationImport(t.Context(), frame); !errors.Is(err, ErrStoragePressureProbe) {
		t.Fatalf("relocation probe failure = %v", err)
	}
	if store.beginCalled {
		t.Fatal("relocation importer opened after failed probe")
	}
	store.replayed = true
	if _, err := backend.BeginWorkspaceRelocationImport(t.Context(), frame); !errors.Is(err, store.beginErr) {
		t.Fatalf("completed import replay = %v", err)
	}
	if !store.beginCalled {
		t.Fatal("completed import did not reach WorkspaceStore")
	}
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
