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
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

func TestSandboxShellUsesRawTTYResizeBinaryIOAndRestoresOnExit(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.terminal.v1"}}
	resizeEvents := make(chan struct{}, 1)
	inputReader, inputWriter := io.Pipe()
	defer inputReader.Close()
	defer inputWriter.Close()
	controller := &fakeShellTerminalController{
		terminal: true,
		sizes:    [][2]int{{80, 24}, {120, 40}},
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/sandboxes/sandbox-1/terminals":
			if request.Header.Get("Authorization") != "Bearer token" ||
				request.Header.Get("SecondBox-Generation") != "3" ||
				request.Header.Get("SecondBox-Lease-ID") != "lease-1" ||
				request.Header.Get("Idempotency-Key") != "sandbox-shell-command" {
				t.Errorf("Terminal creation headers = %#v", request.Header)
			}
			var creation secondboxclient.CreateTerminalRequest
			if err := json.NewDecoder(request.Body).Decode(&creation); err != nil {
				t.Errorf("decode Terminal creation: %v", err)
				return
			}
			if creation.Rows != 24 || creation.Columns != 80 ||
				creation.Command.ShellCommand == nil ||
				creation.Command.ShellCommand.Command != "/bin/bash" {
				t.Errorf("Terminal creation = %#v", creation)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{
				"id":"term-1","sandboxId":"sandbox-1","generation":3,"state":"open",
				"websocketUrl":"%s/attach","subprotocol":"secondbox.terminal.v1",
				"nextClientSequence":0,
				"expiresAt":"%s"
			}`,
				"ws"+server.URL[len("http"):],
				time.Now().Add(time.Minute).Format(time.RFC3339Nano),
			)
		case request.Method == http.MethodGet && request.URL.Path == "/attach":
			connection, err := upgrader.Upgrade(response, request, nil)
			if err != nil {
				t.Errorf("upgrade Terminal: %v", err)
				return
			}
			defer connection.Close()
			assertCLITerminalCredit(t, connection, 0, defaultShellCreditBytes)
			resizeEvents <- struct{}{}
			assertCLITerminalResize(t, connection, 1, 40, 120)
			if _, err := inputWriter.Write([]byte{0x00, 0x01, 0xfe, 0xff}); err != nil {
				t.Errorf("write local Terminal input: %v", err)
				return
			}
			assertCLITerminalInput(t, connection, 2, []byte{0x00, 0x01, 0xfe, 0xff})
			if err := connection.WriteJSON(secondboxclient.TerminalFrame{
				TerminalOutputFrame: &secondboxclient.TerminalOutputFrame{
					Type: "terminal_output", Sequence: 0,
					DataBase64: base64.StdEncoding.EncodeToString([]byte{0x00, 'o', 'k', 0xff}),
				},
			}); err != nil {
				t.Errorf("write Terminal output: %v", err)
				return
			}
			assertCLITerminalCredit(t, connection, 3, 4)
			if err := connection.WriteJSON(secondboxclient.TerminalFrame{
				StreamOutcomeFrame: &secondboxclient.StreamOutcomeFrame{
					Type: "outcome", Sequence: 1,
					Outcome: secondboxclient.ExecOutcome{ExecExited: &secondboxclient.ExecExited{
						Kind: "exited", ExitCode: 0,
					}},
				},
			}); err != nil {
				t.Errorf("write Terminal outcome: %v", err)
			}
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	err := runSandboxShellCommand(
		t.Context(), server.URL, "token",
		[]string{
			"--sandbox", "sandbox-1", "--generation", "3",
			"--lease", "lease-1", "--idempotency-key", "sandbox-shell-command",
			"--command", "/bin/bash",
		},
		sandboxShellEnvironment{
			input: inputReader, output: &output, inputFD: 10, outputFD: 11,
			terminal: controller, resizeEvents: resizeEvents,
			httpClient: server.Client(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), []byte{0x00, 'o', 'k', 0xff}) {
		t.Fatalf("Terminal output = %x", output.Bytes())
	}
	if controller.makeRawCalls != 1 || controller.restoreCalls != 1 {
		t.Fatalf(
			"Terminal raw lifecycle make=%d restore=%d",
			controller.makeRawCalls, controller.restoreCalls,
		)
	}
}

func TestSandboxShellCancellationRestoresRawTTY(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.terminal.v1"}}
	controller := &fakeShellTerminalController{
		terminal: true,
		sizes:    [][2]int{{80, 24}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancelObserved := make(chan struct{})
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost:
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{
				"id":"term-2","sandboxId":"sandbox-1","generation":3,"state":"open",
				"websocketUrl":"%s/attach","subprotocol":"secondbox.terminal.v1",
				"nextClientSequence":0,
				"expiresAt":"%s"
			}`,
				"ws"+server.URL[len("http"):],
				time.Now().Add(time.Minute).Format(time.RFC3339Nano),
			)
		case request.Method == http.MethodGet && request.URL.Path == "/attach":
			connection, err := upgrader.Upgrade(response, request, nil)
			if err != nil {
				t.Errorf("upgrade Terminal: %v", err)
				return
			}
			defer connection.Close()
			assertCLITerminalCredit(t, connection, 0, defaultShellCreditBytes)
			cancel()
			var frame secondboxclient.TerminalFrame
			if err := connection.ReadJSON(&frame); err == nil && frame.StreamCancelFrame != nil {
				close(cancelObserved)
			}
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	err := runSandboxShellCommand(
		ctx, server.URL, "token",
		[]string{
			"--sandbox", "sandbox-1", "--generation", "3",
			"--lease", "lease-2", "--idempotency-key", "sandbox-shell-cancel",
		},
		sandboxShellEnvironment{
			input:  io.LimitReader(&blockingReader{done: ctx.Done()}, 1),
			output: io.Discard, inputFD: 10, outputFD: 11,
			terminal: controller, httpClient: server.Client(),
		},
	)
	if !errorsIsContextCancellation(err) {
		t.Fatalf("sandbox shell cancellation error = %v", err)
	}
	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("sandbox shell did not send ordered Terminal cancellation")
	}
	if controller.restoreCalls != 1 {
		t.Fatalf("Terminal restore calls after cancellation = %d", controller.restoreCalls)
	}
}

