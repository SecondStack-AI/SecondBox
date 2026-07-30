package secondboxclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maximumWaitRequest is the per-request bound the service enforces on waitForSandbox.
const maximumWaitRequest = 55 * time.Second

// NewIdempotencyKey returns one unguessable single-use request key.
//
// Callers that must replay a request across process restarts should supply
// their own durable key instead; a generated key lives only for one call.
func NewIdempotencyKey() (string, error) {
	buffer := make([]byte, 20)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("SecondBox generate idempotency key: %w", err)
	}
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	return "sbk-" + strings.ToLower(encoding.EncodeToString(buffer)), nil
}

// RevisionETag renders one Sandbox revision as its If-Match validator.
func RevisionETag(revision int64) string {
	return fmt.Sprintf("%q", fmt.Sprintf("revision-%d", revision))
}

// ProblemCodeOf returns the typed service problem code carried by err, or "".
func ProblemCodeOf(err error) string {
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.Problem != nil {
		return apiError.Problem.Code
	}
	return ""
}

// ExecResult is one successfully exited command and its decoded output.
type ExecResult struct {
	Stdout              []byte
	Stderr              []byte
	ExitCode            int
	Signal              *int
	ElapsedMilliseconds int64
}

// ExecFailure is one terminal ExecOutcome that did not reach an exit status.
type ExecFailure struct {
	Kind    string
	Message string
	Output  ExecOutput
}

func (failure *ExecFailure) Error() string {
	if failure.Message == "" {
		return "SecondBox command " + failure.Kind
	}
	return "SecondBox command " + failure.Kind + ": " + failure.Message
}

// ExecOutcomeError maps one terminal ExecOutcome to an error, or nil when the
// command exited with status zero. It is the single interpretation of the
// outcome union; buffered, streaming, and terminal callers all share it.
func ExecOutcomeError(outcome ExecOutcome) error {
	switch {
	case outcome.ExecExited != nil && outcome.ExecExited.ExitCode == 0:
		return nil
	case outcome.ExecExited != nil:
		return &ExecFailure{
			Kind: "exited",
			Message: fmt.Sprintf(
				"exited with status %d", outcome.ExecExited.ExitCode,
			),
			Output: outcome.ExecExited.Output,
		}
	case outcome.ExecCancelled != nil:
		return &ExecFailure{Kind: "cancelled", Output: outcome.ExecCancelled.Output}
	case outcome.ExecDeadlineExceeded != nil:
		return &ExecFailure{
			Kind:   "deadline_exceeded",
			Output: outcome.ExecDeadlineExceeded.Output,
		}
	case outcome.ExecOutputExhausted != nil:
		return &ExecFailure{
			Kind: "output_exhausted",
			Message: fmt.Sprintf(
				"output limit of %d bytes was exhausted", outcome.ExecOutputExhausted.LimitBytes,
			),
			Output: outcome.ExecOutputExhausted.Output,
		}
	case outcome.ExecSpawnFailed != nil:
		return &ExecFailure{
			Kind:    "spawn_failed",
			Message: outcome.ExecSpawnFailed.Message,
		}
	case outcome.ExecInfrastructureFailed != nil:
		return &ExecFailure{
			Kind:    "infrastructure_failed",
			Message: outcome.ExecInfrastructureFailed.Message,
		}
	default:
		return errors.New("SecondBox command returned an invalid outcome")
	}
}

