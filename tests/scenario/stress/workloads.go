package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	scenarioharness "github.com/SecondStack-AI/SecondBox/tests/scenario/harness"
)

func (driver *stressDriver) run(
	ctx context.Context,
) ([]workloadResult, secondboxclient.DeploymentTimingSummary, error) {
	if err := driver.waitForRunner(ctx); err != nil {
		return nil, secondboxclient.DeploymentTimingSummary{}, err
	}
	results := make([]workloadResult, 0, len(driver.config.Workloads)*len(driver.config.ConcurrencyLevels))
	binding := driver.config.configuredBinding(driver.guestCIDR)
	for _, workload := range driver.config.Workloads {
		for _, concurrency := range driver.config.ConcurrencyLevels {
			fmt.Printf("Running workload=%s concurrency=%d\n", workload, concurrency)
			samples, err := driver.runLevel(ctx, workload, concurrency)
			if err != nil {
				return nil, secondboxclient.DeploymentTimingSummary{}, err
			}
			results = append(results, samples.report(binding))
		}
	}
	summary, err := scenarioharness.RequestJSON[secondboxclient.DeploymentTimingSummary](
		ctx, driver.client, "getDeploymentTiming", secondboxclient.CallOptions{
			QueryParameters: url.Values{
				"windowSeconds": {strconv.Itoa(driver.config.TimingWindowSeconds)},
			},
		},
	)
	if err != nil {
		return results, secondboxclient.DeploymentTimingSummary{},
			fmt.Errorf("SecondBox stress read deployment timing failed: %w", err)
	}
	return results, summary, nil
}

func (driver *stressDriver) runLevel(
	ctx context.Context,
	workload string,
	concurrency int,
) (resultSamples, error) {
	samples := resultSamples{
		workload: workload, concurrency: concurrency,
		problemCounts: make(map[string]int64),
	}
	if workload == workloadSandboxCreate {
		samples.startedAt = time.Now()
		samples = driver.runCreateWorkers(ctx, samples)
		samples.completedAt = time.Now()
		return samples, nil
	}
	handles, queuedHandles, setupSamples := driver.prepareWorkerSandboxes(
		ctx, workload, concurrency,
	)
	mergeSamples(&samples, setupSamples)
	samples.startedAt = time.Now()
	deadline := samples.startedAt.Add(time.Duration(driver.config.DurationSeconds) * time.Second)
	results := make(chan workerSamples, len(handles))
	var workers sync.WaitGroup
	for workerIndex, handle := range handles {
		workers.Add(1)
		go func(index int, sandbox *secondboxclient.SandboxHandle) {
			defer workers.Done()
			results <- driver.runWorker(ctx, workload, index, sandbox, deadline)
		}(workerIndex, handle)
	}
	workers.Wait()
	close(results)
	for result := range results {
		mergeSamples(&samples, result)
	}
	samples.completedAt = time.Now()
	for index, handle := range handles {
		if err := driver.deleteSandbox(
			ctx, handle, fmt.Sprintf("stress-%s-%d-cleanup", workload, index),
		); err != nil {
			addError(&samples, err)
		}
	}
	for index, handle := range queuedHandles {
		if err := driver.deleteSandbox(
			ctx, handle, fmt.Sprintf("stress-%s-%d-queued-cleanup", workload, index),
		); err != nil {
			addError(&samples, err)
		}
	}
	return samples, nil
}

type workerSamples struct {
	durations         []time.Duration
	admissionRefusals int64
	queuedAdmissions  int64
	failures          int64
	problemCounts     map[string]int64
}