func TestSandboxShellReconnectsStableSessionAtRetainedSequence(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.terminal.v1"}}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/sandboxes/sandbox-1/terminals/term-3":
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{
				"id":"term-3","sandboxId":"sandbox-1","generation":3,"state":"detached",
				"websocketUrl":"%s/attach","subprotocol":"secondbox.terminal.v1",
				"nextClientSequence":5,
				"expiresAt":"%s"
			}`,
				"ws"+server.URL[len("http"):],
				time.Now().Add(time.Minute).Format(time.RFC3339Nano),
			)
		case request.Method == http.MethodGet && request.URL.Path == "/attach":
			connection, err := upgrader.Upgrade(response, request, nil)
			if err != nil {
				t.Errorf("upgrade Terminal reconnect: %v", err)
				return
			}
			defer connection.Close()
			assertCLITerminalCredit(t, connection, 5, defaultShellCreditBytes)
			assertCLITerminalInput(t, connection, 6, []byte{0x04})
			if err := connection.WriteJSON(secondboxclient.TerminalFrame{
				StreamOutcomeFrame: &secondboxclient.StreamOutcomeFrame{
					Type: "outcome", Sequence: 0,
					Outcome: secondboxclient.ExecOutcome{ExecExited: &secondboxclient.ExecExited{
						Kind: "exited", ExitCode: 0,
					}},
				},
			}); err != nil {
				t.Errorf("write reconnected Terminal outcome: %v", err)
			}
		case request.Method == http.MethodPost:
			t.Errorf("Terminal reconnect created a replacement session: %s", request.URL.Path)
			http.Error(response, "unexpected create", http.StatusInternalServerError)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	err := runSandboxShellCommand(
		t.Context(), server.URL, "token",
		[]string{
			"--sandbox", "sandbox-1", "--generation", "3",
			"--session", "term-3",
		},
		sandboxShellEnvironment{
			input: strings.NewReader(""), output: io.Discard, inputFD: -1, outputFD: -1,
			terminal: &fakeShellTerminalController{}, httpClient: server.Client(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

type fakeShellTerminalController struct {
	mu           sync.Mutex
	terminal     bool
	sizes        [][2]int
	makeRawCalls int
	restoreCalls int
}

func (controller *fakeShellTerminalController) IsTerminal(int) bool {
	return controller.terminal
}

func (controller *fakeShellTerminalController) MakeRaw(int) (any, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.makeRawCalls++
	return struct{}{}, nil
}

func (controller *fakeShellTerminalController) Restore(int, any) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.restoreCalls++
	return nil
}

func (controller *fakeShellTerminalController) GetSize(int) (int, int, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.sizes) == 0 {
		return 0, 0, fmt.Errorf("no test Terminal size")
	}
	size := controller.sizes[0]
	controller.sizes = controller.sizes[1:]
	return size[0], size[1], nil
}

type blockingReader struct {
	done <-chan struct{}
}

func (reader *blockingReader) Read([]byte) (int, error) {
	<-reader.done
	return 0, io.EOF
}

func assertCLITerminalCredit(
	t *testing.T,
	connection *websocket.Conn,
	sequence int64,
	bytes int64,
) {
	t.Helper()
	var frame secondboxclient.TerminalFrame
	if err := connection.ReadJSON(&frame); err != nil {
		t.Errorf("read Terminal credit: %v", err)
		return
	}
	if frame.StreamCreditFrame == nil ||
		frame.StreamCreditFrame.Sequence != sequence ||
		frame.StreamCreditFrame.Bytes != bytes {
		t.Errorf("Terminal credit = %#v", frame)
	}
}

func assertCLITerminalResize(
	t *testing.T,
	connection *websocket.Conn,
	sequence int64,
	rows int,
	columns int,
) {
	t.Helper()
	var frame secondboxclient.TerminalFrame
	if err := connection.ReadJSON(&frame); err != nil {
		t.Errorf("read Terminal resize: %v", err)
		return
	}
	if frame.TerminalResizeFrame == nil ||
		frame.TerminalResizeFrame.Sequence != sequence ||
		frame.TerminalResizeFrame.Rows != rows ||
		frame.TerminalResizeFrame.Columns != columns {
		t.Errorf("Terminal resize = %#v", frame)
	}
}

func assertCLITerminalInput(
	t *testing.T,
	connection *websocket.Conn,
	sequence int64,
	data []byte,
) {
	t.Helper()
	var frame secondboxclient.TerminalFrame
	if err := connection.ReadJSON(&frame); err != nil {
		t.Errorf("read Terminal input: %v", err)
		return
	}
	if frame.TerminalInputFrame == nil ||
		frame.TerminalInputFrame.Sequence != sequence ||
		frame.TerminalInputFrame.DataBase64 != base64.StdEncoding.EncodeToString(data) {
		t.Errorf("Terminal input = %#v", frame)
	}
}

func errorsIsContextCancellation(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled"))
}