// DecodeExecOutcome decodes the output any terminal outcome carries.
//
// A non-zero exit status and every non-exited outcome are reported through the
// returned error while the decoded output is still returned, because a command
// that failed usually explains itself on standard error.
func DecodeExecOutcome(outcome ExecOutcome) (ExecResult, error) {
	var output ExecOutput
	result := ExecResult{ExitCode: -1}
	switch {
	case outcome.ExecExited != nil:
		output = outcome.ExecExited.Output
		result.ExitCode = outcome.ExecExited.ExitCode
		result.Signal = outcome.ExecExited.Signal
		result.ElapsedMilliseconds = outcome.ExecExited.ElapsedMilliseconds
	case outcome.ExecCancelled != nil:
		output = outcome.ExecCancelled.Output
	case outcome.ExecDeadlineExceeded != nil:
		output = outcome.ExecDeadlineExceeded.Output
		result.ElapsedMilliseconds = outcome.ExecDeadlineExceeded.ElapsedMilliseconds
	case outcome.ExecOutputExhausted != nil:
		output = outcome.ExecOutputExhausted.Output
	}
	stdout, err := decodeExecStream(output.StdoutBase64, "stdout")
	if err != nil {
		return ExecResult{}, err
	}
	stderr, err := decodeExecStream(output.StderrBase64, "stderr")
	if err != nil {
		return ExecResult{}, err
	}
	result.Stdout, result.Stderr = stdout, stderr
	return result, ExecOutcomeError(outcome)
}

func decodeExecStream(encoded string, name string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	content, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("SecondBox command %s is not canonical base64", name)
	}
	return content, nil
}

// WaitFor blocks until the Sandbox reaches one of the supplied states.
//
// The service bounds a single wait request, so this issues repeated bounded
// waits against the caller's context deadline and reports the last observed
// state when that deadline passes.
func (handle *SandboxHandle) WaitFor(
	ctx context.Context,
	states ...SandboxState,
) (Sandbox, error) {
	if len(states) == 0 {
		return Sandbox{}, errors.New("SecondBox Sandbox wait requires at least one state")
	}
	target := make(map[SandboxState]struct{}, len(states))
	for _, state := range states {
		target[state] = struct{}{}
	}
	last := handle.Snapshot()
	for {
		if _, found := target[last.State]; found {
			return last, nil
		}
		deadline, found := ctx.Deadline()
		if !found {
			return last, errors.New("SecondBox Sandbox wait requires a context deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return last, fmt.Errorf(
				"SecondBox Sandbox %s did not reach %v: last state=%s generation=%d: %w",
				last.ID, states, last.State, last.Generation, context.DeadlineExceeded,
			)
		}
		observed, err := handle.Wait(ctx, states, min(remaining, maximumWaitRequest))
		if err == nil {
			last = observed
			continue
		}
		if ProblemCodeOf(err) != ProblemCodeWaitExpired {
			return last, fmt.Errorf(
				"SecondBox Sandbox %s wait failed in state %s: %w", last.ID, last.State, err,
			)
		}
		refreshed, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			return last, fmt.Errorf(
				"SecondBox Sandbox %s refresh after wait expiry: %w", last.ID, refreshErr,
			)
		}
		last = refreshed
	}
}

// CreateSandbox admits one Sandbox and returns a handle to its representation.
//
// The returned Sandbox is the freshly created resource, which is not yet ready;
// callers wait for the states they require.
func (client *Client) CreateSandbox(
	ctx context.Context,
	request CreateSandboxRequest,
	idempotencyKey string,
) (*SandboxHandle, Operation, error) {
	if idempotencyKey == "" {
		generated, err := NewIdempotencyKey()
		if err != nil {
			return nil, Operation{}, err
		}
		idempotencyKey = generated
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, Operation{}, fmt.Errorf("SecondBox Sandbox encode create request: %w", err)
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", idempotencyKey)
	var operation Operation
	err = client.RequestJSON(ctx, "createSandbox", CallOptions{
		Headers: headers, Body: bytes.NewReader(body), ContentType: "application/json",
	}, &operation)
	if err != nil {
		return nil, Operation{}, err
	}
	if operation.SandboxID == "" {
		return nil, operation, errors.New("SecondBox Sandbox create returned no Sandbox reference")
	}
	var sandbox Sandbox
	err = client.RequestJSON(ctx, "getSandbox", CallOptions{
		PathParameters: map[string]string{"sandboxId": operation.SandboxID},
	}, &sandbox)
	if err != nil {
		return nil, operation, err
	}
	return NewSandboxHandle(client, sandbox), operation, nil
}

// AcquireLease obtains bounded exclusive authority over the observed generation.
func (handle *SandboxHandle) AcquireLease(
	ctx context.Context,
	duration time.Duration,
	idempotencyKey string,
) (Lease, error) {
	seconds := int64(duration / time.Second)
	if seconds < 1 || seconds > 86400 {
		return Lease{}, errors.New("SecondBox Lease duration must be from 1 second through 24 hours")
	}
	if idempotencyKey == "" {
		generated, err := NewIdempotencyKey()
		if err != nil {
			return Lease{}, err
		}
		idempotencyKey = generated
	}
	body, err := json.Marshal(AcquireLeaseRequest{DurationSeconds: seconds})
	if err != nil {
		return Lease{}, fmt.Errorf("SecondBox Lease encode acquire request: %w", err)
	}
	headers := handle.GenerationHeaders("")
	headers.Set("Idempotency-Key", idempotencyKey)
	var lease Lease
	err = handle.client.RequestJSON(ctx, "acquireSandboxLease", CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID},
		Headers:        headers,
		Body:           bytes.NewReader(body),
		ContentType:    "application/json",
	}, &lease)
	return lease, err
}

