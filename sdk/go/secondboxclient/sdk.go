package secondboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// CallOptions supplies public wire values to one generated operation.
type CallOptions struct {
	PathParameters  map[string]string
	QueryParameters url.Values
	Headers         http.Header
	Body            io.Reader
	ContentType     string
}

// Request invokes a generated operation by its stable OpenAPI operationId.
func (client *Client) Request(ctx context.Context, operationID string, options CallOptions) (*http.Response, error) {
	operation, found := LookupOperation(operationID)
	if !found {
		return nil, fmt.Errorf("SecondBox client unknown operation %q", operationID)
	}
	return client.Do(ctx, operation, RequestOptions{
		PathParameters:  options.PathParameters,
		QueryParameters: options.QueryParameters,
		Headers:         options.Headers,
		Body:            options.Body,
		ContentType:     options.ContentType,
	})
}

// RequestJSON invokes a generated operation and decodes its successful JSON response.
func (client *Client) RequestJSON(ctx context.Context, operationID string, options CallOptions, target any) error {
	if target == nil {
		return errors.New("SecondBox client JSON response target is required")
	}
	response, err := client.Request(ctx, operationID, options)
	if err != nil {
		return err
	}
	decodeErr := json.NewDecoder(response.Body).Decode(target)
	closeErr := response.Body.Close()
	if responseErr := errors.Join(decodeErr, closeErr); responseErr != nil {
		return fmt.Errorf("SecondBox client decode and close %s response: %w", operationID, responseErr)
	}
	return nil
}

// OperationFailure is a terminal asynchronous operation that did not succeed.
type OperationFailure struct {
	Operation Operation
}

func (failure *OperationFailure) Error() string {
	if failure.Operation.Error != nil {
		return fmt.Sprintf(
			"SecondBox operation failed: operation=%s state=%s code=%s title=%s",
			failure.Operation.ID,
			failure.Operation.State,
			failure.Operation.Error.Code,
			failure.Operation.Error.Title,
		)
	}
	return fmt.Sprintf("SecondBox operation failed: operation=%s state=%s", failure.Operation.ID, failure.Operation.State)
}

