package secondboxclient

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
)

func TestPortTunnelWriteSplitsIntoBoundedMessages(t *testing.T) {
	received := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		connection.SetReadLimit(contracts.PortTunnelMaximumMessageBytes)
		for {
			_, payload, err := connection.ReadMessage()
			if err != nil {
				close(received)
				return
			}
			received <- payload
		}
	}))
	defer server.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &PortTunnel{websocket: connection}
	payload := make([]byte, 2*contracts.PortTunnelMaximumMessageBytes+5)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	if written, err := tunnel.Write(payload); err != nil || written != len(payload) {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	var joined []byte
	var sizes []int
	for message := range received {
		sizes = append(sizes, len(message))
		joined = append(joined, message...)
	}
	if len(sizes) != 3 || sizes[0] != contracts.PortTunnelMaximumMessageBytes || sizes[2] != 5 {
		t.Fatalf("message sizes = %v", sizes)
	}
	if !bytes.Equal(joined, payload) {
		t.Fatal("split messages do not reassemble the payload")
	}
}
