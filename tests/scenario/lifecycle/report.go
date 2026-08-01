package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

type transitionSummary struct {
	Samples             int   `json:"samples"`
	P50Milliseconds     int64 `json:"p50Milliseconds"`
	P95Milliseconds     int64 `json:"p95Milliseconds"`
	P99Milliseconds     int64 `json:"p99Milliseconds"`
	MaximumMilliseconds int64 `json:"maximumMilliseconds"`
}

type cellResult struct {
	Measurement               string               `json:"measurement"`
	Pattern                   string               `json:"pattern"`
	PatternKind               string               `json:"patternKind"`
	ResidentPopulation        int                  `json:"residentPopulation"`
	OfferedArrivals           int                  `json:"offeredArrivals"`
	CompletedArrivals         int64                `json:"completedArrivals"`
	ShedArrivals              int64                `json:"shedArrivals"`
	OfferedRatePerSecond      *float64             `json:"offeredRatePerSecond"`
	CompletionRatePerSecond   float64              `json:"completionRatePerSecond"`
	ArrivalWindowMilliseconds int64                `json:"arrivalWindowMilliseconds"`
	ElapsedMilliseconds       int64                `json:"elapsedMilliseconds"`
	PeakOutstandingArrivals   int64                `json:"peakOutstandingArrivals"`
	Latency                   *transitionSummary   `json:"latency"`
	BootStages                []bootStageSummary   `json:"bootStages"`
	DominantBootStage         string               `json:"dominantBootStage"`
	StartupSpans              []startupSpanSummary `json:"startupSpans"`
	Refusals                  map[string]int64     `json:"refusals"`
	Failures                  map[string]int64     `json:"failures"`
	Occupancy                 []occupancySample    `json:"occupancy"`
}

type bootStageSummary struct {
	Stage           string `json:"stage"`
	Samples         int    `json:"samples"`
	P50Milliseconds int64  `json:"p50Milliseconds"`
	P95Milliseconds int64  `json:"p95Milliseconds"`
	P99Milliseconds int64  `json:"p99Milliseconds"`
}

type startupSpanSummary struct {
	Span            string `json:"span"`
	Samples         int    `json:"samples"`
	P50Milliseconds int64  `json:"p50Milliseconds"`
	P95Milliseconds int64  `json:"p95Milliseconds"`
	P99Milliseconds int64  `json:"p99Milliseconds"`
}

// IncompleteReason names the cell that ended the run early. A run that stops
// partway is still worth reporting: the cells that completed are valid
// measurements, and discarding them would make a safety abort destroy the data
// it exists to protect. The field is absent from a clean run's report.
type lifecycleReport struct {
	SchemaVersion    int          `json:"schemaVersion"`
	StartedAt        time.Time    `json:"startedAt"`
	CompletedAt      time.Time    `json:"completedAt"`
	SourceCommit     string       `json:"sourceCommit"`
	GoVersion        string       `json:"goVersion"`
	ArtifactManifest string       `json:"artifactManifestDigest"`
	ShedArrivals     int64        `json:"shedArrivals"`
	IncompleteReason string       `json:"incompleteReason,omitempty"`
	Results          []cellResult `json:"results"`
}

func summarizeBootStages(stages map[string][]time.Duration) ([]bootStageSummary, string) {
	summaries := make([]bootStageSummary, 0, len(stages))
	dominant := ""
	dominantP50 := int64(-1)
	for stage, durations := range stages {
		sorted := append([]time.Duration(nil), durations...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		summary := bootStageSummary{
			Stage:           stage,
			Samples:         len(sorted),
			P50Milliseconds: percentile(sorted, 0.50).Milliseconds(),
			P95Milliseconds: percentile(sorted, 0.95).Milliseconds(),
			P99Milliseconds: percentile(sorted, 0.99).Milliseconds(),
		}
		summaries = append(summaries, summary)
		if summary.P50Milliseconds > dominantP50 {
			dominantP50 = summary.P50Milliseconds
			dominant = stage
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].P50Milliseconds != summaries[j].P50Milliseconds {
			return summaries[i].P50Milliseconds > summaries[j].P50Milliseconds
		}
		return summaries[i].Stage < summaries[j].Stage
	})
	return summaries, dominant
}

func summarizeStartupSpans(spans map[string][]time.Duration) []startupSpanSummary {
	summaries := make([]startupSpanSummary, 0, len(spans))
	for name, durations := range spans {
		sorted := append([]time.Duration(nil), durations...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		summaries = append(summaries, startupSpanSummary{
			Span:            name,
			Samples:         len(sorted),
			P50Milliseconds: percentile(sorted, 0.50).Milliseconds(),
			P95Milliseconds: percentile(sorted, 0.95).Milliseconds(),
			P99Milliseconds: percentile(sorted, 0.99).Milliseconds(),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Span < summaries[j].Span
	})
	return summaries
}

func writeLifecycleReport(path string, report lifecycleReport) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle encode report: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("SecondBox lifecycle write report: %w", err)
	}
	return nil
}

