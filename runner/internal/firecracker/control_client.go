package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/sandboxlimits"
	"github.com/SecondStack-AI/SecondBox/runner/internal/toolexecutor"
)

// ControlClient talks to the in-guest control service over Firecracker's host-side
// vsock UDS. The service speaks HTTP after the fc-vsock CONNECT handshake.
type ControlClient struct {
	UDSPath string
	Port    uint32
	Timeout time.Duration
}

type HeartbeatResponse struct {
	InstanceID string `json:"instanceId"`
	SandboxID  string `json:"sandboxId"`
	Healthy    bool   `json:"healthy"`
	Time       string `json:"time,omitempty"`
}

const ToolExecutorContractVersion = toolexecutor.ContractVersion

type ToolExecutorOperation = toolexecutor.Operation

const (
	ToolOpExec           = toolexecutor.OpExec
	ToolOpReadFile       = toolexecutor.OpReadFile
	ToolOpReadFileBuffer = toolexecutor.OpReadFileBuffer
	ToolOpWriteFile      = toolexecutor.OpWriteFile
	ToolOpStat           = toolexecutor.OpStat
	ToolOpReaddir        = toolexecutor.OpReaddir
	ToolOpExists         = toolexecutor.OpExists
	ToolOpMkdir          = toolexecutor.OpMkdir
	ToolOpRm             = toolexecutor.OpRm
)

var ToolExecutorOperations = toolexecutor.Operations()

type ToolExecRequest = toolexecutor.Request
type ToolExecResponse = toolexecutor.Response
type ToolExecutorDirEntry = toolexecutor.DirEntry

type WorkspaceEntry = runtimemanager.WorkspaceEntry

type WorkspaceListResponse struct {
	Entries []WorkspaceEntry `json:"entries"`
}

type BackupResponse struct {
	Frozen bool   `json:"frozen"`
	Detail string `json:"detail,omitempty"`
}

type SecretBundle struct {
	Env   map[string]string `json:"env,omitempty"`
	Files map[string]string `json:"files,omitempty"`
}

type RestoreHardenRequest struct {
	HostTime      string `json:"hostTime,omitempty"`
	EntropyBase64 string `json:"entropyBase64,omitempty"`
}

// AssignmentBindRequest installs identity into a resumed template guest. The
// guest accepts exactly one, after /restore/harden and before its protocol
// listener accepts a connection.
type AssignmentBindRequest struct {
	InstanceID              string `json:"instanceId"`
	SandboxID               string `json:"sandboxId"`
	SandboxGeneration       uint64 `json:"sandboxGeneration"`
	GuestBuildID            string `json:"guestBuildId"`
	ImageManifestDigest     string `json:"imageManifestDigest"`
	ToolchainManifestDigest string `json:"toolchainManifestDigest"`
	HeartbeatIntervalMs     uint64 `json:"heartbeatIntervalMs"`
}

func (c ControlClient) Heartbeat(ctx context.Context) (HeartbeatResponse, error) {
	var out HeartbeatResponse
	if err := c.getJSON(ctx, "/heartbeat", &out); err != nil {
		return HeartbeatResponse{}, err
	}
	return out, nil
}

func (c ControlClient) ListWorkspace(ctx context.Context, relPath string) (WorkspaceListResponse, error) {
	var out WorkspaceListResponse
	escaped := path.Clean("/" + strings.TrimPrefix(relPath, "/"))
	if err := c.getJSON(ctx, "/workspace/list?path="+urlQueryEscape(escaped), &out); err != nil {
		return WorkspaceListResponse{}, err
	}
	return out, nil
}

func (c ControlClient) ReadWorkspaceFile(ctx context.Context, relPath string, maxBytes int64) ([]byte, error) {
	escaped := path.Clean("/" + strings.TrimPrefix(relPath, "/"))
	reqPath := "/workspace/read?path=" + urlQueryEscape(escaped)
	if maxBytes > 0 {
		reqPath += fmt.Sprintf("&maxBytes=%d", maxBytes)
	}
	return c.do(ctx, http.MethodGet, reqPath, nil)
}

