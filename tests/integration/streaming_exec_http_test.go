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
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
)

func TestPublicStreamingExecIsDurableBackpressuredAndCancellable(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "streaming-exec-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-streaming-exec-http")
	scopes := []string{
		contracts.ScopeSandboxRead,
		contracts.ScopeSandboxLifecycle,
		contracts.ScopeSandboxExec,
	}
	if _, err := controlPlane.UpdateServiceAccount(
		t.Context(), admin, project.ID, account.ID,
		contracts.UpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := controlPlane.CreateAPIKey(
		t.Context(), admin, project.ID, account.ID,
		contracts.CreateAPIKeyRequest{Name: "streaming-exec-http", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "streaming-exec-http-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := seedRelayReadyAssignment(t, sandbox, now)
	staleLease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "streaming-stale-lease-acquire", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, staleLease.ID, "streaming-stale-lease-release",
	); err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 2 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	server := httptest.NewUnstartedServer(nil)
	publicBaseURL := "http://" + server.Listener.Addr().String()
	dataPlaneService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, BootstrapAdminToken: "bootstrap-administrator-secret",
		APIKeyHashSecret:    []byte("test-keyed-api-hash-secret-at-least-32-bytes"),
		DefaultProjectQuota: generousQuota(), DefaultProfileQuota: generousQuota(),
		Now: func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
		PublicBaseURL: publicBaseURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: dataPlaneService, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	fake := newStreamingExecFakeRunner(relay, seed.RunnerID, seed.ConnectionTwo)
	fakeContext, stopFake := context.WithCancel(t.Context())
	defer stopFake()
	fakeErrors := make(chan error, 1)
	go func() { fakeErrors <- fake.run(fakeContext) }()

	staleLeaseResponse := createStreamingExecSessionResponse(
		t, server.URL, key.Credential, sandbox, staleLease.ID, "stream-stale-lease", "stream-order", 16,
	)
	assertHTTPStatus(t, staleLeaseResponse, http.StatusConflict)
	var staleLeaseProblem contracts.Problem
	decodeHTTPJSON(t, staleLeaseResponse, &staleLeaseProblem)
	if staleLeaseProblem.Code != "lease_fenced" {
		t.Fatalf("stale Lease problem = %#v", staleLeaseProblem)
	}

	ordered := createStreamingExecSession(
		t, server.URL, key.Credential, sandbox, "", "stream-ordered", "stream-order", 16,
	)
	staleGenerationHeaders := streamingExecHeaders(key.Credential, sandbox.Generation+1)
	staleGenerationDialer := websocket.Dialer{Subprotocols: []string{"secondbox.exec.v1"}}
	_, response, err := staleGenerationDialer.Dial(ordered.WebsocketURL, staleGenerationHeaders)
	if err == nil {
		t.Fatal("stale Sandbox generation attached to streaming Exec")
	}
	if response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("stale generation response = %#v, error %v", response, err)
	}
	response.Body.Close()

	orderedConnection := dialStreamingExec(t, ordered, key.Credential, sandbox.Generation)
	if err := orderedConnection.WriteJSON(map[string]any{
		"type": "stdin", "sequence": 0,
		"dataBase64": base64.StdEncoding.EncodeToString([]byte("one")),
		"endOfInput": false,
	}); err != nil {
		t.Fatal(err)
	}
	waitStreamingRunnerEvent(t, fake.events, "input:stream-order:one")
	if err := orderedConnection.WriteJSON(map[string]any{
		"type": "stdin", "sequence": 1,
		"dataBase64": "", "endOfInput": true,
	}); err != nil {
		t.Fatal(err)
	}
	waitStreamingRunnerEvent(t, fake.events, "eof:stream-order")
	frames, err := relay.ListExecServerFrames(t.Context(), project.ID, string(ordered.ID), -1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("output existed before credit: %#v", frames)
	}
	if err := orderedConnection.WriteJSON(map[string]any{
		"type": "credit", "sequence": 2, "bytes": 16,
	}); err != nil {
		t.Fatal(err)
	}
	assertStreamingOutput(t, orderedConnection, 0, "stdout", []byte("stdout:one"))
	assertStreamingOutput(t, orderedConnection, 1, "stderr", []byte("!"))
	orderedOutcome := readStreamingOutcome(t, orderedConnection, 2)
	if orderedOutcome.Kind != "exited" {
		t.Fatalf("ordered outcome = %#v", orderedOutcome)
	}
	orderedConnection.Close()

	exhausted := createStreamingExecSession(
		t, server.URL, key.Credential, sandbox, "", "stream-exhausted", "output-exhausted", 4,
	)
	exhaustedConnection := dialStreamingExec(t, exhausted, key.Credential, sandbox.Generation)
	if err := exhaustedConnection.WriteJSON(map[string]any{
		"type": "credit", "sequence": 0, "bytes": 4,
	}); err != nil {
		t.Fatal(err)
	}
	assertStreamingOutput(t, exhaustedConnection, 0, "stdout", []byte("part"))
	exhaustedOutcome := readStreamingOutcome(t, exhaustedConnection, 1)
	if exhaustedOutcome.Kind != "output_exhausted" {
		t.Fatalf("output-exhausted outcome = %#v", exhaustedOutcome)
	}
	exhaustedConnection.Close()

	cancelled := createStreamingExecSession(
		t, server.URL, key.Credential, sandbox, "", "stream-cancelled", "wait-cancel", 16,
	)
	cancelledConnection := dialStreamingExec(t, cancelled, key.Credential, sandbox.Generation)
	if err := cancelledConnection.WriteJSON(map[string]any{
		"type": "cancel", "sequence": 0,
	}); err != nil {
		t.Fatal(err)
	}
	waitStreamingRunnerEvent(t, fake.events, "cancel:wait-cancel")
	cancelledOutcome := readStreamingOutcome(t, cancelledConnection, 0)
	if cancelledOutcome.Kind != "cancelled" {
		t.Fatalf("cancelled outcome = %#v", cancelledOutcome)
	}
	cancelledConnection.Close()

	httpCancelled := createStreamingExecSession(
		t, server.URL, key.Credential, sandbox, "", "stream-http-cancelled", "wait-http-cancel", 16,
	)
	cancelRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/exec-streams/"+httpCancelled.ID+":cancel",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(cancelRequest, key.Credential, sandbox.Generation, "stream-http-cancel-request")
	cancelResponse := doHTTP(t, cancelRequest)
	assertHTTPStatus(t, cancelResponse, http.StatusAccepted)
	if cancelResponse.Header.Get("Idempotency-Replayed") != "false" {
		t.Fatalf("HTTP Exec cancellation replay header = %q", cancelResponse.Header.Get("Idempotency-Replayed"))
	}
	var cancellingSession contracts.ExecStreamSession
	decodeHTTPJSON(t, cancelResponse, &cancellingSession)
	if cancellingSession.ID != httpCancelled.ID || cancellingSession.State != "closing" {
		t.Fatalf("HTTP-cancelled Exec session = %#v", cancellingSession)
	}
	waitStreamingRunnerEvent(t, fake.events, "cancel:wait-http-cancel")
	waitDataPlaneSessionState(
		t, relay, project.ID, string(httpCancelled.ID), "completed",
	)

	replayCancelRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/exec-streams/"+httpCancelled.ID+":cancel",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(replayCancelRequest, key.Credential, sandbox.Generation, "stream-http-cancel-request")
	replayCancelResponse := doHTTP(t, replayCancelRequest)
	assertHTTPStatus(t, replayCancelResponse, http.StatusAccepted)
	if replayCancelResponse.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("HTTP Exec cancellation replay header = %q", replayCancelResponse.Header.Get("Idempotency-Replayed"))
	}
	var replayedCancellingSession contracts.ExecStreamSession
	decodeHTTPJSON(t, replayCancelResponse, &replayedCancellingSession)
	if !reflect.DeepEqual(replayedCancellingSession, cancellingSession) {
		t.Fatalf(
			"HTTP Exec cancellation replay changed response: first=%#v replay=%#v",
			cancellingSession, replayedCancellingSession,
		)
	}

	newKeyCancelRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/exec-streams/"+httpCancelled.ID+":cancel",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(
		newKeyCancelRequest, key.Credential, sandbox.Generation,
		"stream-http-cancel-new-request",
	)
	newKeyCancelResponse := doHTTP(t, newKeyCancelRequest)
	assertHTTPStatus(t, newKeyCancelResponse, http.StatusAccepted)
	if newKeyCancelResponse.Header.Get("Idempotency-Replayed") != "false" {
		t.Fatalf(
			"new-key HTTP Exec cancellation replay header = %q",
			newKeyCancelResponse.Header.Get("Idempotency-Replayed"),
		)
	}
	var newKeyCancellingSession contracts.ExecStreamSession
	decodeHTTPJSON(t, newKeyCancelResponse, &newKeyCancellingSession)
	if newKeyCancellingSession.ID != httpCancelled.ID ||
		newKeyCancellingSession.State != "closed" {
		t.Fatalf("new-key HTTP Exec cancellation response = %#v", newKeyCancellingSession)
	}

	conflictingCancelRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL+"/v1/sandboxes/"+sandbox.ID+"/exec-streams/"+httpCancelled.ID+":cancel",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(
		conflictingCancelRequest, key.Credential, sandbox.Generation+1,
		"stream-http-cancel-request",
	)
	conflictingCancelResponse := doHTTP(t, conflictingCancelRequest)
	assertHTTPStatus(t, conflictingCancelResponse, http.StatusConflict)
	var conflictingCancelProblem contracts.Problem
	decodeHTTPJSON(t, conflictingCancelResponse, &conflictingCancelProblem)
	if conflictingCancelProblem.Code != "idempotency_conflict" {
		t.Fatalf("HTTP Exec cancellation conflict = %#v", conflictingCancelProblem)
	}

	detached := createStreamingExecSession(
		t, server.URL, key.Credential, sandbox, "", "stream-detached", "disconnect", 16,
	)
	detachedConnection := dialStreamingExec(t, detached, key.Credential, sandbox.Generation)
	if err := detachedConnection.Close(); err != nil {
		t.Fatal(err)
	}
	waitStreamingRunnerEvent(t, fake.events, "cancel:disconnect")
	reconnected := dialStreamingExec(t, detached, key.Credential, sandbox.Generation)
	detachedOutcome := readStreamingOutcome(t, reconnected, 0)
	if detachedOutcome.Kind != "cancelled" {
		t.Fatalf("detached outcome = %#v", detachedOutcome)
	}
	reconnected.Close()
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
		t.Fatal("streaming fake runner did not stop")
	}
}

