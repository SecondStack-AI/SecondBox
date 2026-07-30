package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
)

const (
	workloadSandboxCreate   = "sandbox_create"
	workloadBufferedExec    = "buffered_exec"
	workloadStreamingExec   = "streaming_exec"
	workloadFileTransfer    = "file_transfer"
	workloadSnapshotRestore = "snapshot_restore"
)

var requiredWorkloads = []string{
	workloadSandboxCreate,
	workloadBufferedExec,
	workloadStreamingExec,
	workloadFileTransfer,
	workloadSnapshotRestore,
}

type stressConfig struct {
	Version                    int                 `json:"version"`
	RunnerPoolName             string              `json:"runnerPoolName"`
	ProfileName                string              `json:"profileName"`
	TenantRef                  string              `json:"tenantRef"`
	SubjectRef                 string              `json:"subjectRef"`
	Workloads                  []string            `json:"workloads"`
	ConcurrencyLevels          []int               `json:"concurrencyLevels"`
	DurationSeconds            int                 `json:"durationSeconds"`
	RequestTimeoutMilliseconds int                 `json:"requestTimeoutMilliseconds"`
	OperationTimeoutSeconds    int                 `json:"operationTimeoutSeconds"`
	PollIntervalMilliseconds   int                 `json:"pollIntervalMilliseconds"`
	TimingWindowSeconds        int                 `json:"timingWindowSeconds"`
	LatencyDegradationRatio    float64             `json:"latencyDegradationRatio"`
	FileTransferBytes          int                 `json:"fileTransferBytes"`
	StreamingOutputBytes       int                 `json:"streamingOutputBytes"`
	Runner                     stressRunnerConfig  `json:"runner"`
	Profile                    stressProfileConfig `json:"profile"`
	SubjectMaxSandboxes        int                 `json:"subjectMaxSandboxes"`
	SubjectMaxActiveInstances  int                 `json:"subjectMaxActiveInstances"`
	SubjectMaxConcurrentOps    int                 `json:"subjectMaxConcurrentOperations"`
	SubjectMaxSnapshots        int                 `json:"subjectMaxSnapshots"`
	SubjectMaxArtifactBytes    int64               `json:"subjectMaxArtifactBytes"`
	SubjectMaxArtifacts        int                 `json:"subjectMaxArtifacts"`
	SubjectMaxPortSessions     int                 `json:"subjectMaxPortSessions"`
	SubjectMaxCPUMillis        int64               `json:"subjectMaxCpuMillis"`
	SubjectMaxMemoryBytes      int64               `json:"subjectMaxMemoryBytes"`
}

type stressRunnerConfig struct {
	SandboxMaxVCPUs                int `json:"sandboxMaxVcpus"`
	SandboxMemoryMiB               int `json:"sandboxMemoryMiB"`
	SandboxDiskMiB                 int `json:"sandboxDiskMiB"`
	MemoryBudgetMiB                int `json:"memoryBudgetMiB"`
	MaxConcurrentPerSandbox        int `json:"maxConcurrentPerSandbox"`
	MaxConcurrentGlobal            int `json:"maxConcurrentGlobal"`
	FileTransferMaxBytes           int `json:"fileTransferMaxBytes"`
	StoragePressureRecoveryPercent int `json:"storagePressureRecoveryPercent"`
	StoragePressureWarningPercent  int `json:"storagePressureWarningPercent"`
	StoragePressureDenyPercent     int `json:"storagePressureAdmissionDenyPercent"`
}

