package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func scheduleFor(t *testing.T, pattern arrivalPattern) arrivalSchedule {
	t.Helper()
	schedule, err := buildArrivalSchedule(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return schedule
}

func TestPeakSimultaneousArrivalsDistinguishesBurstsFromSpreadArrivals(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		pattern arrivalPattern
		peak    int
	}{
		{
			name:    "burst offers every arrival at once",
			pattern: arrivalPattern{Name: "burst-16", Kind: patternBurst, Count: 16},
			peak:    16,
		},
		{
			name: "sawtooth peaks at one burst, not the total",
			pattern: arrivalPattern{
				Name: "sawtooth-4", Kind: patternSawtooth,
				Count: 4, Repeats: 3, QuietSeconds: 20,
			},
			peak: 4,
		},
		{
			name: "steady spreads its arrivals",
			pattern: arrivalPattern{
				Name: "crawl", Kind: patternSteady, ArrivalsPerSecond: 0.25,
				DurationSeconds: 40, Distribution: distributionFixed,
			},
			peak: 1,
		},
		{
			name: "ramp spreads its arrivals",
			pattern: arrivalPattern{
				Name: "ramp", Kind: patternRamp, StartArrivalsPerSecond: 0.5,
				EndArrivalsPerSecond: 4, DurationSeconds: 90,
			},
			peak: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			peak := scheduleFor(t, testCase.pattern).peakSimultaneousArrivals()
			if peak != testCase.peak {
				t.Fatalf("peak simultaneous arrivals = %d, want %d", peak, testCase.peak)
			}
		})
	}
}

func TestConfigRejectsABurstLargerThanTheInFlightCap(t *testing.T) {
	config := validLifecycleConfig()
	config.MaximumInFlight = 8
	config.Patterns = []arrivalPattern{
		{Name: "burst-16", Kind: patternBurst, Count: 16},
	}
	err := validateLifecycleConfig(config)
	if err == nil {
		t.Fatal("a burst above maximumInFlight was accepted")
	}
	for _, fragment := range []string{"burst-16", "16", "8", "simultaneous"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error is missing %q: %v", fragment, err)
		}
	}
}

func TestConfigAcceptsABurstEqualToTheInFlightCap(t *testing.T) {
	config := validLifecycleConfig()
	config.MaximumInFlight = 16
	config.Patterns = []arrivalPattern{
		{Name: "burst-16", Kind: patternBurst, Count: 16},
	}
	if err := validateLifecycleConfig(config); err != nil {
		t.Fatalf("a burst equal to maximumInFlight was rejected: %v", err)
	}
}

// Arrivals spread over time never coincide, so the in-flight cap cannot shed
// them at the moment they are offered. Accumulating in flight because the
// deployment drains slowly is genuine saturation and must stay observable.
func TestConfigAcceptsSpreadArrivalsExceedingTheInFlightCap(t *testing.T) {
	config := validLifecycleConfig()
	config.MaximumInFlight = 2
	config.Patterns = []arrivalPattern{
		{
			Name: "crawl", Kind: patternSteady, ArrivalsPerSecond: 0.25,
			DurationSeconds: 40, Distribution: distributionFixed,
		},
	}
	if err := validateLifecycleConfig(config); err != nil {
		t.Fatalf("spread arrivals above maximumInFlight were rejected: %v", err)
	}
}

func TestCellResultReportsItsOwnShedCount(t *testing.T) {
	pattern := arrivalPattern{Name: "burst-4", Kind: patternBurst, Count: 4}
	result := buildCellResult(cellObservation{
		measurement: measurementCreateReady, pattern: pattern, resident: 0,
		schedule: scheduleFor(t, pattern), samples: newTransitionSamples(),
		timings: newStartupTimingSamples(), completed: 1, shed: 3,
		peakOutstanding: 1, elapsed: time.Second,
	})
	if result.ShedArrivals != 3 {
		t.Fatalf("cell shed arrivals = %d, want 3", result.ShedArrivals)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"shedArrivals":3`) {
		t.Fatalf("cell shed count absent from the machine report: %s", encoded)
	}
}