type streamingExecFakeRunner struct {
	relay        *runnercontrol.PostgresFrameRelay
	runnerID     string
	connectionID string
	events       chan string
	mu           sync.Mutex
	operations   map[string]*streamingFakeOperation
}

type streamingFakeOperation struct {
	open         *runnerv1.ExecFrame
	command      string
	nextSequence uint64
	inputClosed  bool
}

func newStreamingExecFakeRunner(
	relay *runnercontrol.PostgresFrameRelay,
	runnerID string,
	connectionID string,
) *streamingExecFakeRunner {
	return &streamingExecFakeRunner{
		relay: relay, runnerID: runnerID, connectionID: connectionID,
		events: make(chan string, 32), operations: map[string]*streamingFakeOperation{},
	}
}

func (fake *streamingExecFakeRunner) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
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
			ctx, delivery.ID, fake.connectionID, now,
		); err != nil {
			return err
		}
		if err := fake.handle(ctx, delivery.Message, now); err != nil {
			return err
		}
	}
}

func (fake *streamingExecFakeRunner) handle(
	ctx context.Context,
	message *runnerv1.ControlPlaneToRunner,
	now time.Time,
) error {
	frame := message.GetExec()
	if frame == nil {
		return nil
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if open := frame.GetOpen(); open != nil {
		fake.operations[frame.OperationId] = &streamingFakeOperation{
			open: frame, command: open.GetShell(), nextSequence: 1,
		}
		return nil
	}
	operation := fake.operations[frame.OperationId]
	if operation == nil {
		return fmt.Errorf("streaming fake runner frame has no Open: %s", frame.OperationId)
	}
	switch {
	case frame.GetInput() != nil:
		if frame.GetInput().EndOfInput {
			operation.inputClosed = true
			fake.events <- "eof:" + operation.command
		} else {
			fake.events <- "input:" + operation.command + ":" + string(frame.GetInput().Data)
		}
	case frame.GetCredit() != nil:
		switch operation.command {
		case "stream-order":
			if !operation.inputClosed {
				return fmt.Errorf("streaming fake runner received credit before stdin EOF")
			}
			if err := fake.output(ctx, operation, runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, []byte("stdout:one"), now); err != nil {
				return err
			}
			if err := fake.output(ctx, operation, runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR, []byte("!"), now); err != nil {
				return err
			}
			return fake.terminal(ctx, operation, runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED, now)
		case "output-exhausted":
			if err := fake.output(ctx, operation, runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, []byte("part"), now); err != nil {
				return err
			}
			return fake.terminal(ctx, operation, runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED, now)
		}
	case frame.GetCancel() != nil:
		fake.events <- "cancel:" + operation.command
		return fake.terminal(ctx, operation, runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED, now)
	}
	return nil
}

func (fake *streamingExecFakeRunner) output(
	ctx context.Context,
	operation *streamingFakeOperation,
	channel runnerv1.ExecOutputChannel,
	data []byte,
	now time.Time,
) error {
	frame := operation.open
	sequence := operation.nextSequence
	operation.nextSequence++
	_, err := fake.relay.PersistInboundFrame(ctx, runnercontrol.InboundRelayFrame{
		RunnerID: fake.runnerID, ConnectionID: fake.connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
				Fence: frame.Fence, OperationId: frame.OperationId, StreamId: frame.StreamId,
				Sequence: sequence,
				Payload: &runnerv1.ExecFrame_Output{Output: &runnerv1.ExecOutput{
					Channel: channel, Data: bytes.Clone(data),
				}},
			}},
		},
	}, now)
	return err
}

