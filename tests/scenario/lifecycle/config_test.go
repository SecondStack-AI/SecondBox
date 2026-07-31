package main

import (
	"strings"
	"testing"
)

func validLifecycleConfig() lifecycleConfig {
	return lifecycleConfig{
		Version: 2, RunnerPoolName: "lifecycle-local", ProfileName: "lifecycle-local",
		TenantRef: "lifecycle-qualification", SubjectRef: "lifecycle-qualification",
		Measurements: []string{
			measurementCreateReady, measurementStartReady,
			measurementStopStopped, measurementDeleteGone,
		},
		Patterns: []arrivalPattern{{
			Name: "burst-8", Kind: patternBurst, Count: 8,
		}},
		ResidentPopulations:         []int{0, 4},
		MaximumInFlight:             16,
		OccupancySampleMilliseconds: 500,
		RequestTimeoutMilliseconds:  70000,
		OperationTimeoutSeconds:     180,
		PollIntervalMilliseconds:    250,
		Runner: runnerLimits{
			MaxConcurrentGlobal:           16,
			MaxConcurrentStarts:           8,
			MaxConcurrentWorkspaceCreates: 4,
		},
		Profile:                   profileLimits{MemoryBytes: 536870912},
		SubjectMaxActiveInstances: 12,
	}
}

func TestValidLifecycleConfigIsAccepted(t *testing.T) {
	if err := validateLifecycleConfig(validLifecycleConfig()); err != nil {
		t.Fatal(err)
	}
}

// Every runtime setting must be explicit. An omitted value is an error rather
// than a silently substituted default.
func TestAbsentRequiredSettingsAreRejected(t *testing.T) {
	for name, mutate := range map[string]func(*lifecycleConfig){
		"version":                     func(c *lifecycleConfig) { c.Version = 0 },
		"runnerPoolName":              func(c *lifecycleConfig) { c.RunnerPoolName = "" },
		"measurements":                func(c *lifecycleConfig) { c.Measurements = nil },
		"patterns":                    func(c *lifecycleConfig) { c.Patterns = nil },
		"residentPopulations":         func(c *lifecycleConfig) { c.ResidentPopulations = nil },
		"maximumInFlight":             func(c *lifecycleConfig) { c.MaximumInFlight = 0 },
		"occupancySampleMilliseconds": func(c *lifecycleConfig) { c.OccupancySampleMilliseconds = 0 },
		"operationTimeoutSeconds":     func(c *lifecycleConfig) { c.OperationTimeoutSeconds = 0 },
		"runner.maxConcurrentGlobal":  func(c *lifecycleConfig) { c.Runner.MaxConcurrentGlobal = 0 },
		"runner.maxConcurrentStarts":  func(c *lifecycleConfig) { c.Runner.MaxConcurrentStarts = 0 },
		"runner.maxConcurrentWorkspaceCreates": func(c *lifecycleConfig) {
			c.Runner.MaxConcurrentWorkspaceCreates = 0
		},
		"subjectMaxActiveInstances": func(c *lifecycleConfig) { c.SubjectMaxActiveInstances = 0 },
	} {
		config := validLifecycleConfig()
		mutate(&config)
		if err := validateLifecycleConfig(config); err == nil {
			t.Fatalf("absent %s was accepted", name)
		}
	}
}

func TestUnknownMeasurementIsRejected(t *testing.T) {
	config := validLifecycleConfig()
	config.Measurements = []string{"lukewarm"}
	if err := validateLifecycleConfig(config); err == nil {
		t.Fatal("unknown measurement was accepted")
	}
}

func TestDuplicateMeasurementAndPatternAreRejected(t *testing.T) {
	config := validLifecycleConfig()
	config.Measurements = []string{measurementStartReady, measurementStartReady}
	if err := validateLifecycleConfig(config); err == nil {
		t.Fatal("duplicate measurement was accepted")
	}
	config = validLifecycleConfig()
	config.Patterns = append(config.Patterns, config.Patterns[0])
	if err := validateLifecycleConfig(config); err == nil {
		t.Fatal("duplicate pattern name was accepted")
	}
}

// A Poisson pattern without an explicit seed cannot be replayed, so it is
// rejected rather than silently seeded from the clock.
func TestPoissonWithoutSeedIsRejected(t *testing.T) {
	config := validLifecycleConfig()
	config.Patterns = []arrivalPattern{{
		Name: "steady", Kind: patternSteady, ArrivalsPerSecond: 2,
		DurationSeconds: 30, Distribution: distributionPoisson,
	}}
	err := validateLifecycleConfig(config)
	if err == nil {
		t.Fatal("poisson without a seed was accepted")
	}
	if !strings.Contains(err.Error(), "poissonSeed") {
		t.Fatalf("error did not name poissonSeed: %v", err)
	}
}

func TestRampMustIncrease(t *testing.T) {
	config := validLifecycleConfig()
	config.Patterns = []arrivalPattern{{
		Name: "ramp", Kind: patternRamp,
		StartArrivalsPerSecond: 4, EndArrivalsPerSecond: 2, DurationSeconds: 60,
	}}
	if err := validateLifecycleConfig(config); err == nil {
		t.Fatal("a decreasing ramp was accepted")
	}
}

func TestNegativeResidentPopulationIsRejected(t *testing.T) {
	config := validLifecycleConfig()
	config.ResidentPopulations = []int{-1}
	if err := validateLifecycleConfig(config); err == nil {
		t.Fatal("negative resident population was accepted")
	}
}

func TestRelativeConfigurationPathIsRejected(t *testing.T) {
	if _, err := readLifecycleConfig("relative/config.json"); err == nil {
		t.Fatal("relative configuration path was accepted")
	}
}
