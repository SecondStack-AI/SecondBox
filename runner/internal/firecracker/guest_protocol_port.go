package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const (
	firecrackerGuestPortFrameBytes = 64 << 10
	// firecrackerGuestPortReceiveWindowBytes bounds the guest bytes one Port
	// holds on the Runner: credit granted to the guest but not yet used, plus
	// received bytes the caller has not read. Credit returns to the guest only
	// as the caller reads, so a slow caller slows the guest socket instead of
	// growing a buffer or losing the connection.
	firecrackerGuestPortReceiveWindowBytes = 4 * firecrackerGuestPortFrameBytes
	// firecrackerGuestPortCreditGrantBytes is the smallest top-up the adapter
	// sends, so a stream of small reads does not become a stream of credit frames.
	firecrackerGuestPortCreditGrantBytes = firecrackerGuestPortFrameBytes
)

type guestPortConnection struct {
	stream   guestv1.GuestAgent_ConnectClient
	binding  *guestv1.OperationBinding
	cancel   context.CancelFunc
	sendMu   sync.Mutex
	nextSend uint64
	credit   *guestPortCredit
	// opened reports the guest's first writable credit, or the failure that
	// preceded it, exactly once.
	opened chan error
	readMu sync.Mutex
	// receiveMu guards the receive window. The guest never holds more than
	// receiveCredit, and receiveCredit plus receivedBytes never exceeds
	// firecrackerGuestPortReceiveWindowBytes.
	receiveMu     sync.Mutex
	received      [][]byte
	receivedBytes uint64
	receiveCredit uint64
	receiveErr    error
	receiveReady  chan struct{}
	closeOnce     sync.Once
	closeErr      error
}

type guestPortCredit struct {
	mu        sync.Mutex
	available uint64
	notify    chan struct{}
}

func newGuestPortCredit() *guestPortCredit {
	return &guestPortCredit{notify: make(chan struct{}, 1)}
}

func (credit *guestPortCredit) add(value uint64) error {
	if value == 0 {
		return fmt.Errorf("Firecracker guest Port credit must be positive")
	}
	credit.mu.Lock()
	if ^uint64(0)-credit.available < value {
		credit.mu.Unlock()
		return fmt.Errorf("Firecracker guest Port credit exceeds uint64 capacity")
	}
	credit.available += value
	credit.mu.Unlock()
	select {
	case credit.notify <- struct{}{}:
	default:
	}
	return nil
}

func (credit *guestPortCredit) take(ctx context.Context, maximum uint64) (uint64, error) {
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

// OpenPort opens a dedicated guest protocol stream to one guest loopback port.
func (backend *AssignmentBackend) OpenPort(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.PortOpen,
) (runnercontrol.PortConnection, error) {
	session, err := backend.guestSessionForFence(fence)
	if err != nil {
		return nil, err
	}
	return OpenPortOverSession(ctx, session, fence.AssignmentId, open)
}

// OpenPortOverSession opens one generation-fenced guest Port relay on any
// retained negotiated guest session; the caller supplies fence validation.
func OpenPortOverSession(
	ctx context.Context,
	session *GuestProtocolSession,
	assignmentID string,
	open *runnerprotocol.PortOpen,
) (runnercontrol.PortConnection, error) {
	if open == nil || open.GuestPort == 0 || open.GuestPort > 65535 ||
		(open.Protocol != "tcp" && open.Protocol != "http") || open.IdleTimeoutMs == 0 {
		return nil, fmt.Errorf("SecondBox Firecracker Port Open is invalid")
	}
	if !session.EnabledFeatures[guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY] {
		return nil, fmt.Errorf("SecondBox Firecracker guest Port feature was not negotiated")
	}
	portCtx, cancel := context.WithCancel(ctx)
	stream, binding, err := session.openGuestStream(
		portCtx, []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY},
	)
	if err != nil {
		cancel()
		return nil, err
	}
	operationID, err := randomGuestOperationID()
	if err != nil {
		cancel()
		return nil, err
	}
	operationBinding := &guestv1.OperationBinding{
		Connection: binding, AssignmentId: assignmentID,
		OperationId: operationID, StreamId: operationID + "-port", Sequence: 1,
	}
	connection := newGuestPortConnection(stream, operationBinding, cancel)
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Port{Port: &guestv1.PortFrame{
			Binding: cloneGuestOperationBinding(operationBinding),
			Payload: &guestv1.PortFrame_Request{Request: &guestv1.PortRequest{
				GuestPort: open.GuestPort, Protocol: open.Protocol, IdleTimeoutMs: open.IdleTimeoutMs,
			}},
		}},
	}); err != nil {
		cancel()
		return nil, fmt.Errorf("send Firecracker guest Port request: %w", err)
	}
	go connection.receive(portCtx)
	select {
	case <-portCtx.Done():
		return nil, portCtx.Err()
	case err := <-connection.opened:
		if err != nil {
			cancel()
			return nil, err
		}
	}
	return connection, nil
}

