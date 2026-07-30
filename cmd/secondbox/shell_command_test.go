package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

// shellTestServer scripts name resolution, Lease acquisition, Terminal creation,
// and one immediately-exiting attachment.
type shellTestServer struct {
	server *httptest.Server

	mutex           sync.Mutex
	terminalHeaders http.Header
	leaseAcquired   bool
	leaseReleased   bool
	creation        secondboxclient.CreateTerminalRequest
}

func newShellTestServer(t *testing.T) *shellTestServer {
	t.Helper()
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.terminal.v1"}}
	recorder := &shellTestServer{}
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
