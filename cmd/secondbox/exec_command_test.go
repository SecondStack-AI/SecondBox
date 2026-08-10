package main

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
	"testing"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// execTestServer answers getSandbox and one scripted exec outcome, recording
// the request the CLI actually sent.
type execTestServer struct {
	server  *httptest.Server
	request secondboxclient.BufferedExecRequest
	headers http.Header
	path    string
}

func newExecTestServer(t *testing.T, outcomeJSON string) *execTestServer {
	t.Helper()
	recorder := &execTestServer{}
	recorder.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			if !strings.HasSuffix(request.URL.Path, "/exec") {
				_, _ = io.WriteString(writer, execSandboxJSON)
				return
			}
			recorder.path = request.URL.Path
			recorder.headers = request.Header.Clone()
			if err := json.NewDecoder(request.Body).Decode(&recorder.request); err != nil {
				t.Errorf("decode exec request: %v", err)
			}
			_, _ = io.WriteString(writer, outcomeJSON)
		},
	))
	t.Cleanup(recorder.server.Close)
	return recorder
}

const execSandboxJSON = `{
	"id":"sbx_test1","profile":"durable-coding","profileRevisionId":"profile-revision-1",
	"state":"ready","desiredState":"running","generation":4,
	"workspace":{"id":"workspace-1","generation":4,"state":"ready","sizeBytes":1024,
		"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"},
	"metadata":{},"revision":2,
	"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`

func exitedOutcomeJSON(exitCode int, stdout, stderr string) string {
	value := map[string]any{
		"kind": "exited", "exitCode": exitCode, "elapsedMilliseconds": 5,
		"output": map[string]string{
			"stdoutBase64": base64.StdEncoding.EncodeToString([]byte(stdout)),
			"stderrBase64": base64.StdEncoding.EncodeToString([]byte(stderr)),
		},
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func execTestSession(baseURL string) cliSession {
	return cliSession{
		url: baseURL, token: "token-1", tenantRef: "tenant-1", subjectRef: "subject-1",
	}
}

func runExecForTest(
	t *testing.T,
	recorder *execTestServer,
	args []string,
) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runExecCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		args,
		execCommandEnvironment{
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
	)
	return stdout.String(), stderr.String(), err
}

func TestExecBuildsArgvCommandAndSeparatesStreams(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "on stdout\n", "on stderr\n"))
	stdout, stderr, err := runExecForTest(t, recorder, []string{
		"sbx_test1", "--", "echo", "hello", "--not-a-cli-flag",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "on stdout\n" || stderr != "on stderr\n" {
		t.Errorf("stdout = %q, stderr = %q; want the streams kept separate", stdout, stderr)
	}
	argv := recorder.request.Command.ArgvCommand
	if argv == nil || recorder.request.Command.ShellCommand != nil {
		t.Fatalf("command = %+v; want an argv command", recorder.request.Command)
	}
	if argv.Executable != "echo" || argv.Mode != "argv" {
		t.Errorf("argv = %+v; want echo in argv mode", argv)
	}
	// Operands after -- belong to the guest, including ones that look like flags.
	if len(argv.Arguments) != 2 ||
		argv.Arguments[0] != "hello" || argv.Arguments[1] != "--not-a-cli-flag" {
		t.Errorf("arguments = %#v; want the guest's own operands", argv.Arguments)
	}
}

func TestExecAppliesObservedGenerationWithoutBeingTold(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	if _, _, err := runExecForTest(t, recorder, []string{
		"sbx_test1", "--", "true",
	}); err != nil {
		t.Fatal(err)
	}
	if got := recorder.headers.Get("SecondBox-Generation"); got != "4" {
		t.Errorf("SecondBox-Generation = %q; want the Sandbox's own generation", got)
	}
	if key := recorder.headers.Get("Idempotency-Key"); !strings.HasPrefix(key, "sbk-") {
		t.Errorf("Idempotency-Key = %q; want a generated key", key)
	}
	if recorder.headers.Get("SecondBox-Lease-ID") != "" {
		t.Error("exec must not claim a Lease it was not given")
	}
	if recorder.path != "/v1/sandboxes/sbx_test1/exec" {
		t.Errorf("path = %q", recorder.path)
	}
}

func TestExecPropagatesGuestExitStatus(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(23, "", "boom\n"))
	_, stderr, err := runExecForTest(t, recorder, []string{"sbx_test1", "--", "false"})
	var exited *commandExitError
	if !errors.As(err, &exited) {
		t.Fatalf("error = %v; want a typed exit status", err)
	}
	if exited.code != 23 {
		t.Errorf("exit code = %d; want 23", exited.code)
	}
	if stderr != "boom\n" {
		t.Errorf("stderr = %q; want the guest's own diagnosis", stderr)
	}
}

