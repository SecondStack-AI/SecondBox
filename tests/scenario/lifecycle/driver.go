package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	scenarioharness "github.com/SecondStack-AI/SecondBox/tests/scenario/harness"
)

type lifecycleDriver struct {
	config          lifecycleConfig
	client          *secondboxclient.Client
	runtimeDigest   string
	toolchainDigest string

	readyCount   atomic.Int64
	inFlight     atomic.Int64
	shedArrivals atomic.Int64
}

type startupTimingSamples struct {
	mu           sync.Mutex
	bootStages   map[string][]time.Duration
	startupSpans map[string][]time.Duration
}

func newStartupTimingSamples() *startupTimingSamples {
	return &startupTimingSamples{
		bootStages:   make(map[string][]time.Duration),
		startupSpans: make(map[string][]time.Duration),
	}
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
					Startup: secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot},
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
						DataPlaneTransport:          driver.config.Profile.DataPlaneTransport,
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
	timings *startupTimingSamples,
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
			// The Sandbox exists — the create Operation named it — so returning
			// no handle would strand it: cell cleanup can only delete what it
			// was handed, and a leaked Sandbox still counts against the subject
			// quota, which would eventually manufacture a refusal that looks
			// like saturation. A handle addressed by ID is enough, because
			// deleteSandbox refreshes through it before acting.
			if operation.SandboxID == "" {
				return nil, time.Since(startedAt), err
			}
			return secondboxclient.NewSandboxHandle(
				driver.client, secondboxclient.Sandbox{ID: operation.SandboxID},
			), time.Since(startedAt), err
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
	if timings != nil {
		if err := driver.collectBootTiming(ctx, operation.ID, elapsed, timings); err != nil {
			return handle, elapsed, err
		}
	}
	return handle, elapsed, nil
}

// startSandbox performs the warm arrival edge on a Sandbox that is stopped and
// still owns its Workspace. This is the ephemeral hot path.
func (driver *lifecycleDriver) startSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	timings *startupTimingSamples,
) (time.Duration, error) {
	startedAt := time.Now()
	operation, err := issueWithRevisionRetry(ctx, handle, key, handle.Start)
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
	if timings != nil {
		if err := driver.collectBootTiming(ctx, operation.ID, elapsed, timings); err != nil {
			return elapsed, err
		}
	}
	return elapsed, nil
}

func (driver *lifecycleDriver) stopSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	timings *startupTimingSamples,
) (time.Duration, error) {
	startedAt := time.Now()
	operation, err := issueWithRevisionRetry(ctx, handle, key, handle.Stop)
	if err != nil {
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
	if timings != nil {
		if err := driver.collectTeardownTiming(ctx, operation.ID, elapsed, timings); err != nil {
			return elapsed, err
		}
	}
	return elapsed, nil
}

func (driver *lifecycleDriver) deleteSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	timings *startupTimingSamples,
) (time.Duration, error) {
	startedAt := time.Now()
	wasReady := false
	var operation secondboxclient.Operation
	for attempt := 0; attempt < 3; attempt++ {
		sandbox, err := refreshWithRetry(ctx, handle, driver.pollInterval())
		if err != nil {
			return time.Since(startedAt), err
		}
		if sandbox.State == secondboxclient.SandboxStateDeleted {
			return time.Since(startedAt), nil
		}
		wasReady = wasReady || sandbox.State == secondboxclient.SandboxStateReady
		idempotencyKey := key
		if attempt > 0 {
			idempotencyKey = fmt.Sprintf("%s-revision-retry-%d", key, attempt)
		}
		operation, err = handle.Delete(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: idempotencyKey,
			IfMatch:        scenarioharness.RevisionETag(sandbox.Revision),
		})
		if err == nil {
			break
		}
		if secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodePreconditionFailed ||
			attempt == 2 {
			return time.Since(startedAt), err
		}
	}
	_, err := driver.client.WaitOperation(ctx, operation.ID, driver.pollInterval())
	elapsed := time.Since(startedAt)
	if err != nil {
		return elapsed, err
	}
	if wasReady {
		driver.readyCount.Add(-1)
	}
	if timings != nil {
		if err := driver.collectTeardownTiming(ctx, operation.ID, elapsed, timings); err != nil {
			return elapsed, err
		}
	}
	return elapsed, nil
}

