package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type FirecrackerAPIClient struct {
	SocketPath string
	Timeout    time.Duration
}

type vmStateRequest struct {
	State string `json:"state"`
}

type snapshotCreateRequest struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type snapshotLoadRequest struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       *memoryBackend    `json:"mem_backend,omitempty"`
	MemFilePath      string            `json:"mem_file_path,omitempty"`
	TrackDirtyPages  bool              `json:"track_dirty_pages,omitempty"`
	ResumeVM         bool              `json:"resume_vm,omitempty"`
	NetworkOverrides []networkOverride `json:"network_overrides,omitempty"`
	VsockOverride    *vsockOverride    `json:"vsock_override,omitempty"`
	ClockRealtime    bool              `json:"clock_realtime,omitempty"`
}

type memoryBackend struct {
	BackendPath string `json:"backend_path"`
	BackendType string `json:"backend_type"`
}

type networkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

type vsockOverride struct {
	UDSPath string `json:"uds_path"`
}

type mmdsConfigRequest struct {
	Version          string   `json:"version"`
	NetworkInterface []string `json:"network_interfaces,omitempty"`
}

func (c FirecrackerAPIClient) Pause(ctx context.Context) error {
	return c.patchJSON(ctx, "/vm", vmStateRequest{State: "Paused"}, nil)
}

func (c FirecrackerAPIClient) Resume(ctx context.Context) error {
	return c.patchJSON(ctx, "/vm", vmStateRequest{State: "Resumed"}, nil)
}

func (c FirecrackerAPIClient) CreateFullSnapshot(ctx context.Context, snapshotPath, memFilePath string) error {
	req := snapshotCreateRequest{
		SnapshotType: "Full",
		SnapshotPath: strings.TrimSpace(snapshotPath),
		MemFilePath:  strings.TrimSpace(memFilePath),
	}
	if req.SnapshotPath == "" || req.MemFilePath == "" {
		return fmt.Errorf("snapshot and memory file paths are required")
	}
	return c.putJSON(ctx, "/snapshot/create", req, nil)
}

func (c FirecrackerAPIClient) LoadSnapshotWithOptions(ctx context.Context, req snapshotLoadRequest) error {
	req.SnapshotPath = strings.TrimSpace(req.SnapshotPath)
	if req.MemBackend != nil {
		req.MemBackend.BackendPath = strings.TrimSpace(req.MemBackend.BackendPath)
		req.MemBackend.BackendType = strings.TrimSpace(req.MemBackend.BackendType)
	}
	req.MemFilePath = strings.TrimSpace(req.MemFilePath)
	if req.SnapshotPath == "" {
		return fmt.Errorf("snapshot path is required")
	}
	if req.MemBackend == nil && req.MemFilePath == "" {
		return fmt.Errorf("memory backend or memory file path is required")
	}
	if req.MemBackend != nil {
		if req.MemBackend.BackendPath == "" {
			return fmt.Errorf("memory backend path is required")
		}
		if req.MemBackend.BackendType == "" {
			req.MemBackend.BackendType = "File"
		}
	}
	return c.putJSON(ctx, "/snapshot/load", req, nil)
}

func (c FirecrackerAPIClient) ConfigureMMDSV2(ctx context.Context, ifaces []string) error {
	req := mmdsConfigRequest{Version: "V2", NetworkInterface: compactStrings(ifaces)}
	return c.putJSON(ctx, "/mmds/config", req, nil)
}

func (c FirecrackerAPIClient) PutMMDS(ctx context.Context, data any) error {
	if data == nil {
		return fmt.Errorf("mmds data is required")
	}
	return c.putJSON(ctx, "/mmds", data, nil)
}

func (c FirecrackerAPIClient) putJSON(ctx context.Context, path string, payload any, out any) error {
	return c.json(ctx, http.MethodPut, path, payload, out)
}

func (c FirecrackerAPIClient) patchJSON(ctx context.Context, path string, payload any, out any) error {
	return c.json(ctx, http.MethodPatch, path, payload, out)
}

func (c FirecrackerAPIClient) json(ctx context.Context, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode firecracker API payload: %w", err)
		}
		body = bytes.NewReader(data)
	}
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(resp, out); err != nil {
			return fmt.Errorf("decode firecracker API response: %w", err)
		}
	}
	return nil
}

func (c FirecrackerAPIClient) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	if strings.TrimSpace(c.SocketPath) == "" {
		return nil, fmt.Errorf("firecracker API socket path is required")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.SocketPath)
		},
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, body)
	if err != nil {
		return nil, fmt.Errorf("build firecracker API request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("firecracker API request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read firecracker API response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("firecracker API rejected %s with %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