// RenewLease extends active Lease authority by a new bounded duration.
func (client *Client) RenewLease(
	ctx context.Context,
	leaseID string,
	duration time.Duration,
) (Lease, error) {
	seconds := int64(duration / time.Second)
	if leaseID == "" {
		return Lease{}, errors.New("SecondBox Lease renewal requires a Lease ID")
	}
	if seconds < 1 || seconds > 86400 {
		return Lease{}, errors.New("SecondBox Lease duration must be from 1 second through 24 hours")
	}
	body, err := json.Marshal(RenewLeaseRequest{DurationSeconds: seconds})
	if err != nil {
		return Lease{}, fmt.Errorf("SecondBox Lease encode renew request: %w", err)
	}
	var lease Lease
	err = client.RequestJSON(ctx, "renewSandboxLease", CallOptions{
		PathParameters: map[string]string{"leaseId": leaseID},
		Body:           bytes.NewReader(body),
		ContentType:    "application/json",
	}, &lease)
	return lease, err
}

// ReleaseLease surrenders Lease authority before its expiry.
func (client *Client) ReleaseLease(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return errors.New("SecondBox Lease release requires a Lease ID")
	}
	response, err := client.Request(ctx, "releaseSandboxLease", CallOptions{
		PathParameters: map[string]string{"leaseId": leaseID},
	})
	if err != nil {
		return err
	}
	return response.Body.Close()
}

// defaultMinimumRenewalDelay keeps a very short Lease from busy-looping.
const defaultMinimumRenewalDelay = time.Second

// LeaseKeeper holds one Lease active by renewing it before it expires.
type LeaseKeeper struct {
	client       *Client
	lease        Lease
	duration     time.Duration
	minimumDelay time.Duration
	stop         chan struct{}
	done         chan struct{}
	once         sync.Once
	mu           sync.Mutex
	failure      error
}

