package main

import (
	"bytes"
	"strings"
	"testing"
)

func rung(offered int, options ...func(*cellResult)) cellResult {
	result := cellResult{
		Measurement:        measurementCreateReady,
		PatternKind:        patternBurst,
		Pattern:            "burst",
		ResidentPopulation: 0,
		OfferedArrivals:    offered,
		CompletedArrivals:  int64(offered),
		Refusals:           map[string]int64{},
		Failures:           map[string]int64{},
		Latency:            &transitionSummary{Samples: offered, P95Milliseconds: 1000},
	}
	for _, option := range options {
		option(&result)
	}
	return result
}

func withP95(milliseconds int64) func(*cellResult) {
	return func(result *cellResult) {
		result.Latency = &transitionSummary{
			Samples: result.OfferedArrivals, P95Milliseconds: milliseconds,
		}
	}
}

func withRefusals(counts map[string]int64) func(*cellResult) {
	return func(result *cellResult) {
		result.Refusals = counts
		var refused int64
		for _, count := range counts {
			refused += count
		}
		result.CompletedArrivals = int64(result.OfferedArrivals) - refused
	}
}

func withFailures(counts map[string]int64) func(*cellResult) {
	return func(result *cellResult) { result.Failures = counts }
}

func withNoSuccesses() func(*cellResult) {
	return func(result *cellResult) {
		result.Latency = nil
		result.CompletedArrivals = 0
	}
}

func onlySummary(t *testing.T, summaries []capacitySummary) capacitySummary {
	t.Helper()
	if len(summaries) != 1 {
		t.Fatalf("ladders = %d, want 1", len(summaries))
	}
	return summaries[0]
}

var testBinding = configuredLimit{Name: "subject quota: active instances", Capacity: 48}

func TestLadderWithoutStrainReportsNoKnee(t *testing.T) {
	summary := onlySummary(t, identifyLadders(
		[]cellResult{rung(8), rung(16), rung(32)}, 1.5, testBinding,
	))
	if summary.RefusalKnee != nil || summary.LatencyKnee != nil || summary.DistressKnee != nil {
		t.Fatalf("unstrained ladder reported a knee: %+v", summary)
	}
	if summary.LargestFullyAdmitted != 32 {
		t.Fatalf("largest fully admitted = %d, want 32", summary.LargestFullyAdmitted)
	}
	if summary.Steps != 3 {
		t.Fatalf("steps = %d, want 3", summary.Steps)
	}
}

func TestRefusalKneeNamesTheDominantCode(t *testing.T) {
	summary := onlySummary(t, identifyLadders([]cellResult{
		rung(8),
		rung(16),
		rung(32, withRefusals(map[string]int64{
			"quota_exceeded": 9, "home_runner_unavailable": 2,
		})),
		rung(64, withRefusals(map[string]int64{"quota_exceeded": 40})),
	}, 1.5, testBinding))
	if summary.RefusalKnee == nil {
		t.Fatal("no refusal knee")
	}
	if summary.RefusalKnee.Step != 2 || summary.RefusalKnee.OfferedArrivals != 32 {
		t.Fatalf("refusal knee = %+v, want step 2 at 32", summary.RefusalKnee)
	}
	if summary.RefusalKnee.Detail != "quota_exceeded" {
		t.Fatalf("refusal knee code = %q", summary.RefusalKnee.Detail)
	}
	if summary.LargestFullyAdmitted != 16 {
		t.Fatalf("largest fully admitted = %d, want 16", summary.LargestFullyAdmitted)
	}
}

func TestLatencyKneeComparesAgainstTheFirstRung(t *testing.T) {
	summary := onlySummary(t, identifyLadders([]cellResult{
		rung(8, withP95(100)),
		rung(16, withP95(140)),
		rung(32, withP95(150)),
		rung(64, withP95(400)),
	}, 1.5, testBinding))
	if summary.LatencyKnee == nil {
		t.Fatal("no latency knee")
	}
	if summary.LatencyKnee.OfferedArrivals != 32 {
		t.Fatalf("latency knee at %d offered, want 32", summary.LatencyKnee.OfferedArrivals)
	}
}

func TestLatencyKneeIsAbsentWhenNoRungHasSuccesses(t *testing.T) {
	summary := onlySummary(t, identifyLadders([]cellResult{
		rung(8, withNoSuccesses()),
		rung(16, withNoSuccesses()),
	}, 1.5, testBinding))
	if summary.LatencyKnee != nil {
		t.Fatalf("latency knee = %+v, want none", summary.LatencyKnee)
	}
}

