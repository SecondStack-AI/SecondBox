//go:build linux

package gvisor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

type admissionWorkspaceStore struct {
	workspacestore.WorkspaceStore
	receipts    map[string]workspacestore.Receipt
	beginCalled bool
	replayed    bool
	beginErr    error
}

type archiveAdmissionWorkspaceStore struct {
	admissionWorkspaceStore
	workspacestore.WorkspaceArchiveTransferStore
	importCalled bool
}

func (store *archiveAdmissionWorkspaceStore) ImportWorkspaceArchive(_ context.Context, request workspacestore.WorkspaceArchiveImportRequest, _ io.Reader) (workspacestore.Receipt, error) {
	store.importCalled = true
	return workspacestore.Receipt{Generation: request.Generation, CapacityBytes: request.CapacityBytes, Checksum: request.Archive.ImageChecksum, RecordedAt: time.Now()}, nil
}

func TestPhysicalStorageArchiveImportChecksPressureBeforeDownloadAndAllowsReplay(t *testing.T) {
	store := &archiveAdmissionWorkspaceStore{}
	config := Config{
		WorkspaceRoot: "/workspace", WorkspaceStore: store,
		StorageAdmissionMode: "physical", StorageRecoveryPercent: 70,
		StorageWarningPercent: 80, StorageDenyPercent: 90,
	}
	pressure, err := newPhysicalStoragePressure(config)
	if err != nil {
		t.Fatal(err)
	}
	var probeErr error
	pressure.probe = func(string) (uint64, uint64, error) { return 95, 100, probeErr }
	backend := &AssignmentBackend{config: validatedConfig{Config: config}, storagePressure: pressure}
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		if _, err := io.WriteString(w, "archive"); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	command := &runnerprotocol.LocalWorkspaceCommand{
		Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT,
		OperationId: "archive-import", WorkspaceId: "target", ExpectedGeneration: 1,
		LogicalCapacityBytes: 64 << 20, FencingToken: []byte("fencing-token"),
		ArchiveTransfer: &runnerprotocol.WorkspaceArchiveTransfer{
			Url: server.URL, ExpiresAtUnixMs: uint64(time.Now().Add(time.Hour).UnixMilli()),
			ArchiveDescriptor: &runnerprotocol.WorkspaceArchiveDescriptor{
				ArchiveBytes: 7, ArchiveChecksum: "sha256:" + strings.Repeat("a", 64), ImageChecksum: "sha256:" + strings.Repeat("b", 64),
			},
		},
	}
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), command); !errors.Is(err, errPhysicalStoragePressure) {
		t.Fatalf("archive pressure denial = %v", err)
	}
	probeErr = errors.New("probe failed")
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), command); !errors.Is(err, probeErr) {
		t.Fatalf("archive probe failure = %v", err)
	}
	if store.importCalled || downloads != 0 {
		t.Fatal("fresh archive import downloaded or allocated under pressure")
	}
	store.replayed = true
	evidence, err := backend.ExecuteLocalWorkspace(t.Context(), command)
	if err != nil || !store.importCalled || evidence.Generation != 1 || downloads != 1 {
		t.Fatalf("completed archive import replay evidence=%+v downloads=%d error=%v", evidence, downloads, err)
	}
}

