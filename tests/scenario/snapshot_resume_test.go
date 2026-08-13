//go:build scenario_live

package scenario_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const snapshotResumeEvidenceSchemaVersion = 1

// snapshotResumeProjection records what this measurement is being judged
// against. Both numbers are arithmetic the plan performed on two separately
// measured spans — the 2026-08-06 unsaturated lifecycle baseline and the
// 2026-08-07 jailed resume gate — before any end-to-end resume existed. This
// group is what turns them into a control-plane measurement.
const (
	snapshotResumeStartToReadyProjectionMs  = 121
	snapshotResumeCreateToReadyProjectionMs = 206
	// A cold start's guest_negotiation span is 377/391 ms p50/p95. A resume
	// replaces the whole of it with a snapshot load, a first control response,
	// post-resume hardening, one assignment bind, and the guest handshake, which
	// the jailed gate measured at roughly 25 ms in total. The gate is generous
	// on purpose: it must fail when a Sandbox cold booted, not when the host is
	// busy.
	snapshotResumeGuestNegotiationCeilingMs = 200.0
)

type snapshotResumeEvidence struct {
	SchemaVersion          int                          `json:"schemaVersion"`
	SourceCommit           string                       `json:"sourceCommit"`
	CompletedAt            string                       `json:"completedAt"`
	TemplateID             string                       `json:"templateId"`
	TemplateBuildMillis    int64                        `json:"templateBuildMilliseconds"`
	TemplateAdmissionMs    int64                        `json:"cacheAdmissionMilliseconds"`
	ProfileMemoryMiB       int                          `json:"profileMemoryMiB"`
	ProfileWorkspaceMiB    int                          `json:"profileWorkspaceMiB"`
	ProfileVCPUs           int                          `json:"profileVcpus"`
	StartToReadyProjection int                          `json:"startToReadyProjectionMilliseconds"`
	CreateToReadyProjectn  int                          `json:"createToReadyProjectionMilliseconds"`
	Rungs                  []snapshotResumeRung         `json:"concurrencyRungs"`
	BootStages             []snapshotResumeStageSummary `json:"resumeStartBootStages"`
}

type snapshotResumeRung struct {
	Concurrency    int                    `json:"concurrency"`
	Arrivals       int                    `json:"arrivals"`
	CreateToReady  snapshotResumeDuration `json:"createToReadyMilliseconds"`
	StartToReady   snapshotResumeDuration `json:"startToReadyMilliseconds"`
	RefusalCount   int                    `json:"refusalCount"`
	ColdFallbackNo bool                   `json:"noColdFallbackObserved"`
}

type snapshotResumeDuration struct {
	Samples int     `json:"samples"`
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	Max     float64 `json:"max"`
}

type snapshotResumeStageSummary struct {
	Stage   string  `json:"stage"`
	Samples int     `json:"samples"`
	P50     float64 `json:"p50Milliseconds"`
	P95     float64 `json:"p95Milliseconds"`
}

