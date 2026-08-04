package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// runTestServer scripts the whole create, wait, execute, delete sequence and
// records every request the CLI made.
type runTestServer struct {
	server *httptest.Server

	mutex    sync.Mutex
	requests []string
	create   secondboxclient.CreateSandboxRequest
	exec     secondboxclient.BufferedExecRequest
	deleted  bool
	outcome  string
	state    string
}

func newRunTestServer(t *testing.T, outcomeJSON string) *runTestServer {
	t.Helper()
	recorder := &runTestServer{outcome: outcomeJSON, state: "ready"}
	recorder.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			recorder.mutex.Lock()
			defer recorder.mutex.Unlock()
			recorder.requests = append(recorder.requests, request.Method+" "+request.URL.Path)
			writer.Header().Set("Content-Type", "application/json")
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
				if err := json.NewDecoder(request.Body).Decode(&recorder.create); err != nil {
					t.Errorf("decode create request: %v", err)
				}
				_, _ = io.WriteString(writer, `{"id":"op_1","sandboxId":"sbx_run1","kind":"create",
					"state":"pending","requestId":"req_1",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
			case strings.HasSuffix(request.URL.Path, "/exec"):
				if err := json.NewDecoder(request.Body).Decode(&recorder.exec); err != nil {
					t.Errorf("decode exec request: %v", err)
				}
				_, _ = io.WriteString(writer, recorder.outcome)
			case request.Method == http.MethodDelete:
				recorder.deleted = true
				_, _ = io.WriteString(writer, `{"id":"op_2","sandboxId":"sbx_run1","kind":"delete",
					"state":"pending","requestId":"req_2",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
			default:
				_, _ = io.WriteString(writer, runSandboxJSON(recorder.state))
			}
		},
	))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func runSandboxJSON(state string) string {
	return fmt.Sprintf(`{
		"id":"sbx_run1","profile":"durable-coding","profileRevisionId":"prv_1",
		"state":%q,"desiredState":"running","generation":3,
		"workspace":{"id":"wsp_1","generation":3,"state":"ready","sizeBytes":1024,
			"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"},
		"metadata":{},"revision":5,
		"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`, state)
}

func (recorder *runTestServer) joinedRequests() string {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return strings.Join(recorder.requests, ", ")
}

func invokeRun(t *testing.T, recorder *runTestServer, args []string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runRunCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		args,
		execCommandEnvironment{
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
		sandboxShellEnvironment{},
	)
	return stdout.String(), stderr.String(), err
}

func TestRunCreatesWaitsExecutesAndDisposes(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "hello\n", ""))
	stdout, _, err := invokeRun(t, recorder, []string{
		"durable-coding", "--", "echo", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "hello\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if recorder.create.Profile != "durable-coding" {
		t.Errorf("create request = %+v", recorder.create)
	}
	argv := recorder.exec.Command.ArgvCommand
	if argv == nil || argv.Executable != "echo" {
		t.Errorf("exec command = %+v", recorder.exec.Command)
	}
	if !recorder.deleted {
		t.Error("run must delete the Sandbox it created")
	}
	joined := recorder.joinedRequests()
	if !strings.Contains(joined, "POST /v1/sandboxes") ||
		!strings.Contains(joined, "POST /v1/sandboxes/sbx_run1/exec") ||
		!strings.Contains(joined, "DELETE /v1/sandboxes/sbx_run1") {
		t.Errorf("requests = %s; want create, exec, then delete", joined)
	}
}

func TestRunWritesTheReservedNameMetadata(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
	if _, _, err := invokeRun(t, recorder, []string{
		"durable-coding", "--name", "my-box", "--metadata", "tier=gold", "--", "true",
	}); err != nil {
		t.Fatal(err)
	}
	if recorder.create.Metadata[contracts.SandboxNameMetadataKey] != "my-box" {
		t.Errorf("metadata = %#v; want the reserved name", recorder.create.Metadata)
	}
	if recorder.create.Metadata["tier"] != "gold" {
		t.Errorf("metadata = %#v; want the caller's own pairs kept", recorder.create.Metadata)
	}
}

