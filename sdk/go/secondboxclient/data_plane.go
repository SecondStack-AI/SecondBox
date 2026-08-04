package secondboxclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/portdirect"
	"github.com/gorilla/websocket"
)

// ReadFile reads a workspace-relative file under an explicit output bound.
func (handle *SandboxHandle) ReadFile(ctx context.Context, path WorkspacePath, maximumBytes int64, leaseID string) ([]byte, error) {
	if path == "" || maximumBytes < 1 {
		return nil, errors.New("SecondBox file path and positive read bound are required")
	}
	response, err := handle.client.Request(ctx, "readSandboxFile", CallOptions{
		PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
		QueryParameters: url.Values{"path": []string{path}}, Headers: handle.GenerationHeaders(leaseID),
	})
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("SecondBox file read and close: %w", errors.Join(readErr, closeErr))
	}
	if int64(len(content)) > maximumBytes {
		return nil, fmt.Errorf("SecondBox file read exceeds %d bytes", maximumBytes)
	}
	return content, nil
}

func (handle *SandboxHandle) WriteFile(ctx context.Context, path WorkspacePath, content []byte, idempotencyKey, leaseID string) (FileWriteResult, error) {
	if path == "" {
		return FileWriteResult{}, errors.New("SecondBox file path is required")
	}
	idempotencyKey, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return FileWriteResult{}, err
	}
	sum := sha256.Sum256(content)
	headers := handle.GenerationHeaders(leaseID)
	headers.Set("Idempotency-Key", idempotencyKey)
	headers.Set("Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
	var result FileWriteResult
	err = handle.client.RequestJSON(ctx, "writeSandboxFile", CallOptions{
		PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
		QueryParameters: url.Values{"path": []string{path}}, Headers: headers,
		Body: bytes.NewReader(content), ContentType: "application/octet-stream",
	}, &result)
	return result, err
}

func (handle *SandboxHandle) StatFile(ctx context.Context, path WorkspacePath, leaseID string) (FileStat, error) {
	var stat FileStat
	err := handle.fileJSON(ctx, "statSandboxFile", path, leaseID, &stat)
	return stat, err
}

func (handle *SandboxHandle) ListDirectory(ctx context.Context, path WorkspacePath, leaseID string) (DirectoryListing, error) {
	var listing DirectoryListing
	err := handle.fileJSON(ctx, "listSandboxDirectory", path, leaseID, &listing)
	return listing, err
}

func (handle *SandboxHandle) FileExists(ctx context.Context, path WorkspacePath, leaseID string) (bool, error) {
	var result FileExistsResult
	err := handle.fileJSON(ctx, "sandboxFileExists", path, leaseID, &result)
	return result.Exists, err
}

func (handle *SandboxHandle) CreateDirectory(ctx context.Context, path WorkspacePath, recursive bool, idempotencyKey, leaseID string) error {
	return handle.fileMutation(ctx, "createSandboxDirectory", CreateDirectoryRequest{Path: path, Recursive: recursive}, idempotencyKey, leaseID)
}

func (handle *SandboxHandle) RemovePath(ctx context.Context, path WorkspacePath, recursive, force bool, idempotencyKey, leaseID string) error {
	return handle.fileMutation(ctx, "removeSandboxPath", RemovePathRequest{Path: path, Recursive: recursive, Force: force}, idempotencyKey, leaseID)
}

func (handle *SandboxHandle) fileJSON(ctx context.Context, operationID string, path WorkspacePath, leaseID string, target any) error {
	if path == "" {
		return errors.New("SecondBox file path is required")
	}
	return handle.client.RequestJSON(ctx, operationID, CallOptions{
		PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
		QueryParameters: url.Values{"path": []string{path}}, Headers: handle.GenerationHeaders(leaseID),
	}, target)
}

func (handle *SandboxHandle) fileMutation(ctx context.Context, operationID string, request any, idempotencyKey, leaseID string) error {
	idempotencyKey, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return err
	}
	body, err := EncodeJSONBody(request)
	if err != nil {
		return err
	}
	headers := handle.GenerationHeaders(leaseID)
	headers.Set("Idempotency-Key", idempotencyKey)
	response, err := handle.client.Request(ctx, operationID, CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID}, Headers: headers,
		Body: body, ContentType: "application/json",
	})
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func (handle *SandboxHandle) CreatePortSession(ctx context.Context, request CreatePortSessionRequest, idempotencyKey, leaseID string) (PortSession, error) {
	var session PortSession
	err := handle.dataPlaneJSON(ctx, "createSandboxPortSession", request, idempotencyKey, leaseID, &session)
	return session, err
}

func (handle *SandboxHandle) GetPortSession(ctx context.Context, sessionID OpaqueID) (PortSession, error) {
	if sessionID == "" {
		return PortSession{}, errors.New("SecondBox PortSession ID is required")
	}
	var session PortSession
	err := handle.client.RequestJSON(ctx, "getSandboxPortSession", CallOptions{PathParameters: map[string]string{
		"sandboxId": handle.Snapshot().ID, "portSessionId": sessionID,
	}}, &session)
	return session, err
}

func (handle *SandboxHandle) ClosePortSession(ctx context.Context, sessionID OpaqueID, idempotencyKey string) error {
	if sessionID == "" {
		return errors.New("SecondBox PortSession ID is required")
	}
	idempotencyKey, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", idempotencyKey)
	response, err := handle.client.Request(ctx, "closeSandboxPortSession", CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID, "portSessionId": sessionID}, Headers: headers,
	})
	if err != nil {
		return err
	}
	return response.Body.Close()
}