func TestStoragePressureReadinessOrdersCompletedProbes(t *testing.T) {
	pressure, err := newPhysicalStoragePressure(Config{
		WorkspaceRoot: "/workspace", StorageRecoveryPercent: 70, StorageWarningPercent: 80, StorageDenyPercent: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	var completedAt uint64
	pressure.probe = func(string) (uint64, uint64, error) {
		time.Sleep(5 * time.Millisecond)
		completedAt = uint64(time.Now().UnixMilli())
		return 10, 100, nil
	}
	backend := &AssignmentBackend{storagePressure: pressure}
	healthy, err := backend.storagePressureReadiness(t.Context())
	if err != nil || healthy.ObservedAtUnixMs < completedAt {
		t.Fatalf("readiness timestamp predates completed probe: %v, completed %d, err %v", healthy, completedAt, err)
	}
	pressure.probe = func(string) (uint64, uint64, error) { return 95, 100, nil }
	state, observedAt, err := pressure.observePressure(t.Context())
	if err != nil || state != "admission_denied" || observedAt <= healthy.ObservedAtUnixMs {
		t.Fatalf("later refusal is not strictly newer: state=%s at=%d after=%v err=%v", state, observedAt, healthy, err)
	}
	pressure.probe = func(string) (uint64, uint64, error) { return 0, 0, errors.New("probe failed") }
	_, failedAt, err := pressure.observePressure(t.Context())
	if err == nil || failedAt <= observedAt {
		t.Fatalf("probe failure lost its ordering: at=%d after=%d err=%v", failedAt, observedAt, err)
	}
}

func (store *admissionWorkspaceStore) ReplayCreate(
	_ context.Context, request workspacestore.CreateWorkspaceRequest,
) (workspacestore.Receipt, bool, error) {
	receipt, found := store.receipts[request.OperationID]
	return receipt, found, nil
}

func (store *admissionWorkspaceStore) Create(
	_ context.Context, request workspacestore.CreateWorkspaceRequest,
) (workspacestore.Receipt, error) {
	receipt := workspacestore.Receipt{
		Kind: workspacestore.ReceiptWorkspaceCreate, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, Generation: 1, CapacityBytes: request.CapacityBytes,
		RecordedAt: time.Now().UTC(),
	}
	store.receipts[request.OperationID] = receipt
	return receipt, nil
}

func (store *admissionWorkspaceStore) ReplayCloneFromSnapshot(
	_ context.Context, request workspacestore.CloneWorkspaceRequest,
) (workspacestore.Receipt, bool, error) {
	receipt, found := store.receipts[request.OperationID]
	return receipt, found, nil
}

func (store *admissionWorkspaceStore) CloneFromSnapshot(
	_ context.Context, request workspacestore.CloneWorkspaceRequest,
) (workspacestore.Receipt, error) {
	if request.SourceSnapshot != "source-snapshot" {
		return workspacestore.Receipt{}, errors.New("missing source Snapshot")
	}
	receipt := workspacestore.Receipt{
		Kind: workspacestore.ReceiptWorkspaceClone, OperationID: request.OperationID,
		WorkspaceID: request.WorkspaceID, Generation: 1, CapacityBytes: request.CapacityBytes,
		RecordedAt: time.Now().UTC(),
	}
	store.receipts[request.OperationID] = receipt
	return receipt, nil
}

func (store *admissionWorkspaceStore) Inspect(
	_ context.Context, workspaceID string,
) (workspacestore.WorkspaceInspection, error) {
	for _, receipt := range store.receipts {
		if receipt.WorkspaceID == workspaceID {
			return workspacestore.WorkspaceInspection{WorkspaceID: workspaceID}, nil
		}
	}
	return workspacestore.WorkspaceInspection{}, workspacestore.ErrWorkspaceNotFound
}

func (store *admissionWorkspaceStore) ReplayRelocationImport(
	context.Context, workspacestore.RelocationImportRequest,
) (workspacestore.Receipt, bool, error) {
	return workspacestore.Receipt{}, store.replayed, nil
}

func (store *admissionWorkspaceStore) BeginRelocationImport(
	context.Context, workspacestore.RelocationImportRequest,
) (workspacestore.RelocationImport, error) {
	store.beginCalled = true
	return nil, store.beginErr
}

func TestPhysicalStorageAdmissionIgnoresAggregateLogicalDiskAndTracksFilesystem(t *testing.T) {
	config := Config{
		WorkspaceRoot: "/workspace", StorageAdmissionMode: "physical",
		StorageRecoveryPercent: 70, StorageWarningPercent: 80, StorageDenyPercent: 90,
		MaximumVCPUs: 101, MaximumMemoryBytes: 101 << 30,
		MaximumDiskBytes: 200 << 30, MaximumInstances: 101,
	}
	pressure, err := newPhysicalStoragePressure(config)
	if err != nil {
		t.Fatal(err)
	}
	used := uint64(1 << 30)
	pressure.probe = func(string) (uint64, uint64, error) { return used, 200 << 30, nil }
	backend := &AssignmentBackend{config: validatedConfig{Config: config}, storagePressure: pressure}
	request := capacityReservation{vcpus: 1, memory: 1 << 30, disk: 2 << 30, instances: 1}
	for i := 0; i < 101; i++ {
		if err := pressure.admit(t.Context()); err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		if err := backend.reserve(request); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	if backend.reserved.disk != 202<<30 {
		t.Fatalf("logical reservation = %d", backend.reserved.disk)
	}
	logical := &AssignmentBackend{config: validatedConfig{Config: config}, reserved: capacityReservation{disk: 200 << 30}}
	if err := logical.reserve(request); err == nil {
		t.Fatal("logical aggregate disk limit was ignored")
	}
	used = 180 << 30
	if err := pressure.admit(t.Context()); !errors.Is(err, errPhysicalStoragePressure) {
		t.Fatalf("measured deny = %v", err)
	}
	used = 150 << 30
	if err := pressure.admit(t.Context()); !errors.Is(err, errPhysicalStoragePressure) {
		t.Fatalf("hysteresis deny = %v", err)
	}
	used = 140 << 30
	if err := pressure.admit(t.Context()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	probeFailure := errors.New("probe failed")
	pressure.probe = func(string) (uint64, uint64, error) { return 0, 0, probeFailure }
	if err := pressure.admit(context.Background()); !errors.Is(err, probeFailure) {
		t.Fatalf("probe failure = %v", err)
	}
}

func TestPhysicalStorageAdmissionCoversWorkspaceCommandsAndRelocation(t *testing.T) {
	const capacity = 64 << 20
	store := &admissionWorkspaceStore{receipts: make(map[string]workspacestore.Receipt), beginErr: errors.New("replayed import reached")}
	config := Config{
		WorkspaceRoot: "/workspace", WorkspaceStore: store,
		StorageAdmissionMode: "physical", StorageRecoveryPercent: 70,
		StorageWarningPercent: 80, StorageDenyPercent: 90,
	}
	pressure, err := newPhysicalStoragePressure(config)
	if err != nil {
		t.Fatal(err)
	}
	used := uint64(1)
	var probeErr error
	pressure.probe = func(string) (uint64, uint64, error) { return used, 100, probeErr }
	backend := &AssignmentBackend{config: validatedConfig{Config: config}, storagePressure: pressure}
	create := &runnerprotocol.LocalWorkspaceCommand{
		Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		OperationId: "create-source", WorkspaceId: "source", FencingToken: []byte("fencing-token"),
		LogicalCapacityBytes: capacity,
	}
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), create); err != nil {
		t.Fatalf("create source: %v", err)
	}
	clone := &runnerprotocol.LocalWorkspaceCommand{
		Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT,
		OperationId: "clone-target", WorkspaceId: "clone", SnapshotId: "source-snapshot",
		FencingToken: create.FencingToken, LogicalCapacityBytes: capacity,
	}
	relocation := &runnerprotocol.WorkspaceTransferFrame{
		OperationId: "relocate-target", WorkspaceId: "relocated", Generation: 1,
		Payload: &runnerprotocol.WorkspaceTransferFrame_Open{Open: &runnerprotocol.WorkspaceTransferOpen{
			LogicalCapacityBytes: capacity, FencingToken: create.FencingToken,
		}},
	}
	used = 95
	for _, command := range []*runnerprotocol.LocalWorkspaceCommand{
		{
			Kind: create.Kind, OperationId: "create-denied", WorkspaceId: "denied",
			FencingToken: create.FencingToken, LogicalCapacityBytes: capacity,
		}, clone,
	} {
		if _, err := backend.ExecuteLocalWorkspace(t.Context(), command); !errors.Is(err, errPhysicalStoragePressure) {
			t.Fatalf("%s denial = %v", command.OperationId, err)
		}
		if _, err := store.Inspect(t.Context(), command.WorkspaceId); !errors.Is(err, workspacestore.ErrWorkspaceNotFound) {
			t.Fatalf("%s allocated storage under pressure: %v", command.OperationId, err)
		}
	}
	if _, err := backend.BeginWorkspaceRelocationImport(t.Context(), relocation); !errors.Is(err, errPhysicalStoragePressure) {
		t.Fatalf("relocation denial = %v", err)
	}
	if _, err := store.Inspect(t.Context(), relocation.WorkspaceId); !errors.Is(err, workspacestore.ErrWorkspaceNotFound) {
		t.Fatalf("relocation allocated storage under pressure: %v", err)
	}
	if store.beginCalled {
		t.Fatal("relocation importer opened under pressure")
	}
	if evidence, err := backend.ExecuteLocalWorkspace(t.Context(), create); err != nil || evidence.Generation != 1 {
		t.Fatalf("create receipt replay under pressure = %+v, %v", evidence, err)
	}
	used = 1
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), clone); err != nil {
		t.Fatalf("clone after recovery: %v", err)
	}
	used = 95
	if evidence, err := backend.ExecuteLocalWorkspace(t.Context(), clone); err != nil || evidence.Generation != 1 {
		t.Fatalf("clone receipt replay under pressure = %+v, %v", evidence, err)
	}
	probeErr = errors.New("probe failed")
	for _, command := range []*runnerprotocol.LocalWorkspaceCommand{{
		Kind: create.Kind, OperationId: "create-probe-failed", WorkspaceId: "probe-failed",
		FencingToken: create.FencingToken, LogicalCapacityBytes: capacity,
	}, {
		Kind: clone.Kind, OperationId: "clone-probe-failed", WorkspaceId: "clone-probe-failed",
		SnapshotId: clone.SnapshotId, FencingToken: clone.FencingToken, LogicalCapacityBytes: capacity,
	}} {
		if _, err := backend.ExecuteLocalWorkspace(t.Context(), command); !errors.Is(err, probeErr) {
			t.Fatalf("%s probe failure = %v", command.OperationId, err)
		}
	}
	if _, err := backend.BeginWorkspaceRelocationImport(t.Context(), relocation); !errors.Is(err, probeErr) {
		t.Fatalf("relocation probe failure = %v", err)
	}
	if store.beginCalled {
		t.Fatal("relocation importer opened after failed probe")
	}
	store.replayed = true
	if _, err := backend.BeginWorkspaceRelocationImport(t.Context(), relocation); !errors.Is(err, store.beginErr) {
		t.Fatalf("completed relocation replay = %v", err)
	}
	if !store.beginCalled {
		t.Fatal("completed relocation import did not reach WorkspaceStore")
	}
}