func TestRunRejectsNameColludingWithExplicitMetadata(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
	_, _, err := invokeRun(t, recorder, []string{
		"durable-coding", "--name", "my-box",
		"--metadata", contracts.SandboxNameMetadataKey + "=other", "--", "true",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot combine --name") {
		t.Fatalf("error = %v; want a conflicting-name rejection", err)
	}
}

func TestRunKeepsTheSandboxWhenAsked(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
	_, stderr, err := invokeRun(t, recorder, []string{
		"durable-coding", "--keep", "--", "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorder.deleted {
		t.Error("--keep must retain the Sandbox")
	}
	if !strings.Contains(stderr, "sbx_run1") {
		t.Errorf("stderr = %q; want the retained Sandbox reported", stderr)
	}
}

// TestRunDisposesAfterAFailingCommand proves cleanup is not skipped when the
// guest command fails, which is exactly when a leak would be easiest to miss.
func TestRunDisposesAfterAFailingCommand(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(23, "", "boom\n"))
	_, stderr, err := invokeRun(t, recorder, []string{"durable-coding", "--", "false"})
	var exited *commandExitError
	if !errors.As(err, &exited) || exited.code != 23 {
		t.Fatalf("error = %v; want the guest's exit status", err)
	}
	if stderr != "boom\n" {
		t.Errorf("stderr = %q", stderr)
	}
	if !recorder.deleted {
		t.Error("a failing command must still dispose of the Sandbox")
	}
}

func TestRunSkipsDeletionOfAnAlreadyDeletedSandbox(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
	recorder.mutex.Lock()
	recorder.state = "ready"
	recorder.mutex.Unlock()
	if _, _, err := invokeRun(t, recorder, []string{"durable-coding", "--", "true"}); err != nil {
		t.Fatal(err)
	}
	// Now prove the reverse: a Sandbox already reported deleted is left alone.
	deletedRecorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
	deletedRecorder.server.Config.Handler = http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			deletedRecorder.mutex.Lock()
			defer deletedRecorder.mutex.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
				_, _ = io.WriteString(writer, `{"id":"op_1","sandboxId":"sbx_run1","kind":"create",
					"state":"pending","requestId":"req_1",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
			case strings.HasSuffix(request.URL.Path, "/exec"):
				_, _ = io.WriteString(writer, exitedOutcomeJSON(0, "", ""))
			case request.Method == http.MethodDelete:
				deletedRecorder.deleted = true
				writer.WriteHeader(http.StatusInternalServerError)
			case strings.HasSuffix(request.URL.Path, ":wait"):
				_, _ = io.WriteString(writer, runSandboxJSON("ready"))
			default:
				_, _ = io.WriteString(writer, runSandboxJSON("deleted"))
			}
		})
	if _, _, err := invokeRun(t, deletedRecorder, []string{
		"durable-coding", "--", "true",
	}); err != nil {
		t.Fatal(err)
	}
	deletedRecorder.mutex.Lock()
	defer deletedRecorder.mutex.Unlock()
	if deletedRecorder.deleted {
		t.Error("a Sandbox already reported deleted must not be deleted again")
	}
}

func TestRunRejectsMalformedInvocations(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		wantIn string
	}{
		{"no profile", nil, "requires a Profile"},
		{"option before profile", []string{"--keep", "p"}, "before any option"},
		{"no command", []string{"durable-coding"}, "requires a command after --"},
		{
			"non-positive ready timeout",
			[]string{"durable-coding", "--ready-timeout", "0s", "--", "true"},
			"--ready-timeout must be at least",
		},
		{
			"malformed metadata",
			[]string{"durable-coding", "--metadata", "novalue", "--", "true"},
			"run metadata",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
			_, _, err := invokeRun(t, recorder, test.args)
			if err == nil || !strings.Contains(err.Error(), test.wantIn) {
				t.Fatalf("error = %v; want one containing %q", err, test.wantIn)
			}
		})
	}
}

func TestRunOperationalCommandRoutesRun(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "routed\n", ""))
	var output bytes.Buffer
	handled, err := runOperationalCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"run", "durable-coding", "--", "true"},
		&output,
	)
	if !handled {
		t.Fatal("run must be handled as an operational command")
	}
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "routed\n" {
		t.Errorf("output = %q", output.String())
	}
}

func TestSuppliedFlagRecognisesEveryForm(t *testing.T) {
	tests := []struct {
		args []string
		name string
		want bool
	}{
		{[]string{"--lease", "l1"}, "lease", true},
		{[]string{"-lease", "l1"}, "lease", true},
		{[]string{"--lease=l1"}, "lease", true},
		{[]string{"-lease=l1"}, "lease", true},
		{[]string{"--command", "/bin/bash"}, "lease", false},
		{[]string{"--leased", "x"}, "lease", false},
		{nil, "lease", false},
	}
	for _, test := range tests {
		if got := suppliedFlag(test.args, test.name); got != test.want {
			t.Errorf("suppliedFlag(%v, %q) = %v; want %v", test.args, test.name, got, test.want)
		}
	}
}

func TestRunForwardsStandardInput(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
	var stdout, stderr bytes.Buffer
	err := runRunCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"durable-coding", "--stdin", "--", "cat"},
		execCommandEnvironment{
			stdin:  strings.NewReader("piped\n"),
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
		sandboxShellEnvironment{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.exec.StdinBase64 == nil ||
		*recorder.exec.StdinBase64 != base64.StdEncoding.EncodeToString([]byte("piped\n")) {
		t.Errorf("stdinBase64 = %v; want the piped bytes", recorder.exec.StdinBase64)
	}
}

// TestRunRefusesOversizedStdinBeforeCreating proves an input that cannot be sent
// fails before a Sandbox exists, rather than leaving one behind.
func TestRunRefusesOversizedStdinBeforeCreating(t *testing.T) {
	recorder := newRunTestServer(t, exitedOutcomeJSON(0, "", ""))
	var stdout, stderr bytes.Buffer
	err := runRunCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"durable-coding", "--stdin", "--", "cat"},
		execCommandEnvironment{
			stdin:  strings.NewReader(strings.Repeat("x", maximumExecStdinBytes+1)),
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
		sandboxShellEnvironment{},
	)
	if err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("error = %v; want an oversized-input rejection", err)
	}
	if recorder.joinedRequests() != "" {
		t.Errorf("requests = %s; want nothing created", recorder.joinedRequests())
	}
}

// TestRunRetriesDeleteWhenTheRevisionMoved reproduces the disposal failure seen
// against a live deployment: reconciliation advances the Sandbox revision, so
// the If-Match validator read a moment earlier is already stale.
func TestRunRetriesDeleteWhenTheRevisionMoved(t *testing.T) {
	var mutex sync.Mutex
	revision, deleteAttempts := 5, 0
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			mutex.Lock()
			defer mutex.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
				_, _ = io.WriteString(writer, `{"id":"op_1","sandboxId":"sbx_run1","kind":"create",
					"state":"pending","requestId":"req_1",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
			case strings.HasSuffix(request.URL.Path, "/exec"):
				_, _ = io.WriteString(writer, exitedOutcomeJSON(0, "", ""))
			case request.Method == http.MethodDelete:
				deleteAttempts++
				// The first two attempts lose the race, as a busy reconciler causes.
				if deleteAttempts <= 2 {
					writer.WriteHeader(http.StatusPreconditionFailed)
					_, _ = io.WriteString(writer,
						`{"code":"precondition_failed","title":"Resource revision changed"}`)
					return
				}
				_, _ = io.WriteString(writer, `{"id":"op_2","sandboxId":"sbx_run1","kind":"delete",
					"state":"pending","requestId":"req_2",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
			default:
				// Every read reports a Sandbox whose revision has moved on.
				revision++
				_, _ = io.WriteString(writer, strings.Replace(
					runSandboxJSON("ready"), `"revision":5`,
					fmt.Sprintf(`"revision":%d`, revision), 1))
			}
		}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	err := runRunCommand(
		context.Background(),
		execTestSession(server.URL),
		[]string{"durable-coding", "--", "true"},
		execCommandEnvironment{
			stdout: &stdout, stderr: &stderr, httpClient: server.Client(),
		},
		sandboxShellEnvironment{},
	)
	if err != nil {
		t.Fatalf("a stale validator must be retried, not reported: %v", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if deleteAttempts != 3 {
		t.Errorf("delete attempts = %d; want the two losses plus one success", deleteAttempts)
	}
}

// TestRunReportsADeleteFailureThatIsNotARace proves only the stale-validator
// case is retried; anything else is surfaced immediately.
func TestRunReportsADeleteFailureThatIsNotARace(t *testing.T) {
	var mutex sync.Mutex
	deleteAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			mutex.Lock()
			defer mutex.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
				_, _ = io.WriteString(writer, `{"id":"op_1","sandboxId":"sbx_run1","kind":"create",
					"state":"pending","requestId":"req_1",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
			case strings.HasSuffix(request.URL.Path, "/exec"):
				_, _ = io.WriteString(writer, exitedOutcomeJSON(0, "", ""))
			case request.Method == http.MethodDelete:
				deleteAttempts++
				writer.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(writer,
					`{"code":"state_conflict","title":"Sandbox cannot be deleted"}`)
			default:
				_, _ = io.WriteString(writer, runSandboxJSON("ready"))
			}
		}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	err := runRunCommand(
		context.Background(),
		execTestSession(server.URL),
		[]string{"durable-coding", "--", "true"},
		execCommandEnvironment{
			stdout: &stdout, stderr: &stderr, httpClient: server.Client(),
		},
		sandboxShellEnvironment{},
	)
	if err == nil || !strings.Contains(err.Error(), "state_conflict") {
		t.Fatalf("error = %v; want the delete failure surfaced", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if deleteAttempts != 1 {
		t.Errorf("delete attempts = %d; want no retry for a non-race failure", deleteAttempts)
	}
}