// WaitOperation polls an asynchronous operation until it reaches a terminal state.
func (client *Client) WaitOperation(ctx context.Context, operationID string, interval time.Duration) (Operation, error) {
	if interval <= 0 {
		return Operation{}, errors.New("SecondBox operation polling interval must be positive")
	}
	for {
		if err := ctx.Err(); err != nil {
			return Operation{}, err
		}
		var operation Operation
		err := client.RequestJSON(ctx, "getOperation", CallOptions{
			PathParameters: map[string]string{"operationId": operationID},
		}, &operation)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return Operation{}, contextErr
			}
			return Operation{}, err
		}
		switch operation.State {
		case OperationStateSucceeded:
			return operation, nil
		case OperationStateFailed, OperationStateCancelled:
			return Operation{}, &OperationFailure{Operation: operation}
		case OperationStatePending, OperationStateRunning:
		default:
			return Operation{}, fmt.Errorf("SecondBox operation %s has unknown state %q", operation.ID, operation.State)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return Operation{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// LifecycleOptions carries required idempotency and optimistic-concurrency values.
type LifecycleOptions struct {
	IdempotencyKey string
	IfMatch        string
}

// SandboxHandle retains a caller-owned Sandbox identity and its latest observed representation.
type SandboxHandle struct {
	client   *Client
	mu       sync.RWMutex
	snapshot Sandbox
}

// NewSandboxHandle attaches SDK behavior without taking ownership of Sandbox lifetime.
func NewSandboxHandle(client *Client, sandbox Sandbox) *SandboxHandle {
	return &SandboxHandle{client: client, snapshot: sandbox}
}

// Snapshot returns the latest representation observed through this handle.
func (handle *SandboxHandle) Snapshot() Sandbox {
	handle.mu.RLock()
	defer handle.mu.RUnlock()
	return handle.snapshot
}

// Refresh retrieves and retains the current Sandbox representation.
func (handle *SandboxHandle) Refresh(ctx context.Context) (Sandbox, error) {
	current := handle.Snapshot()
	var refreshed Sandbox
	err := handle.client.RequestJSON(ctx, "getSandbox", CallOptions{
		PathParameters: map[string]string{"sandboxId": string(current.ID)},
	}, &refreshed)
	if err != nil {
		return Sandbox{}, err
	}
	handle.store(refreshed)
	return refreshed, nil
}

// Wait asks the service to wait for one of the explicitly supplied Sandbox states.
func (handle *SandboxHandle) Wait(
	ctx context.Context,
	states []SandboxState,
	deadline time.Duration,
) (Sandbox, error) {
	if len(states) == 0 {
		return Sandbox{}, errors.New("SecondBox Sandbox wait requires at least one state")
	}
	if deadline.Milliseconds() < 1 {
		return Sandbox{}, errors.New("SecondBox Sandbox wait deadline must be at least one millisecond")
	}
	const maximumWaitDeadline = 60 * time.Second
	if deadline > maximumWaitDeadline {
		return Sandbox{}, errors.New("SecondBox Sandbox wait deadline must not exceed 60 seconds")
	}
	current := handle.Snapshot()
	body, err := json.Marshal(WaitSandboxRequest{
		States:               states,
		DeadlineMilliseconds: deadline.Milliseconds(),
	})
	if err != nil {
		return Sandbox{}, fmt.Errorf("SecondBox Sandbox encode wait request: %w", err)
	}
	var sandbox Sandbox
	err = handle.client.RequestJSON(ctx, "waitForSandbox", CallOptions{
		PathParameters: map[string]string{"sandboxId": string(current.ID)},
		Body:           bytes.NewReader(body),
		ContentType:    "application/json",
	}, &sandbox)
	if err != nil {
		return Sandbox{}, err
	}
	handle.store(sandbox)
	return sandbox, nil
}

// Start requests that the caller-owned Sandbox become ready.
func (handle *SandboxHandle) Start(ctx context.Context, options LifecycleOptions) (Operation, error) {
	return handle.lifecycle(ctx, "startSandbox", options, nil)
}

// Drain rejects new Sandbox data-plane operations before stop or deletion.
func (handle *SandboxHandle) Drain(ctx context.Context, options LifecycleOptions) (Operation, error) {
	return handle.lifecycle(ctx, "drainSandbox", options, nil)
}

// Stop requests compute teardown while retaining the durable Sandbox.
func (handle *SandboxHandle) Stop(ctx context.Context, options LifecycleOptions) (Operation, error) {
	return handle.lifecycle(ctx, "stopSandbox", options, nil)
}

// Restore replaces the stopped Sandbox workspace with a writable Snapshot copy.
func (handle *SandboxHandle) Restore(
	ctx context.Context,
	options LifecycleOptions,
	snapshotID string,
) (Operation, error) {
	if snapshotID == "" {
		return Operation{}, errors.New("SecondBox Snapshot restore ID is required")
	}
	return handle.lifecycle(
		ctx, "restoreSandboxSnapshot", options, RestoreSnapshotRequest{SnapshotID: snapshotID},
	)
}

// Delete requests deletion; it is never called implicitly by this handle.
func (handle *SandboxHandle) Delete(ctx context.Context, options LifecycleOptions) (Operation, error) {
	return handle.lifecycle(ctx, "deleteSandbox", options, nil)
}

func (handle *SandboxHandle) lifecycle(
	ctx context.Context,
	operationID string,
	options LifecycleOptions,
	bodyValue any,
) (Operation, error) {
	if options.IdempotencyKey == "" {
		return Operation{}, fmt.Errorf("SecondBox %s idempotency key is required", operationID)
	}
	if options.IfMatch == "" {
		return Operation{}, fmt.Errorf("SecondBox %s If-Match value is required", operationID)
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", options.IdempotencyKey)
	headers.Set("If-Match", options.IfMatch)
	request := CallOptions{
		PathParameters: map[string]string{"sandboxId": string(handle.Snapshot().ID)},
		Headers:        headers,
	}
	if bodyValue != nil {
		body, err := json.Marshal(bodyValue)
		if err != nil {
			return Operation{}, fmt.Errorf("SecondBox %s encode request: %w", operationID, err)
		}
		request.Body = bytes.NewReader(body)
		request.ContentType = "application/json"
	}
	var operation Operation
	if err := handle.client.RequestJSON(ctx, operationID, request, &operation); err != nil {
		return Operation{}, err
	}
	if operation.Sandbox != nil {
		handle.store(*operation.Sandbox)
	}
	return operation, nil
}

func (handle *SandboxHandle) store(sandbox Sandbox) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	handle.snapshot = sandbox
}

// CreateSnapshot admits one asynchronous Snapshot clone.
func (handle *SandboxHandle) CreateSnapshot(
	ctx context.Context,
	options LifecycleOptions,
	request CreateSnapshotRequest,
) (Operation, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Operation{}, fmt.Errorf("SecondBox Snapshot encode request: %w", err)
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", options.IdempotencyKey)
	headers.Set("If-Match", options.IfMatch)
	var operation Operation
	err = handle.client.RequestJSON(ctx, "createSandboxSnapshot", CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID},
		Headers: headers, Body: bytes.NewReader(body), ContentType: "application/json",
	}, &operation)
	return operation, err
}

