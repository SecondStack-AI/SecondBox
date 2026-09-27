package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestPhysicalStorageAdmissionSkipsOnlyDiskCapacity(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		Capacity: Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, Instances: 1},
	}
	runner := RunnerSnapshot{
		ID: "home", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, Instances: 1},
		Reserved:   Capacity{DiskBytes: 2 << 30},
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	if _, err := SelectHomeRunner(runner.ID, requirements, []RunnerSnapshot{runner}, now, time.Minute); !errors.Is(err, ErrHomeRunnerUnavailable) {
		t.Fatalf("logical admission error = %v", err)
	}
	runner.Capabilities[contracts.RunnerCapabilityPhysicalStorageAdmission] = true
	if _, err := SelectHomeRunner(runner.ID, requirements, []RunnerSnapshot{runner}, now, time.Minute); err != nil {
		t.Fatalf("physical admission error = %v", err)
	}
	runner.Allocatable.DiskBytes = 1 << 30
	if _, err := SelectHomeRunner(runner.ID, requirements, []RunnerSnapshot{runner}, now, time.Minute); !errors.Is(err, ErrHomeRunnerUnavailable) {
		t.Fatalf("individual disk ceiling error = %v", err)
	}
	runner.Allocatable.DiskBytes = 2 << 30
	runner.Allocatable.Instances = 2
	if _, err := SelectHomeRunner(runner.ID, requirements, []RunnerSnapshot{runner}, now, time.Minute); !errors.Is(err, ErrHomeRunnerUnavailable) {
		t.Fatalf("Firecracker per-Instance disk ceiling error = %v", err)
	}
	runner.Allocatable.Instances = 1
	runner.BackendKind = "gvisor"
	runner.Materializations[0].BackendKind = "gvisor"
	if _, err := SelectHomeRunner(runner.ID, requirements, []RunnerSnapshot{runner}, now, time.Minute); err != nil {
		t.Fatalf("gVisor physical admission error = %v", err)
	}
	runner.Allocatable.DiskBytes = 1 << 30
	if _, err := SelectHomeRunner(runner.ID, requirements, []RunnerSnapshot{runner}, now, time.Minute); !errors.Is(err, ErrHomeRunnerUnavailable) {
		t.Fatalf("gVisor individual disk ceiling error = %v", err)
	}
	runner.BackendKind = "firecracker"
	runner.Materializations[0].BackendKind = "firecracker"
	runner.Allocatable.DiskBytes = 2 << 30
	runner.Allocatable.MemoryBytes = 0
	if _, err := SelectHomeRunner(runner.ID, requirements, []RunnerSnapshot{runner}, now, time.Minute); !errors.Is(err, ErrHomeRunnerUnavailable) {
		t.Fatalf("memory admission error = %v", err)
	}
}

