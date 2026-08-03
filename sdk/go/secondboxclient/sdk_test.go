package secondboxclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRequestJSONUsesGeneratedOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/sandboxes/sandbox-1" {
			t.Fatalf("path = %q, want /v1/sandboxes/sandbox-1", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, sandboxJSON("sandbox-1", "ready"))
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var sandbox Sandbox
	if err := client.RequestJSON(t.Context(), "getSandbox", CallOptions{
		PathParameters: map[string]string{"sandboxId": "sandbox-1"},
	}, &sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.ID != "sandbox-1" || sandbox.State != SandboxStateReady {
		t.Fatalf("sandbox = %#v", sandbox)
	}
}

func TestRequestRejectsUnknownOperation(t *testing.T) {
	client, err := NewSecondBoxClient("https://secondbox.example", "token", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Request(t.Context(), "notAnOperation", CallOptions{})
	if err == nil || err.Error() != `SecondBox client unknown operation "notAnOperation"` {
		t.Fatalf("error = %v", err)
	}
}

func TestWaitOperationPollsUntilSucceeded(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		state := "running"
		if requests == 2 {
			state = "succeeded"
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"id":"operation-1","sandboxId":"sandbox-1","kind":"start","state":"`+state+`",
			"requestId":"request-1","createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"
		}`)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := client.WaitOperation(t.Context(), "operation-1", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if operation.State != OperationStateSucceeded || requests != 2 {
		t.Fatalf("operation state = %q, requests = %d", operation.State, requests)
	}
}

func TestWaitOperationReportsStructuredFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"id":"operation-1","sandboxId":"sandbox-1","kind":"start","state":"failed",
			"requestId":"request-1","error":{"type":"urn:secondbox:problem:state-conflict","title":"failed",
			"status":409,"code":"state_conflict","requestId":"request-1","retryable":false,"details":[]},
			"createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"
		}`)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.WaitOperation(t.Context(), "operation-1", time.Millisecond)
	failure, ok := err.(*OperationFailure)
	if !ok || failure.Operation.Error == nil || failure.Operation.Error.Code != ProblemCodeStateConflict {
		t.Fatalf("error = %#v", err)
	}
}

func TestWaitOperationHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"id":"operation-1","sandboxId":"sandbox-1","kind":"start","state":"running",
			"requestId":"request-1","createdAt":"2026-07-28T00:00:00Z","updatedAt":"2026-07-28T00:00:00Z"
		}`)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.WaitOperation(ctx, "operation-1", time.Second)
	if err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSandboxHandleRefreshesItsSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, sandboxJSON("sandbox-1", "ready"))
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sandbox-1", State: SandboxStateStarting})
	sandbox, err := handle.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.State != SandboxStateReady || handle.Snapshot().State != SandboxStateReady {
		t.Fatalf("sandbox = %#v, snapshot = %#v", sandbox, handle.Snapshot())
	}
}

func TestSandboxHandleNegotiatesExecStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/sandboxes/sandbox-1/exec-streams" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Header.Get("SecondBox-Generation") != "3" {
			t.Fatalf("generation = %q", request.Header.Get("SecondBox-Generation"))
		}
		if request.Header.Get("Idempotency-Key") != "request-1" {
			t.Fatalf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"id":"exec-1","sandboxId":"sandbox-1","generation":3,"state":"open",
			"websocketUrl":"wss://secondbox.example/exec-1","subprotocol":"secondbox.exec.v1",
			"expiresAt":"2026-07-28T00:01:00Z"
		}`)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sandbox-1", Generation: 3})
	session, err := handle.CreateExecStream(t.Context(), StreamingExecRequest{
		Command:              Command{ShellCommand: &ShellCommand{Mode: "shell", Command: "printf hello"}},
		Environment:          StringMap{},
		DeadlineMilliseconds: 500,
		MaximumOutputBytes:   4096,
		WindowBytes:          1024,
	}, "request-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "exec-1" || session.Subprotocol != "secondbox.exec.v1" {
		t.Fatalf("session = %#v", session)
	}
}

