package secondboxclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewIdempotencyKeyIsPrefixedAndUnique(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for range 64 {
		key, err := NewIdempotencyKey()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(key, "sbk-") {
			t.Fatalf("key = %q; want the sbk- prefix", key)
		}
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("key %q was generated twice", key)
		}
		seen[key] = struct{}{}
	}
}

func TestRevisionETagMatchesServiceFormat(t *testing.T) {
	if got := RevisionETag(7); got != `"revision-7"` {
		t.Fatalf("RevisionETag(7) = %s; want \"revision-7\"", got)
	}
}

func TestProblemCodeOfReadsTypedFailures(t *testing.T) {
	failure := &APIError{StatusCode: 409, Problem: &Problem{Code: ProblemCodeGenerationFenced}}
	if got := ProblemCodeOf(failure); got != ProblemCodeGenerationFenced {
		t.Errorf("ProblemCodeOf(typed) = %q; want %q", got, ProblemCodeGenerationFenced)
	}
	if got := ProblemCodeOf(errors.New("plain")); got != "" {
		t.Errorf("ProblemCodeOf(plain) = %q; want an empty code", got)
	}
	if got := ProblemCodeOf(&APIError{StatusCode: 500}); got != "" {
		t.Errorf("ProblemCodeOf(untyped) = %q; want an empty code", got)
	}
}

func execOutput(stdout, stderr string) ExecOutput {
	return ExecOutput{
		StdoutBase64: base64.StdEncoding.EncodeToString([]byte(stdout)),
		StderrBase64: base64.StdEncoding.EncodeToString([]byte(stderr)),
	}
}

func TestExecOutcomeErrorCoversEveryVariant(t *testing.T) {
	tests := []struct {
		name    string
		outcome ExecOutcome
		wantErr bool
		wantIn  string
	}{
		{
			name:    "exited zero",
			outcome: ExecOutcome{ExecExited: &ExecExited{ExitCode: 0}},
		},
		{
			name:    "exited non-zero",
			outcome: ExecOutcome{ExecExited: &ExecExited{ExitCode: 23}},
			wantErr: true, wantIn: "exited with status 23",
		},
		{
			name:    "cancelled",
			outcome: ExecOutcome{ExecCancelled: &ExecCancelled{}},
			wantErr: true, wantIn: "cancelled",
		},
		{
			name:    "deadline exceeded",
			outcome: ExecOutcome{ExecDeadlineExceeded: &ExecDeadlineExceeded{}},
			wantErr: true, wantIn: "deadline_exceeded",
		},
		{
			name:    "output exhausted",
			outcome: ExecOutcome{ExecOutputExhausted: &ExecOutputExhausted{LimitBytes: 16}},
			wantErr: true, wantIn: "16 bytes",
		},
		{
			name:    "spawn failed",
			outcome: ExecOutcome{ExecSpawnFailed: &ExecSpawnFailed{Reason: SpawnFailureKindNotFound, Message: "no such file"}},
			wantErr: true, wantIn: "not_found: no such file",
		},
		{
			name: "infrastructure failed",
			outcome: ExecOutcome{
				ExecInfrastructureFailed: &ExecInfrastructureFailed{Message: "runner lost"},
			},
			wantErr: true, wantIn: "runner lost",
		},
		{
			name: "no variant", outcome: ExecOutcome{},
			wantErr: true, wantIn: "invalid outcome",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ExecOutcomeError(test.outcome)
			if test.wantErr == (err == nil) {
				t.Fatalf("ExecOutcomeError = %v; wantErr = %v", err, test.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), test.wantIn) {
				t.Errorf("error = %q; want it to contain %q", err, test.wantIn)
			}
		})
	}
}

