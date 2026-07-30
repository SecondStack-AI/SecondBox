package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStressConfigRequiresEveryWorkloadAndComputesBinding(t *testing.T) {
	config := validStressConfig()
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	binding := config.configuredBinding()
	if binding.Name != "SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB" || binding.Capacity != 4 {
		t.Fatalf("binding = %#v", binding)
	}
	config.Workloads = config.Workloads[:4]
	if err := config.validate(); err == nil || !strings.Contains(err.Error(), "workloads") {
		t.Fatalf("missing workload error = %v", err)
	}
}

func TestReadStressConfigRejectsUnknownFieldsAndSymlink(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stress.json")
	data := `{"version":1,"unknown":true}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readStressConfig(path); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field error = %v", err)
	}
	link := filepath.Join(directory, "stress-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readStressConfig(link); err == nil || !strings.Contains(err.Error(), "non-symbolic-link") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestGuestIPCapacityMatchesRunnerAccounting(t *testing.T) {
	for cidr, want := range map[string]int{
		"172.31.0.1/24": 253,
		"172.31.0.1/30": 1,
		"172.31.0.1/31": 0,
		"not-a-cidr":    0,
	} {
		if got := guestIPCapacity(cidr); got != want {
			t.Fatalf("guestIPCapacity(%q) = %d, want %d", cidr, got, want)
		}
	}
}

func validStressConfig() stressConfig {
	return stressConfig{
		Version: 1, RunnerPoolName: "stress-pool", ProfileName: "stress-profile",
		TenantRef: "stress-tenant", SubjectRef: "stress-subject",
		Workloads: append([]string(nil), requiredWorkloads...), ConcurrencyLevels: []int{1, 2, 4},
		DurationSeconds: 10, RequestTimeoutMilliseconds: 65000,
		OperationTimeoutSeconds: 180, PollIntervalMilliseconds: 250,
		TimingWindowSeconds: 600, LatencyDegradationRatio: 1.5,
		FileTransferBytes: 4096, StreamingOutputBytes: 4096,
		Runner: stressRunnerConfig{
			SandboxMaxVCPUs: 2, SandboxMemoryMiB: 512, SandboxDiskMiB: 1024,
			MemoryBudgetMiB: 2048, MaxConcurrentPerSandbox: 4, MaxConcurrentGlobal: 16,
			FileTransferMaxBytes: 1 << 20, BridgeName: "sbxstress0",
			BridgeCIDR: "172.31.0.1/24", GuestCIDR: "172.31.0.0/24",
			GuestIP: "172.31.0.2", TapPrefix: "sbst",
			StoragePressureRecoveryPercent: 70, StoragePressureWarningPercent: 80,
			StoragePressureDenyPercent: 90, FirecrackerCPUTemplate: "T2",
			FirecrackerKernelArgs: "console=ttyS0 reboot=k panic=1 pci=off",
		},
		Profile: stressProfileConfig{
			CPUMillis: 1000, MemoryBytes: 512 << 20, WorkspaceBytes: 1 << 30,
			ProcessLimit: 128, ConcurrentOperations: 4, DrainGraceSeconds: 30,
			IdleSeconds: 3600, MaximumDurationSeconds: 7200, LeaseSeconds: 60,
			SnapshotLimit: 32, SnapshotRetentionSeconds: 86400,
			ArtifactRetentionSeconds: 86400, MaximumDeadlineMilliseconds: 60000,
			MaximumBufferedOutputBytes: 1 << 20, StreamWindowBytes: 65536,
			MaximumTransferBytes: 1 << 20, TerminalDetachSeconds: 30,
		},
		SubjectMaxSandboxes: 100, SubjectMaxActiveInstances: 20,
		SubjectMaxConcurrentOps: 100, SubjectMaxSnapshots: 100,
		SubjectMaxArtifactBytes: 1 << 30, SubjectMaxArtifacts: 100,
		SubjectMaxPortSessions: 100, SubjectMaxCPUMillis: 100000,
		SubjectMaxMemoryBytes: 100 << 30,
	}
}
