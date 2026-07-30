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
	Cycle                  string                       `json:"cycle"`
	Pattern                string                       `json:"pattern"`
	PatternKind            string                       `json:"patternKind"`
	ResidentPopulation     int                          `json:"residentPopulation"`
	OfferedArrivals        int                          `json:"offeredArrivals"`
	CompletedCycles        int64                        `json:"completedCycles"`
	OfferedRatePerSecond   float64                      `json:"offeredRatePerSecond"`
	CompletedRatePerSecond float64                      `json:"completedRatePerSecond"`
	ElapsedMilliseconds    int64                        `json:"elapsedMilliseconds"`
	PeakBacklog            int64                        `json:"peakBacklog"`
	Transitions            map[string]transitionSummary `json:"transitions"`
	Refusals               map[string]int64             `json:"refusals"`
	Failures               map[string]int64             `json:"failures"`
	Occupancy              []occupancySample            `json:"occupancy"`
}

type bootStageSummary struct {
	Stage           string `json:"stage"`
	Samples         int    `json:"samples"`
	P50Milliseconds int64  `json:"p50Milliseconds"`
	P95Milliseconds int64  `json:"p95Milliseconds"`
	P99Milliseconds int64  `json:"p99Milliseconds"`
}

type lifecycleReport struct {
	SchemaVersion     int                `json:"schemaVersion"`
	StartedAt         time.Time          `json:"startedAt"`
	CompletedAt       time.Time          `json:"completedAt"`
	SourceCommit      string             `json:"sourceCommit"`
	GoVersion         string             `json:"goVersion"`
	ArtifactManifest  string             `json:"artifactManifestDigest"`
	ShedArrivals      int64              `json:"shedArrivals"`
	Results           []cellResult       `json:"results"`
	BootStages        []bootStageSummary `json:"bootStages"`
	DominantBootStage string             `json:"dominantBootStage"`
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

// writeHumanReport leads with the warm cycle, which is the ephemeral hot path
// the benchmark exists to measure.
func writeHumanReport(writer io.Writer, report lifecycleReport) error {
	if _, err := fmt.Fprintf(writer, "\nSecondBox Sandbox lifecycle benchmark\n"); err != nil {
		return err
	}
	for _, cycle := range []string{cycleWarm, cycleCold} {
		rows := filterCycle(report.Results, cycle)
		if len(rows) == 0 {
			continue
		}
		label := "warm (start/stop, Workspace retained)"
		if cycle == cycleCold {
			label = "cold (create/delete)"
		}
		fmt.Fprintf(writer, "\n%s\n", label)
		fmt.Fprintf(
			writer,
			"%-18s %-9s %8s %9s %9s %9s %9s %9s %8s\n",
			"pattern", "resident", "offered", "done", "off/s", "done/s", "p50ms", "p95ms", "backlog",
		)
		for _, row := range rows {
			primary := transitionStartReady
			if cycle == cycleCold {
				primary = transitionCreateReady
			}
			summary := row.Transitions[primary]
			fmt.Fprintf(
				writer,
				"%-18s %-9d %8d %9d %9.2f %9.2f %9d %9d %8d\n",
				row.Pattern, row.ResidentPopulation, row.OfferedArrivals, row.CompletedCycles,
				row.OfferedRatePerSecond, row.CompletedRatePerSecond,
				summary.P50Milliseconds, summary.P95Milliseconds, row.PeakBacklog,
			)
		}
	}
	fmt.Fprintf(writer, "\nTransition detail (all cells)\n")
	fmt.Fprintf(
		writer, "%-6s %-18s %-9s %-18s %7s %8s %8s %8s\n",
		"cycle", "pattern", "resident", "transition", "n", "p50ms", "p95ms", "p99ms",
	)
	for _, row := range report.Results {
		names := make([]string, 0, len(row.Transitions))
		for name := range row.Transitions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			summary := row.Transitions[name]
			fmt.Fprintf(
				writer, "%-6s %-18s %-9d %-18s %7d %8d %8d %8d\n",
				row.Cycle, row.Pattern, row.ResidentPopulation, name,
				summary.Samples, summary.P50Milliseconds,
				summary.P95Milliseconds, summary.P99Milliseconds,
			)
		}
	}
	if len(report.BootStages) > 0 {
		fmt.Fprintf(writer, "\nBoot stage attribution (dominant: %s)\n", report.DominantBootStage)
		fmt.Fprintf(writer, "%-24s %7s %8s %8s %8s\n", "stage", "n", "p50ms", "p95ms", "p99ms")
		for _, stage := range report.BootStages {
			fmt.Fprintf(
				writer, "%-24s %7d %8d %8d %8d\n",
				stage.Stage, stage.Samples, stage.P50Milliseconds,
				stage.P95Milliseconds, stage.P99Milliseconds,
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
			row.Cycle, row.Pattern, row.ResidentPopulation, row.Refusals, row.Failures,
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

func filterCycle(results []cellResult, cycle string) []cellResult {
	filtered := make([]cellResult, 0, len(results))
	for _, result := range results {
		if result.Cycle == cycle {
			filtered = append(filtered, result)
		}
	}
	return filtered
}