func (fake *streamingExecFakeRunner) terminal(
	ctx context.Context,
	operation *streamingFakeOperation,
	kind runnerv1.ExecTerminalKind,
	now time.Time,
) error {
	frame := operation.open
	sequence := operation.nextSequence
	operation.nextSequence++
	terminal := &runnerv1.ExecTerminal{Kind: kind, ExitCode: -1}
	if kind == runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		terminal.ExitCode = 0
	}
	if kind == runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
		terminal.LimitBytes = frame.GetOpen().OutputLimitBytes
	}
	_, err := fake.relay.PersistInboundFrame(ctx, runnercontrol.InboundRelayFrame{
		RunnerID: fake.runnerID, ConnectionID: fake.connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
				Fence: frame.Fence, OperationId: frame.OperationId, StreamId: frame.StreamId,
				Sequence: sequence,
				Payload:  &runnerv1.ExecFrame_Terminal{Terminal: terminal},
			}},
		},
	}, now)
	return err
}

func createStreamingExecSession(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	command string,
	maximumOutputBytes int64,
) contracts.ExecStreamSession {
	t.Helper()
	response := createStreamingExecSessionResponse(
		t, baseURL, credential, sandbox, leaseID, idempotencyKey, command, maximumOutputBytes,
	)
	assertHTTPStatus(t, response, http.StatusCreated)
	var session contracts.ExecStreamSession
	decodeHTTPJSON(t, response, &session)
	return session
}