type stressProfileConfig struct {
	CPUMillis                   int64 `json:"cpuMillis"`
	MemoryBytes                 int64 `json:"memoryBytes"`
	WorkspaceBytes              int64 `json:"workspaceBytes"`
	ProcessLimit                int64 `json:"processLimit"`
	ConcurrentOperations        int64 `json:"concurrentOperations"`
	DrainGraceSeconds           int64 `json:"drainGraceSeconds"`
	IdleSeconds                 int64 `json:"idleSeconds"`
	MaximumDurationSeconds      int64 `json:"maximumDurationSeconds"`
	LeaseSeconds                int64 `json:"leaseSeconds"`
	SnapshotLimit               int64 `json:"snapshotLimit"`
	SnapshotRetentionSeconds    int64 `json:"snapshotRetentionSeconds"`
	ArtifactRetentionSeconds    int64 `json:"artifactRetentionSeconds"`
	MaximumDeadlineMilliseconds int64 `json:"maximumDeadlineMilliseconds"`
	MaximumBufferedOutputBytes  int64 `json:"maximumBufferedOutputBytes"`
	StreamWindowBytes           int64 `json:"streamWindowBytes"`
	MaximumTransferBytes        int64 `json:"maximumTransferBytes"`
	TerminalDetachSeconds       int64 `json:"terminalDetachSeconds"`
}

