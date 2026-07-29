package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

func TestExecStreamCommandPumpsSequencedJSONLFrames(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.exec.v1"}}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/sandboxes/sandbox-1/exec-streams":
			if request.Header.Get("Authorization") != "Bearer token" ||
				request.Header.Get("SecondBox-Generation") != "3" ||
				request.Header.Get("Idempotency-Key") != "exec-stream-command" {
				t.Errorf("create headers = %#v", request.Header)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{
				"id":"exec-1","sandboxId":"sandbox-1","generation":3,"state":"open",
				"websocketUrl":"%s/attach","subprotocol":"secondbox.exec.v1",
				"expiresAt":"%s"
			}`,
				"ws"+strings.TrimPrefix(server.URL, "http"),
				time.Now().Add(time.Minute).Format(time.RFC3339Nano),
			)
		case request.Method == http.MethodGet && request.URL.Path == "/attach":
			connection, err := upgrader.Upgrade(response, request, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			assertCLIExecInputFrame(t, connection, "stdin", 0, []byte("hello"), false, 0)
			assertCLIExecInputFrame(t, connection, "stdin", 1, nil, true, 0)
			assertCLIExecInputFrame(t, connection, "credit", 2, nil, false, 4096)
			assertCLIExecInputFrame(t, connection, "cancel", 3, nil, false, 0)
			if err := connection.WriteJSON(secondboxclient.ExecStreamFrame{
				StreamOutputFrame: &secondboxclient.StreamOutputFrame{
					Type: "output", Sequence: 0, Stream: "stdout",
					DataBase64: base64.StdEncoding.EncodeToString([]byte("out")),
				},
			}); err != nil {
				t.Errorf("write stdout: %v", err)
				return
			}
			if err := connection.WriteJSON(secondboxclient.ExecStreamFrame{
				StreamOutputFrame: &secondboxclient.StreamOutputFrame{
					Type: "output", Sequence: 1, Stream: "stderr",
					DataBase64: base64.StdEncoding.EncodeToString([]byte("err")),
				},
			}); err != nil {
				t.Errorf("write stderr: %v", err)
				return
			}
			if err := connection.WriteJSON(secondboxclient.ExecStreamFrame{
				StreamOutcomeFrame: &secondboxclient.StreamOutcomeFrame{
					Type: "outcome", Sequence: 2,
					Outcome: secondboxclient.ExecOutcome{
						ExecCancelled: &secondboxclient.ExecCancelled{Kind: "cancelled"},
					},
				},
			}); err != nil {
				t.Errorf("write outcome: %v", err)
			}
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	requestPath := filepath.Join(t.TempDir(), "stream-request.json")
	if err := os.WriteFile(requestPath, []byte(`{
		"command":{"mode":"shell","command":"read value"},
		"environment":{},"deadlineMilliseconds":5000,
		"maximumOutputBytes":4096,"windowBytes":4096
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader(
		`{"type":"stdin","dataBase64":"aGVsbG8=","endOfInput":false}` + "\n" +
			`{"type":"stdin","dataBase64":"","endOfInput":true}` + "\n" +
			`{"type":"credit","bytes":4096}` + "\n" +
			`{"type":"cancel"}` + "\n",
	)
	var output bytes.Buffer
	err := runExecStreamCommand(
		t.Context(), server.URL, "token", "tenant", "subject",
		[]string{
			"--sandbox", "sandbox-1",
			"--generation", "3",
			"--idempotency-key", "exec-stream-command",
			"--request", requestPath,
		},
		input, &output, server.Client(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var stdout secondboxclient.ExecStreamFrame
	if err := decoder.Decode(&stdout); err != nil {
		t.Fatal(err)
	}
	var stderr secondboxclient.ExecStreamFrame
	if err := decoder.Decode(&stderr); err != nil {
		t.Fatal(err)
	}
	var outcome secondboxclient.ExecStreamFrame
	if err := decoder.Decode(&outcome); err != nil {
		t.Fatal(err)
	}
	if stdout.StreamOutputFrame == nil || stdout.StreamOutputFrame.Stream != "stdout" ||
		stderr.StreamOutputFrame == nil || stderr.StreamOutputFrame.Stream != "stderr" ||
		outcome.StreamOutcomeFrame == nil ||
		outcome.StreamOutcomeFrame.Outcome.ExecCancelled == nil {
		t.Fatalf("JSONL output = %#v %#v %#v", stdout, stderr, outcome)
	}
}

func assertCLIExecInputFrame(
	t *testing.T,
	connection *websocket.Conn,
	kind string,
	sequence int64,
	data []byte,
	endOfInput bool,
	credit int64,
) {
	t.Helper()
	var frame secondboxclient.ExecStreamFrame
	if err := connection.ReadJSON(&frame); err != nil {
		t.Errorf("read %s frame: %v", kind, err)
		return
	}
	switch kind {
	case "stdin":
		if frame.StreamInputFrame == nil ||
			frame.StreamInputFrame.Sequence != sequence ||
			frame.StreamInputFrame.DataBase64 != base64.StdEncoding.EncodeToString(data) ||
			frame.StreamInputFrame.EndOfInput != endOfInput {
			t.Errorf("stdin frame = %#v", frame)
		}
	case "credit":
		if frame.StreamCreditFrame == nil ||
			frame.StreamCreditFrame.Sequence != sequence ||
			frame.StreamCreditFrame.Bytes != credit {
			t.Errorf("credit frame = %#v", frame)
		}
	case "cancel":
		if frame.StreamCancelFrame == nil || frame.StreamCancelFrame.Sequence != sequence {
			t.Errorf("cancel frame = %#v", frame)
		}
	}
}