func TestExecOutcomeErrorPreservesSpawnFailureReason(t *testing.T) {
	err := ExecOutcomeError(ExecOutcome{ExecSpawnFailed: &ExecSpawnFailed{
		Reason: SpawnFailureKindNotFound, Message: "command could not be spawned",
	}})
	var failure *ExecFailure
	if !errors.As(err, &failure) {
		t.Fatalf("ExecOutcomeError = %T %v; want *ExecFailure", err, err)
	}
	if failure.SpawnFailureReason != SpawnFailureKindNotFound {
		t.Fatalf("spawn failure reason = %q", failure.SpawnFailureReason)
	}
}

func TestDecodeExecOutcomeReturnsOutputAlongsideFailure(t *testing.T) {
	result, err := DecodeExecOutcome(ExecOutcome{ExecExited: &ExecExited{
		ExitCode: 23, ElapsedMilliseconds: 42, Output: execOutput("out", "err"),
	}})
	if err == nil {
		t.Fatal("a non-zero exit must be reported as an error")
	}
	if string(result.Stdout) != "out" || string(result.Stderr) != "err" {
		t.Errorf("result = %+v; want the decoded streams", result)
	}
	if result.ExitCode != 23 || result.ElapsedMilliseconds != 42 {
		t.Errorf("result = %+v; want the reported status and elapsed time", result)
	}
	var failure *ExecFailure
	if !errors.As(err, &failure) || failure.Kind != "exited" {
		t.Errorf("error = %v; want a typed exited failure", err)
	}
}

func TestDecodeExecOutcomeSucceedsOnZeroExit(t *testing.T) {
	result, err := DecodeExecOutcome(ExecOutcome{ExecExited: &ExecExited{
		ExitCode: 0, Output: execOutput("hello\n", ""),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "hello\n" || len(result.Stderr) != 0 {
		t.Errorf("result = %+v; want only stdout", result)
	}
}

func TestDecodeExecOutcomeCarriesTruncatedOutput(t *testing.T) {
	result, err := DecodeExecOutcome(ExecOutcome{ExecOutputExhausted: &ExecOutputExhausted{
		LimitBytes: 4, Output: execOutput("abcd", ""),
	}})
	if err == nil {
		t.Fatal("an exhausted outcome must be reported as an error")
	}
	if string(result.Stdout) != "abcd" {
		t.Errorf("stdout = %q; want the bounded output", result.Stdout)
	}
	if result.ExitCode != -1 {
		t.Errorf("exit code = %d; want -1 for an outcome without a status", result.ExitCode)
	}
}

func TestDecodeExecOutcomeRejectsNonCanonicalBase64(t *testing.T) {
	_, err := DecodeExecOutcome(ExecOutcome{ExecExited: &ExecExited{
		ExitCode: 0, Output: ExecOutput{StdoutBase64: "not base64!"},
	}})
	if err == nil || !strings.Contains(err.Error(), "canonical base64") {
		t.Fatalf("error = %v; want a canonical base64 rejection", err)
	}
}

// newLifecycleClient builds a client whose every request runs through handler.
func newLifecycleClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewSecondBoxSubjectClient(
		server.URL, "token", "tenant-1", "subject-1", server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func readySandbox(t *testing.T, client *Client) *SandboxHandle {
	t.Helper()
	var sandbox Sandbox
	if err := json.Unmarshal([]byte(sandboxJSON("sandbox-1", "ready")), &sandbox); err != nil {
		t.Fatal(err)
	}
	return NewSandboxHandle(client, sandbox)
}

func TestDataPlaneGeneratesIdempotencyKeyWhenAbsent(t *testing.T) {
	var observedKey, observedGeneration string
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedKey = request.Header.Get("Idempotency-Key")
		observedGeneration = request.Header.Get("SecondBox-Generation")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"kind":"exited","exitCode":0,"elapsedMilliseconds":1,
			"output":{"stdoutBase64":"","stderrBase64":""}}`)
	})
	handle := readySandbox(t, client)
	if _, err := handle.Execute(context.Background(), BufferedExecRequest{
		Command:              Command{ShellCommand: &ShellCommand{Mode: "shell", Command: "true"}},
		Environment:          StringMap{},
		DeadlineMilliseconds: 1000,
		MaximumOutputBytes:   1024,
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(observedKey, "sbk-") {
		t.Errorf("Idempotency-Key = %q; want a generated key", observedKey)
	}
	if observedGeneration != "1" {
		t.Errorf("SecondBox-Generation = %q; want the observed generation", observedGeneration)
	}
}

func TestDataPlanePreservesSuppliedIdempotencyKey(t *testing.T) {
	var observedKey string
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedKey = request.Header.Get("Idempotency-Key")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"kind":"exited","exitCode":0,"elapsedMilliseconds":1,
			"output":{"stdoutBase64":"","stderrBase64":""}}`)
	})
	handle := readySandbox(t, client)
	if _, err := handle.Execute(context.Background(), BufferedExecRequest{
		Command:              Command{ShellCommand: &ShellCommand{Mode: "shell", Command: "true"}},
		Environment:          StringMap{},
		DeadlineMilliseconds: 1000,
		MaximumOutputBytes:   1024,
	}, "caller-key", ""); err != nil {
		t.Fatal(err)
	}
	if observedKey != "caller-key" {
		t.Errorf("Idempotency-Key = %q; want the caller's key", observedKey)
	}
}

