//go:build scenario_live

package scenario_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const microsandboxColdStartSamples = 30

// coldStartMaterializationDigestVariable names the digest input for the
// backend under qualification; the cold-start evidence records it verbatim.
func coldStartMaterializationDigestVariable() string {
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "gvisor" {
		return "SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST"
	}
	return "SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST"
}

type microsandboxColdStartEvidence struct {
	SchemaVersion         int                          `json:"schemaVersion"`
	SourceCommit          string                       `json:"sourceCommit"`
	CompletedAt           string                       `json:"completedAt"`
	BackendVersion        string                       `json:"backendVersion"`
	HostPlatform          string                       `json:"hostPlatform"`
	MaterializationDigest string                       `json:"materializationDigest"`
	Samples               int                          `json:"samples"`
	StartToReady          snapshotResumeDuration       `json:"startToReadyMilliseconds"`
	Stages                []snapshotResumeStageSummary `json:"bootStages"`
	PeakHelperRSSKiB      snapshotResumeDuration       `json:"peakHelperRssKiB"`
}

type microsandboxLifecycleLog struct {
	Message        string `json:"msg"`
	SandboxID      string `json:"sandboxId"`
	Stage          string `json:"stage"`
	HelperPID      int    `json:"helperPid"`
	BackendVersion string `json:"backendVersion"`
	HostPlatform   string `json:"hostPlatform"`
}

func TestScenarioColdBootBackendRecordsThirtyColdStarts(t *testing.T) {
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") == "firecracker" {
		return
	}
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-microsandbox-cold-start-observation",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)

	startToReady := make([]float64, 0, microsandboxColdStartSamples)
	peakRSS := make([]float64, 0, microsandboxColdStartSamples)
	stages := make([]map[string]float64, 0, microsandboxColdStartSamples)
	var backendVersion, hostPlatform string
	for index := 0; index < microsandboxColdStartSamples; index++ {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		handle, createOperation := createScenarioSandbox(
			t, fixture, profile, fmt.Sprintf("microsandbox-cold-%02d", index+1),
		)
		ready := waitForSandbox(
			t, ctx, handle,
			secondboxclient.SandboxStateReady,
			secondboxclient.SandboxStateFailed,
		)
		if ready.State != contracts.SandboxStateReady {
			cancel()
			t.Fatalf("SecondBox Microsandbox cold start %d = %#v", index+1, ready)
		}
		waitForScenarioOperation(t, ctx, fixture.subject, createOperation)

		timing := scenarioJSON[contracts.OperationTiming](
			t, ctx, fixture.subject, "getOperationTiming",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"operationId": createOperation.ID},
			},
		)
		if len(timing.Boots) != 1 || !timing.Boots[0].Completed {
			cancel()
			t.Fatalf("SecondBox Microsandbox cold start %d boot timing = %#v", index+1, timing.Boots)
		}
		boot := timing.Boots[0]
		startToReady = append(startToReady, boot.DurationMilliseconds)
		stageSample := make(map[string]float64, len(boot.Stages))
		for _, stage := range boot.Stages {
			stageSample[stage.Stage] = stage.ElapsedMilliseconds
		}
		stages = append(stages, stageSample)

		lifecycle := microsandboxReadyLifecycleLog(t, ready.ID)
		backendVersion = lifecycle.BackendVersion
		hostPlatform = lifecycle.HostPlatform
		peakRSS = append(peakRSS, float64(microsandboxHelperPeakRSSKiB(t, lifecycle.HelperPID)))

		deleteOperation := requestScenarioLifecycle(
			t, ctx, handle, "microsandbox-cold-delete",
			func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				return handle.Delete(ctx, options)
			},
		)
		waitForScenarioOperation(t, ctx, fixture.subject, deleteOperation)
		waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateDeleted)
		cancel()
	}

	evidence := microsandboxColdStartEvidence{
		SchemaVersion:         1,
		SourceCommit:          requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_SOURCE_COMMIT"),
		CompletedAt:           time.Now().UTC().Format(time.RFC3339Nano),
		BackendVersion:        backendVersion,
		HostPlatform:          hostPlatform,
		MaterializationDigest: requireScenarioEnvironment(t, coldStartMaterializationDigestVariable()),
		Samples:               len(startToReady),
		StartToReady: snapshotResumeDuration{
			Samples: len(startToReady),
			P50:     snapshotResumePercentile(startToReady, 50),
			P95:     snapshotResumePercentile(startToReady, 95),
			Max:     snapshotResumePercentile(startToReady, 100),
		},
		Stages: summarizeSnapshotResumeStages(stages),
		PeakHelperRSSKiB: snapshotResumeDuration{
			Samples: len(peakRSS),
			P50:     snapshotResumePercentile(peakRSS, 50),
			P95:     snapshotResumePercentile(peakRSS, 95),
			Max:     snapshotResumePercentile(peakRSS, 100),
		},
	}
	writeMicrosandboxColdStartEvidence(t, evidence)
	t.Logf(
		"SecondBox Microsandbox 30 cold starts: start-to-ready %.1f/%.1f ms p50/p95; peak helper RSS %.0f/%.0f KiB p50/p95",
		evidence.StartToReady.P50, evidence.StartToReady.P95,
		evidence.PeakHelperRSSKiB.P50, evidence.PeakHelperRSSKiB.P95,
	)
}

