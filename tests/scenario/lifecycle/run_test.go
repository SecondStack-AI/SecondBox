package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBurstResultReportsUndefinedOfferedRateAndSeparateDrainRate(t *testing.T) {
	schedule, err := buildArrivalSchedule(arrivalPattern{
		Name: "burst-4", Kind: patternBurst, Count: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	samples := newTransitionSamples()
	for _, elapsed := range []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
		400 * time.Millisecond,
	} {
		samples.record(measurementStartReady, elapsed)
	}
	result := buildCellResult(cellObservation{
		measurement:     measurementStartReady,
		pattern:         arrivalPattern{Name: "burst-4", Kind: patternBurst, Count: 4},
		resident:        0,
		schedule:        schedule,
		samples:         samples,
		timings:         newStartupTimingSamples(),
		occupancy:       []occupancySample{{OutstandingArrivals: 4}},
		completed:       4,
		peakOutstanding: 4,
		elapsed:         2 * time.Second,
	})
	if result.OfferedRatePerSecond != nil {
		t.Fatalf("burst offered rate = %f, want undefined", *result.OfferedRatePerSecond)
	}
	if result.CompletionRatePerSecond != 2 {
		t.Fatalf("burst drain rate = %f, want 2", result.CompletionRatePerSecond)
	}
	if result.PeakOutstandingArrivals != 4 {
		t.Fatalf("peak outstanding = %d, want 4", result.PeakOutstandingArrivals)
	}
	if result.Latency == nil ||
		result.Latency.P50Milliseconds != 200 ||
		result.Latency.P95Milliseconds != 400 {
		t.Fatalf("latency summary = %+v", result.Latency)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"offeredRatePerSecond":null`) {
		t.Fatalf("burst result manufactured an offered rate: %s", encoded)
	}
}

func TestCellWithoutSuccessReportsNoLatencyPercentiles(t *testing.T) {
	schedule, err := buildArrivalSchedule(arrivalPattern{
		Name: "burst-1", Kind: patternBurst, Count: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := buildCellResult(cellObservation{
		measurement: measurementStartReady,
		pattern:     arrivalPattern{Name: "burst-1", Kind: patternBurst, Count: 1},
		resident:    0,
		schedule:    schedule,
		samples:     newTransitionSamples(),
		timings:     newStartupTimingSamples(),
		elapsed:     time.Second,
	})
	if result.Latency != nil {
		t.Fatalf("failed cell latency = %+v, want nil", result.Latency)
	}
}

func TestStartupSpanSummaryIsStableAndIndependent(t *testing.T) {
	summaries := summarizeStartupSpans(map[string][]time.Duration{
		"runner_event_ingest": {3 * time.Millisecond, 7 * time.Millisecond},
		"pre_assignment":      {100 * time.Millisecond, 200 * time.Millisecond},
	})
	if len(summaries) != 2 {
		t.Fatalf("startup spans = %d, want 2", len(summaries))
	}
	if summaries[0].Span != "pre_assignment" ||
		summaries[0].P50Milliseconds != 100 ||
		summaries[1].Span != "runner_event_ingest" ||
		summaries[1].P95Milliseconds != 7 {
		t.Fatalf("startup spans = %+v", summaries)
	}
}
