package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type stressDriver struct {
	config          stressConfig
	client          *secondboxclient.Client
	runtimeDigest   string
	toolchainDigest string
	bootMu          sync.Mutex
	bootStages      map[string][]time.Duration
}

func (driver *stressDriver) prepare(ctx context.Context) error {
	pool, err := requestJSON[secondboxclient.RunnerPool](
		ctx, driver.client, "createRunnerPool", secondboxclient.CallOptions{
			Body: encodeJSON(secondboxclient.CreateRunnerPoolRequest{
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
		return fmt.Errorf("SecondBox stress create RunnerPool failed: %w", err)
	}
	if pool.Name != driver.config.RunnerPoolName || pool.State != "ready" {
		return fmt.Errorf("SecondBox stress RunnerPool projection is unexpected: %#v", pool)
	}
	profile, err := requestJSON[secondboxclient.Profile](
		ctx, driver.client, "createProfile", secondboxclient.CallOptions{
			Headers: idempotencyHeaders("stress-prepare-profile"),
			Body: encodeJSON(secondboxclient.CreateProfileRequest{
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
		return fmt.Errorf("SecondBox stress create Profile failed: %w", err)
	}
	if profile.Name != driver.config.ProfileName || profile.CurrentRevision.ID == "" {
		return fmt.Errorf("SecondBox stress Profile projection is unexpected: %#v", profile)
	}
	fmt.Printf(
		"Prepared RunnerPool %s and Profile %s through the published API\n",
		pool.Name, profile.Name,
	)
	return nil
}

func (driver *stressDriver) waitForRunner(ctx context.Context) error {
	deadline := time.Now().Add(time.Duration(driver.config.OperationTimeoutSeconds) * time.Second)
	var lastPage secondboxclient.RunnerPage
	for time.Now().Before(deadline) {
		page, err := requestJSON[secondboxclient.RunnerPage](
			ctx, driver.client, "listRunners", secondboxclient.CallOptions{
				QueryParameters: url.Values{
					"pool":  {driver.config.RunnerPoolName},
					"limit": {"200"},
				},
			},
		)
		if err != nil {
			return fmt.Errorf("SecondBox stress list Runners failed: %w", err)
		}
		lastPage = page
		for _, runner := range page.Items {
			if runner.State == "ready" {
				pool, poolErr := requestJSON[secondboxclient.RunnerPool](
					ctx, driver.client, "getRunnerPool", secondboxclient.CallOptions{
						PathParameters: map[string]string{"runnerPoolName": driver.config.RunnerPoolName},
					},
				)
				if poolErr != nil {
					return fmt.Errorf("SecondBox stress get RunnerPool failed: %w", poolErr)
				}
				if pool.ReadyRunnerCount < 1 {
					return errors.New("SecondBox stress Runner is ready but RunnerPool has no ready Runner")
				}
				return nil
			}
		}
		timer := time.NewTimer(time.Duration(driver.config.PollIntervalMilliseconds) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf(
		"SecondBox stress Runner did not become ready before the outer deadline; last page had %d entries",
		len(lastPage.Items),
	)
}

func requestJSON[T any](
	ctx context.Context,
	client *secondboxclient.Client,
	operationID string,
	options secondboxclient.CallOptions,
) (T, error) {
	var result T
	if err := client.RequestJSON(ctx, operationID, options, &result); err != nil {
		return result, err
	}
	return result, nil
}

func encodeJSON(value any) io.Reader {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("SecondBox stress encode static request: %v", err))
	}
	return bytes.NewReader(data)
}

func idempotencyHeaders(value string) http.Header {
	headers := make(http.Header)
	headers.Set("Idempotency-Key", value)
	return headers
}

func revisionETag(revision int64) string {
	return `"revision-` + strconv.FormatInt(revision, 10) + `"`
}

func (driver *stressDriver) createReadySandbox(
	ctx context.Context,
	key string,
) (*secondboxclient.SandboxHandle, string, time.Duration, error) {
	startedAt := time.Now()
	operation, err := requestJSON[secondboxclient.Operation](
		ctx, driver.client, "createSandbox", secondboxclient.CallOptions{
			Headers: idempotencyHeaders(key),
			Body: encodeJSON(secondboxclient.CreateSandboxRequest{
				Profile: driver.config.ProfileName,
				Metadata: secondboxclient.Metadata{
					"qualification": "stress", "key": key,
				},
			}),
		},
	)
	if err != nil {
		return nil, "", 0, err
	}
	var sandbox secondboxclient.Sandbox
	if operation.Sandbox != nil {
		sandbox = *operation.Sandbox
	} else {
		sandbox, err = requestJSON[secondboxclient.Sandbox](
			ctx, driver.client, "getSandbox", secondboxclient.CallOptions{
				PathParameters: map[string]string{"sandboxId": operation.SandboxID},
			},
		)
		if err != nil {
			return nil, operation.ID, 0, err
		}
	}
	handle := secondboxclient.NewSandboxHandle(driver.client, sandbox)
	reached, err := driver.waitSandbox(
		ctx, handle, []secondboxclient.SandboxState{
			secondboxclient.SandboxStateReady, secondboxclient.SandboxStateFailed,
		},
	)
	elapsed := time.Since(startedAt)
	if err != nil {
		return handle, operation.ID, elapsed, err
	}
	if reached.State != secondboxclient.SandboxStateReady {
		operationContext, cancel := context.WithTimeout(
			ctx, time.Duration(driver.config.OperationTimeoutSeconds)*time.Second,
		)
		defer cancel()
		if _, operationErr := driver.client.WaitOperation(
			operationContext, operation.ID,
			time.Duration(driver.config.PollIntervalMilliseconds)*time.Millisecond,
		); operationErr != nil {
			return handle, operation.ID, elapsed, operationErr
		}
		return handle, operation.ID, elapsed, fmt.Errorf(
			"SecondBox stress Sandbox %s reached %s", reached.ID, reached.State,
		)
	}
	operationContext, cancel := context.WithTimeout(
		ctx, time.Duration(driver.config.OperationTimeoutSeconds)*time.Second,
	)
	defer cancel()
	if _, err := driver.client.WaitOperation(
		operationContext, operation.ID,
		time.Duration(driver.config.PollIntervalMilliseconds)*time.Millisecond,
	); err != nil {
		return handle, operation.ID, elapsed, err
	}
	if err := driver.collectBootTiming(ctx, operation.ID); err != nil {
		return handle, operation.ID, elapsed, err
	}
	return handle, operation.ID, elapsed, nil
}

func (driver *stressDriver) waitSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	states []secondboxclient.SandboxState,
) (secondboxclient.Sandbox, error) {
	deadline := time.Now().Add(time.Duration(driver.config.OperationTimeoutSeconds) * time.Second)
	last := handle.Snapshot()
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		wait := min(remaining, 60*time.Second)
		if wait < time.Millisecond {
			break
		}
		sandbox, err := handle.Wait(ctx, states, wait)
		if err == nil {
			return sandbox, nil
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) || apiError.Problem == nil ||
			apiError.Problem.Code != "wait_expired" {
			return last, err
		}
		refreshed, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			return last, refreshErr
		}
		last = refreshed
	}
	return last, fmt.Errorf(
		"SecondBox stress Sandbox wait expired after %d seconds; last state=%s",
		driver.config.OperationTimeoutSeconds, last.State,
	)
}

func (driver *stressDriver) collectBootTiming(ctx context.Context, operationID string) error {
	timing, err := requestJSON[secondboxclient.OperationTiming](
		ctx, driver.client, "getOperationTiming", secondboxclient.CallOptions{
			PathParameters: map[string]string{"operationId": operationID},
		},
	)
	if err != nil {
		return fmt.Errorf("SecondBox stress read Operation timing failed: %w", err)
	}
	driver.bootMu.Lock()
	defer driver.bootMu.Unlock()
	for _, boot := range timing.Boots {
		for _, stage := range boot.Stages {
			driver.bootStages[stage.Stage] = append(
				driver.bootStages[stage.Stage],
				time.Duration(stage.ElapsedMilliseconds)*time.Millisecond,
			)
		}
	}
	return nil
}

func (driver *stressDriver) deleteSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
) error {
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		return err
	}
	if sandbox.State == secondboxclient.SandboxStateDeleted {
		return nil
	}
	operation, err := handle.Delete(ctx, secondboxclient.LifecycleOptions{
		IdempotencyKey: key, IfMatch: revisionETag(sandbox.Revision),
	})
	if err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(
		ctx, time.Duration(driver.config.OperationTimeoutSeconds)*time.Second,
	)
	defer cancel()
	_, err = driver.client.WaitOperation(
		operationContext, operation.ID,
		time.Duration(driver.config.PollIntervalMilliseconds)*time.Millisecond,
	)
	return err
}

func classifyStressError(err error) (bool, string) {
	var operationFailure *secondboxclient.OperationFailure
	if errors.As(err, &operationFailure) && operationFailure.Operation.Error != nil {
		code := operationFailure.Operation.Error.Code
		switch code {
		case "quota_exceeded", "execution_node_unavailable", "home_runner_unavailable",
			"backpressure", "limit_exceeded":
			return true, code
		default:
			return false, code
		}
	}
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) {
		return false, "client_error"
	}
	code := "http_" + strconv.Itoa(apiError.StatusCode)
	if apiError.Problem != nil && strings.TrimSpace(string(apiError.Problem.Code)) != "" {
		code = string(apiError.Problem.Code)
	}
	switch code {
	case "quota_exceeded", "execution_node_unavailable", "home_runner_unavailable",
		"backpressure", "limit_exceeded":
		return true, code
	default:
		return false, code
	}
}

func digestHeader(content []byte) string {
	sum := sha256Bytes(content)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum) + ":"
}

func sha256Bytes(content []byte) []byte {
	sum := sha256.Sum256(content)
	return sum[:]
}