func readStressConfig(path string) (stressConfig, error) {
	if strings.TrimSpace(path) == "" {
		return stressConfig{}, errors.New("SecondBox stress config path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return stressConfig{}, fmt.Errorf("SecondBox stress config inspect failed: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return stressConfig{}, errors.New("SecondBox stress config must be a regular non-symbolic-link file")
	}
	file, err := os.Open(path)
	if err != nil {
		return stressConfig{}, fmt.Errorf("SecondBox stress config open failed: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var config stressConfig
	if err := decoder.Decode(&config); err != nil {
		return stressConfig{}, fmt.Errorf("SecondBox stress config decode failed: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return stressConfig{}, err
	}
	if err := config.validate(); err != nil {
		return stressConfig{}, err
	}
	return config, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("SecondBox stress config trailing content failed: %w", err)
	}
	return errors.New("SecondBox stress config must contain one JSON object")
}

func (config stressConfig) validate() error {
	if config.Version != 1 {
		return errors.New("SecondBox stress config version must be 1")
	}
	for name, value := range map[string]string{
		"runnerPoolName": config.RunnerPoolName,
		"profileName":    config.ProfileName,
		"tenantRef":      config.TenantRef,
		"subjectRef":     config.SubjectRef,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("SecondBox stress config %s is required", name)
		}
	}
	if len(config.Workloads) != len(requiredWorkloads) {
		return fmt.Errorf(
			"SecondBox stress config workloads must contain exactly %s",
			strings.Join(requiredWorkloads, ", "),
		)
	}
	seenWorkloads := make(map[string]struct{}, len(config.Workloads))
	for _, workload := range config.Workloads {
		if !slices.Contains(requiredWorkloads, workload) {
			return fmt.Errorf("SecondBox stress config workload %q is unsupported", workload)
		}
		if _, duplicate := seenWorkloads[workload]; duplicate {
			return fmt.Errorf("SecondBox stress config workload %q is duplicated", workload)
		}
		seenWorkloads[workload] = struct{}{}
	}
	if len(config.ConcurrencyLevels) == 0 {
		return errors.New("SecondBox stress config concurrencyLevels is required")
	}
	previous := 0
	for _, level := range config.ConcurrencyLevels {
		if level <= previous {
			return errors.New("SecondBox stress config concurrencyLevels must be positive and strictly increasing")
		}
		previous = level
	}
	for name, value := range map[string]int{
		"durationSeconds":                config.DurationSeconds,
		"requestTimeoutMilliseconds":     config.RequestTimeoutMilliseconds,
		"operationTimeoutSeconds":        config.OperationTimeoutSeconds,
		"pollIntervalMilliseconds":       config.PollIntervalMilliseconds,
		"timingWindowSeconds":            config.TimingWindowSeconds,
		"fileTransferBytes":              config.FileTransferBytes,
		"streamingOutputBytes":           config.StreamingOutputBytes,
		"subjectMaxSandboxes":            config.SubjectMaxSandboxes,
		"subjectMaxActiveInstances":      config.SubjectMaxActiveInstances,
		"subjectMaxConcurrentOperations": config.SubjectMaxConcurrentOps,
		"subjectMaxSnapshots":            config.SubjectMaxSnapshots,
		"subjectMaxArtifacts":            config.SubjectMaxArtifacts,
		"subjectMaxPortSessions":         config.SubjectMaxPortSessions,
	} {
		if value < 1 {
			return fmt.Errorf("SecondBox stress config %s must be positive", name)
		}
	}
	if config.TimingWindowSeconds < 60 || config.TimingWindowSeconds > 3600 {
		return errors.New("SecondBox stress config timingWindowSeconds must be from 60 through 3600")
	}
	if config.LatencyDegradationRatio <= 1 {
		return errors.New("SecondBox stress config latencyDegradationRatio must be greater than 1")
	}
	for name, value := range map[string]int64{
		"subjectMaxArtifactBytes": config.SubjectMaxArtifactBytes,
		"subjectMaxCpuMillis":     config.SubjectMaxCPUMillis,
		"subjectMaxMemoryBytes":   config.SubjectMaxMemoryBytes,
	} {
		if value < 1 {
			return fmt.Errorf("SecondBox stress config %s must be positive", name)
		}
	}
	if err := config.Runner.validate(); err != nil {
		return err
	}
	if err := config.Profile.validate(); err != nil {
		return err
	}
	if int64(config.FileTransferBytes) > config.Profile.MaximumTransferBytes ||
		config.FileTransferBytes > config.Runner.FileTransferMaxBytes {
		return errors.New("SecondBox stress fileTransferBytes exceeds a configured transfer limit")
	}
	if int64(config.StreamingOutputBytes) > config.Profile.MaximumBufferedOutputBytes {
		return errors.New("SecondBox stress streamingOutputBytes exceeds maximumBufferedOutputBytes")
	}
	if config.Profile.MemoryBytes > int64(config.Runner.SandboxMemoryMiB)<<20 {
		return errors.New("SecondBox stress Profile memory exceeds runner sandbox memory")
	}
	if config.Profile.WorkspaceBytes > int64(config.Runner.SandboxDiskMiB)<<20 {
		return errors.New("SecondBox stress Profile workspace exceeds runner sandbox disk")
	}
	if config.Profile.CPUMillis > int64(config.Runner.SandboxMaxVCPUs)*1000 {
		return errors.New("SecondBox stress Profile CPU exceeds runner sandbox vCPU capacity")
	}
	if config.Runner.MemoryBudgetMiB < config.Runner.SandboxMemoryMiB {
		return errors.New("SecondBox stress runner memory budget cannot admit one Sandbox")
	}
	if config.SubjectMaxCPUMillis < config.Profile.CPUMillis ||
		config.SubjectMaxMemoryBytes < config.Profile.MemoryBytes {
		return errors.New("SecondBox stress subject quota cannot admit one configured Profile")
	}
	return nil
}

func (config stressRunnerConfig) validate() error {
	for name, value := range map[string]int{
		"sandboxMaxVcpus":                     config.SandboxMaxVCPUs,
		"sandboxMemoryMiB":                    config.SandboxMemoryMiB,
		"sandboxDiskMiB":                      config.SandboxDiskMiB,
		"memoryBudgetMiB":                     config.MemoryBudgetMiB,
		"maxConcurrentPerSandbox":             config.MaxConcurrentPerSandbox,
		"maxConcurrentGlobal":                 config.MaxConcurrentGlobal,
		"fileTransferMaxBytes":                config.FileTransferMaxBytes,
		"storagePressureRecoveryPercent":      config.StoragePressureRecoveryPercent,
		"storagePressureWarningPercent":       config.StoragePressureWarningPercent,
		"storagePressureAdmissionDenyPercent": config.StoragePressureDenyPercent,
	} {
		if value < 1 {
			return fmt.Errorf("SecondBox stress runner %s must be positive", name)
		}
	}
	if !(config.StoragePressureRecoveryPercent < config.StoragePressureWarningPercent &&
		config.StoragePressureWarningPercent < config.StoragePressureDenyPercent &&
		config.StoragePressureDenyPercent < 100) {
		return errors.New("SecondBox stress runner storage pressure thresholds must be ordered below 100")
	}
	return nil
}

func (config stressProfileConfig) validate() error {
	for name, value := range map[string]int64{
		"cpuMillis":                   config.CPUMillis,
		"memoryBytes":                 config.MemoryBytes,
		"workspaceBytes":              config.WorkspaceBytes,
		"processLimit":                config.ProcessLimit,
		"concurrentOperations":        config.ConcurrentOperations,
		"drainGraceSeconds":           config.DrainGraceSeconds,
		"idleSeconds":                 config.IdleSeconds,
		"maximumDurationSeconds":      config.MaximumDurationSeconds,
		"leaseSeconds":                config.LeaseSeconds,
		"snapshotLimit":               config.SnapshotLimit,
		"snapshotRetentionSeconds":    config.SnapshotRetentionSeconds,
		"artifactRetentionSeconds":    config.ArtifactRetentionSeconds,
		"maximumDeadlineMilliseconds": config.MaximumDeadlineMilliseconds,
		"maximumBufferedOutputBytes":  config.MaximumBufferedOutputBytes,
		"streamWindowBytes":           config.StreamWindowBytes,
		"maximumTransferBytes":        config.MaximumTransferBytes,
		"terminalDetachSeconds":       config.TerminalDetachSeconds,
	} {
		if value < 1 {
			return fmt.Errorf("SecondBox stress Profile %s must be positive", name)
		}
	}
	return nil
}

func guestIPCapacity(rawCIDR string) int {
	ip, network, err := net.ParseCIDR(strings.TrimSpace(rawCIDR))
	if err != nil || ip.To4() == nil {
		return 0
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return 0
	}
	total := 1 << (bits - ones)
	if total <= 3 {
		return 0
	}
	return total - 3
}

func (config stressConfig) configuredBinding(guestCIDR string) configuredLimit {
	subjectBinding := minimumConfiguredLimit([]configuredLimit{
		{Name: "active instances", Capacity: config.SubjectMaxActiveInstances},
		{Name: "Sandboxes", Capacity: config.SubjectMaxSandboxes},
		{
			Name:     "CPU",
			Capacity: int(config.SubjectMaxCPUMillis / config.Profile.CPUMillis),
		},
		{
			Name:     "memory",
			Capacity: int(config.SubjectMaxMemoryBytes / config.Profile.MemoryBytes),
		},
		{Name: "concurrent Operations", Capacity: config.SubjectMaxConcurrentOps},
		{Name: "Snapshots", Capacity: config.SubjectMaxSnapshots},
	})
	subjectBinding.Name = "subject quota: " + subjectBinding.Name
	limits := []configuredLimit{
		{Name: "SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL", Capacity: config.Runner.MaxConcurrentGlobal},
		{
			Name: "runner CPU capacity",
			Capacity: int(
				int64(config.Runner.SandboxMaxVCPUs*config.Runner.MaxConcurrentGlobal*1000) /
					config.Profile.CPUMillis,
			),
		},
		{
			Name:     "SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB",
			Capacity: config.Runner.MemoryBudgetMiB / config.Runner.SandboxMemoryMiB,
		},
		{
			Name: "runner Workspace capacity",
			Capacity: int(
				(int64(config.Runner.SandboxDiskMiB*config.Runner.MaxConcurrentGlobal) << 20) /
					config.Profile.WorkspaceBytes,
			),
		},
		{
			Name:     "runner Operations capacity",
			Capacity: config.Runner.MaxConcurrentGlobal / int(config.Profile.ConcurrentOperations),
		},
		{Name: "guest IP capacity", Capacity: guestIPCapacity(guestCIDR)},
		subjectBinding,
	}
	return minimumConfiguredLimit(limits)
}

func minimumConfiguredLimit(limits []configuredLimit) configuredLimit {
	binding := limits[0]
	for _, candidate := range limits[1:] {
		if candidate.Capacity < binding.Capacity {
			binding = candidate
		} else if candidate.Capacity == binding.Capacity {
			binding.Name += " + " + candidate.Name
		}
	}
	return binding
}

type configuredLimit struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}
