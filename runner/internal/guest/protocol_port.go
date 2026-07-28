package microvmguest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

const guestPortFrameBytes = 64 << 10

type protocolPortState struct {
	mu           sync.Mutex
	binding      *guestv1.OperationBinding
	nextIncoming uint64
	nextOutgoing uint64
	connection   net.Conn
	credit       *protocolPortCredit
	cancel       context.CancelCauseFunc
	terminal     bool
}

type protocolPortCredit struct {
	mu        sync.Mutex
	available uint64
	notify    chan struct{}
}

func newProtocolPortCredit() *protocolPortCredit {
	return &protocolPortCredit{notify: make(chan struct{}, 1)}
}

func (credit *protocolPortCredit) add(value uint64) error {
	if value == 0 {
		return fmt.Errorf("guest protocol Port credit must be positive")
	}
	credit.mu.Lock()
	if ^uint64(0)-credit.available < value {
		credit.mu.Unlock()
		return fmt.Errorf("guest protocol Port credit exceeds uint64 capacity")
	}
	credit.available += value
	credit.mu.Unlock()
	select {
	case credit.notify <- struct{}{}:
	default:
	}
	return nil
}

func (credit *protocolPortCredit) take(ctx context.Context, maximum uint64) (uint64, error) {
	for {
		credit.mu.Lock()
		if credit.available > 0 {
			granted := min(credit.available, maximum)
			credit.available -= granted
			credit.mu.Unlock()
			return granted, nil
		}
		credit.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-credit.notify:
		}
	}
}

func (c *protocolConnection) handlePortFrame(frame *guestv1.PortFrame) error {
	if !c.enabled[guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY] {
		return fmt.Errorf("guest protocol Port feature was not negotiated")
	}
	if frame == nil || frame.Binding == nil ||
		!sameProtocolConnectionBinding(frame.Binding.Connection, c.binding) {
		return fmt.Errorf("guest protocol Port binding mismatch")
	}
	key := protocolOperationKey(frame.Binding)
	if key == "" {
		return fmt.Errorf("guest protocol Port operation identity is incomplete")
	}
	c.mu.Lock()
	state := c.ports[key]
	c.mu.Unlock()
	if state == nil {
		request := frame.GetRequest()
		if request == nil || frame.Binding.Sequence != 1 {
			return fmt.Errorf("guest protocol Port stream must begin with Request sequence one")
		}
		if request.GuestPort == 0 || request.GuestPort > 65535 ||
			(request.Protocol != "tcp" && request.Protocol != "http") ||
			request.IdleTimeoutMs == 0 || request.IdleTimeoutMs > uint64((24*time.Hour).Milliseconds()) {
			state = &protocolPortState{
				binding: cloneOperationBinding(frame.Binding), nextIncoming: 2, nextOutgoing: 1,
				credit: newProtocolPortCredit(),
			}
			return c.sendPortTerminal(state, guestv1.PortTerminalKind_PORT_TERMINAL_KIND_POLICY_DENIED, "guest Port request is outside policy bounds")
		}
		portCtx, cancel := context.WithTimeout(
			c.stream.Context(), time.Duration(request.IdleTimeoutMs)*time.Millisecond,
		)
		state = &protocolPortState{
			binding: cloneOperationBinding(frame.Binding), nextIncoming: 2, nextOutgoing: 1,
			credit: newProtocolPortCredit(), cancel: func(cause error) { cancel() },
		}
		dialer := net.Dialer{}
		connection, err := dialer.DialContext(
			portCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(request.GuestPort))),
		)
		if err != nil {
			cancel()
			return c.sendPortTerminal(state, guestv1.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED, "guest loopback port is unavailable")
		}
		state.connection = connection
		context.AfterFunc(portCtx, func() {
			_ = connection.Close()
		})
		c.mu.Lock()
		if len(c.ports) >= maxProtocolExecTerminalTombstones {
			c.mu.Unlock()
			cancel()
			_ = connection.Close()
			return c.sendPortTerminal(state, guestv1.PortTerminalKind_PORT_TERMINAL_KIND_LIMIT_EXCEEDED, "guest Port session capacity is exhausted")
		}
		c.ports[key] = state
		c.wait.Add(1)
		c.mu.Unlock()
		if err := c.sendPortCredit(state, guestPortFrameBytes); err != nil {
			cancel()
			_ = connection.Close()
			return err
		}
		go c.pumpPortConnection(portCtx, key, state)
		return nil
	}
	state.mu.Lock()
	if frame.Binding.Sequence != state.nextIncoming {
		state.mu.Unlock()
		return fmt.Errorf(
			"guest protocol Port sequence mismatch: got %d, want %d",
			frame.Binding.Sequence, state.nextIncoming,
		)
	}
	state.nextIncoming++
	terminal := state.terminal
	state.mu.Unlock()
	if terminal {
		return nil
	}
	switch {
	case frame.GetBytes() != nil:
		data := frame.GetBytes().Data
		if len(data) == 0 || len(data) > guestPortFrameBytes {
			return c.sendPortTerminal(state, guestv1.PortTerminalKind_PORT_TERMINAL_KIND_LIMIT_EXCEEDED, "guest Port frame exceeds its byte bound")
		}
		if err := writePortBytes(c.stream.Context(), state.connection, data); err != nil {
			return c.sendPortTerminal(state, guestv1.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED, "guest loopback port write failed")
		}
		return c.sendPortCredit(state, uint64(len(data)))
	case frame.GetCredit() != nil:
		return state.credit.add(frame.GetCredit().ByteCount)
	case frame.GetCancel() != nil:
		state.cancel(context.Canceled)
		return c.sendPortTerminal(state, guestv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED, "guest Port session cancelled")
	default:
		return fmt.Errorf("guest protocol Port accepts only Request, Bytes, Credit, and Cancel frames")
	}
}

