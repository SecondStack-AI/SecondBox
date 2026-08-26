package scheduler

import (
	"errors"
	"testing"
	"time"
)

func TestSelectRunnerFiltersCompatibilityCapacityHealthAndDrain(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities:     []string{"local-workspace", "network-policy"},
		GuestProtocolGeneration:  1,
		Capacity:                 Capacity{VCPUCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30, Instances: 1},
		PreferredArtifactDigests: []string{"sha256:runtime", "sha256:toolchain"},
	}
	candidates := []RunnerSnapshot{
		{
			ID: "runner-draining", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
			Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
			DrainPhase: DrainPhaseDraining, LastHeartbeatAt: now,
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
		},
		{
			ID: "runner-stale", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
			Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
			DrainPhase: DrainPhaseActive, LastHeartbeatAt: now.Add(-31 * time.Second),
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
		},
		{
			ID: "runner-too-small", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
			Capabilities: readyCapabilities(),
			Allocatable:  Capacity{VCPUCount: 1, MemoryBytes: 2 << 30, DiskBytes: 10 << 30, Instances: 1},
			DrainPhase:   DrainPhaseActive, LastHeartbeatAt: now,
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
		},
		{
			ID: "runner-compatible", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
			Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
			DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
			GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
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

func TestSelectRunnerPrefersArtifactLocalityThenStableID(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities:     []string{"local-workspace"},
		GuestProtocolGeneration:  1,
		Capacity:                 Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
		PreferredArtifactDigests: []string{"sha256:runtime", "sha256:toolchain"},
	}
	base := RunnerSnapshot{
		PoolName: "general", Architecture: "amd64", BackendKind: "firecracker", Capabilities: readyCapabilities(),
		Allocatable: abundantCapacity(), DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	candidates := []RunnerSnapshot{
		withRunnerEvidence(base, "runner-z", []string{"sha256:runtime", "sha256:toolchain"}),
		withRunnerEvidence(base, "runner-b", []string{"sha256:runtime"}),
		withRunnerEvidence(base, "runner-a", []string{"sha256:runtime"}),
	}

	selected, err := SelectRunner(requirements, candidates, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "runner-z" {
		t.Fatalf("selected runner = %q, want runner-z artifact locality", selected.ID)
	}
}

func TestSelectRunnerRejectsUnavailablePool(t *testing.T) {
	_, err := SelectRunner(
		Requirements{
			PoolName: "general", Architecture: "amd64",
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

func TestSelectRunnerRejectsMissingExactMaterialization(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities:     []string{"local-workspace", "network-policy"},
		GuestProtocolGeneration:  1,
		Capacity:                 Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
		PreferredArtifactDigests: []string{"sha256:runtime", "sha256:toolchain"},
	}
	runner := RunnerSnapshot{
		ID: "runner-materialized-for-other-toolchain", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1,
		Materializations: []MaterializationSnapshot{{
			BackendKind: "firecracker", Architecture: "amd64",
			RuntimeDigest: "sha256:runtime", ToolchainDigest: "sha256:different-toolchain",
			Digest: "sha256:materialization",
		}},
	}

	if _, err := SelectRunner(requirements, []RunnerSnapshot{runner}, now, 30*time.Second); !errors.Is(err, ErrNoCompatibleRunner) {
		t.Fatalf("Runner without exact materialization selected: %v", err)
	}
}

func TestSelectHomeRunnerNeverFallsBackToCompatibleReplacement(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities:    []string{"local-workspace"},
		GuestProtocolGeneration: 1,
		Capacity: Capacity{
			VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30,
			Instances: 1, Operations: 1,
		},
	}
	replacement := RunnerSnapshot{
		ID: "runner-replacement", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	if _, err := SelectHomeRunner(
		"runner-home", requirements, []RunnerSnapshot{replacement},
		now, 30*time.Second,
	); !errors.Is(err, ErrHomeRunnerUnavailable) {
		t.Fatalf("absent home selection error = %v, want ErrHomeRunnerUnavailable", err)
	}
}

func TestSelectHomeRunnerRejectsDrainingHomeWithoutRelocation(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities:    []string{"local-workspace"},
		GuestProtocolGeneration: 1,
		Capacity: Capacity{
			VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30,
			Instances: 1, Operations: 1,
		},
	}
	home := RunnerSnapshot{
		ID: "runner-home", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseDraining, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	replacement := home
	replacement.ID = "runner-replacement"
	replacement.DrainPhase = DrainPhaseActive
	if _, err := SelectHomeRunner(
		home.ID, requirements, []RunnerSnapshot{home, replacement},
		now, 30*time.Second,
	); !errors.Is(err, ErrHomeRunnerUnavailable) {
		t.Fatalf("draining home selection error = %v, want ErrHomeRunnerUnavailable", err)
	}
}

func readyCapabilities() map[string]bool {
	return map[string]bool{
		"compute":        true,
		"network-policy": true, "storage": true, "cleanup": true, "local-workspace": true,
	}
}

func abundantCapacity() Capacity {
	return Capacity{VCPUCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8, Operations: 32}
}

func readyMaterializations() []MaterializationSnapshot {
	return []MaterializationSnapshot{{
		BackendKind: "firecracker", Architecture: "amd64",
		RuntimeDigest: "sha256:runtime", ToolchainDigest: "sha256:toolchain",
		Digest: "sha256:materialization",
	}}
}

func withRunnerEvidence(
	runner RunnerSnapshot,
	id string,
	artifacts []string,
) RunnerSnapshot {
	runner.ID = id
	runner.ArtifactDigests = artifacts
	runner.Materializations = readyMaterializations()
	return runner
}

// TestSelectRunnerAdmitsSnapshotResumeOnlyOnAdvertisingRunners pins the hard
// placement filter a snapshot_resume Profile depends on. Resume has no cold-boot
// fallback, so a Runner without resume capacity must be invisible to it rather
// than chosen and failed at start.
func TestSelectRunnerAdmitsSnapshotResumeOnlyOnAdvertisingRunners(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities: []string{
			"network-policy", "storage", "cleanup", "local-workspace", "snapshot-resume",
		},
		GuestProtocolGeneration:  1,
		Capacity:                 Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
		PreferredArtifactDigests: []string{"sha256:runtime", "sha256:toolchain"},
	}
	coldOnly := RunnerSnapshot{
		ID: "runner-cold-only", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	resumeCapable := coldOnly
	resumeCapable.ID = "runner-resume"
	resumeCapable.Capabilities = readyCapabilities()
	resumeCapable.Capabilities["snapshot-resume"] = true

	if _, err := SelectRunner(
		requirements, []RunnerSnapshot{coldOnly}, now, 30*time.Second,
	); !errors.Is(err, ErrNoCompatibleRunner) {
		t.Fatalf("cold-only Runner admitted a snapshot_resume Profile: %v", err)
	}
	selected, err := SelectRunner(
		requirements, []RunnerSnapshot{coldOnly, resumeCapable}, now, 30*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "runner-resume" {
		t.Fatalf("selected runner = %q, want runner-resume", selected.ID)
	}

	// A cold_boot Profile carries no resume requirement and must keep placing on
	// every Runner, including the one that also advertises resume capacity.
	coldRequirements := requirements
	coldRequirements.RequiredCapabilities = []string{
		"network-policy", "storage", "cleanup", "local-workspace",
	}
	if _, err := SelectRunner(
		coldRequirements, []RunnerSnapshot{coldOnly}, now, 30*time.Second,
	); err != nil {
		t.Fatalf("cold_boot Profile lost its ordinary Runner: %v", err)
	}
}
