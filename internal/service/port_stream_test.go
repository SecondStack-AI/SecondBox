package service

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type scriptedPortDataPlaneStream struct {
	sent     chan *runnerv1.ControlPlaneToRunner
	received chan *runnerv1.RunnerToControlPlane
}

func (stream *scriptedPortDataPlaneStream) Send(message *runnerv1.ControlPlaneToRunner) error {
	stream.sent <- message
	return nil
}

func (stream *scriptedPortDataPlaneStream) Receive(ctx context.Context) (*runnerv1.RunnerToControlPlane, error) {
	select {
	case message := <-stream.received:
		return message, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*scriptedPortDataPlaneStream) Close() error { return nil }

func newScriptedPortStream() (*SandboxPortStream, *scriptedPortDataPlaneStream) {
	tunnel := runnercontrol.PortTunnel{
		Session:  contracts.PortSession{ID: "port", SandboxID: "sbx", Generation: 1},
		StreamID: "stream", AssignmentID: "asn", InstanceID: "ins", RunnerID: "runner",
		RequestID: "request", LeaseID: "lease", FencingToken: []byte("fence"),
		StreamWindowBytes: 16, MaximumRequestBytes: 1 << 20, MaximumResponseBytes: 1 << 20,
	}
	fake := &scriptedPortDataPlaneStream{
		sent:     make(chan *runnerv1.ControlPlaneToRunner, 16),
		received: make(chan *runnerv1.RunnerToControlPlane, 16),
	}
	return &SandboxPortStream{
		tunnel: tunnel, stream: fake, nextSend: 1, nextReceive: 1,
		responseCredit: tunnel.StreamWindowBytes, creditChanged: make(chan struct{}, 1),
	}, fake
}

func (stream *SandboxPortStream) scriptedFrame(sequence uint64, payload any) *runnerv1.RunnerToControlPlane {
	frame := &runnerv1.PortFrame{
		Fence: portTunnelFence(stream.tunnel), OperationId: stream.tunnel.Session.ID,
		StreamId: stream.tunnel.StreamID, Sequence: sequence,
		Correlation: portTunnelCorrelation(stream.tunnel),
	}
	switch value := payload.(type) {
	case *runnerv1.PortFrame_Credit:
		frame.Payload = value
	case *runnerv1.PortFrame_Terminal:
		frame.Payload = value
	}
	return &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_Port{Port: frame}}
}

// A client message that exceeds the Runner's credit waits for the credit
// frame itself rather than polling, and cancellation or a terminal releases it.
func TestSandboxPortStreamSendWaitsForRunnerCredit(t *testing.T) {
	stream, fake := newScriptedPortStream()
	receiveContext, stopReceive := context.WithCancel(t.Context())
	defer stopReceive()
	events := make(chan error, 4)
	go func() {
		for {
			_, err := stream.Receive(receiveContext)
			events <- err
			if err != nil {
				return
			}
		}
	}()
	sent := make(chan error, 1)
	go func() { sent <- stream.Send(t.Context(), []byte("0123456789")) }()
	select {
	case err := <-sent:
		t.Fatalf("Send without credit returned %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	fake.received <- stream.scriptedFrame(1, &runnerv1.PortFrame_Credit{Credit: &runnerv1.StreamCredit{ByteCount: 16}})
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Millisecond):
		t.Fatal("Send did not wake when the Runner granted credit")
	}
	if bytes := (<-fake.sent).GetPort().GetBytes().GetData(); string(bytes) != "0123456789" {
		t.Fatalf("forwarded client bytes = %q", bytes)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	go func() { sent <- stream.Send(cancelled, []byte("0123456789")) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Send = %v", err)
	}

	go func() { sent <- stream.Send(t.Context(), []byte("0123456789")) }()
	time.Sleep(10 * time.Millisecond)
	fake.received <- stream.scriptedFrame(2, &runnerv1.PortFrame_Terminal{Terminal: &runnerv1.PortTerminal{
		Kind: runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED,
	}})
	select {
	case err := <-sent:
		if !errors.Is(err, runnercontrol.ErrDataPlaneSessionLimit) {
			t.Fatalf("Send after terminal = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send did not wake when the stream ended")
	}
}
