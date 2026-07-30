package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type stressReport struct {
	SchemaVersion     int                                      `json:"schemaVersion"`
	StartedAt         time.Time                                `json:"startedAt"`
	CompletedAt       time.Time                                `json:"completedAt"`
	SourceCommit      string                                   `json:"sourceCommit"`
	GoVersion         string                                   `json:"goVersion"`
	ArtifactManifest  string                                   `json:"artifactManifestDigest"`
	Configuration     stressReportConfiguration                `json:"configuration"`
	ConfiguredBinding configuredLimit                          `json:"configuredFirstBinding"`
	Results           []workloadResult                         `json:"results"`
	BootStages        []bootStageResult                        `json:"bootStages"`
	DominantBootStage string                                   `json:"dominantBootStage,omitempty"`
	DeploymentTiming  *secondboxclient.DeploymentTimingSummary `json:"deploymentTiming,omitempty"`
	CollectionError   string                                   `json:"collectionError,omitempty"`
}

type stressReportConfiguration struct {
	Workloads               []string `json:"workloads"`
	ConcurrencyLevels       []int    `json:"concurrencyLevels"`
	DurationSeconds         int      `json:"durationSeconds"`
	TimingWindowSeconds     int      `json:"timingWindowSeconds"`
	LatencyDegradationRatio float64  `json:"latencyDegradationRatio"`
	FileTransferBytes       int      `json:"fileTransferBytes"`
	StreamingOutputBytes    int      `json:"streamingOutputBytes"`
}

type workloadResult struct {
	Workload               string             `json:"workload"`
	Concurrency            int                `json:"concurrency"`
	ElapsedMilliseconds    int64              `json:"elapsedMilliseconds"`
	Attempts               int64              `json:"attempts"`
	Successes              int64              `json:"successes"`
	AdmissionRefusals      int64              `json:"admissionRefusals"`
	QueuedAdmissions       int64              `json:"queuedAdmissions"`
	Failures               int64              `json:"failures"`
	ThroughputPerSecond    float64            `json:"throughputPerSecond"`
	Latency                latencyPercentiles `json:"latency"`
	ProblemCounts          map[string]int64   `json:"problemCounts"`
	ConfiguredLimitReached bool               `json:"configuredLimitReached"`
	LatencyDegraded        bool               `json:"latencyDegraded"`
}

type latencyPercentiles struct {
	Count           int64  `json:"count"`
	P50Milliseconds *int64 `json:"p50Milliseconds,omitempty"`
	P95Milliseconds *int64 `json:"p95Milliseconds,omitempty"`
	P99Milliseconds *int64 `json:"p99Milliseconds,omitempty"`
}

type bootStageResult struct {
	Stage   string             `json:"stage"`
	Latency latencyPercentiles `json:"latency"`
}

type resultSamples struct {
	workload          string
	concurrency       int
	startedAt         time.Time
	completedAt       time.Time
	durations         []time.Duration
	admissionRefusals int64
	queuedAdmissions  int64
	failures          int64
	problemCounts     map[string]int64
}

