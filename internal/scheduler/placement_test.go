package scheduler

import (
	"errors"
	"testing"
	"time"
)

func TestSelectRunnerFiltersCompatibilityCapacityHealthAndDrain(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", BackendKind: "firecracker", Architecture: "amd64",
		RequiredCapabilities:     []string{"checkpoint", "network-policy"},
		GuestProtocolGeneration:  1,
		Capacity:                 Capacity{CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30, Instances: 1},
		PreferredArtifactDigests: []string{"sha256:runtime", "sha256:toolchain"},
	}
	candidates := []RunnerSnapshot{
		{
			ID: "runner-draining", PoolName: "general", Architecture: "amd64",
			Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
			DrainPhase: DrainPhaseDraining, LastHeartbeatAt: now,
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1,
		},
		{
			ID: "runner-stale", PoolName: "general", Architecture: "amd64",
			Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
			DrainPhase: DrainPhaseActive, LastHeartbeatAt: now.Add(-31 * time.Second),
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1,
		},
		{
			ID: "runner-too-small", PoolName: "general", Architecture: "amd64",
			Capabilities: readyCapabilities(),
			Allocatable:  Capacity{CPUMillis: 1000, MemoryBytes: 2 << 30, DiskBytes: 10 << 30, Instances: 1},
			DrainPhase:   DrainPhaseActive, LastHeartbeatAt: now,
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1,
		},
		{
			ID: "runner-compatible", PoolName: "general", Architecture: "amd64",
			Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
			DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1,
		},
	}

	selected, err := SelectRunner(requirements, candidates, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "runner-compatible" {
		t.Fatalf("selected runner = %q, want runner-compatible", selected.ID)
	}
}

func TestSelectRunnerPrefersWorkspaceAndArtifactLocalityThenStableID(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", BackendKind: "firecracker", Architecture: "amd64",
		RequiredCapabilities:     []string{"checkpoint"},
		GuestProtocolGeneration:  1,
		Capacity:                 Capacity{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
		WorkspaceCheckpointID:    "checkpoint-current",
		PreferredArtifactDigests: []string{"sha256:runtime", "sha256:toolchain"},
	}
	base := RunnerSnapshot{
		PoolName: "general", Architecture: "amd64", Capabilities: readyCapabilities(),
		Allocatable: abundantCapacity(), DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1,
	}
	candidates := []RunnerSnapshot{
		withRunnerEvidence(base, "runner-z", nil, []string{"sha256:runtime", "sha256:toolchain"}),
		withRunnerEvidence(base, "runner-b", []string{"checkpoint-current"}, []string{"sha256:runtime"}),
		withRunnerEvidence(base, "runner-a", []string{"checkpoint-current"}, []string{"sha256:runtime"}),
	}

	selected, err := SelectRunner(requirements, candidates, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "runner-a" {
		t.Fatalf("selected runner = %q, want stable runner-a locality tie-break", selected.ID)
	}
}

func TestSelectRunnerRejectsUnavailablePool(t *testing.T) {
	_, err := SelectRunner(
		Requirements{
			PoolName: "general", BackendKind: "firecracker", Architecture: "amd64",
			GuestProtocolGeneration: 1,
		},
		nil,
		time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		30*time.Second,
	)
	if !errors.Is(err, ErrNoCompatibleRunner) {
		t.Fatalf("SelectRunner error = %v, want ErrNoCompatibleRunner", err)
	}
}

func readyCapabilities() map[string]bool {
	return map[string]bool{
		"firecracker": true, "kvm": true, "jailer": true, "cgroup": true,
		"network-policy": true, "storage": true, "cleanup": true, "checkpoint": true,
	}
}

func abundantCapacity() Capacity {
	return Capacity{CPUMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8, Operations: 32}
}

func withRunnerEvidence(
	runner RunnerSnapshot,
	id string,
	checkpoints []string,
	artifacts []string,
) RunnerSnapshot {
	runner.ID = id
	runner.WorkspaceCheckpoints = checkpoints
	runner.ArtifactDigests = artifacts
	return runner
}