func (driver *stressDriver) runCreateWorkers(
	ctx context.Context,
	samples resultSamples,
) resultSamples {
	deadline := samples.startedAt.Add(time.Duration(driver.config.DurationSeconds) * time.Second)
	results := make(chan workerSamples, samples.concurrency)
	var workers sync.WaitGroup
	var sequence atomic.Int64
	for workerIndex := 0; workerIndex < samples.concurrency; workerIndex++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			result := workerSamples{problemCounts: make(map[string]int64)}
			for time.Now().Before(deadline) {
				key := fmt.Sprintf(
					"stress-create-%d-%d", index, sequence.Add(1),
				)
				handle, _, elapsed, err := driver.createReadySandbox(ctx, key)
				if err == nil {
					if deleteErr := driver.deleteSandbox(ctx, handle, key+"-delete"); deleteErr != nil {
						err = deleteErr
					} else {
						result.durations = append(result.durations, elapsed)
					}
				} else if handle != nil {
					if deleteErr := driver.deleteSandbox(ctx, handle, key+"-failed-delete"); deleteErr != nil {
						err = errors.Join(err, deleteErr)
					}
				}
				if err != nil {
					addWorkerError(&result, err)
					if retryDelay, retry := stressAdmissionRetryDelay(
						err,
						time.Duration(driver.config.PollIntervalMilliseconds)*time.Millisecond,
					); retry && !waitForStressRetry(ctx, deadline, retryDelay) {
						break
					}
				}
			}
			results <- result
		}(workerIndex)
	}
	workers.Wait()
	close(results)
	for result := range results {
		mergeSamples(&samples, result)
	}
	return samples
}

func stressAdmissionRetryDelay(err error, fallback time.Duration) (time.Duration, bool) {
	var problem *secondboxclient.Problem
	var apiError *secondboxclient.APIError
	if errors.As(err, &apiError) {
		problem = apiError.Problem
	}
	var operationFailure *secondboxclient.OperationFailure
	if problem == nil && errors.As(err, &operationFailure) {
		problem = operationFailure.Operation.Error
	}
	if problem == nil || !problem.Retryable || problem.Code != "home_runner_unavailable" {
		return 0, false
	}
	if problem.RetryAfterMilliseconds != nil && *problem.RetryAfterMilliseconds > 0 {
		return time.Duration(*problem.RetryAfterMilliseconds) * time.Millisecond, true
	}
	return fallback, true
}

func waitForStressRetry(
	ctx context.Context,
	deadline time.Time,
	delay time.Duration,
) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(min(delay, remaining))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return time.Now().Before(deadline)
	}
}

func (driver *stressDriver) prepareWorkerSandboxes(
	ctx context.Context,
	workload string,
	concurrency int,
) (
	[]*secondboxclient.SandboxHandle,
	[]*secondboxclient.SandboxHandle,
	workerSamples,
) {
	type setupResult struct {
		handle *secondboxclient.SandboxHandle
		err    error
	}
	runSetupWave := func(waveContext context.Context, start int, count int) []setupResult {
		results := make(chan setupResult, count)
		var workers sync.WaitGroup
		for index := start; index < start+count; index++ {
			workers.Add(1)
			go func(workerIndex int) {
				defer workers.Done()
				key := fmt.Sprintf("stress-%s-%d-setup", workload, workerIndex)
				handle, _, _, err := driver.createReadySandbox(waveContext, key)
				results <- setupResult{handle: handle, err: err}
			}(index)
		}
		workers.Wait()
		close(results)
		wave := make([]setupResult, 0, count)
		for result := range results {
			wave = append(wave, result)
		}
		return wave
	}
	binding := driver.config.configuredBinding(driver.guestCIDR)
	saturationWave := min(concurrency, binding.Capacity)
	results := runSetupWave(ctx, 0, saturationWave)
	if saturationWave < concurrency {
		probeContext, cancel := context.WithTimeout(
			ctx, 4*time.Duration(driver.config.PollIntervalMilliseconds)*time.Millisecond,
		)
		defer cancel()
		results = append(
			results,
			runSetupWave(probeContext, saturationWave, concurrency-saturationWave)...,
		)
	}
	handles := make([]*secondboxclient.SandboxHandle, 0, concurrency)
	queuedHandles := make([]*secondboxclient.SandboxHandle, 0, concurrency)
	samples := workerSamples{problemCounts: make(map[string]int64)}
	for _, result := range results {
		if result.err == nil {
			handles = append(handles, result.handle)
			continue
		}
		if result.handle != nil && errors.Is(result.err, context.DeadlineExceeded) {
			queuedHandles = append(queuedHandles, result.handle)
			samples.queuedAdmissions++
			samples.problemCounts["queued_at_runner_capacity"]++
			continue
		}
		addWorkerError(&samples, result.err)
		if result.handle != nil {
			queuedHandles = append(queuedHandles, result.handle)
		}
	}
	return handles, queuedHandles, samples
}

