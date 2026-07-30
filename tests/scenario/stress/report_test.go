package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurationPercentilesUseNearestRank(t *testing.T) {
	values := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		40 * time.Millisecond, 100 * time.Millisecond,
	}
	got := durationPercentiles(values)
	if got.Count != 5 || *got.P50Milliseconds != 30 ||
		*got.P95Milliseconds != 100 || *got.P99Milliseconds != 100 {
		t.Fatalf("percentiles = %#v", got)
	}
}

func TestLatencyDegradationUsesFirstMeasuredLevel(t *testing.T) {
	p95a, p95b, p95c := int64(100), int64(149), int64(150)
	results := []workloadResult{
		{Workload: workloadBufferedExec, Latency: latencyPercentiles{P95Milliseconds: &p95a}},
		{Workload: workloadBufferedExec, Latency: latencyPercentiles{P95Milliseconds: &p95b}},
		{Workload: workloadBufferedExec, Latency: latencyPercentiles{P95Milliseconds: &p95c}},
	}
	markLatencyDegradation(results, 1.5)
	if results[1].LatencyDegraded || !results[2].LatencyDegraded {
		t.Fatalf("degradation flags = %#v", results)
	}
}

func TestStressReportIsPrivateAndRefusesOverwrite(t *testing.T) {
	output := filepath.Join(t.TempDir(), "stress.json")
	report := stressReport{
		SchemaVersion: 1, StartedAt: time.Unix(1, 0).UTC(),
		CompletedAt: time.Unix(2, 0).UTC(),
	}
	if err := writeStressReport(output, report); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %o", info.Mode().Perm())
	}
	if err := writeStressReport(output, report); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite error = %v", err)
	}
}

func TestHumanReportNamesMeasuredAndConfiguredSaturation(t *testing.T) {
	p50, p95, p99 := int64(10), int64(20), int64(30)
	report := stressReport{
		StartedAt: time.Unix(1, 0).UTC(), CompletedAt: time.Unix(2, 0).UTC(),
		ConfiguredBinding: configuredLimit{Name: "guest IP capacity", Capacity: 4},
		Results: []workloadResult{{
			Workload: workloadSandboxCreate, Concurrency: 4, Attempts: 3,
			Successes: 1, AdmissionRefusals: 2, ThroughputPerSecond: 1,
			Latency:                latencyPercentiles{Count: 1, P50Milliseconds: &p50, P95Milliseconds: &p95, P99Milliseconds: &p99},
			ConfiguredLimitReached: true, LatencyDegraded: true,
		}},
		BootStages: []bootStageResult{{
			Stage:   "ready",
			Latency: latencyPercentiles{Count: 1, P50Milliseconds: &p50, P95Milliseconds: &p95, P99Milliseconds: &p99},
		}},
		DominantBootStage: "ready",
	}
	var output bytes.Buffer
	if err := writeHumanReport(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"guest IP capacity", "refusal+latency+configured-limit", "Dominant boot stage",
	} {
		if !strings.Contains(output.String(), required) {
			t.Fatalf("output missing %q:\n%s", required, output.String())
		}
	}
}

func TestStressResultGateAllowsOnlyAtOrAboveBindingRefusal(t *testing.T) {
	binding := configuredLimit{Name: "memory", Capacity: 4}
	if err := verifyStressResults([]workloadResult{{
		Workload: workloadSandboxCreate, Concurrency: 4, AdmissionRefusals: 1,
	}}, binding); err != nil {
		t.Fatal(err)
	}
	if err := verifyStressResults([]workloadResult{{
		Workload: workloadSandboxCreate, Concurrency: 2, AdmissionRefusals: 1,
	}}, binding); err == nil {
		t.Fatal("refusal below binding passed")
	}
	if err := verifyStressResults([]workloadResult{{
		Workload: workloadBufferedExec, Concurrency: 4, Failures: 1,
	}}, binding); err == nil {
		t.Fatal("unexplained workload failure passed")
	}
}