func TestExecBuildsShellCommand(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	if _, _, err := runExecForTest(t, recorder, []string{
		"sbx_test1", "--shell", "--", "printf x; exit 0",
	}); err != nil {
		t.Fatal(err)
	}
	shell := recorder.request.Command.ShellCommand
	if shell == nil || recorder.request.Command.ArgvCommand != nil {
		t.Fatalf("command = %+v; want a shell command", recorder.request.Command)
	}
	if shell.Command != "printf x; exit 0" || shell.Mode != "shell" {
		t.Errorf("shell = %+v", shell)
	}
}

func TestExecShellRequiresExactlyOneOperand(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	_, _, err := runExecForTest(t, recorder, []string{
		"sbx_test1", "--shell", "--", "printf", "x",
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one command operand") {
		t.Fatalf("error = %v; want a single-operand rejection", err)
	}
}

func TestExecSendsOptionsAndEnvironment(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	if _, _, err := runExecForTest(t, recorder, []string{
		"sbx_test1", "--deadline", "5s", "--max-output-bytes", "4096",
		"--cwd", "project", "--env", "A=1", "--env", "B=2",
		"--lease", "lease-1", "--idempotency-key", "caller-key",
		"--", "true",
	}); err != nil {
		t.Fatal(err)
	}
	if recorder.request.DeadlineMilliseconds != 5000 {
		t.Errorf("deadlineMilliseconds = %d; want 5000", recorder.request.DeadlineMilliseconds)
	}
	if recorder.request.MaximumOutputBytes != 4096 {
		t.Errorf("maximumOutputBytes = %d; want 4096", recorder.request.MaximumOutputBytes)
	}
	if recorder.request.Cwd == nil || *recorder.request.Cwd != "project" {
		t.Errorf("cwd = %v; want project", recorder.request.Cwd)
	}
	if recorder.request.Environment["A"] != "1" || recorder.request.Environment["B"] != "2" {
		t.Errorf("environment = %#v", recorder.request.Environment)
	}
	if recorder.headers.Get("SecondBox-Lease-ID") != "lease-1" ||
		recorder.headers.Get("Idempotency-Key") != "caller-key" {
		t.Errorf("headers = %v; want the caller's Lease and key", recorder.headers)
	}
}

func TestExecAppliesDefaultBounds(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	if _, _, err := runExecForTest(t, recorder, []string{"sbx_test1", "--", "true"}); err != nil {
		t.Fatal(err)
	}
	if recorder.request.DeadlineMilliseconds != defaultExecDeadline.Milliseconds() {
		t.Errorf("deadlineMilliseconds = %d", recorder.request.DeadlineMilliseconds)
	}
	if recorder.request.MaximumOutputBytes != defaultExecOutputBytes {
		t.Errorf("maximumOutputBytes = %d", recorder.request.MaximumOutputBytes)
	}
}

func TestExecJSONPreservesOutcomeAndStatus(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(7, "raw stdout", ""))
	stdout, stderr, err := runExecForTest(t, recorder, []string{
		"sbx_test1", "--json", "--", "false",
	})
	var exited *commandExitError
	if !errors.As(err, &exited) || exited.code != 7 {
		t.Fatalf("error = %v; want exit status 7 alongside JSON output", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q; want JSON mode to write only to stdout", stderr)
	}
	var outcome secondboxclient.ExecOutcome
	if err := json.Unmarshal([]byte(stdout), &outcome); err != nil {
		t.Fatalf("JSON output is not a valid ExecOutcome: %v (%q)", err, stdout)
	}
	if outcome.ExecExited == nil || outcome.ExecExited.ExitCode != 7 {
		t.Errorf("outcome = %+v", outcome)
	}
	// The raw form keeps base64 rather than decoding, so scripts see the wire shape.
	if !strings.Contains(stdout, base64.StdEncoding.EncodeToString([]byte("raw stdout"))) {
		t.Errorf("JSON output = %q; want the encoded stdout preserved", stdout)
	}
}

func TestExecReportsNonExitedOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		wantIn  string
	}{
		{
			name: "spawn failed",
			outcome: `{"kind":"spawn_failed","reason":"not_found",
				"message":"no such file"}`,
			wantIn: "not_found: no such file",
		},
		{
			name: "deadline exceeded",
			outcome: `{"kind":"deadline_exceeded","elapsedMilliseconds":60000,
				"output":{"stdoutBase64":"","stderrBase64":""}}`,
			wantIn: "deadline_exceeded",
		},
		{
			name: "infrastructure failed",
			outcome: `{"kind":"infrastructure_failed","reason":"runner_unavailable",
				"message":"runner lost","retryable":true}`,
			wantIn: "runner lost",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newExecTestServer(t, test.outcome)
			_, _, err := runExecForTest(t, recorder, []string{"sbx_test1", "--", "true"})
			if err == nil || !strings.Contains(err.Error(), test.wantIn) {
				t.Fatalf("error = %v; want one containing %q", err, test.wantIn)
			}
			var exited *commandExitError
			if errors.As(err, &exited) {
				t.Error("an outcome without an exit status must not become one")
			}
		})
	}
}