func (driver *stressDriver) runWorker(
	ctx context.Context,
	workload string,
	workerIndex int,
	handle *secondboxclient.SandboxHandle,
	deadline time.Time,
) workerSamples {
	result := workerSamples{problemCounts: make(map[string]int64)}
	for iteration := 0; time.Now().Before(deadline); iteration++ {
		startedAt := time.Now()
		key := fmt.Sprintf("stress-%s-%d-%d", workload, workerIndex, iteration)
		var err error
		switch workload {
		case workloadBufferedExec:
			err = driver.runBufferedExec(ctx, handle, key)
		case workloadStreamingExec:
			err = driver.runStreamingExec(ctx, handle, key)
		case workloadFileTransfer:
			err = driver.runFileTransfer(ctx, handle, key, workerIndex)
		case workloadSnapshotRestore:
			err = driver.runSnapshotRestore(ctx, handle, key, workerIndex, iteration)
		default:
			err = fmt.Errorf("SecondBox stress unsupported workload %q", workload)
		}
		if err == nil {
			result.durations = append(result.durations, time.Since(startedAt))
			continue
		}
		if admission, _ := classifyStressError(err); !admission {
			fmt.Printf(
				"Workload failure workload=%s worker=%d iteration=%d: %v\n",
				workload, workerIndex, iteration, err,
			)
		}
		addWorkerError(&result, err)
		if workload == workloadSnapshotRestore {
			break
		}
	}
	return result
}

func (driver *stressDriver) runBufferedExec(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
) error {
	outcome, err := handle.Execute(ctx, secondboxclient.BufferedExecRequest{
		Command: secondboxclient.Command{
			ShellCommand: &secondboxclient.ShellCommand{
				Mode: "shell", Command: "printf secondbox-stress",
			},
		},
		Environment:          secondboxclient.StringMap{},
		DeadlineMilliseconds: driver.config.Profile.MaximumDeadlineMilliseconds,
		MaximumOutputBytes:   driver.config.Profile.MaximumBufferedOutputBytes,
	}, key, "")
	if err != nil {
		return err
	}
	if outcome.ExecExited == nil || outcome.ExecExited.ExitCode != 0 {
		return fmt.Errorf("SecondBox stress buffered Exec outcome is not successful: %#v", outcome)
	}
	output, err := base64.StdEncoding.Strict().DecodeString(outcome.ExecExited.Output.StdoutBase64)
	if err != nil || string(output) != "secondbox-stress" {
		return errors.New("SecondBox stress buffered Exec output is invalid")
	}
	return nil
}

func (driver *stressDriver) runStreamingExec(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
) error {
	command := fmt.Sprintf("head -c %d /dev/zero", driver.config.StreamingOutputBytes)
	session, err := handle.CreateExecStream(ctx, secondboxclient.StreamingExecRequest{
		Command: secondboxclient.Command{
			ShellCommand: &secondboxclient.ShellCommand{Mode: "shell", Command: command},
		},
		Environment:          secondboxclient.StringMap{},
		DeadlineMilliseconds: driver.config.Profile.MaximumDeadlineMilliseconds,
		MaximumOutputBytes:   driver.config.Profile.MaximumBufferedOutputBytes,
		WindowBytes:          driver.config.Profile.StreamWindowBytes,
	}, key, "")
	if err != nil {
		return err
	}
	stream, err := handle.ConnectExecStream(ctx, session, nil)
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := stream.CloseInput(); err != nil {
		return err
	}
	if err := stream.GrantOutput(driver.config.Profile.StreamWindowBytes); err != nil {
		return err
	}
	var received int
	for {
		frame, err := stream.Receive()
		if err != nil {
			return err
		}
		if frame.StreamOutputFrame != nil {
			content, decodeErr := base64.StdEncoding.Strict().DecodeString(
				frame.StreamOutputFrame.DataBase64,
			)
			if decodeErr != nil {
				return fmt.Errorf("SecondBox stress streaming Exec output decode failed: %w", decodeErr)
			}
			received += len(content)
			if received < driver.config.StreamingOutputBytes {
				if err := stream.GrantOutput(int64(len(content))); err != nil {
					return err
				}
			}
			continue
		}
		if frame.StreamOutcomeFrame == nil ||
			frame.StreamOutcomeFrame.Outcome.ExecExited == nil ||
			frame.StreamOutcomeFrame.Outcome.ExecExited.ExitCode != 0 {
			return fmt.Errorf("SecondBox stress streaming Exec outcome is invalid: %#v", frame)
		}
		if received != driver.config.StreamingOutputBytes {
			return fmt.Errorf(
				"SecondBox stress streaming Exec received %d bytes, want %d",
				received, driver.config.StreamingOutputBytes,
			)
		}
		return nil
	}
}