func TestSelectRunnerFiltersCompatibilityCapacityHealthAndDrain(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities: []string{"local-workspace", "network-policy"},
		Capacity:             Capacity{VCPUCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30, Instances: 1},
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

func TestSelectRunnerPrefersStableIDAmongEquallyFreeRunners(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities: []string{"local-workspace"},
		Capacity:             Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
	}
	base := RunnerSnapshot{
		PoolName: "general", Architecture: "amd64", BackendKind: "firecracker", Capabilities: readyCapabilities(),
		Allocatable: abundantCapacity(), DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	candidates := []RunnerSnapshot{
		withRunnerID(base, "runner-z"),
		withRunnerID(base, "runner-b"),
		withRunnerID(base, "runner-a"),
	}

	selected, err := SelectRunner(requirements, candidates, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "runner-a" {
		t.Fatalf("selected runner = %q, want runner-a stable identity", selected.ID)
	}
}

func TestSelectRunnerRejectsUnavailablePool(t *testing.T) {
	_, err := SelectRunner(
		Requirements{
			PoolName: "general", Architecture: "amd64",
		},
		nil,
		time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		30*time.Second,
	)
	if !errors.Is(err, ErrNoCompatibleRunner) {
		t.Fatalf("SelectRunner error = %v, want ErrNoCompatibleRunner", err)
	}
}

func TestSelectRunnerUsesPreparedImageWithoutFixedMaterialization(t *testing.T) {
	now := time.Now().UTC()
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities: []string{"client-selected-image"},
		Capacity:             Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, Instances: 1},
	}
	runner := RunnerSnapshot{
		ID: "home", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	runner.Capabilities["client-selected-image"] = true
	if _, err := SelectRunner(requirements, []RunnerSnapshot{runner}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	delete(runner.Capabilities, "client-selected-image")
	if _, err := SelectRunner(requirements, []RunnerSnapshot{runner}, now, time.Minute); !errors.Is(err, ErrNoCompatibleRunner) {
		t.Fatalf("unsupported selected image placement = %v", err)
	}
}

func TestSelectRunnerRequiresExactPinnedEgressContext(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	required := "tenant-blue"
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64", EgressContext: &required,
		RequiredCapabilities: []string{"local-workspace", "network-policy"},
		Capacity:             Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
	}
	base := RunnerSnapshot{
		PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	unsupported := base
	unsupported.ID = "runner-unsupported"
	unsupported.SupportedEgressContexts = []string{"tenant-green"}
	if _, err := SelectRunner(
		requirements, []RunnerSnapshot{unsupported}, now, 30*time.Second,
	); !errors.Is(err, ErrEgressContextUnavailable) {
		t.Fatalf("context-incompatible selection error = %v, want ErrEgressContextUnavailable", err)
	}
	supported := base
	supported.ID = "runner-supported"
	supported.SupportedEgressContexts = []string{"tenant-green", required}
	selected, err := SelectRunner(
		requirements, []RunnerSnapshot{unsupported, supported}, now, 30*time.Second,
	)
	if err != nil || selected.ID != supported.ID {
		t.Fatalf("context-compatible selection = %q, %v; want %q", selected.ID, err, supported.ID)
	}
}

func TestSelectRunnerDoesNotPlaceOnDrainingContextCompatibleRunner(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	required := "tenant-blue"
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64", EgressContext: &required,
		RequiredCapabilities: []string{"local-workspace", "network-policy"},
		Capacity:             Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
	}
	base := RunnerSnapshot{
		PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	draining := base
	draining.ID = "runner-draining"
	draining.DrainPhase = DrainPhaseDraining
	draining.SupportedEgressContexts = []string{required}
	unsupported := base
	unsupported.ID = "runner-active-unsupported"
	unsupported.SupportedEgressContexts = []string{"tenant-green"}
	if _, err := SelectRunner(
		requirements, []RunnerSnapshot{draining, unsupported}, now, 30*time.Second,
	); !errors.Is(err, ErrEgressContextUnavailable) {
		t.Fatalf("selection error = %v, want active context availability failure", err)
	}
}

func TestSelectRunnerWithNoPinnedContextDoesNotRequireAdvertisement(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 30, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities: []string{"local-workspace", "network-policy"},
		Capacity:             Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
	}
	runner := RunnerSnapshot{
		ID: "runner-isolated", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1, Materializations: readyMaterializations(),
	}
	if selected, err := SelectRunner(
		requirements, []RunnerSnapshot{runner}, now, 30*time.Second,
	); err != nil || selected.ID != runner.ID {
		t.Fatalf("context-free selection = %q, %v; want %q", selected.ID, err, runner.ID)
	}
}

// TestSelectRunnerAcceptsAnyVerifiedBundleForItsBackend pins the upgrade
// property: a default-image start needs a verified materialization for the
// Runner's own backend, whichever release bundle that materialization holds.
func TestSelectRunnerAcceptsAnyVerifiedBundleForItsBackend(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities: []string{"local-workspace", "network-policy"},
		Capacity:             Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
	}
	runner := RunnerSnapshot{
		ID: "runner-upgraded", PoolName: "general", Architecture: "amd64", BackendKind: "firecracker",
		Capabilities: readyCapabilities(), Allocatable: abundantCapacity(),
		DrainPhase: DrainPhaseActive, LastHeartbeatAt: now,
		Materializations: []MaterializationSnapshot{{
			BackendKind: "firecracker", Architecture: "amd64",
			RuntimeDigest: "sha256:newer-runtime", ToolchainDigest: "sha256:newer-toolchain",
			Digest: "sha256:materialization",
		}},
	}
	if _, err := SelectRunner(requirements, []RunnerSnapshot{runner}, now, 30*time.Second); err != nil {
		t.Fatalf("Runner with a newer verified bundle was refused: %v", err)
	}
	runner.Materializations = nil
	if _, err := SelectRunner(requirements, []RunnerSnapshot{runner}, now, 30*time.Second); !errors.Is(err, ErrNoCompatibleRunner) {
		t.Fatalf("Runner without a verified materialization selected: %v", err)
	}
}

func TestSelectHomeRunnerNeverFallsBackToCompatibleReplacement(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	requirements := Requirements{
		PoolName: "general", Architecture: "amd64",
		RequiredCapabilities: []string{"local-workspace"},
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
		RequiredCapabilities: []string{"local-workspace"},
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

func withRunnerID(runner RunnerSnapshot, id string) RunnerSnapshot {
	runner.ID = id
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
		Capacity: Capacity{VCPUCount: 1, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Instances: 1},
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