// writeHumanReport leads with start-to-ready, the ephemeral hot path the
// benchmark exists to measure.
func writeHumanReport(writer io.Writer, report lifecycleReport) error {
	if _, err := fmt.Fprintf(writer, "\nSecondBox Sandbox lifecycle benchmark\n"); err != nil {
		return err
	}
	if report.IncompleteReason != "" {
		if _, err := fmt.Fprintf(
			writer,
			"INCOMPLETE: the run ended early and the cells below are a partial record\n  %s\n",
			report.IncompleteReason,
		); err != nil {
			return err
		}
	}
	for _, measurement := range []string{
		measurementStartReady,
		measurementCreateReady,
		measurementStopStopped,
		measurementDeleteGone,
	} {
		rows := filterMeasurement(report.Results, measurement)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(writer, "\n%s\n", measurement)
		fmt.Fprintf(
			writer,
			"%-18s %-9s %8s %9s %9s %9s %9s %9s %11s\n",
			"pattern", "resident", "offered", "done", "off/s", "drain/s",
			"p50ms", "p95ms", "outstanding",
		)
		for _, row := range rows {
			fmt.Fprintf(
				writer,
				"%-18s %-9d %8d %9d %9s %9.2f %9s %9s %11d\n",
				row.Pattern, row.ResidentPopulation, row.OfferedArrivals,
				row.CompletedArrivals, formatRate(row.OfferedRatePerSecond),
				row.CompletionRatePerSecond, formatLatency(row.Latency, 0.50),
				formatLatency(row.Latency, 0.95), row.PeakOutstandingArrivals,
			)
		}
	}
	for _, row := range report.Results {
		if len(row.BootStages) == 0 && len(row.StartupSpans) == 0 {
			continue
		}
		fmt.Fprintf(
			writer,
			"\nStartup attribution %s/%s resident=%d (dominant runner stage: %s)\n",
			row.Measurement, row.Pattern, row.ResidentPopulation, row.DominantBootStage,
		)
		if len(row.BootStages) > 0 {
			fmt.Fprintf(writer, "%-24s %7s %8s %8s %8s\n", "runner stage", "n", "p50ms", "p95ms", "p99ms")
		}
		for _, stage := range row.BootStages {
			fmt.Fprintf(
				writer, "%-24s %7d %8d %8d %8d\n",
				stage.Stage, stage.Samples, stage.P50Milliseconds,
				stage.P95Milliseconds, stage.P99Milliseconds,
			)
		}
		if len(row.StartupSpans) > 0 {
			fmt.Fprintf(writer, "  control-plane spans below overlap and are not additive\n")
			fmt.Fprintf(writer, "%-24s %7s %8s %8s %8s\n", "span", "n", "p50ms", "p95ms", "p99ms")
		}
		for _, span := range row.StartupSpans {
			fmt.Fprintf(
				writer, "%-24s %7d %8d %8d %8d\n",
				span.Span, span.Samples, span.P50Milliseconds,
				span.P95Milliseconds, span.P99Milliseconds,
			)
		}
	}
	fmt.Fprintf(writer, "\nRefusals and failures\n")
	for _, row := range report.Results {
		if len(row.Refusals) == 0 && len(row.Failures) == 0 {
			continue
		}
		fmt.Fprintf(
			writer, "  %s/%s resident=%d refusals=%v failures=%v\n",
			row.Measurement, row.Pattern, row.ResidentPopulation, row.Refusals, row.Failures,
		)
	}
	if report.ShedArrivals > 0 {
		fmt.Fprintf(
			writer,
			"\nShed arrivals: %d (offered while in-flight was at the configured maximum)\n",
			report.ShedArrivals,
		)
	}
	return nil
}

func formatRate(rate *float64) string {
	if rate == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *rate)
}

func formatLatency(latency *transitionSummary, percentile float64) string {
	if latency == nil {
		return "-"
	}
	switch percentile {
	case 0.50:
		return fmt.Sprintf("%d", latency.P50Milliseconds)
	case 0.95:
		return fmt.Sprintf("%d", latency.P95Milliseconds)
	default:
		return "-"
	}
}

func filterMeasurement(results []cellResult, measurement string) []cellResult {
	filtered := make([]cellResult, 0, len(results))
	for _, result := range results {
		if result.Measurement == measurement {
			filtered = append(filtered, result)
		}
	}
	return filtered
}
