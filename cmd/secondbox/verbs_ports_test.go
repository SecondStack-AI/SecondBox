package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

type forwardReadyWriter struct{ ready chan struct{} }

func (writer forwardReadyWriter) Write(content []byte) (int, error) {
	close(writer.ready)
	return len(content), nil
}

func TestPortsForwardLocalWebSocketAndCleanup(t *testing.T) {
	var created, closed, attached, released atomic.Int32
	var server *httptest.Server
	upgrader := websocket.Upgrader{Subprotocols: []string{"secondbox.port.v1"}}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			if !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "secondbox.port.token.credential-") {
				t.Error("missing tunnel credential")
			}
			connection, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			attached.Add(1)
			defer connection.Close()
			for {
				kind, content, err := connection.ReadMessage()
				if err != nil {
					return
				}
				if err := connection.WriteMessage(kind, content); err != nil {
					return
				}
			}
		case r.URL.Path == "/v1/profiles/durable-coding":
			_, _ = io.WriteString(w, `{"name":"durable-coding","currentRevision":{"id":"profile-revision-1","spec":{"ports":[{"name":"development-http","port":3000,"protocol":"http","maximumSessionSeconds":3600}]}}}`)
		case strings.HasSuffix(r.URL.Path, "/leases") && r.Method == "POST":
			assertVerbHeaders(t, r, false, true, true)
			if err := json.NewEncoder(w).Encode(sb.Lease{ID: "lease_forward", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
				t.Error(err)
			}
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/leases/"):
			assertVerbHeaders(t, r, false, false, true)
			released.Add(1)
			w.WriteHeader(204)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/port-sessions"):
			assertVerbHeaders(t, r, false, true, true)
			if r.Header.Get("SecondBox-Lease-ID") != "lease_forward" {
				t.Error("port missing Lease")
			}
			var request sb.CreatePortSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Name != "development-http" || request.DurationSeconds != 3600 {
				t.Errorf("request=%#v", request)
			}
			id := created.Add(1)
			if err := json.NewEncoder(w).Encode(sb.PortSession{ID: fmt.Sprintf("port_%d", id), SandboxID: "sbx_test1", Generation: 4, State: "open", Transport: "proxied", Name: request.Name, ExpiresAt: time.Now().Add(time.Hour), Endpoint: strings.Replace(server.URL, "http://", "ws://", 1) + "/tunnel#credential-" + strconv.Itoa(int(id))}); err != nil {
				t.Error(err)
			}
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/port-sessions/"):
			assertVerbHeaders(t, r, false, false, true)
			closed.Add(1)
			w.WriteHeader(204)
		case r.URL.Path == "/v1/sandboxes":
			_, _ = io.WriteString(w, `{"items":[`+execSandboxJSON+`]}`)
		case r.URL.Path == "/v1/sandboxes/sbx_test1":
			_, _ = io.WriteString(w, execSandboxJSON)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	port := reserved.Addr().(*net.TCPAddr).Port
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runPortForwardVerb(ctx, verbTestSession(server), []string{"mybox", fmt.Sprintf("%d:3000", port)}, forwardReadyWriter{ready}, server.Client())
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("forward before ready: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for i := 0; i < 2; i++ {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		content := []byte(fmt.Sprintf("connection %d", i))
		if _, err := connection.Write(content); err != nil {
			t.Fatal(err)
		}
		echoed := make([]byte, len(content))
		if _, err := io.ReadFull(connection, echoed); err != nil {
			t.Fatal(err)
		}
		if string(echoed) != string(content) {
			t.Errorf("echo=%q", echoed)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("forward result=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forward did not clean up")
	}
	if created.Load() != 2 || attached.Load() != 2 || closed.Load() != 2 || released.Load() != 1 {
		t.Errorf("created=%d attached=%d closed=%d released=%d", created.Load(), attached.Load(), closed.Load(), released.Load())
	}
}

func TestPortsForwardRejectsInvalidPorts(t *testing.T) {
	for _, value := range []string{"0", "-1", "65536", "x", "80:0", "1:2:3"} {
		if _, _, err := parseForwardPorts(value); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
	local, remote, err := parseForwardPorts("3000")
	if err != nil || local != 3000 || remote != 3000 {
		t.Fatalf("ports=%d:%d err=%v", local, remote, err)
	}
}