func (driver *stressDriver) runFileTransfer(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	workerIndex int,
) error {
	content := bytes.Repeat([]byte{byte(workerIndex % 251)}, driver.config.FileTransferBytes)
	path := fmt.Sprintf("secondbox-stress-%d.bin", workerIndex)
	headers := handle.GenerationHeaders("")
	headers.Set("Idempotency-Key", key)
	headers.Set("Digest", digestHeader(content))
	result, err := scenarioharness.RequestJSON[secondboxclient.FileWriteResult](
		ctx, driver.client, "writeSandboxFile", secondboxclient.CallOptions{
			PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
			QueryParameters: url.Values{"path": {path}},
			Headers:         headers, Body: bytes.NewReader(content),
			ContentType: "application/octet-stream",
		},
	)
	if err != nil {
		return err
	}
	if result.SizeBytes != int64(len(content)) {
		return fmt.Errorf("SecondBox stress File write size=%d, want %d", result.SizeBytes, len(content))
	}
	response, err := driver.client.Request(ctx, "readSandboxFile", secondboxclient.CallOptions{
		PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
		QueryParameters: url.Values{"path": {path}},
		Headers:         handle.GenerationHeaders(""),
	})
	if err != nil {
		return err
	}
	downloaded, readErr := io.ReadAll(io.LimitReader(
		response.Body, int64(driver.config.FileTransferBytes)+1,
	))
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if !bytes.Equal(downloaded, content) {
		return errors.New("SecondBox stress File round trip changed content")
	}
	return nil
}

func (driver *stressDriver) runSnapshotRestore(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	workerIndex int,
	iteration int,
) error {
	path := fmt.Sprintf("secondbox-stress-snapshot-%d.txt", workerIndex)
	baseline := []byte(fmt.Sprintf("baseline-%d-%d", workerIndex, iteration))
	if err := driver.writeFile(ctx, handle, key+"-baseline", path, baseline); err != nil {
		return err
	}
	if err := driver.transition(ctx, handle, "stop", key+"-stop-before-snapshot", "stopped", ""); err != nil {
		return err
	}
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		return err
	}
	operation, err := handle.CreateSnapshot(
		ctx,
		secondboxclient.LifecycleOptions{
			IdempotencyKey: key + "-snapshot", IfMatch: scenarioharness.RevisionETag(sandbox.Revision),
		},
		secondboxclient.CreateSnapshotRequest{
			Name: key, Metadata: secondboxclient.Metadata{"qualification": "stress"},
		},
	)
	if err != nil {
		return err
	}
	completed, err := driver.waitOperation(ctx, operation.ID)
	if err != nil {
		return err
	}
	if completed.Snapshot == nil || completed.Snapshot.ID == "" {
		return errors.New("SecondBox stress Snapshot operation has no Snapshot")
	}
	snapshotID := completed.Snapshot.ID
	if err := driver.transition(ctx, handle, "start", key+"-start-after-snapshot", "ready", ""); err != nil {
		return err
	}
	if err := driver.writeFile(ctx, handle, key+"-mutate", path, []byte("mutated")); err != nil {
		return err
	}
	if err := driver.transition(ctx, handle, "stop", key+"-stop-before-restore", "stopped", ""); err != nil {
		return err
	}
	if err := driver.transition(ctx, handle, "restore", key+"-restore", "stopped", snapshotID); err != nil {
		return err
	}
	if err := driver.transition(ctx, handle, "start", key+"-start-after-restore", "ready", ""); err != nil {
		return err
	}
	restored, err := driver.readFile(ctx, handle, path, int64(len(baseline)))
	if err != nil {
		return err
	}
	if !bytes.Equal(restored, baseline) {
		return errors.New("SecondBox stress Snapshot restore did not recover the baseline bytes")
	}
	if err := driver.transition(
		ctx, handle, "stop", key+"-stop-before-snapshot-delete", "stopped", "",
	); err != nil {
		return err
	}
	deleteOperation, err := driver.client.DeleteSnapshot(
		ctx, snapshotID, key+"-delete-snapshot",
	)
	if err != nil {
		return err
	}
	_, err = driver.waitOperation(ctx, deleteOperation.ID)
	return err
}

