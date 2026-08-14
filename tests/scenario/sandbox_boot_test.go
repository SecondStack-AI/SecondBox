//go:build scenario_live

package scenario_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioSandboxBootsToReadyOnRealCompute(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-real-boot",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	handle, createOperation := createScenarioSandbox(t, fixture, profile, "real-boot")
	created := handle.Snapshot()
	if created.State != contracts.SandboxStateCreating ||
		created.ProfileRevisionID != profile.CurrentRevision.ID ||
		created.Profile != profile.Name ||
		created.DesiredState != contracts.SandboxDesiredStateRunning {
		t.Fatalf("SecondBox scenario initial Sandbox = %#v", created)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ready := waitForSandbox(
		t,
		ctx,
		handle,
		secondboxclient.SandboxStateReady,
		secondboxclient.SandboxStateFailed,
	)
	if ready.State != contracts.SandboxStateReady {
		operation := scenarioJSON[contracts.Operation](
			t,
			ctx,
			fixture.subject,
			"getOperation",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"operationId": createOperation.ID},
			},
		)
		t.Fatalf(
			"SecondBox scenario Sandbox boot terminal state = %#v; create operation = %#v; operation error = %+v",
			ready,
			operation,
			operation.Error,
		)
	}
	if ready.Instance == nil ||
		ready.Instance.State != "ready" ||
		ready.Instance.GuestLiveness != "ready" ||
		ready.Instance.ReadyAt == nil ||
		ready.Instance.GuestHeartbeatAt == nil ||
		ready.Instance.Generation != ready.Generation {
		t.Fatalf("SecondBox scenario ready Instance = %#v", ready.Instance)
	}
	if _, err := fixture.subject.WaitOperation(
		ctx,
		createOperation.ID,
		100*time.Millisecond,
	); err != nil {
		t.Fatalf("SecondBox scenario create operation: %v", err)
	}

	inspection := scenarioJSON[contracts.SandboxInspection](
		t,
		ctx,
		fixture.subject,
		"inspectSandbox",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": ready.ID},
			Headers:        handle.GenerationHeaders(""),
		},
	)
	if inspection.SandboxID != ready.ID ||
		inspection.Generation != ready.Generation ||
		!inspection.GuestHealthy {
		t.Fatalf("SecondBox scenario Sandbox inspection = %#v", inspection)
	}
	ping := scenarioJSON[contracts.PingResult](
		t,
		ctx,
		fixture.subject,
		"pingSandbox",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": ready.ID},
			Headers:        handle.GenerationHeaders(""),
		},
	)
	if ping.SandboxID != ready.ID ||
		ping.Generation != ready.Generation ||
		!ping.Healthy {
		t.Fatalf("SecondBox scenario Sandbox ping = %#v", ping)
	}
}

func TestScenarioSandboxRejectsRequirementsAboveRunnerCapacity(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	runner := waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Resources.MemoryBytes = runner.Capacity["MemoryBytes"] + 1
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-over-capacity",
		spec,
	)

	started := time.Now()
	var operation contracts.Operation
	err := fixture.subject.RequestJSON(
		context.Background(),
		"createSandbox",
		secondboxclient.CallOptions{
			Headers: scenarioHeaders(uniqueScenarioKey(t, "over-capacity")),
			Body: scenarioBody(t, contracts.CreateSandboxRequest{
				Profile:  profile.Name,
				Metadata: map[string]string{"scenario": "over-capacity"},
			}),
		},
		&operation,
	)
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) ||
		apiError.StatusCode != http.StatusServiceUnavailable ||
		apiError.Problem == nil ||
		apiError.Problem.Code != "home_runner_unavailable" ||
		!apiError.Problem.Retryable {
		t.Fatalf("SecondBox scenario over-capacity error = %#v, raw error=%v", apiError, err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("SecondBox scenario over-capacity admission took %s", elapsed)
	}
}

func TestScenarioSandboxRejectsUncachedLogicalMaterializationTuple(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	runnerBefore := waitForScenarioRunnerStartupTimingSettled(t, fixture, 15*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.RuntimeBundleDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	profile := createScenarioProfile(t, fixture, "scenario-uncached-materialization", spec)

	var operation contracts.Operation
	err := fixture.subject.RequestJSON(
		context.Background(),
		"createSandbox",
		secondboxclient.CallOptions{
			Headers: scenarioHeaders(uniqueScenarioKey(t, "uncached-materialization")),
			Body: scenarioBody(t, contracts.CreateSandboxRequest{
				Profile:  profile.Name,
				Metadata: map[string]string{"scenario": "uncached-materialization"},
			}),
		},
		&operation,
	)
	if err != nil || operation.ID == "" || operation.SandboxID == "" {
		t.Fatalf("SecondBox scenario uncached materialization durable request = error %v operation %#v", err, operation)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sandbox := scenarioJSON[contracts.Sandbox](
		t, ctx, fixture.subject, "getSandbox",
		secondboxclient.CallOptions{PathParameters: map[string]string{"sandboxId": operation.SandboxID}},
	)
	handle := secondboxclient.NewSandboxHandle(fixture.subject, sandbox)
	t.Cleanup(func() { cleanupScenarioSandbox(t, fixture.subject, handle) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Fatal(refreshErr)
		}
		if current.Instance != nil {
			t.Fatalf("SecondBox uncached materialization created an Instance: %#v", current.Instance)
		}
		time.Sleep(100 * time.Millisecond)
	}
	runnerAfter := waitForScenarioRunnerStartupTimingSettled(t, fixture, 15*time.Second)
	if runnerAfter.SandboxStartSampleCount != runnerBefore.SandboxStartSampleCount {
		t.Fatalf(
			"SecondBox uncached materialization reached compute: start samples %d -> %d",
			runnerBefore.SandboxStartSampleCount,
			runnerAfter.SandboxStartSampleCount,
		)
	}
}

func waitForScenarioRunnerStartupTimingSettled(
	t *testing.T,
	fixture scenarioFixture,
	timeout time.Duration,
) contracts.Runner {
	t.Helper()
	deadline := time.Now().Add(timeout)
	previous := waitForScenarioRunner(t, fixture, timeout)
	stableHeartbeats := 0
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		current := waitForScenarioRunner(t, fixture, remaining)
		if current.LastSeenAt == nil || previous.LastSeenAt == nil ||
			!current.LastSeenAt.After(*previous.LastSeenAt) {
			continue
		}
		if current.SandboxStartSampleCount == previous.SandboxStartSampleCount {
			stableHeartbeats++
		} else {
			stableHeartbeats = 0
		}
		previous = current
		if stableHeartbeats == 2 {
			return current
		}
	}
	t.Fatalf("SecondBox scenario Runner startup timing did not settle within %s", timeout)
	return contracts.Runner{}
}