func TestExecWritesTruncatedOutputBeforeReportingExhaustion(t *testing.T) {
	outcome := `{"kind":"output_exhausted","limitBytes":4,"output":{"stdoutBase64":"` +
		base64.StdEncoding.EncodeToString([]byte("abcd")) + `","stderrBase64":""}}`
	recorder := newExecTestServer(t, outcome)
	stdout, _, err := runExecForTest(t, recorder, []string{"sbx_test1", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "4 bytes") {
		t.Fatalf("error = %v; want an exhaustion report", err)
	}
	if stdout != "abcd" {
		t.Errorf("stdout = %q; want the bounded output still written", stdout)
	}
}

func TestExecRejectsMalformedInvocations(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		wantIn string
	}{
		{"no sandbox", nil, "requires a Sandbox"},
		{"option before sandbox", []string{"--json", "sbx_test1"}, "before any option"},
		{"no command", []string{"sbx_test1"}, "requires a command after --"},
		{
			"non-positive deadline",
			[]string{"sbx_test1", "--deadline", "0s", "--", "true"},
			"--deadline must be at least",
		},
		{
			"non-positive output bound",
			[]string{"sbx_test1", "--max-output-bytes", "0", "--", "true"},
			"--max-output-bytes must be positive",
		},
		{
			"malformed environment",
			[]string{"sbx_test1", "--env", "novalue", "--", "true"},
			"exec environment",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
			_, _, err := runExecForTest(t, recorder, test.args)
			if err == nil || !strings.Contains(err.Error(), test.wantIn) {
				t.Fatalf("error = %v; want one containing %q", err, test.wantIn)
			}
		})
	}
}