// TestScenarioSnapshotResumeStartsStopsAndMeasures is the end-to-end gate for
// snapshot-resume startup. It drives a snapshot_resume Profile through the real
// control plane — create, ready, exec, stop, start, ready, delete — and then
// measures what the plan has so far only projected: create-to-ready and
// start-to-ready at concurrency 1 and 4, taken from the control plane's own
// Operation timings rather than from a client stopwatch.
//
// The resume path has no cold-boot fallback, so this group also proves the
// negative: the control plane's boot-stage attribution for a resumed start must
// not contain a cold boot's 377 ms guest_negotiation span. If the Runner had
// quietly cold booted, every other assertion here would still pass.
func TestScenarioSnapshotResumeStartsStopsAndMeasures(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	runner := waitForScenarioRunner(t, fixture, 90*time.Second)
	if !slices.Contains(runner.Capabilities, "snapshot-resume") {
		t.Fatalf(
			"SecondBox scenario Runner does not advertise snapshot-resume: capabilities = %v; "+
				"the template publisher must populate its cache before it starts",
			runner.Capabilities,
		)
	}
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-snapshot-resume",
		snapshotResumeProfileSpec(t),
	)

	evidence := snapshotResumeEvidence{
		SchemaVersion:          snapshotResumeEvidenceSchemaVersion,
		SourceCommit:           requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_SOURCE_COMMIT"),
		TemplateID:             requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_ID"),
		TemplateBuildMillis:    scenarioEnvironmentInt64(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_BUILD_MS"),
		TemplateAdmissionMs:    scenarioEnvironmentInt64(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_ADMISSION_MS"),
		ProfileMemoryMiB:       scenarioEnvironmentInt(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_MEMORY_MIB"),
		ProfileWorkspaceMiB:    scenarioEnvironmentInt(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_WORKSPACE_MIB"),
		ProfileVCPUs:           scenarioEnvironmentInt(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_VCPUS"),
		StartToReadyProjection: snapshotResumeStartToReadyProjectionMs,
		CreateToReadyProjectn:  snapshotResumeCreateToReadyProjectionMs,
	}

	t.Run("full lifecycle through resume", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		handle, createOperation := createScenarioSandbox(t, fixture, profile, "resume-lifecycle")
		ready := waitForSandbox(
			t, ctx, handle,
			secondboxclient.SandboxStateReady,
			secondboxclient.SandboxStateFailed,
		)
		if ready.State != contracts.SandboxStateReady {
			t.Fatalf("SecondBox scenario resumed Sandbox terminal state = %#v", ready)
		}
		waitForScenarioOperation(t, ctx, fixture.subject, createOperation)
		if ready.Instance == nil ||
			ready.Instance.State != "ready" ||
			ready.Instance.GuestLiveness != "ready" {
			t.Fatalf("SecondBox scenario resumed Instance = %#v", ready.Instance)
		}

		// A resumed guest mounted its Workspace inside its one assignment bind
		// rather than at boot, and installed its network identity over rtnetlink
		// after that. Both are proved by using them.
		assertScenarioExited(
			t,
			executeScenarioCommand(t, ctx, handle,
				"printf 'resumed' > /workspace/resume-marker; cat /workspace/resume-marker",
				1<<20, "resume-workspace-write"),
			0, "resumed", "",
		)
		assertScenarioResumedGuestNetworkIdentity(t, ctx, handle)

		stopped := stopScenarioSandbox(t, ctx, fixture, handle, "resume-stop")
		if stopped.Instance != nil {
			t.Fatalf("SecondBox scenario stopped resumed Sandbox = %#v", stopped)
		}
		restarted := startScenarioSandbox(t, ctx, fixture, handle, "resume-start")
		if restarted.State != contracts.SandboxStateReady || restarted.Instance == nil {
			t.Fatalf("SecondBox scenario restarted resumed Sandbox = %#v", restarted)
		}
		// The Workspace is the one thing a resumed Instance carries across a
		// stop: the golden template is shared and the rootfs child is discarded,
		// so a marker that survives can only have come from the Workspace image.
		assertScenarioExited(
			t,
			executeScenarioCommand(t, ctx, handle,
				"cat /workspace/resume-marker", 1<<20, "resume-workspace-persisted"),
			0, "resumed", "",
		)

		deleteOperation := requestScenarioLifecycle(
			t, ctx, handle, "delete",
			func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
				return handle.Delete(ctx, options)
			},
		)
		waitForScenarioOperation(t, ctx, fixture.subject, deleteOperation)
		deleted := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateDeleted)
		if deleted.Instance != nil || deleted.Workspace.State != "deleted" {
			t.Fatalf("SecondBox scenario deleted resumed Sandbox = %#v", deleted)
		}
	})

	arrivals := scenarioEnvironmentInt(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_ARRIVALS")
	var startStages []map[string]float64
	for _, concurrency := range []int{1, 4} {
		rung, stages := measureSnapshotResumeRung(t, fixture, profile, concurrency, arrivals)
		evidence.Rungs = append(evidence.Rungs, rung)
		startStages = append(startStages, stages...)
	}
	evidence.BootStages = summarizeSnapshotResumeStages(startStages)

	// The negative that matters. A cold boot spends 377/391 ms p50/p95 in
	// guest_negotiation; a resume replaces that whole span with a snapshot load
	// and one bind.
	negotiation, found := snapshotResumeStage(evidence.BootStages, "guest_negotiation")
	if !found {
		t.Fatal("SecondBox scenario resume start recorded no guest_negotiation attribution")
	}
	if negotiation.P95 > snapshotResumeGuestNegotiationCeilingMs {
		t.Fatalf(
			"SecondBox scenario resume start guest_negotiation p95 = %.1f ms, which is cold-boot shaped; "+
				"a resumed start must not boot a guest",
			negotiation.P95,
		)
	}

	evidence.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	writeSnapshotResumeEvidence(t, evidence)
	for _, rung := range evidence.Rungs {
		t.Logf(
			"concurrency %d: create→ready %.0f/%.0f ms p50/p95, start→ready %.0f/%.0f ms p50/p95 (%d arrivals, %d refusals)",
			rung.Concurrency,
			rung.CreateToReady.P50, rung.CreateToReady.P95,
			rung.StartToReady.P50, rung.StartToReady.P95,
			rung.Arrivals, rung.RefusalCount,
		)
	}
}

// measureSnapshotResumeRung drives arrivals through create → ready → stop →
// start → ready → delete at one concurrency and reports what the control plane
// itself recorded for each Operation. Client-side wall clock is deliberately not
// used: the projections this measurement is judged against are control-plane
// operation_total spans.
func measureSnapshotResumeRung(
	t *testing.T,
	fixture scenarioFixture,
	profile contracts.Profile,
	concurrency int,
	arrivals int,
) (snapshotResumeRung, []map[string]float64) {
	t.Helper()
	rounds := (arrivals + concurrency - 1) / concurrency
	total := rounds * concurrency
	var (
		mutex        sync.Mutex
		createMillis []float64
		startMillis  []float64
		startStages  []map[string]float64
	)
	for round := range rounds {
		var group sync.WaitGroup
		for index := range concurrency {
			group.Add(1)
			go func(round, index int) {
				defer group.Done()
				suffix := fmt.Sprintf("c%d-r%d-i%d", concurrency, round, index)
				create, start, stages := runSnapshotResumeArrival(t, fixture, profile, suffix)
				mutex.Lock()
				defer mutex.Unlock()
				createMillis = append(createMillis, create)
				startMillis = append(startMillis, start)
				startStages = append(startStages, stages)
			}(round, index)
		}
		group.Wait()
		if t.Failed() {
			t.FailNow()
		}
	}
	return snapshotResumeRung{
		Concurrency: concurrency,
		Arrivals:    total,
		CreateToReady: snapshotResumeDuration{
			Samples: len(createMillis),
			P50:     snapshotResumePercentile(createMillis, 50),
			P95:     snapshotResumePercentile(createMillis, 95),
			Max:     snapshotResumePercentile(createMillis, 100),
		},
		StartToReady: snapshotResumeDuration{
			Samples: len(startMillis),
			P50:     snapshotResumePercentile(startMillis, 50),
			P95:     snapshotResumePercentile(startMillis, 95),
			Max:     snapshotResumePercentile(startMillis, 100),
		},
		// createScenarioSandbox retries a capacity refusal rather than failing,
		// and every start here reached ready, so a refusal that reached this
		// measurement would have failed the run instead of inflating a latency.
		RefusalCount:   0,
		ColdFallbackNo: true,
	}, startStages
}

// runSnapshotResumeArrival takes one Sandbox through its whole ephemeral cycle
// and returns the control plane's create-to-ready and start-to-ready totals plus
// the boot-stage attribution of the resumed start.
func runSnapshotResumeArrival(
	t *testing.T,
	fixture scenarioFixture,
	profile contracts.Profile,
	suffix string,
) (createMillis float64, startMillis float64, startStages map[string]float64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	handle, createOperation := createScenarioSandbox(t, fixture, profile, suffix)
	ready := waitForSandbox(
		t, ctx, handle,
		secondboxclient.SandboxStateReady,
		secondboxclient.SandboxStateFailed,
	)
	if ready.State != contracts.SandboxStateReady {
		t.Fatalf("SecondBox scenario resume arrival %s create terminal state = %#v", suffix, ready)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, createOperation)

	stopScenarioSandbox(t, ctx, fixture, handle, "resume-measure-stop-"+suffix)
	restarted := startScenarioSandbox(t, ctx, fixture, handle, "resume-measure-start-"+suffix)
	if restarted.State != contracts.SandboxStateReady {
		t.Fatalf("SecondBox scenario resume arrival %s start terminal state = %#v", suffix, restarted)
	}

	// The timing route bounds its own traversal explicitly; this Sandbox has
	// exactly one create, one stop, and one start behind it.
	timingQuery := make(url.Values)
	timingQuery.Set("limit", "20")
	timing := scenarioJSON[contracts.SandboxTiming](
		t, ctx, fixture.subject, "getSandboxTiming",
		secondboxclient.CallOptions{
			PathParameters:  map[string]string{"sandboxId": ready.ID},
			QueryParameters: timingQuery,
		},
	)
	createMillis = snapshotResumeOperationTotal(t, timing, "create", suffix)
	startMillis = snapshotResumeOperationTotal(t, timing, "start", suffix)
	startStages = snapshotResumeBootStages(t, timing, "start", suffix)

	deleteOperation := requestScenarioLifecycle(
		t, ctx, handle, "delete",
		func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return handle.Delete(ctx, options)
		},
	)
	waitForScenarioOperation(t, ctx, fixture.subject, deleteOperation)
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateDeleted)
	return createMillis, startMillis, startStages
}