func TestSandboxHandleConnectsAndSequencesExecStream(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{execStreamSubprotocol}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token" ||
			request.Header.Get("X-SecondBox-Tenant-Ref") != "secondbox" ||
			request.Header.Get("X-SecondBox-Subject-Ref") != "secondbox-admin" ||
			request.Header.Get("SecondBox-Generation") != "3" {
			t.Errorf("stream headers = %#v", request.Header)
		}
		connection, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		var input ExecStreamFrame
		if err := connection.ReadJSON(&input); err != nil {
			t.Errorf("read stdin: %v", err)
			return
		}
		if input.StreamInputFrame == nil ||
			input.StreamInputFrame.Sequence != 0 ||
			input.StreamInputFrame.DataBase64 != base64.StdEncoding.EncodeToString([]byte("hello")) ||
			input.StreamInputFrame.EndOfInput {
			t.Errorf("stdin = %#v", input)
			return
		}
		var endInput ExecStreamFrame
		if err := connection.ReadJSON(&endInput); err != nil {
			t.Errorf("read end input: %v", err)
			return
		}
		if endInput.StreamInputFrame == nil ||
			endInput.StreamInputFrame.Sequence != 1 ||
			endInput.StreamInputFrame.DataBase64 != "" ||
			!endInput.StreamInputFrame.EndOfInput {
			t.Errorf("end input = %#v", endInput)
			return
		}
		var credit ExecStreamFrame
		if err := connection.ReadJSON(&credit); err != nil {
			t.Errorf("read credit: %v", err)
			return
		}
		if credit.StreamCreditFrame == nil ||
			credit.StreamCreditFrame.Sequence != 2 ||
			credit.StreamCreditFrame.Bytes != 4096 {
			t.Errorf("credit = %#v", credit)
			return
		}
		if err := connection.WriteJSON(ExecStreamFrame{StreamOutputFrame: &StreamOutputFrame{
			Type: "output", Sequence: 0, Stream: "stdout",
			DataBase64: base64.StdEncoding.EncodeToString([]byte("hello")),
		}}); err != nil {
			t.Errorf("write output: %v", err)
			return
		}
		if err := connection.WriteJSON(ExecStreamFrame{StreamOutcomeFrame: &StreamOutcomeFrame{
			Type: "outcome", Sequence: 1,
			Outcome: ExecOutcome{ExecExited: &ExecExited{Kind: "exited", ExitCode: 0}},
		}}); err != nil {
			t.Errorf("write outcome: %v", err)
		}
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sandbox-1", Generation: 3})
	session := ExecStreamSession{
		ID: "exec-1", SandboxID: "sandbox-1", Generation: 3,
		State: SessionStateOpen, Subprotocol: execStreamSubprotocol,
		WebsocketURL: "ws" + strings.TrimPrefix(server.URL, "http"),
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	stream, err := handle.ConnectExecStream(t.Context(), session, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := stream.SendInput([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if err := stream.SendInput([]byte("after EOF")); err == nil {
		t.Fatal("Exec stream accepted input after EOF")
	}
	if err := stream.GrantOutput(4096); err != nil {
		t.Fatal(err)
	}
	output, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if output.StreamOutputFrame == nil || output.StreamOutputFrame.Stream != "stdout" {
		t.Fatalf("output = %#v", output)
	}
	outcome, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StreamOutcomeFrame == nil ||
		outcome.StreamOutcomeFrame.Outcome.ExecExited == nil ||
		outcome.StreamOutcomeFrame.Outcome.ExecExited.ExitCode != 0 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if err := stream.SendInput(nil); err == nil {
		t.Fatal("terminal Exec stream accepted additional input")
	}
}

func TestSandboxHandleConnectsAndSequencesTerminal(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{terminalSubprotocol}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token" ||
			request.Header.Get("X-SecondBox-Tenant-Ref") != "secondbox" ||
			request.Header.Get("X-SecondBox-Subject-Ref") != "secondbox-admin" ||
			request.Header.Get("SecondBox-Generation") != "3" {
			t.Errorf("Terminal headers = %#v", request.Header)
		}
		connection, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		var credit TerminalFrame
		if err := connection.ReadJSON(&credit); err != nil {
			t.Errorf("read Terminal credit: %v", err)
			return
		}
		if credit.StreamCreditFrame == nil ||
			credit.StreamCreditFrame.Sequence != 4 ||
			credit.StreamCreditFrame.Bytes != 4096 {
			t.Errorf("Terminal credit = %#v", credit)
			return
		}
		var resize TerminalFrame
		if err := connection.ReadJSON(&resize); err != nil {
			t.Errorf("read Terminal resize: %v", err)
			return
		}
		if resize.TerminalResizeFrame == nil ||
			resize.TerminalResizeFrame.Sequence != 5 ||
			resize.TerminalResizeFrame.Rows != 40 ||
			resize.TerminalResizeFrame.Columns != 120 {
			t.Errorf("Terminal resize = %#v", resize)
			return
		}
		var input TerminalFrame
		if err := connection.ReadJSON(&input); err != nil {
			t.Errorf("read Terminal input: %v", err)
			return
		}
		if input.TerminalInputFrame == nil ||
			input.TerminalInputFrame.Sequence != 6 ||
			input.TerminalInputFrame.DataBase64 != base64.StdEncoding.EncodeToString(
				[]byte{0x00, 0x01, 0xfe, 0xff},
			) {
			t.Errorf("Terminal input = %#v", input)
			return
		}
		if err := connection.WriteJSON(TerminalFrame{TerminalOutputFrame: &TerminalOutputFrame{
			Type: "terminal_output", Sequence: 0,
			DataBase64: base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0xfe, 0xff}),
		}}); err != nil {
			t.Errorf("write Terminal output: %v", err)
			return
		}
		var cancel TerminalFrame
		if err := connection.ReadJSON(&cancel); err != nil {
			t.Errorf("read Terminal cancel: %v", err)
			return
		}
		if cancel.StreamCancelFrame == nil || cancel.StreamCancelFrame.Sequence != 7 {
			t.Errorf("Terminal cancel = %#v", cancel)
			return
		}
		if err := connection.WriteJSON(TerminalFrame{StreamOutcomeFrame: &StreamOutcomeFrame{
			Type: "outcome", Sequence: 1,
			Outcome: ExecOutcome{ExecCancelled: &ExecCancelled{Kind: "cancelled"}},
		}}); err != nil {
			t.Errorf("write Terminal outcome: %v", err)
		}
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sandbox-1", Generation: 3})
	session := TerminalSession{
		ID: "term-1", SandboxID: "sandbox-1", Generation: 3,
		State: SessionStateOpen, Subprotocol: terminalSubprotocol,
		NextClientSequence: 4,
		WebsocketURL:       "ws" + strings.TrimPrefix(server.URL, "http"),
		ExpiresAt:          time.Now().Add(time.Minute),
	}
	terminal, err := handle.ConnectTerminal(t.Context(), session, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := terminal.GrantOutput(4096); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	if err := terminal.SendInput([]byte{0x00, 0x01, 0xfe, 0xff}); err != nil {
		t.Fatal(err)
	}
	output, err := terminal.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if output.TerminalOutputFrame == nil {
		t.Fatalf("Terminal output = %#v", output)
	}
	if err := terminal.Cancel(); err != nil {
		t.Fatal(err)
	}
	outcome, err := terminal.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StreamOutcomeFrame == nil ||
		outcome.StreamOutcomeFrame.Outcome.ExecCancelled == nil {
		t.Fatalf("Terminal outcome = %#v", outcome)
	}
	if err := terminal.SendInput([]byte("after terminal")); err == nil {
		t.Fatal("Terminal accepted input after outcome")
	}
}

func TestSandboxHandleGetsAndCancelsStableTerminal(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/sandboxes/sandbox-1/terminals/term-1" ||
			request.Header.Get("SecondBox-Generation") != "3" {
			t.Errorf("Terminal request = %s %s %#v", request.Method, request.URL.Path, request.Header)
		}
		state := "detached"
		if request.Method == http.MethodDelete {
			state = "closing"
			if request.Header.Get("Idempotency-Key") != "cancel-terminal-1" {
				t.Errorf("Terminal cancellation headers = %#v", request.Header)
			}
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(response, `{
			"id":"term-1","sandboxId":"sandbox-1","generation":3,"state":%q,
			"websocketUrl":"wss://secondbox.example/term-1","subprotocol":"secondbox.terminal.v1",
			"expiresAt":"2026-07-28T00:01:00Z"
		}`, state)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sandbox-1", Generation: 3})
	detached, err := handle.GetTerminal(t.Context(), "term-1")
	if err != nil {
		t.Fatal(err)
	}
	closing, err := handle.CancelTerminal(t.Context(), "term-1", "cancel-terminal-1")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || detached.State != SessionStateDetached || closing.State != SessionStateClosing {
		t.Fatalf("Terminal descriptors detached=%#v closing=%#v requests=%d", detached, closing, requests)
	}
}

func sandboxJSON(id, state string) string {
	value := map[string]any{
		"id": id, "projectId": "project-1", "profile": "default", "profileRevisionId": "profile-revision-1",
		"state": state, "desiredState": "running", "generation": 1,
		"workspace": map[string]any{
			"id": "workspace-1", "generation": 1, "state": "ready", "sizeBytes": 1073741824,
			"createdAt": "2026-07-28T00:00:00Z", "updatedAt": "2026-07-28T00:00:00Z",
		},
		"metadata": map[string]string{}, "revision": 1,
		"createdAt": "2026-07-28T00:00:00Z", "updatedAt": "2026-07-28T00:00:00Z",
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
