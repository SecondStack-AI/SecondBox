package main

import (
	"strings"
	"testing"
	"time"
)

const sampleMeminfo = `MemTotal:       131404936 kB
MemFree:          3611048 kB
MemAvailable:    47185920 kB
Buffers:            43932 kB
Cached:          89104112 kB
SwapTotal:        4194300 kB
SwapFree:             252 kB
`

func TestParseMeminfoReadsAvailableMemory(t *testing.T) {
	memory, err := parseMeminfo(strings.NewReader(sampleMeminfo))
	if err != nil {
		t.Fatal(err)
	}
	if memory.AvailableMiB != 46080 {
		t.Fatalf("available = %d MiB, want 46080", memory.AvailableMiB)
	}
}

func TestParseMeminfoRejectsMalformedContent(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
		names   string
	}{
		{
			name:    "no MemAvailable line",
			content: "MemTotal: 131404936 kB\nMemFree: 3611048 kB\n",
			names:   "MemAvailable",
		},
		{
			name:    "value is not a number",
			content: "MemAvailable:    plenty kB\n",
			names:   "not a number",
		},
		{
			name:    "value is not kB-denominated",
			content: "MemAvailable:    47185920 MB\n",
			names:   "kB-denominated",
		},
		{
			name:    "value has no unit",
			content: "MemAvailable:    47185920\n",
			names:   "kB-denominated",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseMeminfo(strings.NewReader(testCase.content))
			if err == nil {
				t.Fatal("malformed meminfo was accepted")
			}
			if !strings.Contains(err.Error(), testCase.names) {
				t.Fatalf("error is missing %q: %v", testCase.names, err)
			}
		})
	}
}

func enabledRails() hostRailConfig {
	return hostRailConfig{
		Enabled:                 true,
		AvailableMemoryFloorMiB: 8192,
		StepFailureCeiling:      0,
		MaximumWallClockSeconds: 3600,
	}
}

func TestRailsTripOnTheFirstBreachedThreshold(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		observation railObservation
		rail        string
	}{
		{
			name:        "nothing breached",
			observation: railObservation{availableMiB: 40000, elapsed: time.Minute},
			rail:        "",
		},
		{
			name:        "memory floor",
			observation: railObservation{availableMiB: 4096, elapsed: time.Minute},
			rail:        railAvailableMemory,
		},
		{
			name:        "step failures",
			observation: railObservation{availableMiB: 40000, failures: 1, elapsed: time.Minute},
			rail:        railStepFailures,
		},
		{
			name:        "wall clock",
			observation: railObservation{availableMiB: 40000, elapsed: 2 * time.Hour},
			rail:        railWallClock,
		},
		{
			// Deterministic ordering: memory is evaluated first, so a run that
			// breaches everything at once always names the same rail.
			name: "every rail at once names the first",
			observation: railObservation{
				availableMiB: 1, failures: 99, elapsed: 10 * time.Hour,
			},
			rail: railAvailableMemory,
		},
		{
			name:        "floor is exclusive at the boundary",
			observation: railObservation{availableMiB: 8192, elapsed: time.Minute},
			rail:        "",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if rail := enabledRails().evaluate(testCase.observation); rail != testCase.rail {
				t.Fatalf("rail = %q, want %q", rail, testCase.rail)
			}
		})
	}
}

func TestDisabledRailsNeverTrip(t *testing.T) {
	rails := hostRailConfig{
		Enabled:                 false,
		AvailableMemoryFloorMiB: 8192,
		MaximumWallClockSeconds: 60,
	}
	rail := rails.evaluate(railObservation{
		availableMiB: 0, failures: 1000, elapsed: 10 * time.Hour,
	})
	if rail != "" {
		t.Fatalf("disabled rails tripped %q", rail)
	}
}

func TestConfigRequiresAnExplicitHostRailsBlock(t *testing.T) {
	config := validLifecycleConfig()
	config.HostRails = nil
	err := validateLifecycleConfig(config)
	if err == nil {
		t.Fatal("a configuration without hostRails was accepted")
	}
	if !strings.Contains(err.Error(), "hostRails") {
		t.Fatalf("error does not name hostRails: %v", err)
	}
}

func TestEnabledRailsRequirePositiveThresholds(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		rails hostRailConfig
		names string
	}{
		{
			name:  "memory floor absent",
			rails: hostRailConfig{Enabled: true, MaximumWallClockSeconds: 60},
			names: "availableMemoryFloorMiB",
		},
		{
			name:  "wall clock absent",
			rails: hostRailConfig{Enabled: true, AvailableMemoryFloorMiB: 8192},
			names: "maximumWallClockSeconds",
		},
		{
			name: "negative failure ceiling",
			rails: hostRailConfig{
				Enabled: true, AvailableMemoryFloorMiB: 8192,
				MaximumWallClockSeconds: 60, StepFailureCeiling: -1,
			},
			names: "stepFailureCeiling",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := validLifecycleConfig()
			config.HostRails = &testCase.rails
			err := validateLifecycleConfig(config)
			if err == nil {
				t.Fatal("incomplete rails were accepted")
			}
			if !strings.Contains(err.Error(), testCase.names) {
				t.Fatalf("error is missing %q: %v", testCase.names, err)
			}
		})
	}
}

func TestConfigRequiresALatencyKneeRatioAboveOne(t *testing.T) {
	for _, ratio := range []float64{0, 1, -2} {
		config := validLifecycleConfig()
		config.LatencyKneeRatio = ratio
		err := validateLifecycleConfig(config)
		if err == nil {
			t.Fatalf("latencyKneeRatio %v was accepted", ratio)
		}
		if !strings.Contains(err.Error(), "latencyKneeRatio") {
			t.Fatalf("error does not name latencyKneeRatio: %v", err)
		}
	}
}
