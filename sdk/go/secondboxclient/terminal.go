package secondboxclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const terminalSubprotocol = "secondbox.terminal.v1"

// Terminal owns one authenticated, ordered Terminal WebSocket attachment.
type Terminal struct {
	connection    *websocket.Conn
	writeMu       sync.Mutex
	nextInput     int64
	nextOutput    int64
	terminal      bool
	writeDeadline time.Time
}

// GetTerminal returns the current reconnect descriptor for one stable Terminal ID.
func (handle *SandboxHandle) GetTerminal(
	ctx context.Context,
	sessionID OpaqueID,
) (TerminalSession, error) {
	if sessionID == "" {
		return TerminalSession{}, errors.New("SecondBox Terminal session ID is required")
	}
	current := handle.Snapshot()
	var session TerminalSession
	err := handle.client.RequestJSON(ctx, "reconnectSandboxTerminal", CallOptions{
		PathParameters: map[string]string{
			"sandboxId":         string(current.ID),
			"terminalSessionId": string(sessionID),
		},
		Headers: handle.GenerationHeaders(""),
	}, &session)
	return session, err
}

// CancelTerminal durably requests process cancellation for one stable Terminal ID.
func (handle *SandboxHandle) CancelTerminal(
	ctx context.Context,
	sessionID OpaqueID,
	idempotencyKey string,
) (TerminalSession, error) {
	if sessionID == "" {
		return TerminalSession{}, errors.New("SecondBox Terminal session ID is required")
	}
	if idempotencyKey == "" {
		return TerminalSession{}, errors.New("SecondBox Terminal cancellation idempotency key is required")
	}
	current := handle.Snapshot()
	headers := handle.GenerationHeaders("")
	headers.Set("Idempotency-Key", idempotencyKey)
	var session TerminalSession
	err := handle.client.RequestJSON(ctx, "cancelSandboxTerminal", CallOptions{
		PathParameters: map[string]string{
			"sandboxId":         string(current.ID),
			"terminalSessionId": string(sessionID),
		},
		Headers: headers,
	}, &session)
	return session, err
}

// ConnectTerminal attaches to a session returned by CreateTerminal or reconnect lookup.
func (handle *SandboxHandle) ConnectTerminal(
	ctx context.Context,
	session TerminalSession,
	dialer *websocket.Dialer,
) (*Terminal, error) {
	return handle.ConnectTerminalAfter(ctx, session, -1, dialer)
}

// ConnectTerminalAfter attaches and replays output after the last sequence the
// caller received successfully. Pass -1 when no output has been received.
func (handle *SandboxHandle) ConnectTerminalAfter(
	ctx context.Context,
	session TerminalSession,
	afterSequence int64,
	dialer *websocket.Dialer,
) (*Terminal, error) {
	current := handle.Snapshot()
	if session.Subprotocol != terminalSubprotocol ||
		session.SandboxID != current.ID ||
		session.Generation != current.Generation ||
		(session.State != SessionStateOpen && session.State != SessionStateDetached) {
		return nil, errors.New("SecondBox Terminal session does not match the Sandbox handle")
	}
	expiresAt := session.ExpiresAt
	if expiresAt.IsZero() {
		return nil, errors.New("SecondBox Terminal expiration is invalid")
	}
	endpoint, err := url.Parse(session.WebsocketURL)
	if err != nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") ||
		endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("SecondBox Terminal WebSocket URL is invalid")
	}
	selected := websocket.DefaultDialer
	if dialer != nil {
		selected = dialer
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+handle.client.token)
	headers.Set("X-SecondBox-Tenant-Ref", handle.client.tenantRef)
	headers.Set("X-SecondBox-Subject-Ref", handle.client.subjectRef)
	headers.Set("SecondBox-Generation", strconv.FormatInt(session.Generation, 10))
	if afterSequence < -1 {
		return nil, errors.New("SecondBox Terminal replay sequence is invalid")
	}
	headers.Set("SecondBox-Terminal-After-Sequence", strconv.FormatInt(afterSequence, 10))
	selectedCopy := *selected
	selectedCopy.Subprotocols = []string{terminalSubprotocol}
	connection, response, err := selectedCopy.DialContext(ctx, endpoint.String(), headers)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			body, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
			if readErr != nil {
				return nil, fmt.Errorf("SecondBox Terminal attach error response: %w", readErr)
			}
			if len(body) > 4<<20 {
				return nil, errors.New("SecondBox Terminal attach error response exceeds 4194304 bytes")
			}
			failure := &APIError{StatusCode: response.StatusCode, Body: body}
			var problem Problem
			if json.Unmarshal(body, &problem) == nil {
				failure.Problem = &problem
			}
			return nil, failure
		}
		return nil, fmt.Errorf("SecondBox Terminal attach failed: %w", err)
	}
	if connection.Subprotocol() != terminalSubprotocol {
		_ = connection.Close()
		return nil, errors.New("SecondBox Terminal subprotocol was not negotiated")
	}
	return &Terminal{
		connection: connection, nextInput: session.NextClientSequence,
		nextOutput:    afterSequence + 1,
		writeDeadline: expiresAt,
	}, nil
}

