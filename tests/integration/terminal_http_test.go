package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublicTerminalWebSocketIsDurableExclusiveReplayableAndCancellable(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "terminal-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-terminal-http")
	scopes := []string{
		"sandbox:read",
		"sandbox:lifecycle",
		"sandbox:exec",
	}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "terminal-http", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "terminal-http-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "terminal-http-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	leasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(leasePool.Close)
	if _, err := leasePool.Exec(
		t.Context(),
		`UPDATE secondbox.leases SET expires_at=$2, updated_at=$3 WHERE id=$1`,
		lease.ID, now.Add(time.Minute), now,
	); err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	server := httptest.NewUnstartedServer(nil)
	publicBaseURL := "http://" + server.Listener.Addr().String()
	dataPlaneService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		BuiltInProfiles: integrationBuiltInProfiles(t),
		Store:           databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return time.Now().UTC() }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
		PublicBaseURL: publicBaseURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: dataPlaneService, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)

	fake := newTerminalHTTPFakeRunner(relay, seed.RunnerID, seed.ConnectionTwo)
	fakeContext, stopFake := context.WithCancel(t.Context())
	defer stopFake()
	fakeErrors := make(chan error, 1)
	go func() { fakeErrors <- fake.run(fakeContext) }()

	session := createTerminalSession(
		t, server.URL, key.Credential, sandbox, lease.ID, "terminal-http-replay", "terminal-order", true,
	)
	if session.Subprotocol != "secondbox.terminal.v1" || session.State != "open" ||
		session.NextClientSequence != 0 {
		t.Fatalf("created Terminal = %#v", session)
	}
	// The pinned ProfileRevision window is published so a client grants what it
	// can spend instead of guessing a bound that fails the session.
	if session.StreamWindowBytes != 65536 {
		t.Fatalf("created Terminal stream window = %d, want the pinned Profile window 65536",
			session.StreamWindowBytes)
	}
	replayed := createTerminalSessionResponse(
		t, server.URL, key.Credential, sandbox, lease.ID,
		"terminal-http-replay", "terminal-order", true,
	)
	assertHTTPStatus(t, replayed, http.StatusCreated)
	if replayed.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("Terminal replay header = %q", replayed.Header.Get("Idempotency-Replayed"))
	}
	var replayedSession contracts.TerminalSession
	decodeHTTPJSON(t, replayed, &replayedSession)
	if replayedSession.ID != session.ID {
		t.Fatalf("Terminal idempotency changed ID: %q != %q", replayedSession.ID, session.ID)
	}

	connection := dialTerminal(t, session, key.Credential, sandbox.Generation)
	secondDialer := websocket.Dialer{Subprotocols: []string{"secondbox.terminal.v1"}}
	_, secondResponse, secondErr := secondDialer.Dial(
		session.WebsocketURL, terminalHTTPHeaders(t, key.Credential, sandbox.Generation),
	)
	if secondErr == nil {
		t.Fatal("parallel Terminal attachment was accepted")
	}
	if secondResponse == nil || secondResponse.StatusCode != http.StatusConflict {
		t.Fatalf("parallel Terminal attachment = %#v, error %v", secondResponse, secondErr)
	}
	secondResponse.Body.Close()

	if err := connection.WriteJSON(map[string]any{
		"type": "credit", "sequence": 0, "bytes": 8,
	}); err != nil {
		t.Fatal(err)
	}
	waitTerminalRunnerEvent(t, fake.events, "credit:terminal-order:8")
	if err := connection.WriteJSON(map[string]any{
		"type": "resize", "sequence": 1, "rows": 40, "columns": 120,
	}); err != nil {
		t.Fatal(err)
	}
	waitTerminalRunnerEvent(t, fake.events, "resize:terminal-order:40x120")
	assertTerminalOutput(t, connection, 0, []byte{0x00, 0x01, 0xfe, 0xff})
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	detachedSession := waitTerminalState(
		t, server.URL, key.Credential, sandbox, session.ID, "detached",
	)
	if detachedSession.NextClientSequence != 2 {
		t.Fatalf("detached Terminal next client sequence = %d", detachedSession.NextClientSequence)
	}

	reconnected := dialTerminal(t, detachedSession, key.Credential, sandbox.Generation)
	assertTerminalOutput(t, reconnected, 0, []byte{0x00, 0x01, 0xfe, 0xff})
	if err := reconnected.WriteJSON(map[string]any{
		"type": "terminal_input", "sequence": 2,
		"dataBase64": base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0xfe, 0xff}),
	}); err != nil {
		t.Fatal(err)
	}
	waitTerminalRunnerEvent(t, fake.events, "input:terminal-order:0001feff")
	assertTerminalOutput(t, reconnected, 1, []byte("done"))
	outcome := readTerminalOutcome(t, reconnected, 2)
	if outcome.Kind != "exited" {
		t.Fatalf("Terminal outcome = %#v", outcome)
	}
	reconnected.Close()

	cancelled := createTerminalSession(
		t, server.URL, key.Credential, sandbox, lease.ID, "terminal-http-cancel", "wait-cancel", true,
	)
	cancelRequest, err := http.NewRequest(
		http.MethodDelete,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/terminals/"+string(cancelled.ID),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, cancelRequest, key.Credential, sandbox.Generation, "")
	cancelRequest.Header.Set("Idempotency-Key", "terminal-http-cancel-request")
	cancelResponse := doHTTP(t, cancelRequest)
	assertHTTPStatus(t, cancelResponse, http.StatusAccepted)
	var cancellingSession contracts.TerminalSession
	decodeHTTPJSON(t, cancelResponse, &cancellingSession)
	if cancellingSession.ID != cancelled.ID ||
		(cancellingSession.State != "closing" && cancellingSession.State != "closed") {
		t.Fatalf("Terminal cancel response = %#v", cancellingSession)
	}
	waitTerminalRunnerEvent(t, fake.events, "cancel:wait-cancel")
	waitTerminalState(
		t, server.URL, key.Credential, sandbox, string(cancelled.ID), "closed",
	)

	replayCancelRequest, err := http.NewRequest(
		http.MethodDelete,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/terminals/"+string(cancelled.ID),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, replayCancelRequest, key.Credential, sandbox.Generation, "")
	replayCancelRequest.Header.Set("Idempotency-Key", "terminal-http-cancel-request")
	replayCancelResponse := doHTTP(t, replayCancelRequest)
	assertHTTPStatus(t, replayCancelResponse, http.StatusAccepted)
	if replayCancelResponse.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf(
			"Terminal cancellation replay header = %q",
			replayCancelResponse.Header.Get("Idempotency-Replayed"),
		)
	}
	var replayedCancellingSession contracts.TerminalSession
	decodeHTTPJSON(t, replayCancelResponse, &replayedCancellingSession)
	if !reflect.DeepEqual(replayedCancellingSession, cancellingSession) {
		t.Fatalf(
			"Terminal cancellation replay changed response: first=%#v replay=%#v",
			cancellingSession, replayedCancellingSession,
		)
	}

	newKeyCancelRequest, err := http.NewRequest(
		http.MethodDelete,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/terminals/"+string(cancelled.ID),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, newKeyCancelRequest, key.Credential, sandbox.Generation, "")
	newKeyCancelRequest.Header.Set("Idempotency-Key", "terminal-http-cancel-new-request")
	newKeyCancelResponse := doHTTP(t, newKeyCancelRequest)
	assertHTTPStatus(t, newKeyCancelResponse, http.StatusAccepted)
	if newKeyCancelResponse.Header.Get("Idempotency-Replayed") != "false" {
		t.Fatalf(
			"new-key Terminal cancellation replay header = %q",
			newKeyCancelResponse.Header.Get("Idempotency-Replayed"),
		)
	}
	var newKeyCancellingSession contracts.TerminalSession
	decodeHTTPJSON(t, newKeyCancelResponse, &newKeyCancellingSession)
	if newKeyCancellingSession.ID != cancelled.ID ||
		newKeyCancellingSession.State != "closed" {
		t.Fatalf("new-key Terminal cancellation response = %#v", newKeyCancellingSession)
	}

	conflictingCancelRequest, err := http.NewRequest(
		http.MethodDelete,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/terminals/"+string(cancelled.ID),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, conflictingCancelRequest, key.Credential, sandbox.Generation+1, "")
	conflictingCancelRequest.Header.Set("Idempotency-Key", "terminal-http-cancel-request")
	conflictingCancelResponse := doHTTP(t, conflictingCancelRequest)
	assertHTTPStatus(t, conflictingCancelResponse, http.StatusConflict)
	var conflictingCancelProblem contracts.Problem
	decodeHTTPJSON(t, conflictingCancelResponse, &conflictingCancelProblem)
	if conflictingCancelProblem.Code != "idempotency_conflict" {
		t.Fatalf("Terminal cancellation conflict = %#v", conflictingCancelProblem)
	}

	nonDetachable := createTerminalSession(
		t, server.URL, key.Credential, sandbox, lease.ID,
		"terminal-http-nondetachable", "disconnect", false,
	)
	nonDetachableConnection := dialTerminal(t, nonDetachable, key.Credential, sandbox.Generation)
	if err := nonDetachableConnection.Close(); err != nil {
		t.Fatal(err)
	}
	waitTerminalRunnerEvent(t, fake.events, "cancel:disconnect")

	sandboxResponse := dataPlaneGET(
		t, server.URL+"/v1/sandboxes/"+sandbox.ID, key.Credential, sandbox.Generation,
	)
	assertHTTPStatus(t, sandboxResponse, http.StatusOK)
	sandboxResponse.Body.Close()

	stopFake()
	select {
	case err := <-fakeErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Terminal fake runner did not stop")
	}
}

type terminalHTTPFakeRunner struct {
	relay        *runnercontrol.PostgresFrameRelay
	runnerID     string
	connectionID string
	events       chan string
	mu           sync.Mutex
	operations   map[string]*terminalHTTPFakeOperation
}

type terminalHTTPFakeOperation struct {
	open         *runnerv1.ExecFrame
	command      string
	nextSequence uint64
}

func newTerminalHTTPFakeRunner(
	relay *runnercontrol.PostgresFrameRelay,
	runnerID string,
	connectionID string,
) *terminalHTTPFakeRunner {
	return &terminalHTTPFakeRunner{
		relay: relay, runnerID: runnerID, connectionID: connectionID,
		events: make(chan string, 32), operations: map[string]*terminalHTTPFakeOperation{},
	}
}

func (fake *terminalHTTPFakeRunner) run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		now := time.Now().UTC()
		delivery, found, err := fake.relay.ClaimOutboundFrame(
			ctx, fake.runnerID, fake.connectionID, now,
		)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !found {
			time.Sleep(time.Millisecond)
			continue
		}
		if err := fake.relay.MarkOutboundFrameDelivered(
			ctx, delivery.ID, fake.connectionID, delivery.ClaimAttempt, now,
		); err != nil {
			return err
		}
		if err := fake.handle(ctx, delivery.Message, now); err != nil {
			return err
		}
	}
}

