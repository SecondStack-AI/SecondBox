package secondboxclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestExecStreamReplenishesCreditAfterExecutionDeadline(t *testing.T) {
	expiresAt := time.Now().Add(250 * time.Millisecond)
	upgrader := websocket.Upgrader{Subprotocols: []string{execStreamSubprotocol}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		if err := connection.SetReadDeadline(expiresAt.Add(2 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		time.Sleep(time.Until(expiresAt.Add(25 * time.Millisecond)))
		if err := connection.WriteJSON(ExecStreamFrame{StreamOutputFrame: &StreamOutputFrame{
			Type: "output", Sequence: 0, Stream: "stdout", DataBase64: "b2s=",
		}}); err != nil {
			t.Error(err)
			return
		}
		var credit ExecStreamFrame
		if err := connection.ReadJSON(&credit); err != nil {
			t.Errorf("read completion credit: %v", err)
			return
		}
		if credit.StreamCreditFrame == nil || credit.StreamCreditFrame.Bytes != 4096 || credit.StreamCreditFrame.Sequence != 0 {
			t.Errorf("completion credit = %#v", credit)
			return
		}
		if err := connection.WriteJSON(ExecStreamFrame{StreamOutcomeFrame: &StreamOutcomeFrame{
			Type: "outcome", Sequence: 1,
			Outcome: ExecOutcome{ExecExited: &ExecExited{Kind: "exited", ExitCode: 7}},
		}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := NewSecondBoxClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sandbox-1", Generation: 1})
	stream, err := handle.ConnectExecStream(t.Context(), ExecStreamSession{
		ID: "exec-1", SandboxID: "sandbox-1", Generation: 1,
		State: SessionStateOpen, Subprotocol: execStreamSubprotocol,
		WebsocketURL: "ws" + strings.TrimPrefix(server.URL, "http"), ExpiresAt: expiresAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if frame, err := stream.Receive(); err != nil || frame.StreamOutputFrame == nil {
		t.Fatalf("delayed output = %#v: %v", frame, err)
	}
	if err := stream.GrantOutput(4096); err != nil {
		t.Fatalf("credit during completion grace: %v", err)
	}
	frame, err := stream.Receive()
	if err != nil || frame.StreamOutcomeFrame == nil || frame.StreamOutcomeFrame.Outcome.ExecExited == nil || frame.StreamOutcomeFrame.Outcome.ExecExited.ExitCode != 7 {
		t.Fatalf("delayed outcome = %#v: %v", frame, err)
	}
}