// TestStoragePressureDenialDoesNotFailReadiness keeps a Runner over the deny
// threshold connected. Only a connected Runner receives the stops and deletes
// that free storage; admission still refuses new work until recovery.
func TestStoragePressureDenialDoesNotFailReadiness(t *testing.T) {
	config := Config{
		WorkspaceRoot: "/workspace", StorageAdmissionMode: "physical",
		StorageRecoveryPercent: 70, StorageWarningPercent: 80, StorageDenyPercent: 90,
	}
	pressure, err := newPhysicalStoragePressure(config)
	if err != nil {
		t.Fatal(err)
	}
	used := uint64(95)
	var probeErr error
	pressure.probe = func(string) (uint64, uint64, error) { return used, 100, probeErr }
	backend := &AssignmentBackend{config: validatedConfig{Config: config}, storagePressure: pressure}
	if observed, err := backend.storagePressureReadiness(t.Context()); err != nil ||
		observed.Status != "admission_denied" || observed.ObservedAtUnixMs == 0 {
		t.Fatalf("readiness over the deny threshold = %+v, %v, want ready reporting admission_denied", observed, err)
	}
	if err := pressure.admit(t.Context()); !errors.Is(err, errPhysicalStoragePressure) {
		t.Fatalf("admission over the deny threshold = %v", err)
	}
	used = 70
	if observed, err := backend.storagePressureReadiness(t.Context()); err != nil || observed.Status != "healthy" {
		t.Fatalf("readiness after recovery = %+v, %v", observed, err)
	}
	if state, err := pressure.observe(t.Context()); err != nil || state != "healthy" {
		t.Fatalf("state after recovery = %q, %v", state, err)
	}
	probeErr = errors.New("probe failed")
	if _, err := backend.storagePressureReadiness(t.Context()); !errors.Is(err, probeErr) {
		t.Fatalf("readiness with failed probe = %v", err)
	}
	if observed, err := (&AssignmentBackend{}).storagePressureReadiness(t.Context()); err != nil || observed != nil {
		t.Fatalf("readiness without physical admission = %+v, %v, want no report", observed, err)
	}
}
