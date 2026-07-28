package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestControlClientUsesVsockConnectHandshake(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "control.sock")
	handshakes := make(chan string, 4)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	client := ControlClient{UDSPath: socketPath, Port: 2048, Timeout: 2 * time.Second}
	hb, err := client.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !hb.Healthy || hb.SandboxID != "agent-1" {
		t.Fatalf("heartbeat response = %#v", hb)
	}
	if got := <-handshakes; got != "CONNECT 2048" {
		t.Fatalf("handshake = %q", got)
	}
}

func TestControlClientWorkspaceAndBackupVerbs(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	client := ControlClient{UDSPath: socketPath, Port: 2048, Timeout: 2 * time.Second}
	list, err := client.ListWorkspace(context.Background(), "artifacts")
	if err != nil {
		t.Fatalf("list workspace: %v", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Path != "artifacts/report.txt" {
		t.Fatalf("list response = %#v", list)
	}
	data, err := client.ReadWorkspaceFile(context.Background(), "artifacts/report.txt", 1024)
	if err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if string(data) != "report" {
		t.Fatalf("read data = %q", string(data))
	}
	freeze, err := client.FreezeWorkspace(context.Background())
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if !freeze.Frozen {
		t.Fatalf("freeze response = %#v", freeze)
	}
	thaw, err := client.ThawWorkspace(context.Background())
	if err != nil {
		t.Fatalf("thaw: %v", err)
	}
	if thaw.Frozen {
		t.Fatalf("thaw response = %#v", thaw)
	}
}

func TestControlClientWorkspaceFileStreams(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	client := ControlClient{UDSPath: socketPath, Port: 2048, Timeout: time.Millisecond}
	rc, size, err := client.OpenWorkspaceFileStream(context.Background(), "artifacts/report.txt")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	data, err := io.ReadAll(rc)
	if closeErr := rc.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if size != int64(len("report")) || string(data) != "report" {
		t.Fatalf("stream size=%d data=%q", size, string(data))
	}

	payload := bytes.Repeat([]byte("streamed"), 1024)
	wantHash := fmt.Sprintf("%x", sha256.Sum256(payload))
	written, gotHash, err := client.PutWorkspaceFileStream(context.Background(), "artifacts/upload.bin", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("put stream: %v", err)
	}
	if written != int64(len(payload)) || gotHash != wantHash {
		t.Fatalf("put response size=%d sha=%s, want size=%d sha=%s", written, gotHash, len(payload), wantHash)
	}
}

func TestControlClientApplySecrets(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	client := ControlClient{UDSPath: socketPath, Port: 2048, Timeout: 2 * time.Second}
	err := client.ApplySecrets(context.Background(), SecretBundle{
		Env: map[string]string{"SECONDBOX_RUNTIME_CREDENTIAL_ID": "opaque-token"},
	})
	if err != nil {
		t.Fatalf("apply secrets: %v", err)
	}
}

func TestControlClientHardenPostRestore(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	client := ControlClient{UDSPath: socketPath, Port: 2048, Timeout: 2 * time.Second}
	if err := client.HardenPostRestore(context.Background(), time.Date(2026, 6, 24, 22, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("harden post restore: %v", err)
	}
}

func TestControlClientExecuteTool(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	client := ControlClient{UDSPath: socketPath, Port: 2048, Timeout: 2 * time.Second}
	resp, err := client.ExecuteTool(context.Background(), ToolExecRequest{
		Operation:     ToolOpExec,
		Command:       "sh",
		Args:          []string{"-c", "printf ok"},
		Cwd:           ".",
		Env:           map[string]string{"A": "B"},
		TimeoutMillis: 1000,
	})
	if err != nil {
		t.Fatalf("execute tool: %v", err)
	}
	if resp.Stdout != "ok" || resp.ExitCode != 0 {
		t.Fatalf("execute response = %+v", resp)
	}
	if got := <-handshakes; got != "CONNECT 2048" {
		t.Fatalf("handshake = %q", got)
	}
}

func TestControlClientExecuteToolSurfacesGuestError(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "control.sock")
	handshakes := make(chan string, 8)
	closeServer := startFakeControlServer(t, socketPath, handshakes)
	defer closeServer()

	client := ControlClient{UDSPath: socketPath, Port: 2048, Timeout: 2 * time.Second}
	resp, err := client.ExecuteTool(context.Background(), ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "../secret",
	})
	if err == nil || !strings.Contains(err.Error(), "path escapes workspace") {
		t.Fatalf("error = %v, response = %+v", err, resp)
	}
	if resp.Error != "path escapes workspace" {
		t.Fatalf("response = %+v", resp)
	}
}

func startFakeControlServer(t *testing.T, socketPath string, handshakes chan<- string) func() {
	t.Helper()
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{SandboxID: "agent-1", InstanceID: "vm-1", Healthy: true})
	})
	mux.HandleFunc("/workspace/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(WorkspaceListResponse{Entries: []WorkspaceEntry{{Path: "artifacts/report.txt", Type: "file", Size: 6}}})
	})
	mux.HandleFunc("/workspace/read", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		_, _ = w.Write([]byte("report"))
	})
	mux.HandleFunc("/workspace/write", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"size":   len(data),
			"sha256": fmt.Sprintf("%x", sha256.Sum256(data)),
		})
	})
	mux.HandleFunc("/workspace/freeze", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(BackupResponse{Frozen: true})
	})
	mux.HandleFunc("/workspace/thaw", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(BackupResponse{Frozen: false})
	})
	mux.HandleFunc("/secrets/apply", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"applied": true})
	})
	mux.HandleFunc("/restore/harden", func(w http.ResponseWriter, r *http.Request) {
		var req RestoreHardenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.HostTime == "" || req.EntropyBase64 == "" {
			http.Error(w, "invalid harden request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"hardened": true})
	})
	mux.HandleFunc("/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("guest-log\n"))
	})
	mux.HandleFunc("/tool/exec", func(w http.ResponseWriter, r *http.Request) {
		var req ToolExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req.Operation {
		case ToolOpExec:
			if req.Command != "sh" || len(req.Args) != 2 || req.Args[1] != "printf ok" || req.Cwd != "." || req.Env["A"] != "B" || req.TimeoutMillis != 1000 {
				http.Error(w, "invalid exec request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(ToolExecResponse{Stdout: "ok", ExitCode: 0})
		case ToolOpReadFile:
			if req.Path == "../secret" {
				_ = json.NewEncoder(w).Encode(ToolExecResponse{Error: "path escapes workspace"})
				return
			}
			_ = json.NewEncoder(w).Encode(ToolExecResponse{Content: "content"})
		default:
			http.Error(w, "unexpected operation", http.StatusBadRequest)
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				br := bufio.NewReader(conn)
				line, err := br.ReadString('\n')
				if err != nil {
					_ = conn.Close()
					return
				}
				handshakes <- strings.TrimSpace(line)
				if _, err := conn.Write([]byte("OK 3\n")); err != nil {
					_ = conn.Close()
					return
				}
				http.Serve(&singleConnListener{Conn: &bufferedConn{Conn: conn, reader: br}}, mux)
			}(conn)
		}
	}()
	return func() {
		_ = ln.Close()
		<-done
	}
}

type singleConnListener struct {
	Conn net.Conn
	used bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.used {
		return nil, net.ErrClosed
	}
	l.used = true
	return l.Conn, nil
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr { return l.Conn.LocalAddr() }

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