func newLeaseKeeper(
	client *Client,
	lease Lease,
	duration time.Duration,
	minimumDelay time.Duration,
) *LeaseKeeper {
	return &LeaseKeeper{
		client: client, lease: lease, duration: duration, minimumDelay: minimumDelay,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

func (keeper *LeaseKeeper) start() {
	go keeper.renew()
}

// KeepLease acquires a Lease and renews it until the keeper is closed.
//
// Renewal is driven by the expiry the service actually granted rather than by
// the requested duration, because the pinned Profile bounds Lease length.
func (handle *SandboxHandle) KeepLease(
	ctx context.Context,
	duration time.Duration,
) (*LeaseKeeper, error) {
	lease, err := handle.AcquireLease(ctx, duration, "")
	if err != nil {
		return nil, err
	}
	keeper := newLeaseKeeper(handle.client, lease, duration, defaultMinimumRenewalDelay)
	keeper.start()
	return keeper, nil
}

// ID returns the held Lease identifier.
func (keeper *LeaseKeeper) ID() string {
	keeper.mu.Lock()
	defer keeper.mu.Unlock()
	return keeper.lease.ID
}

// Err reports the renewal failure that ended background renewal, if any.
func (keeper *LeaseKeeper) Err() error {
	keeper.mu.Lock()
	defer keeper.mu.Unlock()
	return keeper.failure
}

func (keeper *LeaseKeeper) renew() {
	defer close(keeper.done)
	for {
		wait := keeper.renewalDelay()
		timer := time.NewTimer(wait)
		select {
		case <-keeper.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		lease, err := keeper.client.RenewLease(ctx, keeper.ID(), keeper.duration)
		cancel()
		keeper.mu.Lock()
		if err != nil {
			keeper.failure = err
			keeper.mu.Unlock()
			return
		}
		keeper.lease = lease
		keeper.mu.Unlock()
	}
}

// renewalDelay renews at half the remaining life, with a floor that keeps a
// very short Lease from busy-looping.
func (keeper *LeaseKeeper) renewalDelay() time.Duration {
	keeper.mu.Lock()
	expiresAt := keeper.lease.ExpiresAt
	keeper.mu.Unlock()
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return keeper.minimumDelay
	}
	return max(remaining/2, keeper.minimumDelay)
}

// Close stops renewal and releases the Lease.
func (keeper *LeaseKeeper) Close() error {
	var releaseErr error
	keeper.once.Do(func() {
		close(keeper.stop)
		<-keeper.done
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		releaseErr = keeper.client.ReleaseLease(ctx, keeper.ID())
	})
	return releaseErr
}

// RunRequest is one create-then-execute request against a fresh Sandbox.
type RunRequest struct {
	Profile              ProfileName
	Metadata             Metadata
	SourceSnapshotID     string
	Command              Command
	Cwd                  *WorkspacePath
	Environment          StringMap
	StdinBase64          *string
	DeadlineMilliseconds int64
	MaximumOutputBytes   int64
}

// RunResult carries the Sandbox that ran the command and its decoded output.
type RunResult struct {
	Sandbox Sandbox
	Outcome ExecOutcome
	Result  ExecResult
}

// Run creates a Sandbox, waits for it to become ready, and executes one command.
//
// The Sandbox is deliberately left in place: this handle never deletes a
// Sandbox implicitly. Callers dispose of the returned handle themselves.
// The caller's context deadline bounds the wait for readiness.
func (client *Client) Run(
	ctx context.Context,
	request RunRequest,
) (*SandboxHandle, RunResult, error) {
	if request.Profile == "" {
		return nil, RunResult{}, errors.New("SecondBox run requires a Profile name")
	}
	if request.DeadlineMilliseconds < 1 || request.MaximumOutputBytes < 1 {
		return nil, RunResult{}, errors.New(
			"SecondBox run requires a positive deadline and output bound",
		)
	}
	if _, found := ctx.Deadline(); !found {
		return nil, RunResult{}, errors.New("SecondBox run requires a context deadline")
	}
	metadata := request.Metadata
	if metadata == nil {
		metadata = Metadata{}
	}
	handle, _, err := client.CreateSandbox(ctx, CreateSandboxRequest{
		Profile:          request.Profile,
		Metadata:         metadata,
		SourceSnapshotID: request.SourceSnapshotID,
	}, "")
	if err != nil {
		return nil, RunResult{}, err
	}
	sandbox, err := handle.WaitFor(ctx, SandboxStateReady)
	if err != nil {
		return handle, RunResult{}, err
	}
	environment := request.Environment
	if environment == nil {
		environment = StringMap{}
	}
	outcome, err := handle.Execute(ctx, BufferedExecRequest{
		Command:              request.Command,
		Cwd:                  request.Cwd,
		Environment:          environment,
		StdinBase64:          request.StdinBase64,
		DeadlineMilliseconds: request.DeadlineMilliseconds,
		MaximumOutputBytes:   request.MaximumOutputBytes,
	}, "", "")
	if err != nil {
		return handle, RunResult{Sandbox: sandbox}, err
	}
	result, outcomeErr := DecodeExecOutcome(outcome)
	return handle, RunResult{
		Sandbox: sandbox, Outcome: outcome, Result: result,
	}, outcomeErr
}