// DeleteSnapshot admits one asynchronous Snapshot deletion.
func (client *Client) DeleteSnapshot(
	ctx context.Context,
	snapshotID string,
	idempotencyKey string,
) (Operation, error) {
	headers := make(http.Header)
	headers.Set("Idempotency-Key", idempotencyKey)
	var operation Operation
	err := client.RequestJSON(ctx, "deleteSnapshot", CallOptions{
		PathParameters: map[string]string{"snapshotId": snapshotID}, Headers: headers,
	}, &operation)
	return operation, err
}

// GenerationHeaders binds a data-plane request to the handle's observed generation.
func (handle *SandboxHandle) GenerationHeaders(leaseID string) http.Header {
	headers := make(http.Header)
	headers.Set("SecondBox-Generation", strconv.FormatInt(handle.Snapshot().Generation, 10))
	if leaseID != "" {
		headers.Set("SecondBox-Lease-ID", leaseID)
	}
	return headers
}

// Execute runs one bounded buffered command against the observed Sandbox generation.
func (handle *SandboxHandle) Execute(
	ctx context.Context,
	request BufferedExecRequest,
	idempotencyKey string,
	leaseID string,
) (ExecOutcome, error) {
	var outcome ExecOutcome
	err := handle.dataPlaneJSON(
		ctx,
		"executeSandboxCommand",
		request,
		idempotencyKey,
		leaseID,
		&outcome,
	)
	return outcome, err
}

// CreateExecStream negotiates a streaming-exec WebSocket session.
//
// The generated transport deliberately leaves WebSocket ownership to the caller.
func (handle *SandboxHandle) CreateExecStream(
	ctx context.Context,
	request StreamingExecRequest,
	idempotencyKey string,
	leaseID string,
) (ExecStreamSession, error) {
	var session ExecStreamSession
	err := handle.dataPlaneJSON(
		ctx,
		"createSandboxExecStream",
		request,
		idempotencyKey,
		leaseID,
		&session,
	)
	return session, err
}

// CreateTerminal negotiates a terminal WebSocket session.
//
// The generated transport deliberately leaves WebSocket ownership to the caller.
func (handle *SandboxHandle) CreateTerminal(
	ctx context.Context,
	request CreateTerminalRequest,
	idempotencyKey string,
	leaseID string,
) (TerminalSession, error) {
	var session TerminalSession
	err := handle.dataPlaneJSON(
		ctx,
		"createSandboxTerminal",
		request,
		idempotencyKey,
		leaseID,
		&session,
	)
	return session, err
}

func (handle *SandboxHandle) dataPlaneJSON(
	ctx context.Context,
	operationID string,
	request any,
	idempotencyKey string,
	leaseID string,
	target any,
) error {
	if idempotencyKey == "" {
		return fmt.Errorf("SecondBox %s idempotency key is required", operationID)
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("SecondBox %s encode request: %w", operationID, err)
	}
	headers := handle.GenerationHeaders(leaseID)
	headers.Set("Idempotency-Key", idempotencyKey)
	return handle.client.RequestJSON(ctx, operationID, CallOptions{
		PathParameters: map[string]string{"sandboxId": string(handle.Snapshot().ID)},
		Headers:        headers,
		Body:           bytes.NewReader(body),
		ContentType:    "application/json",
	}, target)
}
