package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDecodePublicExecClientFrameRequiresExplicitOrderedInputClosure(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		payload   string
		wantData  []byte
		wantEnd   bool
		wantError bool
	}{
		{
			name:     "stream bytes",
			payload:  `{"type":"stdin","sequence":0,"dataBase64":"YWJj","endOfInput":false}`,
			wantData: []byte("abc"),
		},
		{
			name:     "final bytes",
			payload:  `{"type":"stdin","sequence":1,"dataBase64":"YWJj","endOfInput":true}`,
			wantData: []byte("abc"),
			wantEnd:  true,
		},
		{
			name:     "empty EOF",
			payload:  `{"type":"stdin","sequence":1,"dataBase64":"","endOfInput":true}`,
			wantData: []byte{},
			wantEnd:  true,
		},
		{
			name:      "missing EOF declaration",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"YWJj"}`,
			wantError: true,
		},
		{
			name:      "empty non EOF",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"","endOfInput":false}`,
			wantError: true,
		},
		{
			name:      "non canonical base64",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"Zh==","endOfInput":false}`,
			wantError: true,
		},
		{
			name:      "unknown field",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"YQ==","endOfInput":false,"extra":true}`,
			wantError: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			frame, err := decodePublicExecClientFrame([]byte(testCase.payload))
			if testCase.wantError {
				if err == nil {
					t.Fatalf("decodePublicExecClientFrame(%s) unexpectedly succeeded", testCase.payload)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodePublicExecClientFrame(%s): %v", testCase.payload, err)
			}
			if !bytes.Equal(frame.Input, testCase.wantData) || frame.EndInput != testCase.wantEnd {
				t.Fatalf("decoded frame = %#v", frame)
			}
		})
	}
}

// A peer that closes first is answered by the connection's default close
// handler, which runs inside the streaming handler's own read goroutine. The
// handler's close write then finds the close frame already sent, and that is
// the handshake completing rather than failing.
func TestWebSocketCloseWriteToleratesAnAlreadyAnsweredPeerClose(t *testing.T) {
	closeResults := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			closeResults <- err
			return
		}
		defer connection.Close()
		// The default close handler writes the answering close frame before
		// this read reports the peer's closure, so the close write below is
		// guaranteed to find one already sent.
		if _, _, err := connection.ReadMessage(); !websocket.IsCloseError(
			err, websocket.CloseNormalClosure,
		) {
			closeResults <- fmt.Errorf("peer close read = %v", err)
			return
		}
		closeResults <- writeWebSocketClose(
			connection, "terminal outcome delivered", time.Now().Add(time.Second),
		)
	}))
	defer server.Close()

	connection, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "peer closed first"),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatalf("peer close: %v", err)
	}
	select {
	case err := <-closeResults:
		if err != nil {
			t.Fatalf("close write after an answered peer close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not report its close write")
	}
}

// The ordinary order still has to deliver: a close write that races nothing
// reaches the peer as a normal closure.
func TestWebSocketCloseWriteDeliversNormalClosureToThePeer(t *testing.T) {
	closeResults := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			closeResults <- err
			return
		}
		defer connection.Close()
		closeResults <- writeWebSocketClose(
			connection, "port tunnel completed", time.Now().Add(time.Second),
		)
	}))
	defer server.Close()

	connection, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _, err = connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) ||
		closeErr.Code != websocket.CloseNormalClosure ||
		closeErr.Text != "port tunnel completed" {
		t.Fatalf("peer read = %v, want a normal closure carrying the reason", err)
	}
	if err := <-closeResults; err != nil {
		t.Fatalf("close write: %v", err)
	}
}

func TestExecTerminalDeliveryCompletesWebSocketCloseHandshake(t *testing.T) {
	handshakeResult := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			handshakeResult <- err
			return
		}
		defer connection.Close()
		readErrors := make(chan error, 1)
		go func() {
			_, _, readErr := connection.ReadMessage()
			readErrors <- readErr
		}()
		if err := connection.WriteJSON(map[string]string{"type": "outcome"}); err != nil {
			handshakeResult <- err
			return
		}
		handshakeResult <- finishExecWebSocketCloseHandshake(connection, readErrors)
	}))
	defer server.Close()

	connection, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var outcome map[string]string
	if err := connection.ReadJSON(&outcome); err != nil {
		t.Fatalf("terminal outcome read: %v", err)
	}
	if outcome["type"] != "outcome" {
		t.Fatalf("terminal outcome = %#v", outcome)
	}
	select {
	case err := <-handshakeResult:
		t.Fatalf("server closed before peer close response: %v", err)
	default:
	}
	if err := connection.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "outcome received"),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatalf("peer close response: %v", err)
	}
	select {
	case err := <-handshakeResult:
		if err != nil {
			t.Fatalf("close handshake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not complete the WebSocket close handshake")
	}
}