func TestReadFileEnforcesExplicitBound(t *testing.T) {
	var observedPath, observedGeneration string
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedPath = request.URL.Query().Get("path")
		observedGeneration = request.Header.Get("SecondBox-Generation")
		_, _ = writer.Write([]byte{0, 1, 2, 3, 4, 5})
	})
	handle := readySandbox(t, client)
	content, err := handle.ReadFile(context.Background(), "bounded.bin", 6, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, []byte{0, 1, 2, 3, 4, 5}) {
		t.Fatalf("content = %v; want all bounded bytes", content)
	}
	if observedPath != "bounded.bin" || observedGeneration != "1" {
		t.Errorf("path = %q, generation = %q; want bounded.bin and 1", observedPath, observedGeneration)
	}
	if _, err := handle.ReadFile(context.Background(), "bounded.bin", 5, ""); err == nil ||
		!strings.Contains(err.Error(), "SecondBox file read exceeds 5 bytes") {
		t.Fatalf("error = %v; want the stable read-bound error", err)
	}
}

func TestLifecycleGeneratesIdempotencyKeyWhenAbsent(t *testing.T) {
	var observedKey, observedIfMatch string
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedKey = request.Header.Get("Idempotency-Key")
		observedIfMatch = request.Header.Get("If-Match")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"operation-1","sandboxId":"sandbox-1","kind":"stop",
			"state":"pending","requestId":"request-1",
			"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
	})
	handle := readySandbox(t, client)
	if _, err := handle.Stop(context.Background(), LifecycleOptions{
		IfMatch: RevisionETag(1),
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(observedKey, "sbk-") {
		t.Errorf("Idempotency-Key = %q; want a generated key", observedKey)
	}
	if observedIfMatch != `"revision-1"` {
		t.Errorf("If-Match = %q; want the supplied validator", observedIfMatch)
	}
}

func TestLifecycleUsesObservedRevisionWhenIfMatchIsOmitted(t *testing.T) {
	var observedIfMatch string
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedIfMatch = request.Header.Get("If-Match")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"operation-1","sandboxId":"sandbox-1","kind":"stop",
			"state":"pending","requestId":"request-1",
			"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
	})
	handle := readySandbox(t, client)
	if _, err := handle.Stop(context.Background(), LifecycleOptions{}); err != nil {
		t.Fatal(err)
	}
	if observedIfMatch != `"revision-1"` {
		t.Fatalf("If-Match = %q; want the handle's observed revision", observedIfMatch)
	}
}

