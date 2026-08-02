package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ladderConfig is a five-cell run: one measurement, five burst patterns of
// increasing size, one resident population.
func ladderConfig() lifecycleConfig {
	return lifecycleConfig{
		Measurements:        []string{measurementCreateReady},
		ResidentPopulations: []int{0},
		Patterns: []arrivalPattern{
			{Name: "burst-1", Kind: patternBurst, Count: 1},
			{Name: "burst-2", Kind: patternBurst, Count: 2},
			{Name: "burst-4", Kind: patternBurst, Count: 4},
			{Name: "burst-8", Kind: patternBurst, Count: 8},
			{Name: "burst-16", Kind: patternBurst, Count: 16},
		},
	}
}

func measuredCell(pattern arrivalPattern) cellResult {
	schedule, err := buildArrivalSchedule(pattern)
	if err != nil {
		panic(err)
	}
	return buildCellResult(cellObservation{
		measurement: measurementCreateReady, pattern: pattern, resident: 0,
		schedule: schedule, samples: newTransitionSamples(),
		timings: newStartupTimingSamples(), completed: int64(pattern.Count),
		peakOutstanding: int64(pattern.Count), elapsed: time.Second,
	})
}

func TestCollectCellsKeepsMeasuredCellsWhenARunEndsEarly(t *testing.T) {
	failure := errors.New("host memory floor reached")
	offered := 0
	results, err := collectCells(
		context.Background(),
		ladderConfig(),
		func(
			_ context.Context, _ string, pattern arrivalPattern, _ int,
		) (cellResult, error) {
			offered++
			// The third cell fails without reaching its measurement window.
			if offered == 3 {
				return cellResult{}, failure
			}
			return measuredCell(pattern), nil
		},
	)
	if err == nil {
		t.Fatal("collectCells returned no error after a cell failed")
	}
	if !errors.Is(err, failure) {
		t.Fatalf("collectCells error = %v, want it to wrap %v", err, failure)
	}
	if offered != 3 {
		t.Fatalf("cells offered = %d, want 3: the run must stop at the failure", offered)
	}
	if len(results) != 2 {
		t.Fatalf("retained cells = %d, want 2", len(results))
	}
	if results[0].Pattern != "burst-1" || results[1].Pattern != "burst-2" {
		t.Fatalf("retained cells = %s, %s", results[0].Pattern, results[1].Pattern)
	}
	if !strings.Contains(err.Error(), "burst-4") {
		t.Fatalf("error does not name the failing cell: %v", err)
	}
}

func TestCollectCellsRetainsAPartiallyMeasuredFailingCell(t *testing.T) {
	results, err := collectCells(
		context.Background(),
		ladderConfig(),
		func(
			_ context.Context, _ string, pattern arrivalPattern, _ int,
		) (cellResult, error) {
			if pattern.Name == "burst-4" {
				// A cancelled cell reports what it observed before stopping.
				return measuredCell(pattern), context.Canceled
			}
			return measuredCell(pattern), nil
		},
	)
	if err == nil {
		t.Fatal("collectCells returned no error after a cell was cancelled")
	}
	if len(results) != 3 {
		t.Fatalf("retained cells = %d, want 3 including the partial cell", len(results))
	}
	if results[2].Pattern != "burst-4" {
		t.Fatalf("partial cell = %s, want burst-4", results[2].Pattern)
	}
}

func TestCollectCellsReturnsEveryCellOnACleanRun(t *testing.T) {
	results, err := collectCells(
		context.Background(),
		ladderConfig(),
		func(
			_ context.Context, _ string, pattern arrivalPattern, _ int,
		) (cellResult, error) {
			return measuredCell(pattern), nil
		},
	)
	if err != nil {
		t.Fatalf("clean run returned %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("retained cells = %d, want 5", len(results))
	}
	for index, expected := range []string{
		"burst-1", "burst-2", "burst-4", "burst-8", "burst-16",
	} {
		if results[index].Pattern != expected {
			t.Fatalf("cell %d = %s, want %s", index, results[index].Pattern, expected)
		}
	}
}

func TestCleanReportOmitsIncompleteReason(t *testing.T) {
	encoded, err := json.Marshal(lifecycleReport{SchemaVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "incompleteReason") {
		t.Fatalf("clean report declared an incomplete reason: %s", encoded)
	}
}

func TestIncompleteReportNamesTheReasonInBothRenderings(t *testing.T) {
	report := lifecycleReport{
		SchemaVersion:    3,
		IncompleteReason: "host memory floor reached",
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"incompleteReason":"host memory floor reached"`) {
		t.Fatalf("incomplete reason absent from the machine report: %s", encoded)
	}
	var human bytes.Buffer
	if err := writeHumanReport(&human, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "INCOMPLETE") ||
		!strings.Contains(human.String(), "host memory floor reached") {
		t.Fatalf("incomplete reason absent from the human report:\n%s", human.String())
	}
}
