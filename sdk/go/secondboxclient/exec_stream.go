package secondboxclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const execStreamSubprotocol = "secondbox.exec.v1"

// ExecStream owns one negotiated, sequenced streaming-exec WebSocket.
type ExecStream struct {
	connection    *websocket.Conn
	writeMu       sync.Mutex
	nextInput     int64
	nextOutput    int64
	inputClosed   bool
	terminal      bool
	writeDeadline time.Time
}

// ConnectExecStream attaches to a session returned by CreateExecStream.
func (handle *SandboxHandle) ConnectExecStream(
	ctx context.Context,
	session ExecStreamSession,
	dialer *websocket.Dialer,
) (*ExecStream, error) {
	current := handle.Snapshot()
	if session.Subprotocol != execStreamSubprotocol ||
		session.SandboxID != current.ID ||
		session.Generation != current.Generation {
		return nil, errors.New("SecondBox Exec stream session does not match the Sandbox handle")
	}
	expiresAt := session.ExpiresAt
	if expiresAt.IsZero() {
		return nil, errors.New("SecondBox Exec stream expiration is invalid")
	}
	endpoint, err := url.Parse(session.WebsocketURL)
	if err != nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") ||
		endpoint.Host == "" || endpoint.Fragment != "" {
		return nil, errors.New("SecondBox Exec stream WebSocket URL is invalid")
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
	selectedCopy := *selected
	selectedCopy.Subprotocols = []string{execStreamSubprotocol}
	connection, response, err := selectedCopy.DialContext(ctx, endpoint.String(), headers)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			detail, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
			if readErr == nil && len(detail) != 0 {
				return nil, fmt.Errorf(
					"SecondBox Exec stream attach failed: status=%d detail=%s: %w",
					response.StatusCode,
					strings.TrimSpace(string(detail)),
					err,
				)
			}
			return nil, fmt.Errorf("SecondBox Exec stream attach failed: status=%d: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("SecondBox Exec stream attach failed: %w", err)
	}
	if connection.Subprotocol() != execStreamSubprotocol {
		_ = connection.Close()
		return nil, errors.New("SecondBox Exec stream subprotocol was not negotiated")
	}
	return &ExecStream{connection: connection, writeDeadline: expiresAt}, nil
}

// SendInput sends the next binary-safe standard-input frame.
func (stream *ExecStream) SendInput(data []byte) error {
	return stream.SendInputFrame(data, false)
}

// CloseInput closes guest standard input after all prior bytes.
func (stream *ExecStream) CloseInput() error {
	return stream.SendInputFrame(nil, true)
}

// SendInputFrame sends bytes and can close guest standard input in the same ordered frame.
func (stream *ExecStream) SendInputFrame(data []byte, endOfInput bool) error {
	if len(data) == 0 && !endOfInput {
		return errors.New("SecondBox Exec stream stdin frame is empty")
	}
	if stream == nil || stream.connection == nil {
		return errors.New("SecondBox Exec stream is not connected")
	}
	stream.writeMu.Lock()
	defer stream.writeMu.Unlock()
	if stream.inputClosed {
		return errors.New("SecondBox Exec stream standard input is already closed")
	}
	err := stream.writeFrameLocked(func(sequence int64) ExecStreamFrame {
		return ExecStreamFrame{StreamInputFrame: &StreamInputFrame{
			Type: "stdin", Sequence: sequence,
			DataBase64: base64.StdEncoding.EncodeToString(data),
			EndOfInput: endOfInput,
		}}
	})
	if err == nil && endOfInput {
		stream.inputClosed = true
	}
	return err
}

// GrantOutput grants the server explicit output bytes without exceeding its negotiated window.
func (stream *ExecStream) GrantOutput(bytes int64) error {
	if bytes < 1 {
		return errors.New("SecondBox Exec stream output credit must be positive")
	}
	return stream.writeFrame(func(sequence int64) ExecStreamFrame {
		return ExecStreamFrame{StreamCreditFrame: &StreamCreditFrame{
			Type: "credit", Sequence: sequence, Bytes: bytes,
		}}
	})
}

// Cancel requests guest-process cancellation on the ordered stream.
func (stream *ExecStream) Cancel() error {
	return stream.writeFrame(func(sequence int64) ExecStreamFrame {
		return ExecStreamFrame{StreamCancelFrame: &StreamCancelFrame{
			Type: "cancel", Sequence: sequence,
		}}
	})
}

func (stream *ExecStream) writeFrame(build func(int64) ExecStreamFrame) error {
	if stream == nil || stream.connection == nil {
		return errors.New("SecondBox Exec stream is not connected")
	}
	stream.writeMu.Lock()
	defer stream.writeMu.Unlock()
	return stream.writeFrameLocked(build)
}

func (stream *ExecStream) writeFrameLocked(build func(int64) ExecStreamFrame) error {
	if stream.terminal {
		return errors.New("SecondBox Exec stream is terminal")
	}
	if err := stream.connection.SetWriteDeadline(stream.writeDeadline); err != nil {
		return fmt.Errorf("SecondBox Exec stream set write deadline: %w", err)
	}
	if err := stream.connection.WriteJSON(build(stream.nextInput)); err != nil {
		return fmt.Errorf("SecondBox Exec stream write frame: %w", err)
	}
	stream.nextInput++
	return nil
}

// Receive reads the next ordered output or terminal outcome frame.
func (stream *ExecStream) Receive() (ExecStreamFrame, error) {
	if stream == nil || stream.connection == nil {
		return ExecStreamFrame{}, errors.New("SecondBox Exec stream is not connected")
	}
	var frame ExecStreamFrame
	if err := stream.connection.ReadJSON(&frame); err != nil {
		return ExecStreamFrame{}, fmt.Errorf("SecondBox Exec stream read frame: %w", err)
	}
	var sequence int64
	switch {
	case frame.StreamOutputFrame != nil:
		sequence = frame.StreamOutputFrame.Sequence
	case frame.StreamOutcomeFrame != nil:
		sequence = frame.StreamOutcomeFrame.Sequence
		stream.writeMu.Lock()
		stream.terminal = true
		stream.writeMu.Unlock()
	default:
		return ExecStreamFrame{}, errors.New("SecondBox Exec stream received a client-only frame")
	}
	if sequence != stream.nextOutput {
		return ExecStreamFrame{}, fmt.Errorf(
			"SecondBox Exec stream output sequence mismatch: got %d, want %d",
			sequence, stream.nextOutput,
		)
	}
	stream.nextOutput++
	return frame, nil
}

// Close detaches the WebSocket. A nonterminal detach requests server-side cancellation.
func (stream *ExecStream) Close() error {
	if stream == nil || stream.connection == nil {
		return nil
	}
	stream.writeMu.Lock()
	defer stream.writeMu.Unlock()
	deadline := time.Now().Add(time.Second)
	controlErr := stream.connection.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client detached"),
		deadline,
	)
	closeErr := stream.connection.Close()
	return errors.Join(controlErr, closeErr)
}
