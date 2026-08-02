package runnercontrol

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestRunnerPortProxyIsFencedBackpressuredAndCancelled(t *testing.T) {
	connection := newTestPortConnection()
	backend := &portRelayAssignmentBackend{
		relayAssignmentBackend: relayAssignmentBackend{},
		connection:             connection,
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{
		stream: &threadSafeRunnerStream{},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceSink := &recordingEvidenceSink{}
	service.SetEvidenceSink(evidenceSink)
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY: true,
	}
	asyncErrors := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	open := relayPortOpen(fence, "port-1", "port-stream-1")
	if err := service.handlePortFrame(ctx, stream, open, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 1)
	if credit := stream.messages()[0].GetPort().GetCredit(); credit == nil || credit.ByteCount != runnerRelayChunkBytes {
		t.Fatalf("initial Port credit = %#v", credit)
	}

	clientBytes := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(fence), OperationId: "port-1", StreamId: "port-stream-1", Sequence: 2,
		Payload: &runnerprotocol.PortFrame_Bytes{Bytes: &runnerprotocol.PortBytes{Data: []byte{0, 1, 0xff}}},
	}
	if err := service.handlePortFrame(ctx, stream, clientBytes, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if got := connection.writtenBytes(); !bytes.Equal(got, []byte{0, 1, 0xff}) {
		t.Fatalf("guest Port write = %v", got)
	}
	waitRunnerMessages(t, stream, 2)
	if credit := stream.messages()[1].GetPort().GetCredit(); credit == nil || credit.ByteCount != 3 {
		t.Fatalf("replenished Port credit = %#v", credit)
	}

	connection.queueRead([]byte{0xfe, 2}, nil)
	time.Sleep(10 * time.Millisecond)
	if got := len(stream.messages()); got != 2 {
		t.Fatalf("Port response escaped without control-plane credit: %d messages", got)
	}
	responseCredit := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(fence), OperationId: "port-1", StreamId: "port-stream-1", Sequence: 3,
		Payload: &runnerprotocol.PortFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 2}},
	}
	if err := service.handlePortFrame(ctx, stream, responseCredit, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 3)
	if got := stream.messages()[2].GetPort().GetBytes().Data; !bytes.Equal(got, []byte{0xfe, 2}) {
		t.Fatalf("credit-gated Port response = %v", got)
	}

	cancelFrame := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(fence), OperationId: "port-1", StreamId: "port-stream-1", Sequence: 4,
		Payload: &runnerprotocol.PortFrame_Cancel{Cancel: &runnerprotocol.ExecCancel{Reason: "client disconnected"}},
	}
	if err := service.handlePortFrame(ctx, stream, cancelFrame, enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 4)
	if terminal := stream.messages()[3].GetPort().GetTerminal(); terminal == nil ||
		terminal.Kind != runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED {
		t.Fatalf("cancelled Port terminal = %#v", terminal)
	}
	if !connection.isClosed() {
		t.Fatal("Port connection remained open after cancellation")
	}
	records := evidenceSink.snapshot()
	if len(records) != 1 ||
		records[0].Event != "port_terminal" ||
		records[0].RequestID != "request-port-1" ||
		records[0].OperationID != "port-1" ||
		records[0].LeaseID != "lease-port-1" ||
		records[0].SandboxID != fence.SandboxId ||
		records[0].InstanceID != fence.InstanceId ||
		records[0].SandboxGeneration != fence.SandboxGeneration ||
		records[0].AssignmentID != fence.AssignmentId ||
		records[0].RunnerID != "runner-1" {
		t.Fatalf("Port terminal evidence = %+v", records)
	}

	if err := service.handlePortFrame(ctx, stream, cancelFrame, enabled, asyncErrors); err != nil {
		t.Fatalf("exact duplicate Port frame: %v", err)
	}
	noSession := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(fence), OperationId: "absent", StreamId: "absent-stream", Sequence: 1,
		Payload: &runnerprotocol.PortFrame_Bytes{Bytes: &runnerprotocol.PortBytes{Data: []byte("blocked")}},
	}
	if err := service.handlePortFrame(ctx, stream, noSession, enabled, asyncErrors); err == nil {
		t.Fatal("Port bytes without an admitted session were accepted")
	}
}