func leaseJSON(id string, expiresAt time.Time) string {
	value := map[string]any{
		"id": id, "sandboxId": "sandbox-1", "generation": 1, "state": "active",
		"expiresAt": expiresAt.UTC().Format(time.RFC3339Nano),
		"createdAt": "2026-07-28T00:00:00Z", "updatedAt": "2026-07-28T00:00:00Z",
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestAcquireLeaseSendsGenerationAndDuration(t *testing.T) {
	var observedGeneration, observedKey string
	var observedBody AcquireLeaseRequest
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedGeneration = request.Header.Get("SecondBox-Generation")
		observedKey = request.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(request.Body).Decode(&observedBody); err != nil {
			t.Errorf("decode acquire body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, leaseJSON("lease-1", time.Now().Add(time.Minute)))
	})
	handle := readySandbox(t, client)
	lease, err := handle.AcquireLease(context.Background(), 90*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != "lease-1" || lease.State != LeaseStateActive {
		t.Errorf("lease = %+v; want the granted active Lease", lease)
	}
	if observedBody.DurationSeconds != 90 {
		t.Errorf("durationSeconds = %d; want 90", observedBody.DurationSeconds)
	}
	if observedGeneration != "1" || !strings.HasPrefix(observedKey, "sbk-") {
		t.Errorf("generation = %q, key = %q", observedGeneration, observedKey)
	}
}

func TestAcquireLeaseRejectsOutOfRangeDuration(t *testing.T) {
	client := newLifecycleClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an out-of-range duration must not reach the service")
	})
	handle := readySandbox(t, client)
	for _, duration := range []time.Duration{0, 25 * time.Hour} {
		if _, err := handle.AcquireLease(context.Background(), duration, ""); err == nil {
			t.Errorf("AcquireLease(%v) must be rejected", duration)
		}
	}
}

func TestReleaseLeaseIssuesDelete(t *testing.T) {
	var observedMethod, observedPath, observedKey string
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedMethod, observedPath = request.Method, request.URL.Path
		observedKey = request.Header.Get("Idempotency-Key")
		writer.WriteHeader(http.StatusNoContent)
	})
	if err := client.ReleaseLease(context.Background(), "lease-1", ""); err != nil {
		t.Fatal(err)
	}
	if observedMethod != http.MethodDelete || observedPath != "/v1/leases/lease-1" {
		t.Errorf("%s %s; want DELETE /v1/leases/lease-1", observedMethod, observedPath)
	}
	// The route requires the key; omitting it is a 400 the stub would not show.
	if !strings.HasPrefix(observedKey, "sbk-") {
		t.Errorf("Idempotency-Key = %q; want a generated key", observedKey)
	}
	if err := client.ReleaseLease(context.Background(), "lease-1", "caller-key"); err != nil {
		t.Fatal(err)
	}
	if observedKey != "caller-key" {
		t.Errorf("Idempotency-Key = %q; want the caller's key", observedKey)
	}
	if err := client.ReleaseLease(context.Background(), "", ""); err == nil {
		t.Error("releasing without a Lease ID must be rejected")
	}
}