// issueWithRevisionRetry re-reads the Sandbox and reissues a lifecycle request
// whose optimistic-concurrency precondition lost a race.
//
// Every lifecycle reconciliation commits a new public revision, including the
// ones that decide to wait, so a Sandbox the deployment is still reconciling
// changes revision underneath a caller that read it microseconds earlier. That
// is a lost race, not a rejection of the request, and the delete path already
// treats it that way. The retry is inside the measured span deliberately: it is
// latency the client really paid, and hiding it would understate the
// transition.
func issueWithRevisionRetry(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	issue func(
		context.Context, secondboxclient.LifecycleOptions,
	) (secondboxclient.Operation, error),
) (secondboxclient.Operation, error) {
	var operation secondboxclient.Operation
	for attempt := 0; attempt < 3; attempt++ {
		sandbox, err := handle.Refresh(ctx)
		if err != nil {
			return secondboxclient.Operation{}, err
		}
		idempotencyKey := key
		if attempt > 0 {
			idempotencyKey = fmt.Sprintf("%s-revision-retry-%d", key, attempt)
		}
		operation, err = issue(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: idempotencyKey,
			IfMatch:        scenarioharness.RevisionETag(sandbox.Revision),
		})
		if err == nil {
			return operation, nil
		}
		if secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodePreconditionFailed ||
			attempt == 2 {
			return secondboxclient.Operation{}, err
		}
	}
	return operation, nil
}