func TestRunnerPortFrameContractRequiresAndValidatesCorrelation(t *testing.T) {
	if field := (&runnerprotocol.PortFrame{}).ProtoReflect().Descriptor().Fields().ByName("correlation"); field == nil {
		t.Fatal("Runner PortFrame contract lacks operation correlation")
	}
	connection := newTestPortConnection()
	backend := &portRelayAssignmentBackend{
		relayAssignmentBackend: relayAssignmentBackend{},
		connection:             connection,
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{
		stream: &threadSafeRunnerStream{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	err = service.handlePortFrame(
		t.Context(),
		&threadSafeRunnerStream{},
		&runnerprotocol.PortFrame{
			Fence: cloneRunnerFence(fence), OperationId: "missing-correlation",
			StreamId: "missing-correlation-stream", Sequence: 1,
			Payload: &runnerprotocol.PortFrame_Open{Open: &runnerprotocol.PortOpen{
				GuestPort: 8080, Protocol: "tcp", IdleTimeoutMs: 30_000,
			}},
		},
		map[runnerprotocol.RunnerFeature]bool{
			runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY: true,
		},
		make(chan error, 1),
	)
	if err == nil {
		t.Fatal("Runner Port Open without correlation was accepted")
	}
}

func TestRunnerPortProxyRejectsStaleFenceAndClosesOnRunnerDisconnect(t *testing.T) {
	connection := newTestPortConnection()
	backend := &portRelayAssignmentBackend{
		relayAssignmentBackend: relayAssignmentBackend{},
		connection:             connection,
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{
		stream: &threadSafeRunnerStream{},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY: true,
	}
	asyncErrors := make(chan error, 1)

	stale := cloneRunnerFence(fence)
	stale.SandboxGeneration++
	if err := service.handlePortFrame(t.Context(), stream, relayPortOpen(stale, "stale", "stale-stream"), enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if terminal := stream.messages()[0].GetPort().GetTerminal(); terminal == nil ||
		terminal.Kind != runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FENCED {
		t.Fatalf("stale Port terminal = %#v", terminal)
	}

	ctx, disconnect := context.WithCancel(t.Context())
	if err := service.handlePortFrame(ctx, stream, relayPortOpen(fence, "disconnect", "disconnect-stream"), enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	disconnect()
	deadline := time.Now().Add(time.Second)
	for !connection.isClosed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !connection.isClosed() {
		t.Fatal("Runner disconnect did not close the guest Port connection")
	}
}

type portRelayAssignmentBackend struct {
	relayAssignmentBackend
	connection *testPortConnection
}

func (backend *portRelayAssignmentBackend) OpenPort(
	context.Context,
	*runnerprotocol.AssignmentFence,
	*runnerprotocol.PortOpen,
) (PortConnection, error) {
	backend.connection.markOpened()
	return backend.connection, nil
}

type testPortRead struct {
	data []byte
	err  error
}

type testPortConnection struct {
	mu      sync.Mutex
	written []byte
	closed  bool
	wasOpen bool
	reads   chan testPortRead
}

func newTestPortConnection() *testPortConnection {
	return &testPortConnection{reads: make(chan testPortRead, 4)}
}

func (connection *testPortConnection) Read(ctx context.Context, maximum int) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case read := <-connection.reads:
		if len(read.data) > maximum {
			return nil, io.ErrShortBuffer
		}
		return bytes.Clone(read.data), read.err
	}
}

func (connection *testPortConnection) Write(_ context.Context, data []byte) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.written = append(connection.written, data...)
	return nil
}

func (connection *testPortConnection) Close() error {
	connection.mu.Lock()
	connection.closed = true
	connection.mu.Unlock()
	return nil
}

func (connection *testPortConnection) queueRead(data []byte, err error) {
	connection.reads <- testPortRead{data: bytes.Clone(data), err: err}
}

func (connection *testPortConnection) writtenBytes() []byte {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return bytes.Clone(connection.written)
}

func (connection *testPortConnection) isClosed() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.closed
}

func (connection *testPortConnection) markOpened() {
	connection.mu.Lock()
	connection.wasOpen = true
	connection.mu.Unlock()
}

func (connection *testPortConnection) opened() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.wasOpen
}

func relayPortOpen(
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	streamID string,
) *runnerprotocol.PortFrame {
	return &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(fence), OperationId: operationID, StreamId: streamID, Sequence: 1,
		Correlation: relayOperationCorrelation(
			fence, operationID, "request-"+operationID, "lease-"+operationID,
		),
		Payload: &runnerprotocol.PortFrame_Open{Open: &runnerprotocol.PortOpen{
			GuestPort: 8080, Protocol: "tcp", IdleTimeoutMs: 30_000,
		}},
	}
}