func microsandboxReadyLifecycleLog(t *testing.T, sandboxID string) microsandboxLifecycleLog {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		logs := scenarioComposeOutput(t, "logs", "--no-color", "secondbox-runner")
		for _, line := range strings.Split(string(logs), "\n") {
			start := strings.IndexByte(line, '{')
			if start < 0 {
				continue
			}
			var record microsandboxLifecycleLog
			if json.Unmarshal([]byte(line[start:]), &record) == nil &&
				record.Message == "runner operation evidence" &&
				record.SandboxID == sandboxID && record.Stage == "ready" &&
				record.HelperPID > 0 && record.BackendVersion != "" && record.HostPlatform != "" {
				return record
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("SecondBox Microsandbox ready lifecycle evidence missing for Sandbox %s", sandboxID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func microsandboxHelperPeakRSSKiB(t *testing.T, helperPID int) int {
	t.Helper()
	var output []byte
	if os.Getenv("SECONDBOX_SCENARIO_SERVICE_CONTROL") != "" {
		output = scenarioComposeOutput(t, "helper-rss-kib", strconv.Itoa(helperPID))
	} else {
		// The compute may be a process tree (the gVisor mount supervisor
		// parents the runsc sentry and gofer), so the sample sums peak RSS
		// across the helper and every descendant rather than the outer
		// process alone; a childless Microsandbox helper sums to itself.
		output = scenarioComposeOutput(
			t, "exec", "-T", "secondbox-runner", "sh", "-c",
			`peak=0
walk() {
  v=$(awk '$1=="VmHWM:"{print $2}' "/proc/$1/status" 2>/dev/null)
  [ -n "$v" ] && peak=$((peak+v))
  for child in $(cat /proc/$1/task/*/children 2>/dev/null); do walk "$child"; done
}
walk "$0"
echo "$peak"`, strconv.Itoa(helperPID),
		)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || value <= 0 {
		t.Fatalf("SecondBox Microsandbox helper %d peak RSS = %q: %v", helperPID, output, err)
	}
	return value
}

func writeMicrosandboxColdStartEvidence(t *testing.T, evidence microsandboxColdStartEvidence) {
	t.Helper()
	path := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_MICROSANDBOX_COLD_START_EVIDENCE")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatalf("SECONDBOX_SCENARIO_MICROSANDBOX_COLD_START_EVIDENCE must be a clean absolute path: %q", path)
	}
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatalf("SecondBox Microsandbox cold-start evidence encoding: %v", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("SecondBox Microsandbox cold-start evidence write: %v", err)
	}
}
