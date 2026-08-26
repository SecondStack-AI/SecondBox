//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioStoppedSnapshotFreeWorkspaceRelocatesBetweenCompatibleRunners(t *testing.T) {
	if os.Getenv("SECONDBOX_SCENARIO_COMPUTE_BACKEND") != "microsandbox" {
		return
	}
	targetStarted := false
	// Register this before createScenarioSandbox so its cleanup runs after the
	// Sandbox cleanup. A failed relocation proof must keep the target home
	// runner available long enough to delete the relocated Workspace.
	t.Cleanup(func() {
		if targetStarted {
			scenarioCompose(t, "--profile", "relocation", "stop", "secondbox-runner-relocation")
		}
	})
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-microsandbox-relocation",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	handle, _ := createScenarioSandbox(t, fixture, profile, "microsandbox-relocation")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	marker := []byte("secondbox-relocation-marker-not-control-plane-retained")
	writeScenarioFile(t, ctx, fixture.subject, handle, "relocation-marker", marker)
	stopScenarioSandbox(t, ctx, fixture, handle, "relocation-stop-source")

	scenarioCompose(
		t,
		"--profile", "relocation", "up", "--detach", "--wait", "--wait-timeout", "180",
		"secondbox-runner-relocation",
	)
	targetStarted = true
	targetID := requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_RELOCATION_RUNNER_ID")
	waitForScenarioRunnerID(t, fixture, targetID, 90*time.Second)
	waitForRunnerPoolReadyCount(t, fixture, 2, 30*time.Second)

	current, err := handle.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targetRunnerID := secondboxclient.RunnerID(targetID)
	operation, err := handle.Relocate(
		ctx,
		secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, "relocate-compatible-target"),
			IfMatch:        sandboxRevisionETag(current.Revision),
		},
		secondboxclient.RelocateSandboxRequest{TargetRunnerID: &targetRunnerID},
	)
	if err != nil {
		t.Fatalf("SecondBox scenario relocate Workspace: %v", err)
	}
	terminal := waitForScenarioOperation(t, ctx, fixture.subject, operation)
	if terminal.Kind != "relocate" {
		t.Fatalf("SecondBox scenario relocation terminal = %#v", terminal)
	}

	// With the source Runner offline, only a committed home move to the target
	// can start the Sandbox and recover these exact Workspace bytes. Every
	// later scenario shares this baseline runner, so a failure anywhere before
	// the inline restart must still put it back.
	baselineStopped := false
	t.Cleanup(func() {
		if baselineStopped {
			scenarioCompose(t, "start", "secondbox-runner")
			waitForScenarioRunner(t, fixture, 90*time.Second)
		}
	})
	scenarioCompose(t, "stop", "secondbox-runner")
	baselineStopped = true
	waitForRunnerPoolReadyCount(t, fixture, 1, 30*time.Second)
	restarted := startScenarioSandbox(t, ctx, fixture, handle, "relocation-start-target-only")
	if restarted.State != contracts.SandboxStateReady {
		t.Fatalf("SecondBox scenario relocated Sandbox = %#v", restarted)
	}
	if got := readScenarioFile(t, ctx, fixture.subject, handle, "relocation-marker"); !bytes.Equal(got, marker) {
		t.Fatalf("SecondBox scenario relocated Workspace marker = %q, want %q", got, marker)
	}

	controlPlaneLogs := scenarioComposeOutput(t, "logs", "--no-color", "control-plane")
	if bytes.Contains(controlPlaneLogs, marker) {
		t.Fatal("SecondBox control-plane logs retained relocated Workspace contents")
	}

	scenarioCompose(t, "start", "secondbox-runner")
	waitForScenarioRunner(t, fixture, 90*time.Second)
	baselineStopped = false

	// Delete while the target runner still owns the relocated home, then put
	// the shared scenario topology back into its single-runner baseline for
	// every test that follows in the complete suite.
	cleanupScenarioSandbox(t, fixture.subject, handle)
	scenarioCompose(t, "--profile", "relocation", "stop", "secondbox-runner-relocation")
	targetStarted = false
	waitForRunnerPoolReadyCount(t, fixture, 1, 30*time.Second)
}

func waitForScenarioRunnerID(
	t *testing.T,
	fixture scenarioFixture,
	runnerID string,
	timeout time.Duration,
) contracts.Runner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	query := make(url.Values)
	query.Set("pool", scenarioRunnerPool)
	for {
		var page contracts.RunnerPage
		err := fixture.admin.RequestJSON(ctx, "listRunners", secondboxclient.CallOptions{
			QueryParameters: query,
		}, &page)
		if err == nil {
			for _, runner := range page.Items {
				if runner.ID == runnerID && runner.State == "ready" && runner.LastSeenAt != nil {
					return runner
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SecondBox scenario Runner %s did not enroll: %v", runnerID, errors.Join(err, ctx.Err()))
		case <-time.After(250 * time.Millisecond):
		}
	}
}