func (c ControlClient) OpenWorkspaceFileStream(ctx context.Context, relPath string) (io.ReadCloser, int64, error) {
	escaped := path.Clean("/" + strings.TrimPrefix(relPath, "/"))
	resp, transport, err := c.doStreaming(ctx, http.MethodGet, "/workspace/read?path="+urlQueryEscape(escaped), nil)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		defer transport.CloseIdleConnections()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, 0, fmt.Errorf("control stream rejected with %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	size := resp.ContentLength
	if raw := strings.TrimSpace(resp.Header.Get("Content-Length")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			defer resp.Body.Close()
			defer transport.CloseIdleConnections()
			return nil, 0, fmt.Errorf("control stream returned invalid Content-Length %q", raw)
		}
		size = parsed
	}
	if size < 0 {
		defer resp.Body.Close()
		defer transport.CloseIdleConnections()
		return nil, 0, fmt.Errorf("control stream missing Content-Length")
	}
	return &transportReadCloser{ReadCloser: resp.Body, transport: transport}, size, nil
}

func (c ControlClient) PutWorkspaceFileStream(ctx context.Context, relPath string, r io.Reader) (int64, string, error) {
	escaped := path.Clean("/" + strings.TrimPrefix(relPath, "/"))
	counter := &countingReader{r: r}
	resp, transport, err := c.doStreaming(ctx, http.MethodPut, "/workspace/write?path="+urlQueryEscape(escaped), counter)
	if err != nil {
		return 0, "", err
	}
	defer transport.CloseIdleConnections()
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", fmt.Errorf("read control stream response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("control stream rejected with %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, "", fmt.Errorf("decode control stream response: %w", err)
	}
	if out.Size != counter.n {
		return 0, "", fmt.Errorf("workspace write byte count mismatch: sent %d bytes, guest wrote %d", counter.n, out.Size)
	}
	if strings.TrimSpace(out.SHA256) == "" {
		return 0, "", fmt.Errorf("workspace write response missing sha256")
	}
	return out.Size, out.SHA256, nil
}

func (c ControlClient) FreezeWorkspace(ctx context.Context) (BackupResponse, error) {
	var out BackupResponse
	if err := c.postJSON(ctx, "/workspace/freeze", nil, &out); err != nil {
		return BackupResponse{}, err
	}
	return out, nil
}

func (c ControlClient) ThawWorkspace(ctx context.Context) (BackupResponse, error) {
	var out BackupResponse
	if err := c.postJSON(ctx, "/workspace/thaw", nil, &out); err != nil {
		return BackupResponse{}, err
	}
	return out, nil
}

func (c ControlClient) ExecuteTool(ctx context.Context, req ToolExecRequest) (ToolExecResponse, error) {
	var out ToolExecResponse
	if strings.TrimSpace(string(req.Operation)) == "" {
		return ToolExecResponse{}, fmt.Errorf("tool executor operation is required")
	}
	if err := c.postJSON(ctx, "/tool/exec", req, &out); err != nil {
		return ToolExecResponse{}, err
	}
	if out.Error != "" {
		return out, fmt.Errorf("tool executor %s failed: %s", req.Operation, out.Error)
	}
	return out, nil
}

func (c ControlClient) ApplySecrets(ctx context.Context, bundle SecretBundle) error {
	var out struct {
		Applied bool `json:"applied"`
	}
	if err := c.postJSON(ctx, "/secrets/apply", bundle, &out); err != nil {
		return err
	}
	if !out.Applied {
		return fmt.Errorf("secret bundle was not applied")
	}
	return nil
}

func (c ControlClient) BindAssignment(ctx context.Context, req AssignmentBindRequest) error {
	var out struct {
		Bound      bool   `json:"bound"`
		InstanceID string `json:"instanceId"`
		SandboxID  string `json:"sandboxId"`
	}
	if err := c.postJSON(ctx, "/assignment/bind", req, &out); err != nil {
		return err
	}
	if !out.Bound {
		return fmt.Errorf("assignment identity was not installed")
	}
	if out.InstanceID != req.InstanceID || out.SandboxID != req.SandboxID {
		return fmt.Errorf(
			"resumed guest bound a different assignment: instance %q sandbox %q",
			out.InstanceID,
			out.SandboxID,
		)
	}
	return nil
}

