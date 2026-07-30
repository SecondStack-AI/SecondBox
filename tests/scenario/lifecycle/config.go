package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Cycle kinds. The warm cycle is the ephemeral hot path: an already-created
// Sandbox that retains its Workspace is started and stopped repeatedly.
const (
	cycleWarm = "warm"
	cycleCold = "cold"
)

// Arrival pattern kinds. Every pattern is open loop: arrivals are offered on a
// schedule and are never withheld because earlier cycles are still running.
const (
	patternBurst    = "burst"
	patternSteady   = "steady"
	patternRamp     = "ramp"
	patternSawtooth = "sawtooth"
)

// Steady arrival distributions.
const (
	distributionFixed   = "fixed"
	distributionPoisson = "poisson"
)

type arrivalPattern struct {
	Name                   string  `json:"name"`
	Kind                   string  `json:"kind"`
	Count                  int     `json:"count"`
	ArrivalsPerSecond      float64 `json:"arrivalsPerSecond"`
	StartArrivalsPerSecond float64 `json:"startArrivalsPerSecond"`
	EndArrivalsPerSecond   float64 `json:"endArrivalsPerSecond"`
	DurationSeconds        int64   `json:"durationSeconds"`
	Distribution           string  `json:"distribution"`
	PoissonSeed            int64   `json:"poissonSeed"`
	QuietSeconds           int64   `json:"quietSeconds"`
	Repeats                int     `json:"repeats"`
}

type runnerLimits struct {
	SandboxMaxVcpus                     int   `json:"sandboxMaxVcpus"`
	SandboxMemoryMiB                    int   `json:"sandboxMemoryMiB"`
	SandboxDiskMiB                      int   `json:"sandboxDiskMiB"`
	MemoryBudgetMiB                     int   `json:"memoryBudgetMiB"`
	MaxConcurrentPerSandbox             int   `json:"maxConcurrentPerSandbox"`
	MaxConcurrentGlobal                 int   `json:"maxConcurrentGlobal"`
	MaxConcurrentOperationsGlobal       int   `json:"maxConcurrentOperationsGlobal"`
	FileTransferMaxBytes                int64 `json:"fileTransferMaxBytes"`
	StoragePressureRecoveryPercent      int   `json:"storagePressureRecoveryPercent"`
	StoragePressureWarningPercent       int   `json:"storagePressureWarningPercent"`
	StoragePressureAdmissionDenyPercent int   `json:"storagePressureAdmissionDenyPercent"`
}

