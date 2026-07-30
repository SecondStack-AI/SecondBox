package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	scenarioharness "github.com/SecondStack-AI/SecondBox/tests/scenario/harness"
)

// Transition names. Every lifecycle edge is timed independently; a refusal or a
// queue wait is never folded into a latency sample.
const (
	transitionCreateReady = "create_to_ready"
	transitionStartReady  = "start_to_ready"
	transitionStopStopped = "stop_to_stopped"
	transitionDeleteGone  = "delete_to_deleted"
)

type lifecycleDriver struct {
	config          lifecycleConfig
	client          *secondboxclient.Client
	runtimeDigest   string
	toolchainDigest string

	mu         sync.Mutex
	bootStages map[string][]time.Duration

	readyCount   atomic.Int64
	inFlight     atomic.Int64
	shedArrivals atomic.Int64
}

func (driver *lifecycleDriver) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		ctx, time.Duration(driver.config.OperationTimeoutSeconds)*time.Second,
	)
}

func (driver *lifecycleDriver) pollInterval() time.Duration {
	return time.Duration(driver.config.PollIntervalMilliseconds) * time.Millisecond
}

func (driver *lifecycleDriver) prepare(ctx context.Context) error {
	pool, err := scenarioharness.RequestJSON[secondboxclient.RunnerPool](
		ctx, driver.client, "createRunnerPool", secondboxclient.CallOptions{
			Body: scenarioharness.JSONBody(secondboxclient.CreateRunnerPoolRequest{
				Name: driver.config.RunnerPoolName, State: "ready",
				Architectures: []string{"amd64"},
				Capabilities: []string{
					"cleanup", "compute", "local-workspace", "network-policy", "storage",
				},
				CapacityPolicy: map[string]int64{
					"maxInstances": int64(driver.config.Runner.MaxConcurrentGlobal),
				},
			}),
		},
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle create RunnerPool failed: %w", err)
	}
	profile, err := scenarioharness.RequestJSON[secondboxclient.Profile](
		ctx, driver.client, "createProfile", secondboxclient.CallOptions{
			Headers: scenarioharness.IdempotencyHeaders("lifecycle-prepare-profile"),
			Body: scenarioharness.JSONBody(secondboxclient.CreateProfileRequest{
				Name: driver.config.ProfileName,
				Spec: secondboxclient.ProfileRevisionSpec{
					Pool: driver.config.RunnerPoolName, Architecture: "amd64",
					RuntimeBundleDigest:   driver.runtimeDigest,
					ToolchainBundleDigest: driver.toolchainDigest,
					Resources: secondboxclient.ResourcePolicy{
						CPUMillis:            driver.config.Profile.CPUMillis,
						MemoryBytes:          driver.config.Profile.MemoryBytes,
						WorkspaceBytes:       driver.config.Profile.WorkspaceBytes,
						ProcessLimit:         driver.config.Profile.ProcessLimit,
						ConcurrentOperations: driver.config.Profile.ConcurrentOperations,
					},
					Lifecycle: secondboxclient.LifecyclePolicy{
						InitialState:           "running",
						DrainGraceSeconds:      driver.config.Profile.DrainGraceSeconds,
						IdleSeconds:            driver.config.Profile.IdleSeconds,
						MaximumDurationSeconds: driver.config.Profile.MaximumDurationSeconds,
						LeaseSeconds:           driver.config.Profile.LeaseSeconds,
					},
					Retention: secondboxclient.RetentionPolicy{
						SnapshotLimit:            driver.config.Profile.SnapshotLimit,
						SnapshotRetentionSeconds: driver.config.Profile.SnapshotRetentionSeconds,
						ArtifactRetentionSeconds: driver.config.Profile.ArtifactRetentionSeconds,
					},
					Execution: secondboxclient.ExecutionPolicy{
						MaximumDeadlineMilliseconds: driver.config.Profile.MaximumDeadlineMilliseconds,
						MaximumBufferedOutputBytes:  driver.config.Profile.MaximumBufferedOutputBytes,
						StreamWindowBytes:           driver.config.Profile.StreamWindowBytes,
						MaximumTransferBytes:        driver.config.Profile.MaximumTransferBytes,
						TerminalDetachSeconds:       driver.config.Profile.TerminalDetachSeconds,
					},
					Network: secondboxclient.NetworkPolicy{
						Mode: "deny_all", Destinations: []secondboxclient.NetworkDestination{},
					},
					Ports: []secondboxclient.PortPolicy{},
				},
			}),
		},
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle create Profile failed: %w", err)
	}
	fmt.Printf(
		"Prepared RunnerPool %s and Profile %s through the published API\n",
		pool.Name, profile.Name,
	)
	return nil
}