// PortTunnel is a bidirectional byte stream over either the relay WebSocket or
// the Runner's SPKI-pinned direct TLS endpoint.
type PortTunnel struct {
	direct    net.Conn
	websocket *websocket.Conn
	readMu    sync.Mutex
	writeMu   sync.Mutex
	reader    io.Reader
}

// ConnectPortTunnel consumes one PortSession credential and selects only the
// transport declared by that session.
func (handle *SandboxHandle) ConnectPortTunnel(ctx context.Context, session PortSession, websocketDialer *websocket.Dialer, directDialer *net.Dialer) (*PortTunnel, error) {
	current := handle.Snapshot()
	if session.SandboxID != current.ID || session.Generation != current.Generation || session.State != "open" {
		return nil, errors.New("SecondBox PortSession does not match the Sandbox handle")
	}
	switch session.Transport {
	case "relay":
		return connectRelayPortTunnel(ctx, session, websocketDialer)
	case "direct":
		return connectDirectPortTunnel(ctx, session, directDialer)
	default:
		return nil, fmt.Errorf("SecondBox PortSession transport %q is unsupported", session.Transport)
	}
}

func connectRelayPortTunnel(ctx context.Context, session PortSession, dialer *websocket.Dialer) (*PortTunnel, error) {
	endpoint, credential, err := portEndpoint(session.Endpoint, "ws", "wss")
	if err != nil {
		return nil, err
	}
	selected := websocket.DefaultDialer
	if dialer != nil {
		selected = dialer
	}
	copy := *selected
	copy.Subprotocols = []string{"secondbox.port.v1", "secondbox.port.token." + credential}
	connection, response, err := copy.DialContext(ctx, endpoint.String(), nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("SecondBox Port tunnel attach failed: %w", err)
	}
	if connection.Subprotocol() != "secondbox.port.v1" {
		_ = connection.Close()
		return nil, errors.New("SecondBox Port tunnel subprotocol was not negotiated")
	}
	return &PortTunnel{websocket: connection}, nil
}

func connectDirectPortTunnel(ctx context.Context, session PortSession, dialer *net.Dialer) (*PortTunnel, error) {
	endpoint, credential, err := portEndpoint(session.Endpoint, "secondbox+tcp")
	if err != nil {
		return nil, err
	}
	if session.CertificateSpkiSha256 == nil {
		return nil, errors.New("SecondBox direct PortSession certificate SPKI pin is required")
	}
	tlsConfig, err := portdirect.TLSConfigForSPKIPin(*session.CertificateSpkiSha256)
	if err != nil {
		return nil, err
	}
	selected := &net.Dialer{}
	if dialer != nil {
		selected = dialer
	}
	connection, err := (&tls.Dialer{NetDialer: selected, Config: tlsConfig}).DialContext(ctx, "tcp", endpoint.Host)
	if err != nil {
		return nil, fmt.Errorf("SecondBox direct Port tunnel connect: %w", err)
	}
	if deadline, found := ctx.Deadline(); found {
		_ = connection.SetDeadline(deadline)
	}
	if err := portdirect.WriteCredential(connection, portdirect.SessionKindPort, credential); err != nil {
		_ = connection.Close()
		return nil, err
	}
	verdict, detail, err := portdirect.ReadVerdict(connection)
	if err != nil || verdict != portdirect.VerdictAdmitted {
		_ = connection.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("SecondBox direct Port connection was denied: %s", detail)
	}
	_ = connection.SetDeadline(time.Time{})
	return &PortTunnel{direct: connection}, nil
}

func portEndpoint(raw string, schemes ...string) (*url.URL, string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" {
		return nil, "", errors.New("SecondBox PortSession endpoint is invalid")
	}
	if !slices.Contains(schemes, endpoint.Scheme) {
		return nil, "", errors.New("SecondBox PortSession endpoint scheme is invalid")
	}
	credential := endpoint.Fragment
	if credential == "" || len(credential) > portdirect.MaximumCredentialBytes {
		return nil, "", errors.New("SecondBox PortSession endpoint credential is invalid")
	}
	endpoint.Fragment = ""
	return endpoint, credential, nil
}

func (tunnel *PortTunnel) Read(payload []byte) (int, error) {
	tunnel.readMu.Lock()
	defer tunnel.readMu.Unlock()
	if tunnel.direct != nil {
		return tunnel.direct.Read(payload)
	}
	for tunnel.websocket != nil {
		if tunnel.reader == nil {
			messageType, reader, err := tunnel.websocket.NextReader()
			if err != nil {
				return 0, err
			}
			if messageType != websocket.BinaryMessage {
				return 0, errors.New("SecondBox Port tunnel received a non-binary frame")
			}
			tunnel.reader = reader
		}
		read, err := tunnel.reader.Read(payload)
		if errors.Is(err, io.EOF) {
			tunnel.reader = nil
			if read > 0 {
				return read, nil
			}
			continue
		}
		return read, err
	}
	return 0, net.ErrClosed
}

func (tunnel *PortTunnel) Write(payload []byte) (int, error) {
	tunnel.writeMu.Lock()
	defer tunnel.writeMu.Unlock()
	if tunnel.direct != nil {
		return tunnel.direct.Write(payload)
	}
	if tunnel.websocket == nil {
		return 0, net.ErrClosed
	}
	if err := tunnel.websocket.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (tunnel *PortTunnel) Close() error {
	if tunnel == nil {
		return nil
	}
	if tunnel.direct != nil {
		return tunnel.direct.Close()
	}
	if tunnel.websocket != nil {
		return tunnel.websocket.Close()
	}
	return nil
}
