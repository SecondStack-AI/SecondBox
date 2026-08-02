package api

import (
	"bytes"
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