// TestRenewLeaseSendsItsRequiredIdempotencyKey guards the contract requirement
// that a stub answering every request would otherwise hide.
func TestRenewLeaseSendsItsRequiredIdempotencyKey(t *testing.T) {
	var observedKey string
	var observedBody RenewLeaseRequest
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		observedKey = request.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(request.Body).Decode(&observedBody); err != nil {
			t.Errorf("decode renew body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, leaseJSON("lease-1", time.Now().Add(time.Minute)))
	})
	if _, err := client.RenewLease(context.Background(), "lease-1", 90*time.Second, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(observedKey, "sbk-") {
		t.Errorf("Idempotency-Key = %q; want a generated key", observedKey)
	}
	if observedBody.DurationSeconds != 90 {
		t.Errorf("durationSeconds = %d; want 90", observedBody.DurationSeconds)
	}
	if _, err := client.RenewLease(context.Background(), "", time.Minute, ""); err == nil {
		t.Error("renewing without a Lease ID must be rejected")
	}
}

func TestKeepLeaseRenewsUntilClosedThenReleases(t *testing.T) {
	var mutex sync.Mutex
	var renewals, releases int
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if request.Header.Get("Idempotency-Key") == "" {
			t.Errorf("%s %s carried no Idempotency-Key", request.Method, request.URL.Path)
		}
		switch {
		case request.Method == http.MethodDelete:
			releases++
			writer.WriteHeader(http.StatusNoContent)
			return
		case strings.HasSuffix(request.URL.Path, ":renew"):
			renewals++
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, leaseJSON("lease-1", time.Now().Add(20*time.Millisecond)))
	})
	handle := readySandbox(t, client)
	lease, err := handle.AcquireLease(context.Background(), time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	keeper := newLeaseKeeper(client, lease, time.Minute, 5*time.Millisecond)
	keeper.start()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mutex.Lock()
		observed := renewals
		mutex.Unlock()
		if observed >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := keeper.Close(); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if renewals < 2 {
		t.Errorf("renewals = %d; want the keeper to renew repeatedly", renewals)
	}
	if releases != 1 {
		t.Errorf("releases = %d; want exactly one release on Close", releases)
	}
	if err := keeper.Err(); err != nil {
		t.Errorf("keeper reported a renewal failure: %v", err)
	}
}

func TestKeepLeaseRecordsRenewalFailure(t *testing.T) {
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, ":renew") {
			writer.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(writer, `{"code":"lease_fenced","title":"Lease is fenced"}`)
			return
		}
		if request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, leaseJSON("lease-1", time.Now().Add(10*time.Millisecond)))
	})
	handle := readySandbox(t, client)
	lease, err := handle.AcquireLease(context.Background(), time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	keeper := newLeaseKeeper(client, lease, time.Minute, 5*time.Millisecond)
	keeper.start()
	<-keeper.done
	if code := ProblemCodeOf(keeper.Err()); code != ProblemCodeLeaseFenced {
		t.Fatalf("keeper failure code = %q; want %q", code, ProblemCodeLeaseFenced)
	}
	// Close reports the renewal failure rather than the release error it causes,
	// so a caller is pointed at the cause and can still read the typed code.
	err = keeper.Close()
	if err == nil || !strings.Contains(err.Error(), "Lease renewal stopped") {
		t.Fatalf("Close() = %v; want the renewal failure surfaced", err)
	}
	if code := ProblemCodeOf(err); code != ProblemCodeLeaseFenced {
		t.Errorf("Close() problem code = %q; want %q", code, ProblemCodeLeaseFenced)
	}
}