func (c *protocolConnection) pumpPortConnection(
	ctx context.Context,
	key string,
	state *protocolPortState,
) {
	defer c.wait.Done()
	defer c.removePort(key)
	buffer := make([]byte, guestPortFrameBytes)
	for {
		credit, err := state.credit.take(ctx, guestPortFrameBytes)
		if err != nil {
			return
		}
		count, readErr := state.connection.Read(buffer[:credit])
		if count > 0 {
			if err := c.sendPort(state, &guestv1.PortFrame{
				Payload: &guestv1.PortFrame_Bytes{Bytes: &guestv1.PortBytes{
					Data: bytes.Clone(buffer[:count]),
				}},
			}); err != nil {
				c.recordAsyncError("send guest Port bytes", err)
				return
			}
		}
		if readErr != nil {
			kind := guestv1.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED
			detail := "guest loopback port closed"
			if !errors.Is(readErr, io.EOF) && ctx.Err() == nil {
				detail = "guest loopback port read failed"
			}
			c.recordAsyncError("send guest Port terminal", c.sendPortTerminal(state, kind, detail))
			return
		}
	}
}

func writePortBytes(ctx context.Context, connection net.Conn, data []byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetWriteDeadline(deadline); err != nil {
			return err
		}
	}
	for len(data) > 0 {
		written, err := connection.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
		data = data[written:]
	}
	return nil
}

func (c *protocolConnection) sendPortCredit(state *protocolPortState, credit uint64) error {
	return c.sendPort(state, &guestv1.PortFrame{
		Payload: &guestv1.PortFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: credit}},
	})
}

func (c *protocolConnection) sendPortTerminal(
	state *protocolPortState,
	kind guestv1.PortTerminalKind,
	detail string,
) error {
	state.mu.Lock()
	if state.terminal {
		state.mu.Unlock()
		return nil
	}
	state.terminal = true
	state.mu.Unlock()
	if state.cancel != nil {
		state.cancel(context.Canceled)
	}
	if state.connection != nil {
		_ = state.connection.Close()
	}
	return c.sendPort(state, &guestv1.PortFrame{
		Payload: &guestv1.PortFrame_Terminal{Terminal: &guestv1.PortTerminal{
			Kind: kind, SafeDetail: detail,
		}},
	})
}

func (c *protocolConnection) sendPort(state *protocolPortState, frame *guestv1.PortFrame) error {
	state.mu.Lock()
	frame.Binding = cloneOperationBinding(state.binding)
	frame.Binding.Sequence = state.nextOutgoing
	state.nextOutgoing++
	state.mu.Unlock()
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(&guestv1.GuestToRunner{
		Message: &guestv1.GuestToRunner_Port{Port: frame},
	})
}

func (c *protocolConnection) removePort(key string) {
	c.mu.Lock()
	delete(c.ports, key)
	c.mu.Unlock()
}

func (c *protocolConnection) cancelAllPorts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.ports {
		if state.cancel != nil {
			state.cancel(context.Canceled)
		}
		if state.connection != nil {
			_ = state.connection.Close()
		}
	}
}