func (driver *lifecycleDriver) waitForRunner(ctx context.Context) error {
	deadline := time.Now().Add(time.Duration(driver.config.OperationTimeoutSeconds) * time.Second)
	for time.Now().Before(deadline) {
		page, err := scenarioharness.RequestJSON[secondboxclient.RunnerPage](
			ctx, driver.client, "listRunners", secondboxclient.CallOptions{
				QueryParameters: url.Values{
					"pool": {driver.config.RunnerPoolName}, "limit": {"200"},
				},
			},
		)
		if err != nil {
			return fmt.Errorf("SecondBox lifecycle list Runners failed: %w", err)
		}
		for _, runner := range page.Items {
			if runner.State == "ready" {
				return nil
			}
		}
		timer := time.NewTimer(driver.pollInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("SecondBox lifecycle Runner did not become ready before the outer deadline")
}

// createSandbox performs the cold arrival edge and returns the create-to-ready
// duration measured from the moment the request is issued.
func (driver *lifecycleDriver) createSandbox(
	ctx context.Context,
	key string,
) (*secondboxclient.SandboxHandle, time.Duration, error) {
	startedAt := time.Now()
	operation, err := scenarioharness.RequestJSON[secondboxclient.Operation](
		ctx, driver.client, "createSandbox", secondboxclient.CallOptions{
			Headers: scenarioharness.IdempotencyHeaders(key),
			Body: scenarioharness.JSONBody(secondboxclient.CreateSandboxRequest{
				Profile: driver.config.ProfileName,
				Metadata: secondboxclient.Metadata{
					"qualification": "lifecycle", "key": key,
				},
			}),
		},
	)
	if err != nil {
		return nil, time.Since(startedAt), err
	}
	var sandbox secondboxclient.Sandbox
	if operation.Sandbox != nil {
		sandbox = *operation.Sandbox
	} else {
		sandbox, err = scenarioharness.RequestJSON[secondboxclient.Sandbox](
			ctx, driver.client, "getSandbox", secondboxclient.CallOptions{
				PathParameters: map[string]string{"sandboxId": operation.SandboxID},
			},
		)
		if err != nil {
			return nil, time.Since(startedAt), err
		}
	}
	handle := secondboxclient.NewSandboxHandle(driver.client, sandbox)
	reached, err := driver.wait(ctx, handle, secondboxclient.SandboxStateReady)
	elapsed := time.Since(startedAt)
	if err != nil {
		return handle, elapsed, err
	}
	if reached.State != secondboxclient.SandboxStateReady {
		return handle, elapsed, fmt.Errorf(
			"SecondBox lifecycle Sandbox %s reached %s instead of ready", reached.ID, reached.State,
		)
	}
	driver.readyCount.Add(1)
	driver.collectBootTiming(ctx, operation.ID)
	return handle, elapsed, nil
}

// startSandbox performs the warm arrival edge on a Sandbox that is stopped and
// still owns its Workspace. This is the ephemeral hot path.
func (driver *lifecycleDriver) startSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
) (time.Duration, error) {
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		return 0, err
	}
	startedAt := time.Now()
	operation, err := handle.Start(ctx, secondboxclient.LifecycleOptions{
		IdempotencyKey: key, IfMatch: scenarioharness.RevisionETag(sandbox.Revision),
	})
	if err != nil {
		return time.Since(startedAt), err
	}
	reached, err := driver.wait(ctx, handle, secondboxclient.SandboxStateReady)
	elapsed := time.Since(startedAt)
	if err != nil {
		return elapsed, err
	}
	if reached.State != secondboxclient.SandboxStateReady {
		return elapsed, fmt.Errorf(
			"SecondBox lifecycle Sandbox %s reached %s instead of ready", reached.ID, reached.State,
		)
	}
	driver.readyCount.Add(1)
	driver.collectBootTiming(ctx, operation.ID)
	return elapsed, nil
}