func TestExecRequiresCredentials(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runExecCommand(
		context.Background(),
		cliSession{url: "https://a.example.com"},
		[]string{"sbx_test1", "--", "true"},
		execCommandEnvironment{stdout: &stdout, stderr: &stderr},
	)
	if err == nil || !strings.Contains(err.Error(), sessionSourceHint) {
		t.Fatalf("error = %v; want credential guidance", err)
	}
}

func TestRunOperationalCommandRoutesExec(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "routed\n", ""))
	var output bytes.Buffer
	handled, err := runOperationalCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"exec", "sbx_test1", "--", "true"},
		&output,
	)
	if !handled {
		t.Fatal("exec must be handled as an operational command")
	}
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "routed\n" {
		t.Errorf("output = %q", output.String())
	}
}

// TestRunOperationalCommandLeavesExecSubcommands proves `exec stream` and
// `exec cancel` keep their existing routing.
func TestRunOperationalCommandLeavesExecSubcommands(t *testing.T) {
	var output bytes.Buffer
	handled, _ := runOperationalCommand(
		context.Background(), cliSession{}, []string{"exec", "cancel"}, &output,
	)
	if handled {
		t.Error("exec cancel must stay an alias for cancelSandboxExecStream")
	}
	operation, _, err := resolveCommand([]string{"exec", "cancel"})
	if err != nil || operation != "cancelSandboxExecStream" {
		t.Errorf("resolveCommand(exec cancel) = %q, %v", operation, err)
	}
}

func TestExecForwardsStandardInput(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	var stdout, stderr bytes.Buffer
	err := runExecCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"sbx_test1", "--stdin", "--", "cat"},
		execCommandEnvironment{
			stdin:  strings.NewReader("piped input\n"),
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.request.StdinBase64 == nil {
		t.Fatal("stdinBase64 must be sent when --stdin is given")
	}
	decoded, err := base64.StdEncoding.DecodeString(*recorder.request.StdinBase64)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "piped input\n" {
		t.Errorf("stdin = %q; want the piped bytes", decoded)
	}
}

func TestExecOmitsStandardInputByDefault(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	var stdout, stderr bytes.Buffer
	err := runExecCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"sbx_test1", "--", "true"},
		execCommandEnvironment{
			stdin:  strings.NewReader("ignored"),
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.request.StdinBase64 != nil {
		t.Errorf("stdinBase64 = %v; want none without --stdin", *recorder.request.StdinBase64)
	}
}

// TestExecRefusesOversizedStandardInput proves the bounded buffered route
// refuses rather than silently truncating the caller's input.
func TestExecRefusesOversizedStandardInput(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	var stdout, stderr bytes.Buffer
	err := runExecCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"sbx_test1", "--stdin", "--", "cat"},
		execCommandEnvironment{
			stdin:  strings.NewReader(strings.Repeat("x", maximumExecStdinBytes+1)),
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("error = %v; want an oversized-input rejection", err)
	}
	if !strings.Contains(err.Error(), "exec stream") {
		t.Errorf("error = %v; want it to name the unbounded alternative", err)
	}
}

// TestExecAcceptsStandardInputAtTheBound proves the limit matches the schema's
// base64 bound exactly rather than being conservative by an unstated margin.
func TestExecAcceptsStandardInputAtTheBound(t *testing.T) {
	recorder := newExecTestServer(t, exitedOutcomeJSON(0, "", ""))
	var stdout, stderr bytes.Buffer
	err := runExecCommand(
		context.Background(),
		execTestSession(recorder.server.URL),
		[]string{"sbx_test1", "--stdin", "--", "cat"},
		execCommandEnvironment{
			stdin:  strings.NewReader(strings.Repeat("x", maximumExecStdinBytes)),
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(*recorder.request.StdinBase64) != 1398104 {
		t.Errorf("encoded length = %d; want the schema bound 1398104",
			len(*recorder.request.StdinBase64))
	}
}