func (samples resultSamples) report(binding configuredLimit) workloadResult {
	elapsed := samples.completedAt.Sub(samples.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	successes := int64(len(samples.durations))
	attempts := successes + samples.admissionRefusals + samples.queuedAdmissions + samples.failures
	throughput := 0.0
	if elapsed > 0 {
		throughput = float64(successes) / elapsed.Seconds()
	}
	return workloadResult{
		Workload: samples.workload, Concurrency: samples.concurrency,
		ElapsedMilliseconds: elapsed.Milliseconds(), Attempts: attempts,
		Successes: successes, AdmissionRefusals: samples.admissionRefusals,
		QueuedAdmissions: samples.queuedAdmissions,
		Failures:         samples.failures, ThroughputPerSecond: throughput,
		Latency:                durationPercentiles(samples.durations),
		ProblemCounts:          samples.problemCounts,
		ConfiguredLimitReached: samples.concurrency >= binding.Capacity,
	}
}

func durationPercentiles(values []time.Duration) latencyPercentiles {
	if len(values) == 0 {
		return latencyPercentiles{}
	}
	milliseconds := make([]int64, len(values))
	for index, value := range values {
		milliseconds[index] = max(value.Milliseconds(), 0)
	}
	slices.Sort(milliseconds)
	p50 := nearestRank(milliseconds, 0.50)
	p95 := nearestRank(milliseconds, 0.95)
	p99 := nearestRank(milliseconds, 0.99)
	return latencyPercentiles{
		Count:           int64(len(milliseconds)),
		P50Milliseconds: &p50, P95Milliseconds: &p95, P99Milliseconds: &p99,
	}
}

func nearestRank(sorted []int64, percentile float64) int64 {
	index := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	index = max(index, 0)
	index = min(index, len(sorted)-1)
	return sorted[index]
}

func markLatencyDegradation(results []workloadResult, ratio float64) {
	baselines := make(map[string]int64)
	for index := range results {
		result := &results[index]
		if result.Latency.P95Milliseconds == nil {
			continue
		}
		baseline, exists := baselines[result.Workload]
		if !exists {
			baselines[result.Workload] = *result.Latency.P95Milliseconds
			continue
		}
		if baseline == 0 {
			result.LatencyDegraded = *result.Latency.P95Milliseconds > 0
			continue
		}
		result.LatencyDegraded =
			float64(*result.Latency.P95Milliseconds) >= float64(baseline)*ratio
	}
}

func summarizeBootStages(values map[string][]time.Duration) ([]bootStageResult, string) {
	stages := make([]string, 0, len(values))
	for stage := range values {
		stages = append(stages, stage)
	}
	slices.Sort(stages)
	results := make([]bootStageResult, 0, len(stages))
	dominant := ""
	var dominantP95 int64 = -1
	for _, stage := range stages {
		latency := durationPercentiles(values[stage])
		results = append(results, bootStageResult{Stage: stage, Latency: latency})
		if latency.P95Milliseconds != nil && *latency.P95Milliseconds > dominantP95 {
			dominant = stage
			dominantP95 = *latency.P95Milliseconds
		}
	}
	return results, dominant
}

func writeStressReport(path string, report stressReport) (returnErr error) {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("SecondBox stress output path must be absolute")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("SecondBox stress output path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox stress output inspect failed: %w", err)
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("SecondBox stress output parent must be an existing non-symbolic-link directory")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("SecondBox stress output create failed: %w", err)
	}
	defer func() {
		closeErr := file.Close()
		if returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("SecondBox stress output close failed: %w", closeErr)
		}
		if returnErr != nil {
			returnErr = errors.Join(returnErr, os.Remove(path))
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("SecondBox stress output encode failed: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("SecondBox stress output sync failed: %w", err)
	}
	return nil
}

func writeHumanReport(writer io.Writer, report stressReport) error {
	if _, err := fmt.Fprintf(
		writer,
		"SecondBox stress run %s to %s\nConfigured first binding: %s (%d concurrent instances)\n\n",
		report.StartedAt.Format(time.RFC3339),
		report.CompletedAt.Format(time.RFC3339),
		report.ConfiguredBinding.Name,
		report.ConfiguredBinding.Capacity,
	); err != nil {
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(
		table,
		"WORKLOAD\tCONCURRENCY\tATTEMPTS\tSUCCESS\tQUEUED\tREFUSED\tFAILED\tOPS/S\tP50 MS\tP95 MS\tP99 MS\tSATURATION",
	); err != nil {
		return err
	}
	for _, result := range report.Results {
		saturation := "none"
		if result.QueuedAdmissions > 0 {
			saturation = "queue"
		}
		if result.AdmissionRefusals > 0 {
			if saturation == "none" {
				saturation = "refusal"
			} else {
				saturation += "+refusal"
			}
		}
		if result.LatencyDegraded {
			if saturation == "none" {
				saturation = "latency"
			} else {
				saturation += "+latency"
			}
		}
		if result.ConfiguredLimitReached {
			if saturation == "none" {
				saturation = "configured-limit"
			} else {
				saturation += "+configured-limit"
			}
		}
		if _, err := fmt.Fprintf(
			table,
			"%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\t%s\t%s\t%s\t%s\n",
			result.Workload, result.Concurrency, result.Attempts, result.Successes,
			result.QueuedAdmissions, result.AdmissionRefusals,
			result.Failures, result.ThroughputPerSecond,
			optionalMilliseconds(result.Latency.P50Milliseconds),
			optionalMilliseconds(result.Latency.P95Milliseconds),
			optionalMilliseconds(result.Latency.P99Milliseconds),
			saturation,
		); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "\nBOOT STAGE\tSAMPLES\tP50 MS\tP95 MS\tP99 MS"); err != nil {
		return err
	}
	stageTable := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	for _, stage := range report.BootStages {
		if _, err := fmt.Fprintf(
			stageTable, "%s\t%d\t%s\t%s\t%s\n",
			stage.Stage, stage.Latency.Count,
			optionalMilliseconds(stage.Latency.P50Milliseconds),
			optionalMilliseconds(stage.Latency.P95Milliseconds),
			optionalMilliseconds(stage.Latency.P99Milliseconds),
		); err != nil {
			return err
		}
	}
	if err := stageTable.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Dominant boot stage by p95: %s\n", report.DominantBootStage); err != nil {
		return err
	}
	if report.DeploymentTiming != nil {
		if _, err := fmt.Fprintf(
			writer,
			"Deployment window: %ds; boot p95=%s ms; Exec p95=%s ms; API p95=%s ms\n",
			report.DeploymentTiming.WindowSeconds,
			optionalMilliseconds(report.DeploymentTiming.Boot.P95Milliseconds),
			optionalMilliseconds(report.DeploymentTiming.Exec.P95Milliseconds),
			optionalMilliseconds(report.DeploymentTiming.API.P95Milliseconds),
		); err != nil {
			return err
		}
	}
	if report.CollectionError != "" {
		if _, err := fmt.Fprintf(
			writer, "Collection error after workload sweep: %s\n", report.CollectionError,
		); err != nil {
			return err
		}
	}
	return nil
}

func optionalMilliseconds(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10)
}