func (driver *lifecycleDriver) stopSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
) (time.Duration, error) {
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		return 0, err
	}
	startedAt := time.Now()
	if _, err := handle.Stop(ctx, secondboxclient.LifecycleOptions{
		IdempotencyKey: key, IfMatch: scenarioharness.RevisionETag(sandbox.Revision),
	}); err != nil {
		return time.Since(startedAt), err
	}
	reached, err := driver.wait(ctx, handle, secondboxclient.SandboxStateStopped)
	elapsed := time.Since(startedAt)
	if err != nil {
		return elapsed, err
	}
	if reached.State != secondboxclient.SandboxStateStopped {
		return elapsed, fmt.Errorf(
			"SecondBox lifecycle Sandbox %s reached %s instead of stopped", reached.ID, reached.State,
		)
	}
	driver.readyCount.Add(-1)
	return elapsed, nil
}

func (driver *lifecycleDriver) deleteSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	countedReady bool,
) (time.Duration, error) {
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		return 0, err
	}
	if sandbox.State == secondboxclient.SandboxStateDeleted {
		return 0, nil
	}
	startedAt := time.Now()
	operation, err := handle.Delete(ctx, secondboxclient.LifecycleOptions{
		IdempotencyKey: key, IfMatch: scenarioharness.RevisionETag(sandbox.Revision),
	})
	if err != nil {
		return time.Since(startedAt), err
	}
	_, err = driver.client.WaitOperation(ctx, operation.ID, driver.pollInterval())
	elapsed := time.Since(startedAt)
	if countedReady {
		driver.readyCount.Add(-1)
	}
	return elapsed, err
}

func (driver *lifecycleDriver) wait(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	target secondboxclient.SandboxState,
) (secondboxclient.Sandbox, error) {
	waitContext, cancel := driver.operationContext(ctx)
	defer cancel()
	return scenarioharness.WaitSandbox(
		waitContext, handle,
		[]secondboxclient.SandboxState{target, secondboxclient.SandboxStateFailed},
		60*time.Second,
	)
}

// collectBootTiming records the corrected public stage attribution so the report
// can name the stage that dominates start latency and show whether the dominant
// stage changes under load. A timing read failure must not fail the cycle, so it
// is recorded as an absent sample rather than an error.
func (driver *lifecycleDriver) collectBootTiming(ctx context.Context, operationID string) {
	timing, err := scenarioharness.RequestJSON[secondboxclient.OperationTiming](
		ctx, driver.client, "getOperationTiming", secondboxclient.CallOptions{
			PathParameters: map[string]string{"operationId": operationID},
		},
	)
	if err != nil {
		return
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	for _, boot := range timing.Boots {
		for _, stage := range boot.Stages {
			driver.bootStages[stage.Stage] = append(
				driver.bootStages[stage.Stage],
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		}
	}
}

func classifyLifecycleError(err error) (bool, string) {
	var operationFailure *secondboxclient.OperationFailure
	if errors.As(err, &operationFailure) && operationFailure.Operation.Error != nil {
		return admissionCode(string(operationFailure.Operation.Error.Code))
	}
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) {
		if errors.Is(err, context.DeadlineExceeded) {
			return false, "deadline_exceeded"
		}
		return false, "client_error"
	}
	code := "http_" + strconv.Itoa(apiError.StatusCode)
	if apiError.Problem != nil && strings.TrimSpace(string(apiError.Problem.Code)) != "" {
		code = string(apiError.Problem.Code)
	}
	return admissionCode(code)
}

// admissionCode separates refusals that mean "the system declined to admit this
// arrival" from genuine failures. Conflating them would hide saturation inside
// the failure count.
func admissionCode(code string) (bool, string) {
	switch code {
	case "quota_exceeded", "execution_node_unavailable", "home_runner_unavailable",
		"backpressure", "limit_exceeded":
		return true, code
	default:
		return false, code
	}
}