// A first rung with no successes cannot establish the baseline, so the next
// rung that does have latency becomes it.
func TestLatencyBaselineSkipsRungsWithoutSuccesses(t *testing.T) {
	summary := onlySummary(t, identifyLadders([]cellResult{
		rung(8, withNoSuccesses()),
		rung(16, withP95(100)),
		rung(32, withP95(300)),
	}, 1.5, testBinding))
	if summary.LatencyKnee == nil || summary.LatencyKnee.OfferedArrivals != 32 {
		t.Fatalf("latency knee = %+v, want 32 offered", summary.LatencyKnee)
	}
}

func TestDistressKneeCoversFailuresAndRailAborts(t *testing.T) {
	t.Run("failures", func(t *testing.T) {
		summary := onlySummary(t, identifyLadders([]cellResult{
			rung(8),
			rung(16, withFailures(map[string]int64{"startup_failed": 3})),
		}, 1.5, testBinding))
		if summary.DistressKnee == nil || summary.DistressKnee.Detail != "startup_failed" {
			t.Fatalf("distress knee = %+v", summary.DistressKnee)
		}
	})
	t.Run("rail abort", func(t *testing.T) {
		aborted := rung(16)
		aborted.AbortedAtRail = railAvailableMemory
		summary := onlySummary(t, identifyLadders(
			[]cellResult{rung(8), aborted}, 1.5, testBinding,
		))
		if summary.DistressKnee == nil || summary.DistressKnee.Detail != railAvailableMemory {
			t.Fatalf("distress knee = %+v", summary.DistressKnee)
		}
	})
}

func TestKneesAtDifferentRungsAreReportedIndependently(t *testing.T) {
	summary := onlySummary(t, identifyLadders([]cellResult{
		rung(8, withP95(100)),
		rung(16, withP95(300)),
		rung(32, withP95(300), withRefusals(map[string]int64{"quota_exceeded": 1})),
		rung(64, withP95(300), withFailures(map[string]int64{"startup_failed": 2})),
	}, 1.5, testBinding))
	if summary.LatencyKnee.OfferedArrivals != 16 {
		t.Fatalf("latency knee at %d, want 16", summary.LatencyKnee.OfferedArrivals)
	}
	if summary.RefusalKnee.OfferedArrivals != 32 {
		t.Fatalf("refusal knee at %d, want 32", summary.RefusalKnee.OfferedArrivals)
	}
	if summary.DistressKnee.OfferedArrivals != 64 {
		t.Fatalf("distress knee at %d, want 64", summary.DistressKnee.OfferedArrivals)
	}
}

func TestSingleRungLadderIsSummarised(t *testing.T) {
	summary := onlySummary(t, identifyLadders([]cellResult{rung(8)}, 1.5, testBinding))
	if summary.Steps != 1 || summary.LargestFullyAdmitted != 8 {
		t.Fatalf("single-rung summary = %+v", summary)
	}
	if summary.LatencyKnee != nil {
		t.Fatal("a single rung cannot have a latency knee")
	}
}

func TestNonMonotonicCellsAreExcludedFromTheLadder(t *testing.T) {
	summary := onlySummary(t, identifyLadders([]cellResult{
		rung(8), rung(16), rung(4), rung(16), rung(32),
	}, 1.5, testBinding))
	if summary.Steps != 3 {
		t.Fatalf("steps = %d, want 3: only strictly increasing rungs count", summary.Steps)
	}
	if summary.LargestFullyAdmitted != 32 {
		t.Fatalf("largest fully admitted = %d, want 32", summary.LargestFullyAdmitted)
	}
}

func TestLaddersAreSeparatedByMeasurementKindAndResident(t *testing.T) {
	createReady := rung(8)
	startReady := rung(8)
	startReady.Measurement = measurementStartReady
	sawtooth := rung(8)
	sawtooth.PatternKind = patternSawtooth
	resident := rung(8)
	resident.ResidentPopulation = 4
	summaries := identifyLadders(
		[]cellResult{createReady, startReady, sawtooth, resident}, 1.5, testBinding,
	)
	if len(summaries) != 4 {
		t.Fatalf("ladders = %d, want 4", len(summaries))
	}
}

func TestCapacitySectionNamesEveryKnee(t *testing.T) {
	summaries := identifyLadders([]cellResult{
		rung(8, withP95(100)),
		rung(16, withP95(400), withRefusals(map[string]int64{"quota_exceeded": 4})),
	}, 1.5, testBinding)
	var human bytes.Buffer
	if err := writeCapacitySection(&human, summaries); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"capacity", "largest fully admitted 8", "quota_exceeded",
		"subject quota: active instances", "refusal knee", "latency knee", "distress knee", "none",
	} {
		if !strings.Contains(human.String(), fragment) {
			t.Fatalf("capacity section is missing %q:\n%s", fragment, human.String())
		}
	}
}
