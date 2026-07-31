package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

// shellTestServer scripts name resolution, Lease acquisition, Terminal creation,
// and one immediately-exiting attachment.
type shellTestServer struct {
	server *httptest.Server
	// attached closes once one attachment has granted credit. A test that
	// interrupts the session waits for it so the cancellation lands on a live
	// attachment rather than racing setup.
	attached chan struct{}
	// holdAttachment keeps the attachment open instead of ending it with an
	// outcome, so an interrupted session can be observed.
	holdAttachment bool

	mutex           sync.Mutex
	terminalHeaders http.Header
	leaseAcquired   bool
	leaseReleased   bool
	deleted         bool
	creation        secondboxclient.CreateTerminalRequest
}

func newShellTestServer(t *testing.T) *shellTestServer {
	t.Helper()
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.terminal.v1"}}
	recorder := &shellTestServer{attached: make(chan struct{})}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(
		func(response http.ResponseWriter, request *http.Request) {
			recorder.mutex.Lock()
			switch {
			case request.URL.Path == "/v1/sandboxes" && request.Method == http.MethodGet:
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, fmt.Sprintf(`{"items":[%s]}`,
					shellSandboxJSON("sbx_shell1", 7)))
				return
			case strings.HasSuffix(request.URL.Path, "/leases") &&
				request.Method == http.MethodPost:
				recorder.leaseAcquired = true
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, fmt.Sprintf(
					`{"id":"lea_shell1","sandboxId":"sbx_shell1","generation":7,"state":"active",
					  "expiresAt":%q,"createdAt":"2026-07-28T00:00:00Z",
					  "updatedAt":"2026-07-28T00:00:00Z"}`,
					time.Now().Add(time.Hour).Format(time.RFC3339Nano)))
				return
			case strings.HasPrefix(request.URL.Path, "/v1/leases/") &&
				request.Method == http.MethodDelete:
				recorder.leaseReleased = true
				recorder.mutex.Unlock()
				response.WriteHeader(http.StatusNoContent)
				return
			case strings.HasSuffix(request.URL.Path, "/terminals") &&
				request.Method == http.MethodPost:
				recorder.terminalHeaders = request.Header.Clone()
				if err := json.NewDecoder(request.Body).Decode(&recorder.creation); err != nil {
					t.Errorf("decode Terminal creation: %v", err)
				}
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(response, `{
					"id":"term-1","sandboxId":"sbx_shell1","generation":7,"state":"open",
					"websocketUrl":"%s/attach","subprotocol":"secondbox.terminal.v1",
					"nextClientSequence":0,"expiresAt":%q}`,
					"ws"+server.URL[len("http"):],
					time.Now().Add(time.Minute).Format(time.RFC3339Nano))
				return
			case request.URL.Path == "/attach":
				recorder.mutex.Unlock()
				connection, err := upgrader.Upgrade(response, request, nil)
				if err != nil {
					t.Errorf("upgrade Terminal: %v", err)
					return
				}
				defer connection.Close()
				// Read the client's opening credit grant before answering, so the
				// outcome is never written into a connection still being set up.
				assertCLITerminalCredit(t, connection, 0, defaultShellCreditBytes)
				close(recorder.attached)
				if recorder.holdAttachment {
					// Leave the session running so only cancellation can end it.
					<-request.Context().Done()
					return
				}
				if err := connection.WriteJSON(secondboxclient.TerminalFrame{
					StreamOutcomeFrame: &secondboxclient.StreamOutcomeFrame{
						Type: "outcome", Sequence: 0,
						Outcome: secondboxclient.ExecOutcome{
							ExecExited: &secondboxclient.ExecExited{Kind: "exited", ExitCode: 0},
						},
					},
				}); err != nil {
					t.Errorf("write Terminal outcome: %v", err)
				}
				return
			default:
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, shellSandboxJSON("sbx_shell1", 7))
				return
			}
		}))
	t.Cleanup(server.Close)
	recorder.server = server
	return recorder
}

func shellSandboxJSON(id string, generation int) string {
	return fmt.Sprintf(`{
		"id":%q,"profile":"coding-environment","profileRevisionId":"prv_1",
		"state":"ready","desiredState":"running","generation":%d,
		"workspace":{"id":"wsp_1","generation":%d,"state":"ready","sizeBytes":1024,
			"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"},
		"metadata":{"secondbox.dev/name":"my-box"},"revision":1,
		"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`,
		id, generation, generation)
}