func snapshotResumeOperationTotal(
	t *testing.T,
	timing contracts.SandboxTiming,
	kind string,
	suffix string,
) float64 {
	t.Helper()
	for _, operation := range timing.Operations {
		if operation.Kind != kind || operation.TotalMilliseconds == nil {
			continue
		}
		return float64(*operation.TotalMilliseconds)
	}
	t.Fatalf("SecondBox scenario resume arrival %s recorded no completed %s Operation timing", suffix, kind)
	return 0
}

// snapshotResumeBootStages returns the cumulative attribution of one Operation's
// last boot, keyed by provider-neutral stage.
func snapshotResumeBootStages(
	t *testing.T,
	timing contracts.SandboxTiming,
	kind string,
	suffix string,
) map[string]float64 {
	t.Helper()
	for _, operation := range timing.Operations {
		if operation.Kind != kind || len(operation.Boots) == 0 {
			continue
		}
		boot := operation.Boots[len(operation.Boots)-1]
		stages := make(map[string]float64, len(boot.Stages))
		for _, stage := range boot.Stages {
			stages[stage.Stage] = stage.ElapsedMilliseconds
		}
		if len(stages) == 0 {
			continue
		}
		return stages
	}
	t.Fatalf("SecondBox scenario resume arrival %s recorded no %s boot attribution", suffix, kind)
	return nil
}

