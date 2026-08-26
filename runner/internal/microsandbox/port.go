package microsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	microsandboxprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type helperPortConnection struct {
	backend      *AssignmentBackend
	active       *activeAssignment
	fence        *runnerprotocol.AssignmentFence
	process      *helperProcess
	requestID    uint64
	nextSequence uint64
	release      func()
	readMu       sync.Mutex
	writeMu      sync.Mutex
	buffer       bytes.Buffer
	sawEOF       bool
	terminal     error
	closeOnce    sync.Once
	closeErr     error
}

func (backend *AssignmentBackend) OpenPort(ctx context.Context, fence *runnerprotocol.AssignmentFence, open *runnerprotocol.PortOpen) (runnercontrol.PortConnection, error) {
	// "http" is a TCP relay with public HTTP semantics layered above the
	// runner; both public protocols reach the guest as one byte stream.
	if open == nil || (open.Protocol != "tcp" && open.Protocol != "http") || open.GuestPort == 0 || open.GuestPort > 65535 {
		return nil, fmt.Errorf("SecondBox Microsandbox Port Open requires a TCP or HTTP relay to a valid guest port")
	}
	active, opCtx, release, err := backend.acquireOperation(ctx, fence)
	if err != nil {
		return nil, err
	}
	process := active.process
	// The helper serves one request at a time on its single control
	// connection and its request handlers read frames inline, so this lock is
	// held for the tunnel's lifetime and other operations on the same
	// Instance queue behind it. Lifting that requires a helper protocol
	// revision (response multiplexing or independent channels), which means
	// re-pinning the reviewed helper build; the serialization is documented
	// as a known limitation of the experimental backend.
	process.requestMu.Lock()
	requestID := process.nextRequestID
	process.nextRequestID++
	request := &microsandboxprotocol.Envelope{
		ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID,
		Message: &microsandboxprotocol.Envelope_Tcp{Tcp: &microsandboxprotocol.TcpRequest{Host: "127.0.0.1", Port: open.GuestPort}},
	}
	deadline := time.Now().Add(10 * time.Second)
	if value, ok := opCtx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	err = process.control.SetDeadline(deadline)
	if err == nil {
		err = microsandboxprotocol.WriteFrame(process.control, request)
	}
	if err == nil {
		var response *microsandboxprotocol.Envelope
		response, err = microsandboxprotocol.ReadFrame(process.control)
		if err == nil && (response.RequestId != requestID || response.GetTcpConnected() == nil) {
			if diagnostic := response.GetDiagnostic(); diagnostic != nil {
				err = fmt.Errorf("%s: %s", diagnostic.Code, diagnostic.Text)
			} else if terminal := response.GetTerminal(); terminal != nil {
				err = fmt.Errorf("guest TCP connection failed: %s", terminal.Reason)
			} else {
				err = fmt.Errorf("invalid TCP connected event")
			}
		}
	}
	if err != nil {
		process.requestMu.Unlock()
		release()
		return nil, fmt.Errorf("SecondBox Microsandbox open guest Port: %w", err)
	}
	_ = process.control.SetDeadline(time.Time{})
	connection := &helperPortConnection{backend: backend, active: active, fence: cloneFence(fence), process: process, requestID: requestID, nextSequence: 1, release: release}
	context.AfterFunc(opCtx, func() { _ = connection.Close() })
	return connection, nil
}

