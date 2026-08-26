//go:build scenario_live

package scenario_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioOrdinaryLifecycleAndCapacityRelease(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	runnerBefore := waitForScenarioRunner(t, fixture, 90*time.Second)
	capacityBefore := maps.Clone(runnerBefore.Capacity)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-lifecycle",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	handle, createOperation := createScenarioSandbox(t, fixture, profile, "lifecycle")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	created := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	waitForScenarioOperation(t, ctx, fixture.subject, createOperation)
	lease := acquireScenarioLease(t, ctx, fixture, handle, 30, "lifecycle-drain-lease")
	drainOperation := requestScenarioLifecycle(
		t,
		ctx,
		handle,
		"drain",
		func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return handle.Drain(ctx, options)
		},
	)
	draining := waitForSandbox(
		t,
		ctx,
		handle,
		secondboxclient.SandboxStateDraining,
		secondboxclient.SandboxStateStopped,
	)
	if draining.DesiredState != contracts.SandboxDesiredStateStopped {
		t.Fatalf("SecondBox scenario drain transition = %#v", draining)
	}
	if draining.State == secondboxclient.SandboxStateDraining &&
		(draining.Instance == nil ||
			draining.Instance.TerminationReason != contracts.TerminationReasonRequestedDrain) {
		t.Fatalf("SecondBox scenario draining Sandbox = %#v", draining)
	}
	released := scenarioJSON[contracts.Lease](
		t,
		ctx,
		fixture.subject,
		"releaseSandboxLease",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"leaseId": lease.ID},
			Headers:        scenarioHeaders(uniqueScenarioKey(t, "lifecycle-drain-release")),
		},
	)
	if released.State != contracts.LeaseStateReleased &&
		released.State != contracts.LeaseStateFenced {
		t.Fatalf("SecondBox scenario drain Lease release = %#v", released)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, drainOperation)
	stoppedAfterDrain := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
	if stoppedAfterDrain.Generation <= created.Generation ||
		stoppedAfterDrain.Instance != nil {
		t.Fatalf("SecondBox scenario stopped-after-drain Sandbox = %#v", stoppedAfterDrain)
	}

	restarted := startScenarioSandbox(t, ctx, fixture, handle, "lifecycle-start")
	if restarted.Generation != stoppedAfterDrain.Generation ||
		restarted.Instance == nil ||
		restarted.Instance.Generation != restarted.Generation {
		t.Fatalf("SecondBox scenario restarted Sandbox = %#v", restarted)
	}
	stopped := stopScenarioSandbox(t, ctx, fixture, handle, "lifecycle-stop")
	if stopped.Generation <= restarted.Generation || stopped.Instance != nil {
		t.Fatalf("SecondBox scenario stopped Sandbox = %#v", stopped)
	}

	deleteOperation := requestScenarioLifecycle(
		t,
		ctx,
		handle,
		"delete",
		func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return handle.Delete(ctx, options)
		},
	)
	waitForScenarioOperation(t, ctx, fixture.subject, deleteOperation)
	deleted := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateDeleted)
	if deleted.DesiredState != contracts.SandboxDesiredStateDeleted ||
		deleted.Instance != nil ||
		deleted.Workspace.State != "deleted" {
		t.Fatalf("SecondBox scenario deleted Sandbox = %#v", deleted)
	}
	waitForScenarioRunnerCapacity(t, fixture, capacityBefore, 30*time.Second)
}