func createStreamingExecSessionResponse(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	command string,
	maximumOutputBytes int64,
) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"command":     map[string]any{"mode": "shell", "command": command},
		"environment": map[string]string{}, "deadlineMilliseconds": 5000,
		"maximumOutputBytes": maximumOutputBytes, "windowBytes": 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/v1/sandboxes/"+sandbox.ID+"/exec-streams",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(request, credential, sandbox.Generation, idempotencyKey)
	request.Header.Set("Content-Type", "application/json")
	if leaseID != "" {
		request.Header.Set("SecondBox-Lease-ID", leaseID)
	}
	return doHTTP(t, request)
}

func dialStreamingExec(
	t *testing.T,
	session contracts.ExecStreamSession,
	credential string,
	generation int64,
) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{"secondbox.exec.v1"}}
	connection, response, err := dialer.Dial(session.WebsocketURL, streamingExecHeaders(credential, generation))
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("dial streaming Exec: status=%d body=%s error=%v", response.StatusCode, body, err)
		}
		t.Fatal(err)
	}
	if connection.Subprotocol() != "secondbox.exec.v1" {
		connection.Close()
		t.Fatalf("streaming Exec subprotocol = %q", connection.Subprotocol())
	}
	return connection
}

func streamingExecHeaders(credential string, generation int64) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+credential)
	headers.Set("SecondBox-Generation", fmt.Sprintf("%d", generation))
	return headers
}

func assertStreamingOutput(
	t *testing.T,
	connection *websocket.Conn,
	sequence int64,
	channel string,
	expected []byte,
) {
	t.Helper()
	var frame struct {
		Type       string `json:"type"`
		Sequence   int64  `json:"sequence"`
		Stream     string `json:"stream"`
		DataBase64 string `json:"dataBase64"`
	}
	if err := connection.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(frame.DataBase64)
	if err != nil ||
		frame.Type != "output" ||
		frame.Sequence != sequence ||
		frame.Stream != channel ||
		!bytes.Equal(data, expected) {
		t.Fatalf("streaming output = %#v data=%q error=%v", frame, data, err)
	}
}

func readStreamingOutcome(
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
		t.Fatalf("streaming outcome frame = %#v", frame)
	}
	var outcome streamingExecOutcome
	if err := json.Unmarshal(frame.Outcome, &outcome); err != nil {
		t.Fatal(err)
	}
	return outcome
}

type streamingExecOutcome struct {
	Kind string `json:"kind"`
}

func waitStreamingRunnerEvent(t *testing.T, events <-chan string, expected string) {
	t.Helper()
	select {
	case event := <-events:
		if event != expected {
			t.Fatalf("streaming runner event = %q, want %q", event, expected)
		}
	case <-time.After(time.Second):
		t.Fatalf("streaming runner event %q was not observed", strings.TrimSpace(expected))
	}
}

func waitDataPlaneSessionState(
	t *testing.T,
	relay *runnercontrol.PostgresFrameRelay,
	projectID string,
	sessionID string,
	expected string,
) runnercontrol.DataPlaneSession {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session, err := relay.GetDataPlaneSession(t.Context(), projectID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.State == expected {
			return session
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("data-plane session %s did not reach state %q", sessionID, expected)
	return runnercontrol.DataPlaneSession{}
}