func (driver *stressDriver) transition(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	action string,
	key string,
	wantState secondboxclient.SandboxState,
	snapshotID string,
) error {
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		return err
	}
	options := secondboxclient.LifecycleOptions{
		IdempotencyKey: key, IfMatch: scenarioharness.RevisionETag(sandbox.Revision),
	}
	var operation secondboxclient.Operation
	switch action {
	case "start":
		operation, err = handle.Start(ctx, options)
	case "stop":
		operation, err = handle.Stop(ctx, options)
	case "restore":
		operation, err = handle.Restore(ctx, options, snapshotID)
	default:
		return fmt.Errorf("SecondBox stress lifecycle action %q is unsupported", action)
	}
	if err != nil {
		return err
	}
	if _, err := driver.waitOperation(ctx, operation.ID); err != nil {
		return err
	}
	reached, err := driver.waitSandbox(
		ctx, handle, []secondboxclient.SandboxState{wantState, secondboxclient.SandboxStateFailed},
	)
	if err != nil {
		return err
	}
	if reached.State != wantState {
		return fmt.Errorf(
			"SecondBox stress lifecycle %s reached %s, want %s",
			action, reached.State, wantState,
		)
	}
	return nil
}

func (driver *stressDriver) waitOperation(
	ctx context.Context,
	operationID string,
) (secondboxclient.Operation, error) {
	operationContext, cancel := context.WithTimeout(
		ctx, time.Duration(driver.config.OperationTimeoutSeconds)*time.Second,
	)
	defer cancel()
	return driver.client.WaitOperation(
		operationContext, operationID,
		time.Duration(driver.config.PollIntervalMilliseconds)*time.Millisecond,
	)
}

func (driver *stressDriver) writeFile(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	key string,
	path string,
	content []byte,
) error {
	headers := handle.GenerationHeaders("")
	headers.Set("Idempotency-Key", key)
	headers.Set("Digest", digestHeader(content))
	_, err := scenarioharness.RequestJSON[secondboxclient.FileWriteResult](
		ctx, driver.client, "writeSandboxFile", secondboxclient.CallOptions{
			PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
			QueryParameters: url.Values{"path": {path}},
			Headers:         headers, Body: bytes.NewReader(content),
			ContentType: "application/octet-stream",
		},
	)
	return err
}

func (driver *stressDriver) readFile(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	path string,
	maximumBytes int64,
) ([]byte, error) {
	response, err := driver.client.Request(ctx, "readSandboxFile", secondboxclient.CallOptions{
		PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
		QueryParameters: url.Values{"path": {path}},
		Headers:         handle.GenerationHeaders(""),
	})
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if int64(len(content)) > maximumBytes {
		return nil, errors.New("SecondBox stress File read exceeded its explicit byte bound")
	}
	return content, nil
}

func addWorkerError(samples *workerSamples, err error) {
	admission, code := classifyStressError(err)
	if admission {
		samples.admissionRefusals++
	} else {
		samples.failures++
	}
	samples.problemCounts[code]++
}

func addError(samples *resultSamples, err error) {
	admission, code := classifyStressError(err)
	if admission {
		samples.admissionRefusals++
	} else {
		samples.failures++
	}
	samples.problemCounts[code]++
}

func mergeSamples(target *resultSamples, source workerSamples) {
	target.durations = append(target.durations, source.durations...)
	target.admissionRefusals += source.admissionRefusals
	target.queuedAdmissions += source.queuedAdmissions
	target.failures += source.failures
	for code, count := range source.problemCounts {
		target.problemCounts[code] += count
	}
}