// SendInput sends the next binary-safe terminal-input frame.
func (terminal *Terminal) SendInput(data []byte) error {
	if len(data) == 0 {
		return errors.New("SecondBox Terminal input frame is empty")
	}
	owned := append([]byte(nil), data...)
	return terminal.writeFrame(func(sequence int64) TerminalFrame {
		return TerminalFrame{TerminalInputFrame: &TerminalInputFrame{
			Type: "terminal_input", Sequence: sequence,
			DataBase64: base64.StdEncoding.EncodeToString(owned),
		}}
	})
}

// Resize applies the next ordered terminal dimensions.
func (terminal *Terminal) Resize(rows int, columns int) error {
	if rows < 1 || rows > 1000 || columns < 1 || columns > 1000 {
		return errors.New("SecondBox Terminal resize dimensions are invalid")
	}
	return terminal.writeFrame(func(sequence int64) TerminalFrame {
		return TerminalFrame{TerminalResizeFrame: &TerminalResizeFrame{
			Type: "resize", Sequence: sequence, Rows: rows, Columns: columns,
		}}
	})
}

// GrantOutput grants the server explicit terminal output bytes.
func (terminal *Terminal) GrantOutput(bytes int64) error {
	if bytes < 1 {
		return errors.New("SecondBox Terminal output credit must be positive")
	}
	return terminal.writeFrame(func(sequence int64) TerminalFrame {
		return TerminalFrame{StreamCreditFrame: &StreamCreditFrame{
			Type: "credit", Sequence: sequence, Bytes: bytes,
		}}
	})
}

// Cancel requests guest-process cancellation on the ordered terminal stream.
func (terminal *Terminal) Cancel() error {
	return terminal.writeFrame(func(sequence int64) TerminalFrame {
		return TerminalFrame{StreamCancelFrame: &StreamCancelFrame{
			Type: "cancel", Sequence: sequence,
		}}
	})
}

func (terminal *Terminal) writeFrame(build func(int64) TerminalFrame) error {
	if terminal == nil || terminal.connection == nil {
		return errors.New("SecondBox Terminal is not connected")
	}
	terminal.writeMu.Lock()
	defer terminal.writeMu.Unlock()
	if terminal.terminal {
		return errors.New("SecondBox Terminal is terminal")
	}
	if err := terminal.connection.SetWriteDeadline(terminal.writeDeadline); err != nil {
		return fmt.Errorf("SecondBox Terminal set write deadline: %w", err)
	}
	if err := terminal.connection.WriteJSON(build(terminal.nextInput)); err != nil {
		return fmt.Errorf("SecondBox Terminal write frame: %w", err)
	}
	terminal.nextInput++
	return nil
}

// Receive reads the next ordered terminal output or outcome frame.
func (terminal *Terminal) Receive() (TerminalFrame, error) {
	if terminal == nil || terminal.connection == nil {
		return TerminalFrame{}, errors.New("SecondBox Terminal is not connected")
	}
	var frame TerminalFrame
	if err := terminal.connection.ReadJSON(&frame); err != nil {
		return TerminalFrame{}, fmt.Errorf("SecondBox Terminal read frame: %w", err)
	}
	var sequence int64
	switch {
	case frame.TerminalOutputFrame != nil:
		sequence = frame.TerminalOutputFrame.Sequence
		content, err := base64.StdEncoding.Strict().DecodeString(frame.TerminalOutputFrame.DataBase64)
		if err != nil || base64.StdEncoding.EncodeToString(content) != frame.TerminalOutputFrame.DataBase64 {
			return TerminalFrame{}, errors.New("SecondBox Terminal output is not canonical base64")
		}
	case frame.StreamOutcomeFrame != nil:
		sequence = frame.StreamOutcomeFrame.Sequence
		terminal.writeMu.Lock()
		terminal.terminal = true
		terminal.writeMu.Unlock()
	default:
		return TerminalFrame{}, errors.New("SecondBox Terminal received a client-only frame")
	}
	if sequence != terminal.nextOutput {
		return TerminalFrame{}, fmt.Errorf(
			"SecondBox Terminal output sequence mismatch: got %d, want %d",
			sequence, terminal.nextOutput,
		)
	}
	terminal.nextOutput++
	return frame, nil
}

// Close detaches the WebSocket without deleting the Sandbox.
func (terminal *Terminal) Close() error {
	if terminal == nil || terminal.connection == nil {
		return nil
	}
	terminal.writeMu.Lock()
	defer terminal.writeMu.Unlock()
	deadline := time.Now().Add(time.Second)
	controlErr := terminal.connection.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client detached"),
		deadline,
	)
	closeErr := terminal.connection.Close()
	return errors.Join(controlErr, closeErr)
}
