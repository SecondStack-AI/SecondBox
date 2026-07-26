package microvm

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-manager/internal/config"
)

func TestFirecrackerAPIClientSnapshotAndMMDS(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "firecracker.sock")
	seen := make(chan apiCall, 8)
	closeServer := startFakeFirecrackerAPIServer(t, socketPath, seen)
	defer closeServer()

	client := FirecrackerAPIClient{SocketPath: socketPath, Timeout: 2 * time.Second}
	ctx := context.Background()
	if err := client.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := client.CreateFullSnapshot(ctx, "/snap/state", "/snap/mem"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := client.ConfigureMMDSV2(ctx, []string{"eth0", ""}); err != nil {
		t.Fatalf("mmds config: %v", err)
	}
	if err := client.PutMMDS(ctx, map[string]any{"agentId": "agent-1"}); err != nil {
		t.Fatalf("mmds data: %v", err)
	}
	if err := client.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}

	calls := drainAPICalls(seen, 5)
	if calls[0].Path != "/vm" || calls[0].Method != http.MethodPatch || calls[0].Body["state"] != "Paused" {
		t.Fatalf("pause call = %#v", calls[0])
	}
	if calls[1].Path != "/snapshot/create" || calls[1].Body["snapshot_type"] != "Full" || calls[1].Body["snapshot_path"] != "/snap/state" || calls[1].Body["mem_file_path"] != "/snap/mem" {
		t.Fatalf("snapshot call = %#v", calls[1])
	}
	if calls[2].Path != "/mmds/config" || calls[2].Body["version"] != "V2" {
		t.Fatalf("mmds config call = %#v", calls[2])
	}
	if calls[3].Path != "/mmds" || calls[3].Body["agentId"] != "agent-1" {
		t.Fatalf("mmds call = %#v", calls[3])
	}
	if calls[4].Path != "/vm" || calls[4].Method != http.MethodPatch || calls[4].Body["state"] != "Resumed" {
		t.Fatalf("resume call = %#v", calls[4])
	}
}

func TestFirecrackerAPIClientLoadSnapshotWithOverrides(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "firecracker.sock")
	seen := make(chan apiCall, 2)
	closeServer := startFakeFirecrackerAPIServer(t, socketPath, seen)
	defer closeServer()

	client := FirecrackerAPIClient{SocketPath: socketPath, Timeout: 2 * time.Second}
	err := client.LoadSnapshotWithOptions(context.Background(), snapshotLoadRequest{
		SnapshotPath: "/snap/state",
		MemBackend:   &memoryBackend{BackendPath: "/snap/mem", BackendType: "File"},
		ResumeVM:     true,
		NetworkOverrides: []networkOverride{{
			IfaceID:     "eth0",
			HostDevName: "agfc123",
		}},
		VsockOverride: &vsockOverride{UDSPath: "agent-manager.vsock"},
	})
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	call := drainAPICalls(seen, 1)[0]
	overrides, ok := call.Body["network_overrides"].([]any)
	if !ok || len(overrides) != 1 {
		t.Fatalf("network overrides = %#v", call.Body["network_overrides"])
	}
	override, ok := overrides[0].(map[string]any)
	if !ok || override["iface_id"] != "eth0" || override["host_dev_name"] != "agfc123" {
		t.Fatalf("network override = %#v", overrides[0])
	}
	vsock, ok := call.Body["vsock_override"].(map[string]any)
	if !ok || vsock["uds_path"] != "agent-manager.vsock" {
		t.Fatalf("vsock override = %#v", call.Body["vsock_override"])
	}
}

func TestManagerPutMMDSConfiguresV2WhenTapBacked(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "firecracker.sock")
	seen := make(chan apiCall, 4)
	closeServer := startFakeFirecrackerAPIServer(t, socketPath, seen)
	defer closeServer()

	instanceID := "fc-agent-mmds"
	m := &Manager{
		cfg: &config.Config{},
		instances: map[string]*instance{
			instanceID: {id: instanceID, agentID: "agent-1", socket: socketPath, tapName: "agfc123"},
		},
	}
	if err := m.PutMMDS(context.Background(), instanceID, map[string]string{"agentId": "agent-1"}); err != nil {
		t.Fatalf("put mmds: %v", err)
	}
	calls := drainAPICalls(seen, 2)
	if calls[0].Path != "/mmds/config" {
		t.Fatalf("first call = %#v", calls[0])
	}
	if calls[1].Path != "/mmds" {
		t.Fatalf("second call = %#v", calls[1])
	}
}

type apiCall struct {
	Method string
	Path   string
	Body   map[string]any
}

func startFakeFirecrackerAPIServer(t *testing.T, socketPath string, seen chan<- apiCall) func() {
	t.Helper()
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut && r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		seen <- apiCall{Method: r.Method, Path: r.URL.Path, Body: body}
		w.WriteHeader(http.StatusNoContent)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = http.Serve(ln, mux)
	}()
	return func() {
		_ = ln.Close()
		<-done
	}
}

func drainAPICalls(ch <-chan apiCall, n int) []apiCall {
	out := make([]apiCall, 0, n)
	for len(out) < n {
		out = append(out, <-ch)
	}
	return out
}