// TestKeepLeaseCloseReportsReleaseFailureWhenRenewalHeld proves the release
// error still surfaces when renewal itself never failed.
func TestKeepLeaseCloseReportsReleaseFailureWhenRenewalHeld(t *testing.T) {
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `{"code":"internal_error","title":"Release failed"}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, leaseJSON("lease-1", time.Now().Add(time.Hour)))
	})
	handle := readySandbox(t, client)
	lease, err := handle.AcquireLease(context.Background(), time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	keeper := newLeaseKeeper(client, lease, time.Minute, time.Hour)
	keeper.start()
	if err := keeper.Close(); err == nil ||
		ProblemCodeOf(err) != "internal_error" {
		t.Fatalf("Close() = %v; want the release failure", err)
	}
	if keeper.Err() != nil {
		t.Errorf("keeper renewal failure = %v; want none", keeper.Err())
	}
}

func TestWaitForRetriesAfterWaitExpiry(t *testing.T) {
	var mutex sync.Mutex
	waits := 0
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if strings.HasSuffix(request.URL.Path, ":wait") {
			waits++
			if waits == 1 {
				writer.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(writer, `{"code":"wait_expired","title":"Wait expired"}`)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, sandboxJSON("sandbox-1", "ready"))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, sandboxJSON("sandbox-1", "starting"))
	})
	var starting Sandbox
	if err := json.Unmarshal([]byte(sandboxJSON("sandbox-1", "starting")), &starting); err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, starting)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sandbox, err := handle.WaitFor(ctx, SandboxStateReady)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.State != SandboxStateReady {
		t.Errorf("state = %q; want ready", sandbox.State)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if waits < 2 {
		t.Errorf("wait requests = %d; want a retry after the expiry", waits)
	}
}

func TestWaitForReturnsImmediatelyWhenAlreadyInState(t *testing.T) {
	client := newLifecycleClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an already-satisfied wait must not reach the service")
	})
	handle := readySandbox(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := handle.WaitFor(ctx, SandboxStateReady); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForRequiresDeadlineAndStates(t *testing.T) {
	client := newLifecycleClient(t, func(http.ResponseWriter, *http.Request) {})
	handle := readySandbox(t, client)
	if _, err := handle.WaitFor(context.Background()); err == nil {
		t.Error("WaitFor without states must be rejected")
	}
	var starting Sandbox
	if err := json.Unmarshal([]byte(sandboxJSON("sandbox-1", "starting")), &starting); err != nil {
		t.Fatal(err)
	}
	starting.State = SandboxStateStarting
	deadlineless := NewSandboxHandle(client, starting)
	if _, err := deadlineless.WaitFor(context.Background(), SandboxStateReady); err == nil ||
		!strings.Contains(err.Error(), "context deadline") {
		t.Errorf("WaitFor without a context deadline must be rejected, got %v", err)
	}
}

func TestCreateSandboxReturnsHandleForTheCreatedResource(t *testing.T) {
	var observedKey string
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			observedKey = request.Header.Get("Idempotency-Key")
			_, _ = io.WriteString(writer, `{"id":"operation-1","sandboxId":"sandbox-1",
				"kind":"create","state":"pending","requestId":"request-1",
				"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
			return
		}
		_, _ = io.WriteString(writer, sandboxJSON("sandbox-1", "creating"))
	})
	handle, operation, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Profile: "durable-coding", Metadata: Metadata{},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if operation.SandboxID != "sandbox-1" || handle.Snapshot().ID != "sandbox-1" {
		t.Errorf("operation = %+v, snapshot = %+v", operation, handle.Snapshot())
	}
	if !strings.HasPrefix(observedKey, "sbk-") {
		t.Errorf("Idempotency-Key = %q; want a generated key", observedKey)
	}
}

func TestCreateSandboxRejectsOperationWithoutSandboxReference(t *testing.T) {
	client := newLifecycleClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"operation-1","sandboxId":"","kind":"create",
			"state":"pending","requestId":"request-1",
			"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
	})
	_, _, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Profile: "durable-coding", Metadata: Metadata{},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "no Sandbox reference") {
		t.Fatalf("error = %v; want a missing-reference rejection", err)
	}
}