func (c ControlClient) HardenPostRestore(ctx context.Context, hostTime time.Time) error {
	var entropy [64]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return fmt.Errorf("read host entropy: %w", err)
	}
	var out struct {
		Hardened bool `json:"hardened"`
	}
	if err := c.postJSON(ctx, "/restore/harden", RestoreHardenRequest{
		HostTime:      hostTime.UTC().Format(time.RFC3339Nano),
		EntropyBase64: base64.StdEncoding.EncodeToString(entropy[:]),
	}, &out); err != nil {
		return err
	}
	if !out.Hardened {
		return fmt.Errorf("restore hardening was not applied")
	}
	return nil
}

func (c ControlClient) Logs(ctx context.Context, tail string) ([]byte, error) {
	reqPath := "/logs"
	if strings.TrimSpace(tail) != "" {
		reqPath += "?tail=" + urlQueryEscape(strings.TrimSpace(tail))
	}
	return c.do(ctx, http.MethodGet, reqPath, nil)
}

func (c ControlClient) getJSON(ctx context.Context, reqPath string, out any) error {
	data, err := c.do(ctx, http.MethodGet, reqPath, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode control response: %w", err)
	}
	return nil
}

func (c ControlClient) postJSON(ctx context.Context, reqPath string, payload any, out any) error {
	data, err := c.postRawJSON(ctx, reqPath, payload)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode control response: %w", err)
	}
	return nil
}

func (c ControlClient) postRawJSON(ctx context.Context, reqPath string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode control payload: %w", err)
		}
		body = bytes.NewReader(data)
	}
	return c.do(ctx, http.MethodPost, reqPath, body)
}

func (c ControlClient) do(ctx context.Context, method, reqPath string, body io.Reader) ([]byte, error) {
	if strings.TrimSpace(c.UDSPath) == "" {
		return nil, fmt.Errorf("control UDS path is required")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialFirecrackerVsock(ctx, c.UDSPath, c.Port)
		},
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://microvm"+reqPath, body)
	if err != nil {
		return nil, fmt.Errorf("build control request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("control request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, sandboxlimits.ControlClientResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read control response: %w", err)
	}
	if int64(len(data)) > sandboxlimits.ControlClientResponseBytes {
		return nil, fmt.Errorf("control response exceeded %d bytes", sandboxlimits.ControlClientResponseBytes)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("control request rejected with %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (c ControlClient) doStreaming(ctx context.Context, method, reqPath string, body io.Reader) (*http.Response, *http.Transport, error) {
	if strings.TrimSpace(c.UDSPath) == "" {
		return nil, nil, fmt.Errorf("control UDS path is required")
	}
	transport := c.newTransport(30 * time.Second)
	req, err := http.NewRequestWithContext(ctx, method, "http://microvm"+reqPath, body)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, fmt.Errorf("build control request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, fmt.Errorf("control stream request: %w", err)
	}
	return resp, transport, nil
}

func (c ControlClient) newTransport(idleDeadline time.Duration) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dialFirecrackerVsock(ctx, c.UDSPath, c.Port)
			if err != nil {
				return nil, err
			}
			if idleDeadline > 0 {
				return &idleDeadlineConn{Conn: conn, idle: idleDeadline}, nil
			}
			return conn, nil
		},
	}
}

func dialFirecrackerVsock(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	if strings.TrimSpace(udsPath) == "" {
		return nil, fmt.Errorf("Firecracker vsock UDS path is required")
	}
	if port == 0 {
		return nil, fmt.Errorf("Firecracker vsock port is required")
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := readVsockConnectResponse(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func readVsockConnectResponse(conn net.Conn) (resultErr error) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return fmt.Errorf("set vsock CONNECT response deadline: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.SetReadDeadline(time.Time{}))
	}()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read vsock CONNECT response: %w", err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("vsock CONNECT rejected: %s", line)
	}
	return nil
}

type transportReadCloser struct {
	io.ReadCloser
	transport *http.Transport
}

func (r *transportReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.transport != nil {
		r.transport.CloseIdleConnections()
	}
	return err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

type idleDeadlineConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleDeadlineConn) Read(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Read(p)
}

func (c *idleDeadlineConn) Write(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Write(p)
}

func urlQueryEscape(value string) string {
	replacer := strings.NewReplacer("%", "%25", " ", "%20", "?", "%3F", "&", "%26", "=", "%3D", "#", "%23", "+", "%2B")
	return replacer.Replace(value)
}