func (fake *terminalHTTPFakeRunner) handle(
	ctx context.Context,
	message *runnerv1.ControlPlaneToRunner,
	now time.Time,
) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if execFrame := message.GetExec(); execFrame != nil {
		if open := execFrame.GetOpen(); open != nil {
			if !open.AllocatePty || !open.Streaming {
				return fmt.Errorf("Terminal fake runner received non-PTY Open: %#v", open)
			}
			fake.operations[execFrame.OperationId] = &terminalHTTPFakeOperation{
				open: execFrame, command: open.GetShell(), nextSequence: 1,
			}
			return nil
		}
		operation := fake.operations[execFrame.OperationId]
		if operation == nil {
			return fmt.Errorf("Terminal fake runner Exec frame has no Open: %s", execFrame.OperationId)
		}
		if execFrame.GetCancel() != nil {
			if err := fake.terminal(
				ctx, operation, runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED, now,
			); err != nil {
				return err
			}
			fake.events <- "cancel:" + operation.command
			return nil
		}
		return fmt.Errorf("Terminal fake runner received unsupported Exec frame")
	}
	ptyFrame := message.GetPty()
	if ptyFrame == nil {
		return nil
	}
	operation := fake.operations[ptyFrame.OperationId]
	if operation == nil {
		return fmt.Errorf("Terminal fake runner PTY frame has no Open: %s", ptyFrame.OperationId)
	}
	switch {
	case ptyFrame.GetCredit() != nil:
		fake.events <- fmt.Sprintf("credit:%s:%d", operation.command, ptyFrame.GetCredit().ByteCount)
		if operation.command == "terminal-order" {
			return fake.output(ctx, operation, []byte{0x00, 0x01, 0xfe, 0xff}, now)
		}
	case ptyFrame.GetResize() != nil:
		fake.events <- fmt.Sprintf(
			"resize:%s:%dx%d", operation.command,
			ptyFrame.GetResize().Rows, ptyFrame.GetResize().Columns,
		)
	case ptyFrame.GetInput() != nil:
		fake.events <- fmt.Sprintf("input:%s:%x", operation.command, ptyFrame.GetInput().Data)
		if operation.command == "terminal-order" {
			if err := fake.output(ctx, operation, []byte("done"), now); err != nil {
				return err
			}
			return fake.terminal(ctx, operation, runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED, now)
		}
	default:
		return fmt.Errorf("Terminal fake runner received unsupported PTY frame")
	}
	return nil
}