func newGuestPortConnection(
	stream guestv1.GuestAgent_ConnectClient,
	binding *guestv1.OperationBinding,
	cancel context.CancelFunc,
) *guestPortConnection {
	return &guestPortConnection{
		stream: stream, binding: binding, cancel: cancel, nextSend: 2,
		credit: newGuestPortCredit(), opened: make(chan error, 1),
		receiveReady: make(chan struct{}, 1),
	}
}

func (connection *guestPortConnection) receive(ctx context.Context) {
	expectedSequence := uint64(1)
	initial := true
	fail := func(err error) {
		if initial {
			connection.opened <- err
			return
		}
		connection.finishReceive(err)
	}
	for {
		message, err := connection.stream.Recv()
		if err != nil {
			fail(err)
			return
		}
		frame := message.GetPort()
		if frame == nil || frame.Binding == nil ||
			frame.Binding.AssignmentId != connection.binding.AssignmentId ||
			frame.Binding.OperationId != connection.binding.OperationId ||
			frame.Binding.StreamId != connection.binding.StreamId ||
			!sameConnectionBinding(frame.Binding.Connection, connection.binding.Connection) ||
			frame.Binding.Sequence != expectedSequence {
			fail(fmt.Errorf("Firecracker guest Port frame binding or sequence is invalid"))
			return
		}
		expectedSequence++
		switch {
		case frame.GetCredit() != nil:
			if err := connection.credit.add(frame.GetCredit().ByteCount); err != nil {
				fail(err)
				return
			}
			if initial {
				initial = false
				connection.opened <- nil
			}
		case frame.GetBytes() != nil:
			if initial || len(frame.GetBytes().Data) == 0 {
				fail(fmt.Errorf("Firecracker guest Port byte ordering is invalid"))
				return
			}
			if len(frame.GetBytes().Data) > firecrackerGuestPortFrameBytes {
				fail(fmt.Errorf("Firecracker guest Port bytes exceed the frame bound"))
				return
			}
			if err := connection.enqueueReceived(frame.GetBytes().Data); err != nil {
				fail(err)
				return
			}
		case frame.GetTerminal() != nil:
			detail := frame.GetTerminal().SafeDetail
			if detail == "" {
				detail = frame.GetTerminal().Kind.String()
			}
			fail(fmt.Errorf("Firecracker guest Port terminated: %s", detail))
			return
		default:
			fail(fmt.Errorf("Firecracker guest Port payload is invalid"))
			return
		}
		select {
		case <-ctx.Done():
			fail(ctx.Err())
			return
		default:
		}
	}
}

// enqueueReceived spends granted credit on one guest frame. A guest that sends
// more than it was granted has broken the protocol; nothing it sent is queued.
func (connection *guestPortConnection) enqueueReceived(data []byte) error {
	connection.receiveMu.Lock()
	if uint64(len(data)) > connection.receiveCredit {
		connection.receiveMu.Unlock()
		return fmt.Errorf("Firecracker guest Port bytes exceed granted credit")
	}
	connection.receiveCredit -= uint64(len(data))
	connection.received = append(connection.received, bytes.Clone(data))
	connection.receivedBytes += uint64(len(data))
	connection.receiveMu.Unlock()
	connection.signalReceive()
	return nil
}

// finishReceive records the outcome that follows every byte already queued.
func (connection *guestPortConnection) finishReceive(err error) {
	connection.receiveMu.Lock()
	if connection.receiveErr == nil {
		connection.receiveErr = err
	}
	connection.receiveMu.Unlock()
	connection.signalReceive()
}