// isTransientTransportError reports whether an error is a connection-level fault
// rather than an answer from the deployment. A server that times out a long poll
// closes the connection, so a pooled keep-alive connection reused straight after
// is reset. That is exactly the moment a capacity run is under strain, and it
// must not be the moment cleanup gives up: a Sandbox that cannot be deleted is
// leaked, and leaked Sandboxes count against the subject quota for the rest of
// the ladder.
func isTransientTransportError(err error) bool {
	if err == nil || secondboxclient.ProblemCodeOf(err) != "" {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// refreshWithRetry re-reads a Sandbox, retrying connection-level faults. A
// typed answer from the deployment, including a refusal, is returned unchanged.
func refreshWithRetry(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	backoff time.Duration,
) (secondboxclient.Sandbox, error) {
	var sandbox secondboxclient.Sandbox
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		sandbox, err = handle.Refresh(ctx)
		if err == nil || !isTransientTransportError(err) {
			return sandbox, err
		}
		select {
		case <-ctx.Done():
			return sandbox, ctx.Err()
		case <-time.After(backoff * time.Duration(attempt+1)):
		}
	}
	return sandbox, err
}

func (driver *lifecycleDriver) wait(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	target secondboxclient.SandboxState,
) (secondboxclient.Sandbox, error) {
	waitContext, cancel := driver.operationContext(ctx)
	defer cancel()
	// This bounds one long-poll request, not the total wait. WaitSandbox reissues
	// requests until the operation context expires, treating each wait_expired as
	// "ask again", so how long the driver is willing to wait for a Sandbox is
	// operationTimeoutSeconds. The API rejects a single request above 60 seconds
	// (control_plane_service.go:1006), which is why this is bounded rather than
	// free.
	return scenarioharness.WaitSandbox(
		waitContext, handle,
		[]secondboxclient.SandboxState{target, secondboxclient.SandboxStateFailed},
		time.Duration(driver.config.SandboxWaitDeadlineSeconds)*time.Second,
	)
}

// collectBootTiming records runner stages plus non-additive control-plane spans
// derived from the persisted Operation and runner observation clocks. A timing
// read failure makes the measurement invalid and fails the arrival explicitly.
func (driver *lifecycleDriver) collectBootTiming(
	ctx context.Context,
	operationID string,
	clientElapsed time.Duration,
	timings *startupTimingSamples,
) error {
	timing, err := scenarioharness.RequestJSON[secondboxclient.OperationTiming](
		ctx, driver.client, "getOperationTiming", secondboxclient.CallOptions{
			PathParameters: map[string]string{"operationId": operationID},
		},
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle startup timing read failed: %w", err)
	}
	timings.mu.Lock()
	defer timings.mu.Unlock()
	timings.recordStartupSpanLocked(
		"operation_total",
		millisecondsPointerDuration(timing.TotalMilliseconds),
	)
	if timing.TotalMilliseconds != nil {
		visibility := clientElapsed - time.Duration(*timing.TotalMilliseconds)*time.Millisecond
		timings.recordStartupSpanLocked("client_visibility", max(visibility, 0))
	}
	var readyProjectedAt *time.Time
	var workspaceReadyAt *time.Time
	for _, stage := range timing.Orchestration {
		switch stage.Stage {
		case "workspace_ready":
			observedAt := stage.ObservedAt
			workspaceReadyAt = &observedAt
			timings.recordStartupSpanLocked(
				"workspace_provision",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_reconcile_started":
			timings.recordStartupSpanLocked(
				"placement_pickup",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_effect_started":
			timings.recordStartupSpanLocked(
				"placement_reconcile",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_plan_ready":
			timings.recordStartupSpanLocked(
				"placement_plan",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_schedule_started":
			timings.recordStartupSpanLocked(
				"placement_handoff",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_attempt_started":
			timings.recordStartupSpanLocked(
				"placement_retry",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_sandbox_locked":
			timings.recordStartupSpanLocked(
				"placement_sandbox_lock",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_assignment_checked":
			timings.recordStartupSpanLocked(
				"placement_assignment_check",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_candidates_locked":
			timings.recordStartupSpanLocked(
				"placement_candidate_lock",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_candidate_selected":
			timings.recordStartupSpanLocked(
				"placement_candidate_select",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "placement_ready":
			placement := time.Duration(stage.ElapsedMilliseconds * float64(time.Millisecond))
			if workspaceReadyAt != nil {
				placement = max(stage.ObservedAt.Sub(*workspaceReadyAt), 0)
			}
			timings.recordStartupSpanLocked(
				"placement",
				placement,
			)
			timings.recordStartupSpanLocked(
				"placement_prepare",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
			timings.recordStartupSpanLocked(
				"pre_assignment",
				time.Duration(stage.CumulativeMilliseconds*float64(time.Millisecond)),
			)
		case "startup_dispatched":
			timings.recordStartupSpanLocked(
				"startup_dispatch",
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
		case "ready_projected":
			projectedAt := stage.ObservedAt
			readyProjectedAt = &projectedAt
		}
	}
	for _, boot := range timing.Boots {
		if len(boot.Stages) == 0 {
			continue
		}
		first := boot.Stages[0]
		assignmentCreatedAt := first.ObservedAt.Add(
			-time.Duration(first.CumulativeMilliseconds * float64(time.Millisecond)),
		)
		if len(timing.Orchestration) == 0 {
			timings.recordStartupSpanLocked(
				"pre_assignment",
				max(assignmentCreatedAt.Sub(timing.CreatedAt), 0),
			)
		}
		timings.recordStartupSpanLocked(
			"runner_boot",
			time.Duration(boot.DurationMilliseconds*float64(time.Millisecond)),
		)
		for _, stage := range boot.Stages {
			timings.bootStages[stage.Stage] = append(
				timings.bootStages[stage.Stage],
				time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
			)
			ingest := max(stage.ReceivedAt.Sub(stage.ObservedAt), 0)
			timings.recordStartupSpanLocked("runner_event_ingest", ingest)
			if stage.Stage == "ready" {
				timings.recordStartupSpanLocked("ready_event_ingest", ingest)
				if readyProjectedAt != nil {
					timings.recordStartupSpanLocked(
						"ready_projection",
						max(readyProjectedAt.Sub(stage.ReceivedAt), 0),
					)
				} else if timing.CompletedAt != nil {
					timings.recordStartupSpanLocked(
						"ready_projection",
						max(timing.CompletedAt.Sub(stage.ReceivedAt), 0),
					)
				}
			}
		}
	}
	return nil
}

// collectTeardownTiming records the durable orchestration milestones of one
// stop or delete Operation. Startup already had stage attribution; teardown did
// not, so ~900 ms of a ~1,016 ms delete could only be named as "orchestration".
//
// Each persisted milestone becomes one span carrying its own elapsed value, so
// a span is the cost of exactly one hop rather than a running total. Unlike the
// startup spans these do not overlap: they partition the Operation's wall clock
// end to end, and orchestration_unattributed is whatever no milestone claims.
func (driver *lifecycleDriver) collectTeardownTiming(
	ctx context.Context,
	operationID string,
	clientElapsed time.Duration,
	timings *startupTimingSamples,
) error {
	timing, err := scenarioharness.RequestJSON[secondboxclient.OperationTiming](
		ctx, driver.client, "getOperationTiming", secondboxclient.CallOptions{
			PathParameters: map[string]string{"operationId": operationID},
		},
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle teardown timing read failed: %w", err)
	}
	timings.mu.Lock()
	defer timings.mu.Unlock()
	timings.recordStartupSpanLocked(
		"operation_total",
		millisecondsPointerDuration(timing.TotalMilliseconds),
	)
	attributed := 0.0
	for _, stage := range timing.Orchestration {
		timings.recordStartupSpanLocked(
			stage.Stage,
			time.Duration(stage.ElapsedMilliseconds*float64(time.Millisecond)),
		)
		attributed = stage.CumulativeMilliseconds
	}
	if timing.TotalMilliseconds != nil {
		total := float64(*timing.TotalMilliseconds)
		timings.recordStartupSpanLocked(
			"orchestration_unattributed",
			max(time.Duration((total-attributed)*float64(time.Millisecond)), 0),
		)
		// The Operation's own clock stops at its durable completion; the driver
		// only learns about it on its next poll. That gap is the benchmark's
		// observation cost, not the deployment's.
		timings.recordStartupSpanLocked(
			"client_visibility",
			max(clientElapsed-time.Duration(total*float64(time.Millisecond)), 0),
		)
	}
	return nil
}

func millisecondsPointerDuration(value *int64) time.Duration {
	if value == nil {
		return -1
	}
	return time.Duration(*value) * time.Millisecond
}

func (timings *startupTimingSamples) recordStartupSpanLocked(name string, value time.Duration) {
	if value < 0 {
		return
	}
	timings.startupSpans[name] = append(timings.startupSpans[name], value)
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