func (fake *terminalHTTPFakeRunner) output(
	ctx context.Context,
	operation *terminalHTTPFakeOperation,
	data []byte,
	now time.Time,
) error {
	frame := operation.open
	sequence := operation.nextSequence
	operation.nextSequence++
	_, err := fake.relay.PersistInboundFrame(ctx, runnercontrol.InboundRelayFrame{
		RunnerID: fake.runnerID, ConnectionID: fake.connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
				Fence: frame.Fence, OperationId: frame.OperationId, StreamId: frame.StreamId,
				Sequence: sequence,
				Payload: &runnerv1.PtyFrame_Output{Output: &runnerv1.ExecOutput{
					Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
					Data:    bytes.Clone(data),
				}},
			}},
		},
	}, now)
	return err
}

func (fake *terminalHTTPFakeRunner) terminal(
	ctx context.Context,
	operation *terminalHTTPFakeOperation,
	kind runnerv1.ExecTerminalKind,
	now time.Time,
) error {
	frame := operation.open
	sequence := operation.nextSequence
	operation.nextSequence++
	exitCode := int32(-1)
	if kind == runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		exitCode = 0
	}
	_, err := fake.relay.PersistInboundFrame(ctx, runnercontrol.InboundRelayFrame{
		RunnerID: fake.runnerID, ConnectionID: fake.connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
				Fence: frame.Fence, OperationId: frame.OperationId, StreamId: frame.StreamId,
				Sequence: sequence,
				Payload: &runnerv1.PtyFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
					Kind: kind, ExitCode: exitCode,
				}},
			}},
		},
	}, now)
	return err
}