func TestScenarioControlPlaneRestartDuringStartConverges(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-control-restart",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateStopped),
	)
	handle, createOperation := createScenarioSandbox(t, fixture, profile, "control-restart")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	waitForScenarioOperation(t, ctx, fixture.subject, createOperation)
	stopped := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
	startOperation := requestScenarioLifecycle(
		t,
		ctx,
		handle,
		"control-restart-start",
		func(options secondboxclient.LifecycleOptions) (contracts.Operation, error) {
			return handle.Start(ctx, options)
		},
	)
	starting := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStarting)
	if starting.Generation != stopped.Generation || starting.Instance == nil {
		t.Fatalf("SecondBox scenario starting Sandbox before control restart = %#v", starting)
	}

	scenarioCompose(t, "restart", "--no-deps", "control-plane")
	waitForScenarioControlPlaneReady(t, fixture, 60*time.Second)
	waitForScenarioOperation(t, ctx, fixture.subject, startOperation)
	ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	if ready.Generation != stopped.Generation ||
		ready.Instance == nil ||
		ready.Instance.GuestLiveness != contracts.GuestLivenessReady {
		t.Fatalf("SecondBox scenario Sandbox after control restart = %#v", ready)
	}
}

func TestScenarioRunnerLossDuringExecRecoversHomeWorkspace(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-runner-loss",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	handle, _ := createScenarioSandbox(t, fixture, profile, "runner-loss")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ready := waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)
	const durablePath = "runner-loss-durable.txt"
	const durableContents = "same home workspace survived runner loss\n"
	writeScenarioFile(
		t,
		ctx,
		fixture.subject,
		handle,
		durablePath,
		[]byte(durableContents),
	)

	type execResult struct {
		outcome secondboxclient.ExecOutcome
		err     error
	}
	execResults := make(chan execResult, 1)
	go func() {
		outcome, err := handle.Execute(
			ctx,
			scenarioExecRequest(
				"printf started > /workspace/runner-loss-exec-started; sync; sleep 60",
				4096,
			),
			"runner-loss-mid-exec",
			"",
		)
		execResults <- execResult{outcome: outcome, err: err}
	}()
	time.Sleep(time.Second)

	runnerRunning := true
	t.Cleanup(func() {
		if runnerRunning {
			return
		}
		scenarioCompose(t, "start", "secondbox-runner")
		waitForScenarioRunner(t, fixture, 90*time.Second)
	})
	scenarioCompose(t, "stop", "secondbox-runner")
	runnerRunning = false

	select {
	case result := <-execResults:
		if result.err != nil {
			var apiError *secondboxclient.APIError
			if !errors.As(result.err, &apiError) ||
				apiError.Problem == nil ||
				(apiError.Problem.Code != "execution_node_unavailable" &&
					apiError.Problem.Code != "dependency_unavailable") {
				t.Fatalf("SecondBox scenario in-flight Exec error after runner loss = %v", result.err)
			}
		} else if result.outcome.ExecInfrastructureFailed == nil &&
			result.outcome.ExecCancelled == nil &&
			result.outcome.ExecDeadlineExceeded == nil {
			t.Fatalf(
				"SecondBox scenario in-flight Exec lacked a terminal failure after runner loss: %#v",
				result.outcome,
			)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SecondBox scenario in-flight Exec remained wedged after runner loss")
	}

	waitForRunnerPoolReadyCount(t, fixture, 0, 20*time.Second)
	waitForScenarioTypedUnavailable(t, ctx, fixture, handle, 20*time.Second)
	// A disconnected runner is immediately unavailable, but assignment fencing
	// starts only after the explicit five-second heartbeat grace period. This
	// prevents a control-plane restart from being misclassified as runner loss.
	time.Sleep(6 * time.Second)

	scenarioCompose(t, "start", "secondbox-runner")
	runnerRunning = true
	waitForScenarioRunner(t, fixture, 90*time.Second)
	recovered := waitForScenarioGenerationReady(
		t,
		ctx,
		handle,
		ready.Generation+1,
	)
	if recovered.Instance == nil ||
		recovered.Instance.GuestLiveness != contracts.GuestLivenessReady {
		t.Fatalf("SecondBox scenario recovered Sandbox = %#v", recovered)
	}
	if got := string(readScenarioFile(
		t,
		ctx,
		fixture.subject,
		handle,
		durablePath,
	)); got != durableContents {
		t.Fatalf(
			"SecondBox scenario recovered workspace contents = %q, want %q",
			got,
			durableContents,
		)
	}
	if got := string(readScenarioFile(
		t,
		ctx,
		fixture.subject,
		handle,
		"runner-loss-exec-started",
	)); got != "started" {
		t.Fatalf("SecondBox scenario killed Exec start marker = %q, want started", got)
	}
}

func requestScenarioLifecycle(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	request func(secondboxclient.LifecycleOptions) (contracts.Operation, error),
) contracts.Operation {
	t.Helper()
	var (
		operation contracts.Operation
		err       error
	)
	for attempt := 0; attempt < 10; attempt++ {
		current, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Fatalf("SecondBox scenario refresh before %s: %v", key, refreshErr)
		}
		operation, err = request(secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, key),
			IfMatch:        sandboxRevisionETag(current.Revision),
		})
		if err == nil {
			return operation
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) ||
			apiError.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("SecondBox scenario %s lifecycle request: %v", key, err)
		}
	}
	t.Fatalf("SecondBox scenario %s lifecycle request remained revision-conflicted: %v", key, err)
	return contracts.Operation{}
}