func (connection *helperPortConnection) Read(ctx context.Context, maximum int) ([]byte, error) {
	connection.readMu.Lock()
	defer connection.readMu.Unlock()
	if maximum <= 0 {
		return nil, fmt.Errorf("SecondBox Microsandbox Port read bound must be positive")
	}
	if connection.buffer.Len() > 0 {
		return connection.buffer.Next(min(maximum, connection.buffer.Len())), nil
	}
	if connection.sawEOF {
		return nil, io.EOF
	}
	if connection.terminal != nil {
		return nil, connection.terminal
	}
	deadline := time.Now().Add(24 * time.Hour)
	if value, ok := ctx.Deadline(); ok {
		deadline = value
	}
	_ = connection.process.control.SetReadDeadline(deadline)
	for {
		event, err := microsandboxprotocol.ReadFrame(connection.process.control)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("SecondBox Microsandbox read guest Port: %w", err)
		}
		if event.RequestId != connection.requestID {
			return nil, fmt.Errorf("SecondBox Microsandbox Port event identity mismatch")
		}
		if data := event.GetStreamData(); data != nil {
			if data.Eof {
				connection.sawEOF = true
				return nil, io.EOF
			}
			connection.buffer.Write(data.Data)
			return connection.buffer.Next(min(maximum, connection.buffer.Len())), nil
		}
		if terminal := event.GetTerminal(); terminal != nil {
			if terminal.Success {
				connection.terminal = io.EOF
			} else {
				connection.terminal = fmt.Errorf("SecondBox Microsandbox guest Port failed: %s", terminal.Reason)
			}
			return nil, connection.terminal
		}
	}
}

func (connection *helperPortConnection) Write(ctx context.Context, data []byte) error {
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	if len(data) == 0 {
		return nil
	}
	deadline := time.Now().Add(24 * time.Hour)
	if value, ok := ctx.Deadline(); ok {
		deadline = value
	}
	if err := connection.process.control.SetWriteDeadline(deadline); err != nil {
		return err
	}
	sequence := connection.nextSequence
	connection.nextSequence++
	return microsandboxprotocol.WriteFrame(connection.process.control, &microsandboxprotocol.Envelope{
		ProtocolVersion: microsandboxprotocol.Version, RequestId: connection.requestID, StreamId: connection.requestID, Sequence: sequence,
		Message: &microsandboxprotocol.Envelope_StreamData{StreamData: &microsandboxprotocol.StreamData{Data: bytes.Clone(data), Channel: microsandboxprotocol.StreamChannel_STREAM_CHANNEL_TCP}},
	})
}

func (connection *helperPortConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.writeMu.Lock()
		sequence := connection.nextSequence
		connection.nextSequence++
		err := microsandboxprotocol.WriteFrame(connection.process.control, &microsandboxprotocol.Envelope{
			ProtocolVersion: microsandboxprotocol.Version, RequestId: connection.requestID, StreamId: connection.requestID, Sequence: sequence,
			Message: &microsandboxprotocol.Envelope_Cancel{Cancel: &microsandboxprotocol.CancelRequest{TargetRequestId: connection.requestID}},
		})
		connection.writeMu.Unlock()
		connection.readMu.Lock()
		_ = connection.process.control.SetReadDeadline(time.Now().Add(5 * time.Second))
		for connection.terminal == nil {
			event, readErr := microsandboxprotocol.ReadFrame(connection.process.control)
			if readErr != nil {
				err = errors.Join(err, readErr)
				break
			}
			if event.RequestId != connection.requestID {
				err = errors.Join(err, fmt.Errorf("SecondBox Microsandbox Port terminal identity mismatch"))
				break
			}
			if diagnostic := event.GetDiagnostic(); diagnostic != nil {
				err = errors.Join(err, fmt.Errorf("SecondBox Microsandbox helper %s: %s", diagnostic.Code, diagnostic.Text))
				break
			}
			if terminal := event.GetTerminal(); terminal != nil {
				if terminal.Success {
					connection.terminal = io.EOF
				} else {
					connection.terminal = fmt.Errorf("SecondBox Microsandbox guest Port failed: %s", terminal.Reason)
				}
			}
		}
		connection.readMu.Unlock()
		connection.process.requestMu.Unlock()
		connection.release()
		connection.closeErr = err
	})
	return connection.closeErr
}

var _ runnercontrol.PortBackend = (*AssignmentBackend)(nil)
var _ runnercontrol.PortConnection = (*helperPortConnection)(nil)
