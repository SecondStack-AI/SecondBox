package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const firecrackerGuestPortFrameBytes = 64 << 10

type guestPortConnection struct {
	stream    guestv1.GuestAgent_ConnectClient
	binding   *guestv1.OperationBinding
	cancel    context.CancelFunc
	sendMu    sync.Mutex
	nextSend  uint64
	credit    *guestPortCredit
	reads     chan guestPortRead
	closeOnce sync.Once
	closeErr  error
}

type guestPortRead struct {
	data []byte
	err  error
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
	if open == nil || open.GuestPort == 0 || open.GuestPort > 65535 ||
		(open.Protocol != "tcp" && open.Protocol != "http") || open.IdleTimeoutMs == 0 {
		return nil, fmt.Errorf("SecondBox Firecracker Port Open is invalid")
	}
	if !session.EnabledFeatures[guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY] {
		return nil, fmt.Errorf("SecondBox Firecracker guest Port feature was not negotiated")
	}
	portCtx, cancel := context.WithCancel(ctx)
	stream, binding, err := session.openPortProtocolStream(portCtx)
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
		Connection: binding, AssignmentId: fence.AssignmentId,
		OperationId: operationID, StreamId: operationID + "-port", Sequence: 1,
	}
	connection := &guestPortConnection{
		stream: stream, binding: operationBinding, cancel: cancel, nextSend: 2,
		credit: newGuestPortCredit(), reads: make(chan guestPortRead, 16),
	}
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
	case first := <-connection.reads:
		if first.err != nil {
			cancel()
			return nil, first.err
		}
		if len(first.data) != 0 {
			cancel()
			return nil, fmt.Errorf("Firecracker guest Port emitted bytes before initial credit")
		}
	}
	return connection, nil
}

func (session *GuestProtocolSession) openPortProtocolStream(
	ctx context.Context,
) (guestv1.GuestAgent_ConnectClient, *guestv1.ConnectionBinding, error) {
	nonce := make([]byte, guestConnectionNonceByteCount)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("create guest Port connection nonce: %w", err)
	}
	binding := &guestv1.ConnectionBinding{
		InstanceId: session.Binding.InstanceId, SandboxId: session.Binding.SandboxId,
		SandboxGeneration: session.Binding.SandboxGeneration, ConnectionNonce: nonce,
	}
	stream, err := guestv1.NewGuestAgentClient(session.Connection).Connect(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open guest Port protocol stream: %w", err)
	}
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
			Binding: binding,
			SupportedGenerations: &guestv1.ProtocolGenerationRange{
				Minimum: currentGuestProtocolGeneration, Maximum: currentGuestProtocolGeneration,
			},
			RequestedFeatures:               []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY},
			MandatoryFeatures:               []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY},
			ExpectedImageManifestDigest:     session.ImageManifestDigest,
			ExpectedToolchainManifestDigest: session.ToolchainManifestDigest,
		}},
	}); err != nil {
		return nil, nil, fmt.Errorf("send guest Port protocol hello: %w", err)
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, nil, fmt.Errorf("receive guest Port protocol welcome: %w", err)
	}
	welcome := first.GetWelcome()
	if welcome == nil || welcome.SelectedGeneration != currentGuestProtocolGeneration ||
		welcome.GuestBuildId != session.GuestBuildID ||
		welcome.ImageManifestDigest != session.ImageManifestDigest ||
		welcome.ToolchainManifestDigest != session.ToolchainManifestDigest ||
		!sameConnectionBinding(welcome.Binding, binding) ||
		len(welcome.EnabledFeatures) != 1 ||
		welcome.EnabledFeatures[0] != guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY {
		return nil, nil, fmt.Errorf("guest Port protocol welcome is invalid")
	}
	return stream, binding, nil
}

func (connection *guestPortConnection) receive(ctx context.Context) {
	expectedSequence := uint64(1)
	initial := true
	for {
		message, err := connection.stream.Recv()
		if err != nil {
			connection.deliver(guestPortRead{err: err})
			return
		}
		frame := message.GetPort()
		if frame == nil || frame.Binding == nil ||
			frame.Binding.AssignmentId != connection.binding.AssignmentId ||
			frame.Binding.OperationId != connection.binding.OperationId ||
			frame.Binding.StreamId != connection.binding.StreamId ||
			!sameConnectionBinding(frame.Binding.Connection, connection.binding.Connection) ||
			frame.Binding.Sequence != expectedSequence {
			connection.deliver(guestPortRead{err: fmt.Errorf("Firecracker guest Port frame binding or sequence is invalid")})
			return
		}
		expectedSequence++
		switch {
		case frame.GetCredit() != nil:
			if err := connection.credit.add(frame.GetCredit().ByteCount); err != nil {
				connection.deliver(guestPortRead{err: err})
				return
			}
			if initial {
				initial = false
				connection.deliver(guestPortRead{})
			}
		case frame.GetBytes() != nil:
			if initial || len(frame.GetBytes().Data) == 0 {
				connection.deliver(guestPortRead{err: fmt.Errorf("Firecracker guest Port byte ordering is invalid")})
				return
			}
			connection.deliver(guestPortRead{data: bytes.Clone(frame.GetBytes().Data)})
		case frame.GetTerminal() != nil:
			detail := frame.GetTerminal().SafeDetail
			if detail == "" {
				detail = frame.GetTerminal().Kind.String()
			}
			connection.deliver(guestPortRead{err: fmt.Errorf("Firecracker guest Port terminated: %s", detail)})
			return
		default:
			connection.deliver(guestPortRead{err: fmt.Errorf("Firecracker guest Port payload is invalid")})
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (connection *guestPortConnection) deliver(read guestPortRead) {
	select {
	case connection.reads <- read:
	default:
		connection.cancel()
	}
}

func (connection *guestPortConnection) Read(
	ctx context.Context,
	maximum int,
) ([]byte, error) {
	if maximum < 1 || maximum > firecrackerGuestPortFrameBytes {
		return nil, fmt.Errorf("Firecracker guest Port read bound is invalid")
	}
	if err := connection.send(&guestv1.PortFrame{
		Payload: &guestv1.PortFrame_Credit{Credit: &guestv1.ByteCredit{
			ByteCount: uint64(maximum),
		}},
	}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case read := <-connection.reads:
		return read.data, read.err
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