func createTerminalSession(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	command string,
	detachable bool,
) contracts.TerminalSession {
	t.Helper()
	response := createTerminalSessionResponse(
		t, baseURL, credential, sandbox, leaseID, idempotencyKey, command, detachable,
	)
	assertHTTPStatus(t, response, http.StatusCreated)
	var session contracts.TerminalSession
	decodeHTTPJSON(t, response, &session)
	return session
}

func createTerminalSessionResponse(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	command string,
	detachable bool,
) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"command":     map[string]any{"mode": "shell", "command": command},
		"environment": map[string]string{}, "rows": 24, "columns": 80,
		"deadlineMilliseconds": 30_000, "detachable": detachable,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/v1/sandboxes/"+sandbox.ID+"/terminals",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, request, credential, sandbox.Generation, idempotencyKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("SecondBox-Lease-ID", leaseID)
	return doHTTP(t, request)
}

func dialTerminal(
	t *testing.T,
	session contracts.TerminalSession,
	credential string,
	generation int64,
) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{"secondbox.terminal.v1"}}
	connection, response, err := dialer.Dial(
		session.WebsocketURL, terminalHTTPHeaders(t, credential, generation),
	)
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("dial Terminal: status=%d body=%s error=%v", response.StatusCode, body, err)
		}
		t.Fatal(err)
	}
	if connection.Subprotocol() != "secondbox.terminal.v1" {
		connection.Close()
		t.Fatalf("Terminal subprotocol = %q", connection.Subprotocol())
	}
	return connection
}

func terminalHTTPHeaders(t *testing.T, credential string, generation int64) http.Header {
	headers := make(http.Header)
	setPlatformAuthorizationHeaders(t, headers, credential)
	headers.Set("SecondBox-Generation", fmt.Sprintf("%d", generation))
	return headers
}

func assertTerminalOutput(
	t *testing.T,
	connection *websocket.Conn,
	sequence int64,
	expected []byte,
) {
	t.Helper()
	var frame contracts.TerminalOutputFrame
	if err := connection.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(frame.DataBase64)
	if err != nil || frame.Type != "terminal_output" ||
		frame.Sequence != sequence || !bytes.Equal(data, expected) {
		t.Fatalf("Terminal output = %#v data=%x error=%v", frame, data, err)
	}
}

func readTerminalOutcome(
	t *testing.T,
	connection *websocket.Conn,
	sequence int64,
) streamingExecOutcome {
	t.Helper()
	var frame struct {
		Type     string          `json:"type"`
		Sequence int64           `json:"sequence"`
		Outcome  json.RawMessage `json:"outcome"`
	}
	if err := connection.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "outcome" || frame.Sequence != sequence {
		t.Fatalf("Terminal outcome frame = %#v", frame)
	}
	var outcome streamingExecOutcome
	if err := json.Unmarshal(frame.Outcome, &outcome); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func waitTerminalState(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	sessionID string,
	expected string,
) contracts.TerminalSession {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response := dataPlaneGET(
			t,
			baseURL+"/v1/sandboxes/"+sandbox.ID+"/terminals/"+sessionID,
			credential, sandbox.Generation,
		)
		if response.StatusCode == http.StatusOK {
			var session contracts.TerminalSession
			decodeHTTPJSON(t, response, &session)
			if session.State == expected {
				return session
			}
		} else {
			response.Body.Close()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Terminal %s did not reach state %q", sessionID, expected)
	return contracts.TerminalSession{}
}

func waitTerminalRunnerEvent(t *testing.T, events <-chan string, expected string) {
	t.Helper()
	select {
	case event := <-events:
		if event != expected {
			t.Fatalf("Terminal runner event = %q, want %q", event, expected)
		}
	case <-time.After(time.Second):
		t.Fatalf("Terminal runner event %q was not observed", expected)
	}
}