type profileLimits struct {
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

type lifecycleConfig struct {
	Version                     int              `json:"version"`
	RunnerPoolName              string           `json:"runnerPoolName"`
	ProfileName                 string           `json:"profileName"`
	TenantRef                   string           `json:"tenantRef"`
	SubjectRef                  string           `json:"subjectRef"`
	Cycles                      []string         `json:"cycles"`
	Patterns                    []arrivalPattern `json:"patterns"`
	ResidentPopulations         []int            `json:"residentPopulations"`
	MaximumInFlight             int              `json:"maximumInFlight"`
	OccupancySampleMilliseconds int64            `json:"occupancySampleMilliseconds"`
	RequestTimeoutMilliseconds  int64            `json:"requestTimeoutMilliseconds"`
	OperationTimeoutSeconds     int64            `json:"operationTimeoutSeconds"`
	PollIntervalMilliseconds    int64            `json:"pollIntervalMilliseconds"`
	Runner                      runnerLimits     `json:"runner"`
	Profile                     profileLimits    `json:"profile"`

	SubjectMaxSandboxes            int64 `json:"subjectMaxSandboxes"`
	SubjectMaxActiveInstances      int64 `json:"subjectMaxActiveInstances"`
	SubjectMaxConcurrentOperations int64 `json:"subjectMaxConcurrentOperations"`
	SubjectMaxSnapshots            int64 `json:"subjectMaxSnapshots"`
	SubjectMaxArtifactBytes        int64 `json:"subjectMaxArtifactBytes"`
	SubjectMaxArtifacts            int64 `json:"subjectMaxArtifacts"`
	SubjectMaxPortSessions         int64 `json:"subjectMaxPortSessions"`
	SubjectMaxCPUMillis            int64 `json:"subjectMaxCpuMillis"`
	SubjectMaxMemoryBytes          int64 `json:"subjectMaxMemoryBytes"`
}

func readLifecycleConfig(path string) (lifecycleConfig, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return lifecycleConfig{}, errors.New("SecondBox lifecycle driver requires --config")
	}
	if !filepath.IsAbs(trimmed) {
		return lifecycleConfig{}, fmt.Errorf(
			"SecondBox lifecycle configuration must be an absolute path: %s", trimmed,
		)
	}
	content, err := os.ReadFile(trimmed)
	if err != nil {
		return lifecycleConfig{}, fmt.Errorf("SecondBox lifecycle read configuration: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	var config lifecycleConfig
	if err := decoder.Decode(&config); err != nil {
		return lifecycleConfig{}, fmt.Errorf("SecondBox lifecycle decode configuration: %w", err)
	}
	if err := validateLifecycleConfig(config); err != nil {
		return lifecycleConfig{}, err
	}
	return config, nil
}

// validateLifecycleConfig rejects every absent or contradictory setting. The
// repository forbids implicit runtime defaults, so an omitted value is an error
// rather than a substituted constant.
func validateLifecycleConfig(config lifecycleConfig) error {
	if config.Version != 1 {
		return fmt.Errorf("SecondBox lifecycle configuration version must be 1, got %d", config.Version)
	}
	for name, value := range map[string]string{
		"runnerPoolName": config.RunnerPoolName,
		"profileName":    config.ProfileName,
		"tenantRef":      config.TenantRef,
		"subjectRef":     config.SubjectRef,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("SecondBox lifecycle configuration requires %s", name)
		}
	}
	if len(config.Cycles) == 0 {
		return errors.New("SecondBox lifecycle configuration requires at least one cycle")
	}
	seenCycle := make(map[string]struct{}, len(config.Cycles))
	for _, cycle := range config.Cycles {
		if cycle != cycleWarm && cycle != cycleCold {
			return fmt.Errorf("SecondBox lifecycle cycle must be warm or cold, got %q", cycle)
		}
		if _, duplicate := seenCycle[cycle]; duplicate {
			return fmt.Errorf("SecondBox lifecycle cycle %q is duplicated", cycle)
		}
		seenCycle[cycle] = struct{}{}
	}
	if len(config.Patterns) == 0 {
		return errors.New("SecondBox lifecycle configuration requires at least one arrival pattern")
	}
	seenPattern := make(map[string]struct{}, len(config.Patterns))
	for _, pattern := range config.Patterns {
		if err := validateArrivalPattern(pattern); err != nil {
			return err
		}
		if _, duplicate := seenPattern[pattern.Name]; duplicate {
			return fmt.Errorf("SecondBox lifecycle arrival pattern %q is duplicated", pattern.Name)
		}
		seenPattern[pattern.Name] = struct{}{}
	}
	if len(config.ResidentPopulations) == 0 {
		return errors.New("SecondBox lifecycle configuration requires at least one resident population")
	}
	for _, resident := range config.ResidentPopulations {
		if resident < 0 {
			return fmt.Errorf("SecondBox lifecycle resident population must not be negative, got %d", resident)
		}
	}
	if config.MaximumInFlight < 1 {
		return errors.New("SecondBox lifecycle configuration requires a positive maximumInFlight")
	}
	for name, value := range map[string]int64{
		"occupancySampleMilliseconds": config.OccupancySampleMilliseconds,
		"requestTimeoutMilliseconds":  config.RequestTimeoutMilliseconds,
		"operationTimeoutSeconds":     config.OperationTimeoutSeconds,
		"pollIntervalMilliseconds":    config.PollIntervalMilliseconds,
	} {
		if value < 1 {
			return fmt.Errorf("SecondBox lifecycle configuration requires a positive %s", name)
		}
	}
	if config.Runner.MaxConcurrentGlobal < 1 {
		return errors.New("SecondBox lifecycle configuration requires a positive runner.maxConcurrentGlobal")
	}
	if config.Profile.MemoryBytes < 1 {
		return errors.New("SecondBox lifecycle configuration requires a positive profile.memoryBytes")
	}
	if config.SubjectMaxActiveInstances < 1 {
		return errors.New("SecondBox lifecycle configuration requires a positive subjectMaxActiveInstances")
	}
	return nil
}

func validateArrivalPattern(pattern arrivalPattern) error {
	if strings.TrimSpace(pattern.Name) == "" {
		return errors.New("SecondBox lifecycle arrival pattern requires a name")
	}
	switch pattern.Kind {
	case patternBurst:
		if pattern.Count < 1 {
			return fmt.Errorf("SecondBox lifecycle burst %q requires a positive count", pattern.Name)
		}
	case patternSteady:
		if pattern.ArrivalsPerSecond <= 0 {
			return fmt.Errorf(
				"SecondBox lifecycle steady %q requires a positive arrivalsPerSecond", pattern.Name,
			)
		}
		if pattern.DurationSeconds < 1 {
			return fmt.Errorf(
				"SecondBox lifecycle steady %q requires a positive durationSeconds", pattern.Name,
			)
		}
		switch pattern.Distribution {
		case distributionFixed:
		case distributionPoisson:
			if pattern.PoissonSeed == 0 {
				return fmt.Errorf(
					"SecondBox lifecycle steady %q requires an explicit non-zero poissonSeed so the run is reproducible",
					pattern.Name,
				)
			}
		default:
			return fmt.Errorf(
				"SecondBox lifecycle steady %q distribution must be fixed or poisson, got %q",
				pattern.Name, pattern.Distribution,
			)
		}
	case patternRamp:
		if pattern.StartArrivalsPerSecond <= 0 || pattern.EndArrivalsPerSecond <= 0 {
			return fmt.Errorf(
				"SecondBox lifecycle ramp %q requires positive start and end arrivalsPerSecond", pattern.Name,
			)
		}
		if pattern.EndArrivalsPerSecond <= pattern.StartArrivalsPerSecond {
			return fmt.Errorf(
				"SecondBox lifecycle ramp %q must increase from start to end", pattern.Name,
			)
		}
		if pattern.DurationSeconds < 1 {
			return fmt.Errorf(
				"SecondBox lifecycle ramp %q requires a positive durationSeconds", pattern.Name,
			)
		}
	case patternSawtooth:
		if pattern.Count < 1 {
			return fmt.Errorf("SecondBox lifecycle sawtooth %q requires a positive count", pattern.Name)
		}
		if pattern.QuietSeconds < 1 {
			return fmt.Errorf(
				"SecondBox lifecycle sawtooth %q requires a positive quietSeconds", pattern.Name,
			)
		}
		if pattern.Repeats < 1 {
			return fmt.Errorf("SecondBox lifecycle sawtooth %q requires a positive repeats", pattern.Name)
		}
	default:
		return fmt.Errorf(
			"SecondBox lifecycle arrival pattern %q kind must be burst, steady, ramp, or sawtooth, got %q",
			pattern.Name, pattern.Kind,
		)
	}
	return nil
}