func TestRunCreatesWaitsAndExecutes(t *testing.T) {
	var mutex sync.Mutex
	var paths []string
	var execRequest BufferedExecRequest
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		paths = append(paths, request.Method+" "+request.URL.Path)
		mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/v1/sandboxes" && request.Method == http.MethodPost:
			_, _ = io.WriteString(writer, `{"id":"operation-1","sandboxId":"sandbox-1",
				"kind":"create","state":"pending","requestId":"request-1",
				"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
		case strings.HasSuffix(request.URL.Path, "/exec"):
			if err := json.NewDecoder(request.Body).Decode(&execRequest); err != nil {
				t.Errorf("decode exec request: %v", err)
				return
			}
			_, _ = io.WriteString(writer, `{"kind":"exited","exitCode":0,"elapsedMilliseconds":7,
				"output":{"stdoutBase64":"`+
				base64.StdEncoding.EncodeToString([]byte("hello from a sandbox\n"))+
				`","stderrBase64":""}}`)
		default:
			_, _ = io.WriteString(writer, sandboxJSON("sandbox-1", "ready"))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdinBase64 := base64.StdEncoding.EncodeToString([]byte("input\n"))
	handle, result, err := client.Run(ctx, RunRequest{
		Profile: "durable-coding",
		Command: Command{ArgvCommand: &ArgvCommand{
			Mode: "argv", Executable: "echo", Arguments: []string{"hello"},
		}},
		StdinBase64:          &stdinBase64,
		DeadlineMilliseconds: 5000,
		MaximumOutputBytes:   1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if handle == nil || handle.Snapshot().ID != "sandbox-1" {
		t.Fatalf("handle = %+v; want the created Sandbox", handle)
	}
	if string(result.Result.Stdout) != "hello from a sandbox\n" {
		t.Errorf("stdout = %q", result.Result.Stdout)
	}
	if result.Result.ExitCode != 0 || result.Sandbox.State != SandboxStateReady {
		t.Errorf("result = %+v", result)
	}
	if execRequest.Command.ArgvCommand == nil ||
		execRequest.Command.ArgvCommand.Executable != "echo" ||
		execRequest.StdinBase64 == nil ||
		*execRequest.StdinBase64 != base64.StdEncoding.EncodeToString([]byte("input\n")) {
		t.Errorf("exec request = %+v; want argv command and stdin", execRequest)
	}
	mutex.Lock()
	defer mutex.Unlock()
	joined := strings.Join(paths, ", ")
	if !strings.Contains(joined, "POST /v1/sandboxes") ||
		!strings.Contains(joined, "POST /v1/sandboxes/sandbox-1/exec") {
		t.Errorf("requests = %s; want create then exec", joined)
	}
}

// TestRunReportsCommandFailureWithOutput proves a failing command still yields
// its output, so a caller can report why it failed.
func TestRunReportsCommandFailureWithOutput(t *testing.T) {
	client := newLifecycleClient(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/v1/sandboxes" && request.Method == http.MethodPost:
			_, _ = io.WriteString(writer, `{"id":"operation-1","sandboxId":"sandbox-1",
				"kind":"create","state":"pending","requestId":"request-1",
				"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
		case strings.HasSuffix(request.URL.Path, "/exec"):
			_, _ = io.WriteString(writer, `{"kind":"exited","exitCode":23,"elapsedMilliseconds":7,
				"output":{"stdoutBase64":"","stderrBase64":"`+
				base64.StdEncoding.EncodeToString([]byte("boom\n"))+`"}}`)
		default:
			_, _ = io.WriteString(writer, sandboxJSON("sandbox-1", "ready"))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, result, err := client.Run(ctx, RunRequest{
		Profile:              "durable-coding",
		Command:              Command{ShellCommand: &ShellCommand{Mode: "shell", Command: "exit 23"}},
		DeadlineMilliseconds: 5000,
		MaximumOutputBytes:   1 << 20,
	})
	if err == nil {
		t.Fatal("a non-zero exit must be reported")
	}
	if string(result.Result.Stderr) != "boom\n" || result.Result.ExitCode != 23 {
		t.Errorf("result = %+v; want the failing command's output and status", result.Result)
	}
}

func TestRunRejectsIncompleteRequests(t *testing.T) {
	client := newLifecycleClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an incomplete run must not reach the service")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tests := []struct {
		name    string
		request RunRequest
	}{
		{"no profile", RunRequest{DeadlineMilliseconds: 1, MaximumOutputBytes: 1}},
		{"no deadline", RunRequest{Profile: "p", MaximumOutputBytes: 1}},
		{"no output bound", RunRequest{Profile: "p", DeadlineMilliseconds: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := client.Run(ctx, test.request); err == nil {
				t.Error("incomplete run must be rejected")
			}
		})
	}
	if _, _, err := client.Run(context.Background(), RunRequest{
		Profile: "p", DeadlineMilliseconds: 1, MaximumOutputBytes: 1,
	}); err == nil || !strings.Contains(err.Error(), "context deadline") {
		t.Errorf("run without a context deadline must be rejected, got %v", err)
	}
}