func invokeShell(t *testing.T, recorder *shellTestServer, args []string) error {
	t.Helper()
	// A pipe that stays open keeps the input pump blocked, as a real terminal
	// would. A reader at EOF would instead send its end-of-input byte into a
	// connection the exited session has already closed.
	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = inputWriter.Close()
		_ = inputReader.Close()
	})
	var output bytes.Buffer
	return runShellCommand(
		t.Context(),
		execTestSession(recorder.server.URL),
		args,
		sandboxShellEnvironment{
			input: inputReader, output: &output, inputFD: -1, outputFD: -1,
			terminal: &fakeShellTerminalController{}, httpClient: recorder.server.Client(),
		},
		recorder.server.Client(),
	)
}

// TestShellResolvesNameAcquiresLeaseAndAttaches proves the composite supplies
// every value the underlying terminal command used to demand by hand.
func TestShellResolvesNameAcquiresLeaseAndAttaches(t *testing.T) {
	recorder := newShellTestServer(t)
	if err := invokeShell(t, recorder, []string{"my-box"}); err != nil {
		t.Fatal(err)
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if !recorder.leaseAcquired {
		t.Error("shell must acquire a Lease when the caller supplied none")
	}
	if !recorder.leaseReleased {
		t.Error("shell must release the Lease it acquired")
	}
	if got := recorder.terminalHeaders.Get("SecondBox-Generation"); got != "7" {
		t.Errorf("SecondBox-Generation = %q; want the resolved generation", got)
	}
	if got := recorder.terminalHeaders.Get("SecondBox-Lease-ID"); got != "lea_shell1" {
		t.Errorf("SecondBox-Lease-ID = %q; want the acquired Lease", got)
	}
	if key := recorder.terminalHeaders.Get("Idempotency-Key"); !strings.HasPrefix(key, "sbk-") {
		t.Errorf("Idempotency-Key = %q; want a generated key", key)
	}
}

// TestShellReleasesItsLeaseWhenInterrupted proves an interrupted session does
// not strand the Lease it acquired. A stranded Lease stayed active until the
// service expired it and made the next attach fail with a state conflict.
func TestShellReleasesItsLeaseWhenInterrupted(t *testing.T) {
	recorder := newShellTestServer(t)
	recorder.holdAttachment = true
	ctx, interrupt := context.WithCancel(t.Context())
	defer interrupt()
	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = inputWriter.Close()
		_ = inputReader.Close()
	})
	var output bytes.Buffer
	shellDone := make(chan error, 1)
	go func() {
		shellDone <- runShellCommand(
			ctx,
			execTestSession(recorder.server.URL),
			[]string{"my-box"},
			sandboxShellEnvironment{
				input: inputReader, output: &output, inputFD: -1, outputFD: -1,
				terminal: &fakeShellTerminalController{}, httpClient: recorder.server.Client(),
			},
			recorder.server.Client(),
		)
	}()
	select {
	case <-recorder.attached:
	case err := <-shellDone:
		t.Fatalf("shell ended before it attached: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("shell never attached")
	}
	interrupt()
	select {
	case <-shellDone:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted shell never returned")
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if !recorder.leaseAcquired {
		t.Fatal("shell must acquire a Lease when the caller supplied none")
	}
	if !recorder.leaseReleased {
		t.Error("an interrupted shell must release the Lease it acquired")
	}
}

// TestInterruptibleContextCancelsOnTerminalSignals proves the process-level
// wiring that makes the release above run for a real interrupt.
func TestInterruptibleContextCancelsOnTerminalSignals(t *testing.T) {
	ctx := interruptibleContext()
	if ctx.Err() != nil {
		t.Fatalf("context started cancelled: %v", ctx.Err())
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("raise SIGINT: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("SIGINT did not cancel the CLI context")
	}
}

// TestShellLetsCallerValuesOverrideInjectedDefaults proves the injected values
// precede the caller's own arguments, so the caller always wins.
func TestShellLetsCallerValuesOverrideInjectedDefaults(t *testing.T) {
	recorder := newShellTestServer(t)
	err := invokeShell(t, recorder, []string{
		"my-box", "--lease", "caller-lease", "--idempotency-key", "caller-key",
		"--command", "/bin/bash",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.leaseAcquired {
		t.Error("a caller-supplied Lease must not trigger acquisition")
	}
	if got := recorder.terminalHeaders.Get("SecondBox-Lease-ID"); got != "caller-lease" {
		t.Errorf("SecondBox-Lease-ID = %q; want the caller's Lease", got)
	}
	if got := recorder.terminalHeaders.Get("Idempotency-Key"); got != "caller-key" {
		t.Errorf("Idempotency-Key = %q; want the caller's key", got)
	}
	if recorder.creation.Command.ShellCommand == nil ||
		recorder.creation.Command.ShellCommand.Command != "/bin/bash" {
		t.Errorf("command = %+v; want the caller's shell", recorder.creation.Command)
	}
}

func TestShellAcceptsAnIdentifierDirectly(t *testing.T) {
	recorder := newShellTestServer(t)
	if err := invokeShell(t, recorder, []string{"sbx_shell1"}); err != nil {
		t.Fatal(err)
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if got := recorder.terminalHeaders.Get("SecondBox-Generation"); got != "7" {
		t.Errorf("SecondBox-Generation = %q", got)
	}
}

func TestShellRejectsMalformedInvocations(t *testing.T) {
	recorder := newShellTestServer(t)
	for _, test := range []struct {
		name   string
		args   []string
		wantIn string
	}{
		{"no sandbox", nil, "requires a Sandbox"},
		{"option before sandbox", []string{"--command", "/bin/sh"}, "before any option"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := invokeShell(t, recorder, test.args)
			if err == nil || !strings.Contains(err.Error(), test.wantIn) {
				t.Fatalf("error = %v; want one containing %q", err, test.wantIn)
			}
		})
	}
}

func TestRunOperationalCommandLeavesShellSubcommands(t *testing.T) {
	for _, subcommand := range []string{"create", "reconnect", "close"} {
		if !isShellSubcommand([]string{"shell", subcommand}) {
			t.Errorf("shell %s must stay a terminal negotiation alias", subcommand)
		}
	}
	if isShellSubcommand([]string{"shell", "my-box"}) {
		t.Error("shell my-box must be the composite command")
	}
	operation, _, err := resolveCommand([]string{"shell", "create"})
	if err != nil || operation != "createSandboxTerminal" {
		t.Errorf("resolveCommand(shell create) = %q, %v", operation, err)
	}
}

// ttyRunServer scripts create, wait, Lease, Terminal, and delete for an
// ephemeral interactive Sandbox.
func newTTYRunServer(t *testing.T) *shellTestServer {
	t.Helper()
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.terminal.v1"}}
	recorder := &shellTestServer{attached: make(chan struct{})}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(
		func(response http.ResponseWriter, request *http.Request) {
			recorder.mutex.Lock()
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, `{"id":"op_1","sandboxId":"sbx_shell1",
					"kind":"create","state":"pending","requestId":"r1",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
				return
			case request.Method == http.MethodDelete &&
				strings.HasPrefix(request.URL.Path, "/v1/sandboxes/"):
				recorder.deleted = true
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, `{"id":"op_2","sandboxId":"sbx_shell1",
					"kind":"delete","state":"pending","requestId":"r2",
					"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"}`)
				return
			case strings.HasSuffix(request.URL.Path, "/leases"):
				recorder.leaseAcquired = true
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, fmt.Sprintf(
					`{"id":"lea_shell1","sandboxId":"sbx_shell1","generation":7,"state":"active",
					  "expiresAt":%q,"createdAt":"2026-07-28T00:00:00Z",
					  "updatedAt":"2026-07-28T00:00:00Z"}`,
					time.Now().Add(time.Hour).Format(time.RFC3339Nano)))
				return
			case strings.HasPrefix(request.URL.Path, "/v1/leases/") &&
				request.Method == http.MethodDelete:
				recorder.leaseReleased = true
				recorder.mutex.Unlock()
				response.WriteHeader(http.StatusNoContent)
				return
			case strings.HasSuffix(request.URL.Path, "/terminals"):
				recorder.terminalHeaders = request.Header.Clone()
				if err := json.NewDecoder(request.Body).Decode(&recorder.creation); err != nil {
					t.Errorf("decode Terminal creation: %v", err)
				}
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(response, `{
					"id":"term-1","sandboxId":"sbx_shell1","generation":7,"state":"open",
					"websocketUrl":"%s/attach","subprotocol":"secondbox.terminal.v1",
					"nextClientSequence":0,"expiresAt":%q}`,
					"ws"+server.URL[len("http"):],
					time.Now().Add(time.Minute).Format(time.RFC3339Nano))
				return
			case request.URL.Path == "/attach":
				recorder.mutex.Unlock()
				connection, err := upgrader.Upgrade(response, request, nil)
				if err != nil {
					t.Errorf("upgrade Terminal: %v", err)
					return
				}
				defer connection.Close()
				assertCLITerminalCredit(t, connection, 0, defaultShellCreditBytes)
				if err := connection.WriteJSON(secondboxclient.TerminalFrame{
					StreamOutcomeFrame: &secondboxclient.StreamOutcomeFrame{
						Type: "outcome", Sequence: 0,
						Outcome: secondboxclient.ExecOutcome{
							ExecExited: &secondboxclient.ExecExited{Kind: "exited", ExitCode: 0},
						},
					},
				}); err != nil {
					t.Errorf("write Terminal outcome: %v", err)
				}
				return
			default:
				recorder.mutex.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, shellSandboxJSON("sbx_shell1", 7))
				return
			}
		}))
	t.Cleanup(server.Close)
	recorder.server = server
	return recorder
}

func invokeTTYRun(t *testing.T, recorder *shellTestServer, args []string) (string, error) {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() { _ = inputWriter.Close(); _ = inputReader.Close() })
	var stdout, stderr bytes.Buffer
	err := runRunCommand(
		t.Context(),
		execTestSession(recorder.server.URL),
		args,
		execCommandEnvironment{
			stdout: &stdout, stderr: &stderr, httpClient: recorder.server.Client(),
		},
		sandboxShellEnvironment{
			input: inputReader, output: &stdout, inputFD: -1, outputFD: -1,
			terminal: &fakeShellTerminalController{}, httpClient: recorder.server.Client(),
		},
	)
	return stderr.String(), err
}

// TestRunWithTTYCreatesAttachesAndDisposes proves the ephemeral interactive
// shape: one command creates a Sandbox, attaches a Terminal, and deletes the
// Sandbox when the Terminal ends.
func TestRunWithTTYCreatesAttachesAndDisposes(t *testing.T) {
	recorder := newTTYRunServer(t)
	if _, err := invokeTTYRun(t, recorder, []string{"coding-environment", "--tty"}); err != nil {
		t.Fatal(err)
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if !recorder.leaseAcquired || !recorder.leaseReleased {
		t.Errorf("lease acquired=%t released=%t; want both",
			recorder.leaseAcquired, recorder.leaseReleased)
	}
	if recorder.terminalHeaders.Get("SecondBox-Generation") != "7" {
		t.Errorf("generation = %q", recorder.terminalHeaders.Get("SecondBox-Generation"))
	}
	if !recorder.deleted {
		t.Error("an ephemeral interactive Sandbox must be deleted when the Terminal ends")
	}
}

func TestRunWithTTYKeepsTheSandboxWhenAsked(t *testing.T) {
	recorder := newTTYRunServer(t)
	stderr, err := invokeTTYRun(t, recorder, []string{"coding-environment", "--tty", "--keep"})
	if err != nil {
		t.Fatal(err)
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.deleted {
		t.Error("--keep must retain the Sandbox")
	}
	if !strings.Contains(stderr, "sbx_shell1") {
		t.Errorf("stderr = %q; want the retained identifier reported", stderr)
	}
}

func TestRunWithTTYUsesTheOperandAsItsCommand(t *testing.T) {
	recorder := newTTYRunServer(t)
	if _, err := invokeTTYRun(t, recorder, []string{
		"coding-environment", "--tty", "--", "/bin/bash",
	}); err != nil {
		t.Fatal(err)
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.creation.Command.ShellCommand == nil ||
		recorder.creation.Command.ShellCommand.Command != "/bin/bash" {
		t.Errorf("terminal command = %+v; want the operand", recorder.creation.Command)
	}
}

func TestRunWithTTYRejectsIncompatibleOptions(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"stdin", []string{"coding-environment", "--tty", "--stdin"}},
		{"json", []string{"coding-environment", "--tty", "--json"}},
		{"shell", []string{"coding-environment", "--tty", "--shell", "--", "echo hi"}},
		{"two operands", []string{"coding-environment", "--tty", "--", "/bin/bash", "extra"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := newTTYRunServer(t)
			_, err := invokeTTYRun(t, recorder, test.args)
			if err == nil {
				t.Fatal("incompatible --tty options must be rejected")
			}
			recorder.mutex.Lock()
			defer recorder.mutex.Unlock()
			if recorder.leaseAcquired {
				t.Error("a rejected invocation must not create anything")
			}
		})
	}
}