func (connection *guestPortConnection) signalReceive() {
	select {
	case connection.receiveReady <- struct{}{}:
	default:
	}
}

// grantReceiveCreditLocked tops the guest's credit up to the receive window.
func (connection *guestPortConnection) grantReceiveCreditLocked() uint64 {
	held := connection.receiveCredit + connection.receivedBytes
	if held >= firecrackerGuestPortReceiveWindowBytes ||
		firecrackerGuestPortReceiveWindowBytes-held < firecrackerGuestPortCreditGrantBytes {
		return 0
	}
	grant := firecrackerGuestPortReceiveWindowBytes - held
	connection.receiveCredit += grant
	return grant
}

// takeReceivedLocked removes at most maximum bytes from the head of the queue.
func (connection *guestPortConnection) takeReceivedLocked(maximum int) []byte {
	head := connection.received[0]
	size := min(maximum, len(head))
	data := head[:size:size]
	if size == len(head) {
		connection.received[0] = nil
		connection.received = connection.received[1:]
	} else {
		connection.received[0] = head[size:]
	}
	connection.receivedBytes -= uint64(size)
	return data
}

func (connection *guestPortConnection) Read(
	ctx context.Context,
	maximum int,
) ([]byte, error) {
	if maximum < 1 || maximum > firecrackerGuestPortFrameBytes {
		return nil, fmt.Errorf("Firecracker guest Port read bound is invalid")
	}
	connection.readMu.Lock()
	defer connection.readMu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		connection.receiveMu.Lock()
		var data []byte
		if len(connection.received) > 0 {
			data = connection.takeReceivedLocked(maximum)
		} else if connection.receiveErr != nil {
			err := connection.receiveErr
			connection.receiveMu.Unlock()
			return nil, err
		}
		grant := uint64(0)
		if connection.receiveErr == nil {
			grant = connection.grantReceiveCreditLocked()
		}
		connection.receiveMu.Unlock()
		if grant > 0 {
			if err := connection.send(&guestv1.PortFrame{
				Payload: &guestv1.PortFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: grant}},
			}); err != nil {
				// A guest that finished its stream refuses further credit while
				// its last bytes may still be queued here; they are delivered
				// before this outcome, unless the guest's own terminal came first.
				connection.finishReceive(fmt.Errorf("grant Firecracker guest Port credit: %w", err))
			}
		}
		if data != nil {
			return data, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-connection.receiveReady:
		}
	}
}

func (connection *guestPortConnection) Write(
	ctx context.Context,
	data []byte,
) error {
	for len(data) > 0 {
		credit, err := connection.credit.take(ctx, uint64(min(len(data), firecrackerGuestPortFrameBytes)))
		if err != nil {
			return err
		}
		size := min(len(data), int(credit))
		if err := connection.send(&guestv1.PortFrame{
			Payload: &guestv1.PortFrame_Bytes{Bytes: &guestv1.PortBytes{
				Data: bytes.Clone(data[:size]),
			}},
		}); err != nil {
			return err
		}
		data = data[size:]
	}
	return nil
}

func (connection *guestPortConnection) Close() error {
	connection.closeOnce.Do(func() {
		_ = connection.send(&guestv1.PortFrame{
			Payload: &guestv1.PortFrame_Cancel{Cancel: &guestv1.ExecCancel{
				Reason: "runner Port proxy closed",
			}},
		})
		connection.closeErr = connection.stream.CloseSend()
		connection.cancel()
	})
	return connection.closeErr
}

func (connection *guestPortConnection) send(frame *guestv1.PortFrame) error {
	connection.sendMu.Lock()
	defer connection.sendMu.Unlock()
	frame.Binding = cloneGuestOperationBinding(connection.binding)
	frame.Binding.Sequence = connection.nextSend
	connection.nextSend++
	if err := connection.stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Port{Port: frame},
	}); err != nil {
		return fmt.Errorf("send Firecracker guest Port frame: %w", err)
	}
	return nil
}

var _ runnercontrol.PortBackend = (*AssignmentBackend)(nil)
var _ runnercontrol.PortConnection = (*guestPortConnection)(nil)
