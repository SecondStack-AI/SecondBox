//go:build linux

package gvisor

import (
	"context"
	"errors"
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
	if err := backend.storagePressureReadiness(t.Context()); err != nil {
		t.Fatalf("readiness over the deny threshold = %v, want ready", err)
	}
	if err := pressure.admit(t.Context()); !errors.Is(err, errPhysicalStoragePressure) {
		t.Fatalf("admission over the deny threshold = %v", err)
	}
	used = 70
	if err := backend.storagePressureReadiness(t.Context()); err != nil {
		t.Fatalf("readiness after recovery = %v", err)
	}
	if state, err := pressure.observe(t.Context()); err != nil || state != "healthy" {
		t.Fatalf("state after recovery = %q, %v", state, err)
	}
	probeErr = errors.New("probe failed")
	if err := backend.storagePressureReadiness(t.Context()); !errors.Is(err, probeErr) {
		t.Fatalf("readiness with failed probe = %v", err)
	}
	if err := (&AssignmentBackend{}).storagePressureReadiness(t.Context()); err != nil {
		t.Fatalf("readiness without physical admission = %v", err)
	}
}