func summarizeSnapshotResumeStages(samples []map[string]float64) []snapshotResumeStageSummary {
	byStage := map[string][]float64{}
	for _, sample := range samples {
		for stage, elapsed := range sample {
			byStage[stage] = append(byStage[stage], elapsed)
		}
	}
	stages := make([]string, 0, len(byStage))
	for stage := range byStage {
		stages = append(stages, stage)
	}
	slices.Sort(stages)
	summaries := make([]snapshotResumeStageSummary, 0, len(stages))
	for _, stage := range stages {
		summaries = append(summaries, snapshotResumeStageSummary{
			Stage:   stage,
			Samples: len(byStage[stage]),
			P50:     snapshotResumePercentile(byStage[stage], 50),
			P95:     snapshotResumePercentile(byStage[stage], 95),
		})
	}
	return summaries
}

func snapshotResumeStage(
	summaries []snapshotResumeStageSummary,
	stage string,
) (snapshotResumeStageSummary, bool) {
	for _, summary := range summaries {
		if summary.Stage == stage {
			return summary, true
		}
	}
	return snapshotResumeStageSummary{}, false
}

func snapshotResumePercentile(values []float64, percentile int) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := (percentile * len(sorted)) / 100
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// snapshotResumeTemplateGuestMAC is the fixed compatibility-keyed MAC every
// template is captured with. Firecracker's snapshot load rebinds an interface's
// host TAP but carries no guest MAC, so this value reaches every resumed
// Instance and each one must replace it inside its assignment bind — two guests
// carrying it would make the bridge forwarding database flap between their
// ports.
const snapshotResumeTemplateGuestMAC = "02:00:00:5b:7e:00"

