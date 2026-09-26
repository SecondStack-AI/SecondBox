//go:build linux

package gvisor

import (
	"context"
	"errors"
	"testing"
)

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