func waitForScenarioRunnerCapacity(
	t *testing.T,
	fixture scenarioFixture,
	want map[string]int64,
	timeout time.Duration,
) contracts.Runner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var last contracts.Runner
	for {
		var err error
		last, err = getScenarioRunner(ctx, fixture)
		if err == nil && maps.Equal(last.Capacity, want) {
			return last
		}
		select {
		case <-ctx.Done():
			t.Fatalf(
				"SecondBox scenario Runner capacity = %#v, want %#v: %v",
				last.Capacity,
				want,
				errors.Join(err, ctx.Err()),
			)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func getScenarioRunner(
	ctx context.Context,
	fixture scenarioFixture,
) (contracts.Runner, error) {
	var runner contracts.Runner
	err := fixture.admin.RequestJSON(
		ctx,
		"getRunner",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"runnerId": scenarioRunnerID},
		},
		&runner,
	)
	return runner, err
}

func waitForScenarioControlPlaneReady(
	t *testing.T,
	fixture scenarioFixture,
	timeout time.Duration,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			fixture.baseURL+"/readyz",
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		response, requestErr := fixture.httpClient.Do(request)
		if requestErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf(
				"SecondBox scenario control plane did not become ready: %v",
				errors.Join(requestErr, ctx.Err()),
			)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitForScenarioTypedUnavailable(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := handle.Refresh(ctx); err != nil {
			lastErr = err
			continue
		}
		_, err := handle.Execute(
			ctx,
			secondboxclient.BufferedExecRequest{
				Command: secondboxclient.Command{
					ShellCommand: &secondboxclient.ShellCommand{
						Mode:    "shell",
						Command: "true",
					},
				},
				Environment:          map[string]string{},
				DeadlineMilliseconds: 1000,
				MaximumOutputBytes:   1024,
			},
			"runner-loss-unavailable",
			"",
		)
		var apiError *secondboxclient.APIError
		if errors.As(err, &apiError) &&
			apiError.Problem != nil &&
			apiError.Problem.Code == "execution_node_unavailable" &&
			apiError.Problem.Retryable {
			return
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("SecondBox scenario runner loss did not produce typed unavailable: %v", lastErr)
}

func waitForScenarioGenerationReady(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	minimumGeneration int64,
) contracts.Sandbox {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last contracts.Sandbox
	for {
		current, err := handle.Refresh(ctx)
		if err == nil {
			last = current
			if current.Generation >= minimumGeneration &&
				current.State == secondboxclient.SandboxStateReady &&
				current.Instance != nil &&
				current.Instance.Generation == current.Generation {
				return current
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf(
				"SecondBox scenario Sandbox did not recover at generation >= %d; last = %#v: %v",
				minimumGeneration,
				last,
				errors.Join(err, ctx.Err()),
			)
		case <-ticker.C:
		}
	}
}
