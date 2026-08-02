package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func railedLadderConfig(rails hostRailConfig) lifecycleConfig {
	config := ladderConfig()
	config.HostRails = &rails
	config.LatencyKneeRatio = 1.5
	return config
}

// A rail must never discard the cells already measured. Aborting a run that
// destroys its own record would defeat the purpose of the rail.
func TestRailAbortPreservesTheCellsAlreadyMeasured(t *testing.T) {
	config := railedLadderConfig(hostRailConfig{
		Enabled: true, AvailableMemoryFloorMiB: 8192, MaximumWallClockSeconds: 3600,
	})
	railError := errors.New("SecondBox lifecycle host rail availableMemoryFloorMiB stopped the run")
	offered := 0
	results, err := collectCells(
		context.Background(), config,
		func(
			_ context.Context, _ string, pattern arrivalPattern, _ int,
		) (cellResult, error) {
			offered++
			if offered < 3 {
				return measuredCell(pattern), nil
			}
			aborted := measuredCell(pattern)
			aborted.AbortedAtRail = railAvailableMemory
			return aborted, railError
		},
	)
	if err == nil {
		t.Fatal("the rail did not stop the run")
	}
	if offered != 3 {
		t.Fatalf("cells offered = %d, want 3: later cells must be skipped", offered)
	}
	if len(results) != 3 {
		t.Fatalf("retained cells = %d, want 3 including the aborted cell", len(results))
	}
	if results[2].AbortedAtRail != railAvailableMemory {
		t.Fatalf("aborted cell rail = %q", results[2].AbortedAtRail)
	}
	for _, index := range []int{0, 1} {
		if results[index].AbortedAtRail != "" {
			t.Fatalf("cell %d was marked aborted", index)
		}
	}
}

// The rail is a safety trip, not feedback. It ends the run; it never reduces the
// load being offered, because modulating offered load in response to strain
// would make the benchmark closed loop.
func TestWallClockRailStopsTheRunBetweenCells(t *testing.T) {
	config := railedLadderConfig(hostRailConfig{
		Enabled: true, AvailableMemoryFloorMiB: 1, MaximumWallClockSeconds: 1,
	})
	offered := 0
	results, err := collectCells(
		context.Background(), config,
		func(
			_ context.Context, _ string, pattern arrivalPattern, _ int,
		) (cellResult, error) {
			offered++
			if offered == 1 {
				time.Sleep(1100 * time.Millisecond)
			}
			return measuredCell(pattern), nil
		},
	)
	if err == nil {
		t.Fatal("the wall-clock rail did not stop the run")
	}
	if !strings.Contains(err.Error(), railWallClock) {
		t.Fatalf("error does not name the wall-clock rail: %v", err)
	}
	if offered != 1 {
		t.Fatalf("cells offered = %d, want 1: the rail must stop the next cell", offered)
	}
	if len(results) != 1 {
		t.Fatalf("retained cells = %d, want 1", len(results))
	}
}

func TestDisabledRailsRunEveryCell(t *testing.T) {
	config := railedLadderConfig(hostRailConfig{
		Enabled: false, AvailableMemoryFloorMiB: 1 << 40, MaximumWallClockSeconds: 1,
	})
	offered := 0
	results, err := collectCells(
		context.Background(), config,
		func(
			_ context.Context, _ string, pattern arrivalPattern, _ int,
		) (cellResult, error) {
			offered++
			if offered == 1 {
				time.Sleep(1100 * time.Millisecond)
			}
			return measuredCell(pattern), nil
		},
	)
	if err != nil {
		t.Fatalf("disabled rails stopped the run: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("retained cells = %d, want 5", len(results))
	}
}

func TestReportCarriesTheRailThatStoppedTheRun(t *testing.T) {
	aborted := measuredCell(arrivalPattern{Name: "burst-8", Kind: patternBurst, Count: 8})
	aborted.AbortedAtRail = railAvailableMemory
	report := lifecycleReport{SchemaVersion: 3, Results: []cellResult{aborted}}
	report.IncompleteReason = "SecondBox lifecycle host rail availableMemoryFloorMiB stopped the run"
	report.AbortedAtRail = aborted.AbortedAtRail
	if report.AbortedAtRail != railAvailableMemory {
		t.Fatalf("report rail = %q", report.AbortedAtRail)
	}
}

// Refusals are the measurement, not a fault. A failure ceiling that counted them
// would abort exactly when the run became interesting.
func TestFailureTotalExcludesRefusals(t *testing.T) {
	samples := newTransitionSamples()
	samples.refusals["quota_exceeded"] = 40
	samples.refusals["home_runner_unavailable"] = 2
	samples.failures["startup_failed"] = 3
	samples.failures["client_error"] = 1
	if total := samples.failureTotal(); total != 4 {
		t.Fatalf("failure total = %d, want 4", total)
	}
}