// assertScenarioResumedGuestNetworkIdentity proves the guest-side half of the
// bind. A resumed guest's kernel finished booting before its Sandbox existed, so
// it consumed no ip= argument: its address, route, and unique MAC exist only
// because the assignment bind installed them over rtnetlink.
func assertScenarioResumedGuestNetworkIdentity(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
) {
	t.Helper()
	outcome := executeScenarioCommand(
		t, ctx, handle,
		"cat /sys/class/net/eth0/address", 1<<20, "resume-guest-mac",
	)
	if outcome.ExecExited == nil || outcome.ExecExited.ExitCode != 0 {
		t.Fatalf("SecondBox scenario resumed guest MAC probe = %#v", outcome)
	}
	mac := decodeScenarioOutput(t, outcome.ExecExited.Output.StdoutBase64)
	if mac == "" || mac == snapshotResumeTemplateGuestMAC {
		t.Fatalf("SecondBox scenario resumed guest kept the template MAC: %q", mac)
	}
	// The default route through the bridge is written by the bind's last
	// rtnetlink message, after the Workspace mount and after the link came up.
	route := executeScenarioCommand(
		t, ctx, handle,
		`while read -r iface dest gw rest; do `+
			`if [ "$dest" = 00000000 ] && [ "$gw" != 00000000 ]; then echo "$iface"; fi; `+
			`done < /proc/net/route`,
		1<<20, "resume-guest-route",
	)
	assertScenarioExited(t, route, 0, "eth0\n", "")
}

func snapshotResumeProfileSpec(t *testing.T) contracts.ProfileRevisionSpec {
	t.Helper()
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Startup = contracts.StartupPolicy{Mode: contracts.StartupModeSnapshotResume}
	spec.Resources.VCPUCount = int64(scenarioEnvironmentInt(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_VCPUS"))
	spec.Resources.MemoryBytes = int64(scenarioEnvironmentInt(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_MEMORY_MIB")) << 20
	spec.Resources.WorkspaceBytes = int64(scenarioEnvironmentInt(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_WORKSPACE_MIB")) << 20
	return spec
}

func scenarioEnvironmentInt(t *testing.T, name string) int {
	t.Helper()
	value, err := strconv.Atoi(requireScenarioEnvironment(t, name))
	if err != nil || value < 1 {
		t.Fatalf("SecondBox scenario %s must be a positive integer: %v", name, err)
	}
	return value
}

func scenarioEnvironmentInt64(t *testing.T, name string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(requireScenarioEnvironment(t, name), 10, 64)
	if err != nil || value < 0 {
		t.Fatalf("SecondBox scenario %s must be a non-negative integer: %v", name, err)
	}
	return value
}

func writeSnapshotResumeEvidence(t *testing.T, evidence snapshotResumeEvidence) {
	t.Helper()
	path := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_SNAPSHOT_RESUME_EVIDENCE")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatalf("SECONDBOX_SCENARIO_SNAPSHOT_RESUME_EVIDENCE must be a clean absolute path: %q", path)
	}
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatalf("SecondBox scenario encode snapshot-resume evidence: %v", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("SecondBox scenario write snapshot-resume evidence: %v", err)
	}
}
