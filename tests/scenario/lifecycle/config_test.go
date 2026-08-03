package main

import (
	"strings"
	"testing"
)

func validLifecycleConfig() lifecycleConfig {
	return lifecycleConfig{
		Version: 3, RunnerPoolName: "lifecycle-local", ProfileName: "lifecycle-local",
		TenantRef: "lifecycle-qualification", SubjectRef: "lifecycle-qualification",
		HostRails:        &hostRailConfig{Enabled: false},
		Gate:             &gateConfig{Mode: gateObserve},
		LatencyKneeRatio: 1.5, SandboxWaitDeadlineSeconds: 60,
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
		Profile: profileLimits{
			MemoryBytes: 536870912, DataPlaneTransport: "proxied",
		},
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
		"dataPlaneTransport":          func(c *lifecycleConfig) { c.Profile.DataPlaneTransport = "" },
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

// The Sandbox wait deadline is configuration, not a constant. A capacity ladder
// drives the deployment past the point where a Sandbox becomes ready quickly, so
// a fixed deadline turns "slower than the constant" into a wall: every arrival
// beyond it fails identically and the ladder can see that a rung was slow but
// never how slow.
func TestSandboxWaitDeadlineIsRequiredConfiguration(t *testing.T) {
	config := validLifecycleConfig()
	config.SandboxWaitDeadlineSeconds = 0
	err := validateLifecycleConfig(config)
	if err == nil {
		t.Fatal("an absent sandboxWaitDeadlineSeconds was accepted")
	}
	if !strings.Contains(err.Error(), "sandboxWaitDeadlineSeconds") {
		t.Fatalf("error does not name the setting: %v", err)
	}
}

// One wait request, not the total wait. The API rejects a single Sandbox wait
// above 60 seconds, so a larger value fails every arrival immediately. How long
// the driver waits for a Sandbox is operationTimeoutSeconds, which reissues
// requests until it expires.
func TestSandboxWaitDeadlineIsBoundedByTheAPIMaximum(t *testing.T) {
	config := validLifecycleConfig()
	config.SandboxWaitDeadlineSeconds = 180
	err := validateLifecycleConfig(config)
	if err == nil {
		t.Fatal("a wait deadline above the API maximum was accepted")
	}
	if !strings.Contains(err.Error(), "60") {
		t.Fatalf("error does not name the API maximum: %v", err)
	}
}

func TestCapacityConfigsWaitWithinTheAPIMaximum(t *testing.T) {
	for _, name := range []string{
		"capacity-config.example.json",
		"capacity-gate-config.example.json",
	} {
		config := loadExample(t, name)
		if config.SandboxWaitDeadlineSeconds > 60 {
			t.Fatalf("%s requests a %ds wait, which the API rejects",
				name, config.SandboxWaitDeadlineSeconds)
		}
		// The total wait must still be long enough to outlast a slow rung.
		if config.OperationTimeoutSeconds <= 60 {
			t.Fatalf("%s waits only %ds in total, so a slow rung reports a wall",
				name, config.OperationTimeoutSeconds)
		}
	}
}
