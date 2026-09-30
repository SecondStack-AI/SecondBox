package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

// The control plane sends a proxied Port's first Credit right behind its Open,
// before it can observe the Runner's answer. A refused Open must therefore end
// only its own stream: the Credit and any later frame are discarded rather than
// treated as frames for an unknown stream, which drops the Runner connection
// and every other operation on it.
func TestRunnerPortRefusedOpenEndsOnlyItsStream(t *testing.T) {
	for _, test := range []struct {
		name   string
		stale  bool
		kind   runnerprotocol.PortTerminalKind
		detail string
	}{
		{
			name: "guest port unavailable",
			kind: runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE, detail: "guest port is unavailable",
		},
		{
			name: "stale fence", stale: true,
			kind: runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FENCED, detail: "assignment fence is not active",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewRunnerProtocolService(testRunnerConfig(), &refusingPortBackend{}, staticProtocolConnector{
				stream: &threadSafeRunnerStream{},
			})
			if err != nil {
				t.Fatal(err)
			}
			evidenceSink := &recordingEvidenceSink{}
			service.evidence = evidenceSink
			stream := &threadSafeRunnerStream{}
			fence := relayRunnerFence()
			service.recordActiveAssignment(fence, "fc-instance-1")
			enabled := map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY: true}
			asyncErrors := make(chan error, 1)
			openFence := cloneRunnerFence(fence)
			if test.stale {
				openFence.SandboxGeneration++
			}

			if err := service.handlePortFrame(t.Context(), stream, relayPortOpen(openFence, "refused", "refused-stream"), enabled, asyncErrors); err != nil {
				t.Fatal(err)
			}
			messages := stream.messages()
			if len(messages) != 1 {
				t.Fatalf("refused Port Open produced %d frames, want one terminal", len(messages))
			}
			terminal := messages[0].GetPort()
			if terminal.GetSequence() != 1 || terminal.GetTerminal().GetKind() != test.kind ||
				terminal.GetTerminal().GetSafeDetail() != test.detail {
				t.Fatalf("refused Port terminal = %v", terminal)
			}
			later := []*runnerprotocol.PortFrame{
				{
					Fence: cloneRunnerFence(openFence), OperationId: "refused", StreamId: "refused-stream", Sequence: 2,
					Payload: &runnerprotocol.PortFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 1 << 20}},
				},
				{
					Fence: cloneRunnerFence(openFence), OperationId: "refused", StreamId: "refused-stream", Sequence: 3,
					Payload: &runnerprotocol.PortFrame_Bytes{Bytes: &runnerprotocol.PortBytes{Data: []byte("late")}},
				},
				{
					Fence: cloneRunnerFence(openFence), OperationId: "refused", StreamId: "refused-stream", Sequence: 4,
					Payload: &runnerprotocol.PortFrame_Cancel{Cancel: &runnerprotocol.ExecCancel{Reason: "late"}},
				},
			}
			for _, frame := range later {
				if err := service.handlePortFrame(t.Context(), stream, frame, enabled, asyncErrors); err != nil {
					t.Fatalf("frame %d behind a refused Port Open: %v", frame.Sequence, err)
				}
			}
			if got := len(stream.messages()); got != 1 {
				t.Fatalf("frames behind a refused Port Open produced %d frames, want only the terminal", got)
			}
			if err := service.handlePortFrame(t.Context(), stream, later[len(later)-1], enabled, asyncErrors); err != nil {
				t.Fatalf("exact duplicate behind a refused Port Open: %v", err)
			}
			if replay := stream.messages(); len(replay) != 2 || !proto.Equal(replay[1].GetPort(), terminal) {
				t.Fatalf("duplicate did not replay the retained terminal: %v", replay)
			}
			if records := evidenceSink.snapshot(); len(records) != 1 || records[0].Event != "port_terminal" {
				t.Fatalf("refused Port evidence = %+v", records)
			}
			unknown := &runnerprotocol.PortFrame{
				Fence: cloneRunnerFence(fence), OperationId: "never-opened", StreamId: "never-opened-stream", Sequence: 2,
				Payload: &runnerprotocol.PortFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 1}},
			}
			if err := service.handlePortFrame(t.Context(), stream, unknown, enabled, asyncErrors); err == nil {
				t.Fatal("Credit for a Port stream that was never opened was accepted")
			}
		})
	}
}

func TestRunnerExecCapacityRefusalEndsOnlyItsStream(t *testing.T) {
	backend := &relayAssignmentBackend{
		exec: func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen) (BufferedExecResult, error) {
			t.Error("refused Exec reached the backend")
			return BufferedExecResult{}, nil
		},
	}
	service := newRelayRunnerService(t, backend)
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	fillRunnerOperationCapacity(service)
	enabled := map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true}
	asyncErrors := make(chan error, 1)

	if err := service.handleExecFrame(t.Context(), stream, relayExecOpen(fence, "refused", "refused-stream", "true"), enabled, asyncErrors); err != nil {
		t.Fatal(err)
	}
	if terminal := stream.messages()[0].GetExec().GetBufferedResult().GetTerminal(); terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED ||
		terminal.GetSafeDetail() != "runner Exec operation capacity is exhausted" {
		t.Fatalf("capacity-refused Exec terminal = %v", terminal)
	}
	if err := service.handleExecFrame(t.Context(), stream, relayExecCredit(fence, "refused", "refused-stream", 2, 1024), enabled, asyncErrors); err != nil {
		t.Fatalf("Credit behind a capacity-refused Exec: %v", err)
	}
	if got := len(stream.messages()); got != 1 {
		t.Fatalf("Credit behind a capacity-refused Exec produced %d frames", got)
	}
}

// A shell can exit while its user is still typing, so input sequenced after the
// PTY terminal is routine rather than a protocol violation.
func TestRunnerPTYInputAfterTerminalIsDiscarded(t *testing.T) {
	backend := &relayAssignmentBackend{
		pty: func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen, <-chan PTYControl, func([]byte) error) (*runnerprotocol.ExecTerminal, error) {
			return &runnerprotocol.ExecTerminal{Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED}, nil
		},
	}
	service := newRelayRunnerService(t, backend)
	stream := &threadSafeRunnerStream{}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY:            true,
	}
	open := relayExecOpen(fence, "exited", "exited-stream", "exit")
	open.GetOpen().AllocatePty = true
	if err := service.handleExecFrame(t.Context(), stream, open, enabled, make(chan error, 1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		service.operationMu.Lock()
		terminal := service.execOperations[runnerDataPlaneOperationKey(fence, "exited", "exited-stream")].terminal
		service.operationMu.Unlock()
		if terminal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PTY operation did not reach its terminal")
		}
		time.Sleep(time.Millisecond)
	}
	for sequence, frame := range []*runnerprotocol.PtyFrame{
		{Payload: &runnerprotocol.PtyFrame_Input{Input: &runnerprotocol.PtyInput{Data: []byte("typed late")}}},
		{Payload: &runnerprotocol.PtyFrame_Resize{Resize: &runnerprotocol.PtyResize{Rows: 40, Columns: 120}}},
		{Payload: &runnerprotocol.PtyFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 64}}},
	} {
		frame.Fence, frame.OperationId, frame.StreamId = cloneRunnerFence(fence), "exited", "exited-stream"
		frame.Sequence = uint64(sequence + 2)
		if err := service.handlePTYFrame(t.Context(), stream, frame, enabled); err != nil {
			t.Fatalf("PTY frame %d after its terminal: %v", frame.Sequence, err)
		}
	}
}

// A File write sends every chunk behind its Open without waiting for an answer.
// Chunks behind a refused write must neither fail the connection nor start the
// write the Runner refused.
func TestRunnerFileRefusedWriteDiscardsItsChunks(t *testing.T) {
	for _, test := range []struct {
		name       string
		stale      bool
		atCapacity bool
		kind       runnerprotocol.FileTerminalKind
	}{
		{name: "stale fence", stale: true, kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FENCED},
		{name: "capacity", atCapacity: true, kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED},
	} {
		t.Run(test.name, func(t *testing.T) {
			var backendCalls atomic.Int32
			backend := &relayAssignmentBackend{
				file: func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.FileOpen, []byte) (FileOperationResult, error) {
					backendCalls.Add(1)
					return FileOperationResult{}, nil
				},
			}
			service := newRelayRunnerService(t, backend)
			stream := &threadSafeRunnerStream{}
			fence := relayRunnerFence()
			service.recordActiveAssignment(fence, "fc-instance-1")
			if test.atCapacity {
				fillRunnerOperationCapacity(service)
			}
			openFence := cloneRunnerFence(fence)
			if test.stale {
				openFence.SandboxGeneration++
			}
			enabled := map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING: true}
			asyncErrors := make(chan error, 1)
			content := []byte("refused write")
			frames := []*runnerprotocol.FileFrame{
				{
					Sequence: 1, Correlation: relayOperationCorrelation(openFence, "write", "request-write", "lease-write"),
					Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{
						Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "refused.txt",
						ExpectedSize: uint64(len(content)), ExpectedChecksum: "sha256:unused",
					}},
				},
				{Sequence: 2, Payload: &runnerprotocol.FileFrame_Chunk{Chunk: &runnerprotocol.FileChunk{Data: content}}},
			}
			for _, frame := range frames {
				frame.Fence, frame.OperationId, frame.StreamId = cloneRunnerFence(openFence), "write", "write-stream"
				if err := service.handleFileFrame(t.Context(), stream, frame, enabled, asyncErrors); err != nil {
					t.Fatalf("File frame %d: %v", frame.Sequence, err)
				}
			}
			time.Sleep(20 * time.Millisecond)
			if calls := backendCalls.Load(); calls != 0 {
				t.Fatalf("refused File write reached the backend %d times", calls)
			}
			messages := stream.messages()
			if len(messages) != 1 || messages[0].GetFile().GetTerminal().GetKind() != test.kind {
				t.Fatalf("refused File write frames = %v", messages)
			}
		})
	}
}

// One Runner connection carries every tenant's operations. A Port session to a
// guest port without a listener must fail by itself while an Exec already
// running on the same connection keeps running and completes.
func TestRunnerPortOpenFailureKeepsProtocolSessionForConcurrentExec(t *testing.T) {
	fence := relayRunnerFence()
	exec := relayExecOpen(fence, "neighbour-exec", "neighbour-exec-stream", "echo neighbour")
	exec.GetOpen().Streaming = false
	port := relayPortOpen(fence, "closed-port", "closed-port-stream")
	portCredit := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(fence), OperationId: "closed-port", StreamId: "closed-port-stream", Sequence: 2,
		Payload: &runnerprotocol.PortFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 1 << 20}},
	}
	welcome := runnerWelcomeFrame("connection-1")
	welcome.GetWelcome().EnabledFeatures = append(welcome.GetWelcome().EnabledFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
	)
	stream := &scriptedProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			welcome,
			{Message: &runnerprotocol.ControlPlaneToRunner_Exec{Exec: exec}},
			{Message: &runnerprotocol.ControlPlaneToRunner_Port{Port: port}},
			{Message: &runnerprotocol.ControlPlaneToRunner_Port{Port: portCredit}},
			// The receive loop is serial, so this marker's answer proves the
			// refused stream's Credit was handled on a connection still up.
			{Message: &runnerprotocol.ControlPlaneToRunner_Port{Port: relayPortOpen(fence, "marker-port", "marker-port-stream")}},
		},
		sent: make(chan *runnerprotocol.RunnerToControlPlane, 64),
	}
	connector := &sequenceProtocolConnector{streams: []RunnerProtocolStream{stream}}
	config := testRunnerConfig()
	config.MandatoryFeatures = append(config.MandatoryFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
	)
	// The Exec is admitted before the Port Open and cannot finish until the
	// test has observed the Port refusal, so the two always overlap.
	releaseExec := make(chan struct{})
	backend := &refusingPortBackend{relayAssignmentBackend: relayAssignmentBackend{
		exec: func(ctx context.Context, _ *runnerprotocol.AssignmentFence, _ *runnerprotocol.ExecOpen) (BufferedExecResult, error) {
			select {
			case <-releaseExec:
				return BufferedExecResult{
					Stdout:   []byte("neighbour\n"),
					Terminal: &runnerprotocol.ExecTerminal{Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED},
				}, nil
			case <-ctx.Done():
				return BufferedExecResult{}, ctx.Err()
			}
		},
	}}
	service, err := NewRunnerProtocolService(config, backend, connector)
	if err != nil {
		t.Fatal(err)
	}
	service.recordActiveAssignment(fence, "fc-instance-1")
	runContext, cancelRun := context.WithCancel(t.Context())
	runResult := make(chan error, 1)
	go func() { runResult <- service.Run(runContext) }()

	var portTerminal *runnerprotocol.PortTerminal
	var execResult *runnerprotocol.ExecBufferedResult
	timeout := time.After(5 * time.Second)
	for execResult == nil {
		select {
		case message := <-stream.sent:
			if frame := message.GetPort(); frame.GetOperationId() == "closed-port" && frame.GetTerminal() != nil {
				portTerminal = frame.GetTerminal()
			}
			if frame := message.GetPort(); frame.GetOperationId() == "marker-port" && frame.GetTerminal() != nil {
				close(releaseExec)
			}
			if frame := message.GetExec(); frame.GetOperationId() == "neighbour-exec" && frame.GetBufferedResult() != nil {
				execResult = frame.GetBufferedResult()
			}
		case err := <-runResult:
			t.Fatalf("Run() stopped: %v", err)
		case <-timeout:
			t.Fatalf("Port terminal %v and neighbouring Exec result %v did not both arrive on one connection", portTerminal, execResult)
		}
	}
	if portTerminal.GetKind() != runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE ||
		portTerminal.GetSafeDetail() != "guest port is unavailable" {
		t.Fatalf("closed guest Port terminal = %v", portTerminal)
	}
	if execResult.GetTerminal().GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		string(execResult.Stdout) != "neighbour\n" {
		t.Fatalf("neighbouring Exec result = %v", execResult)
	}
	if calls := connector.connectCalls.Load(); calls != 1 {
		t.Fatalf("control-plane connections = %d, want the original one", calls)
	}
	if stream.closed() {
		t.Fatal("the Runner protocol session ended")
	}
	cancelRun()
	if err := <-runResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() cancellation error = %v", err)
	}
}

type refusingPortBackend struct {
	relayAssignmentBackend
}

func (*refusingPortBackend) OpenPort(
	context.Context,
	*runnerprotocol.AssignmentFence,
	*runnerprotocol.PortOpen,
) (PortConnection, error) {
	return nil, errors.New("guest connection refused")
}

// fillRunnerOperationCapacity occupies every Exec and File operation slot with
// active work so the next Open is refused for capacity.
func fillRunnerOperationCapacity(service *RunnerProtocolService) {
	service.operationMu.Lock()
	defer service.operationMu.Unlock()
	for index := range maxRunnerDataPlaneOperationStates {
		key := fmt.Sprintf("occupied-%d", index)
		service.execOperations[key] = &runnerExecOperation{key: key, done: make(chan struct{})}
		service.fileOperations[key] = &runnerFileOperation{key: key}
	}
}

// scriptedProtocolStream delivers a fixed control-plane script, then blocks
// until the Runner ends the session.
type scriptedProtocolStream struct {
	ctx     context.Context
	mu      sync.Mutex
	inbound []*runnerprotocol.ControlPlaneToRunner
	sent    chan *runnerprotocol.RunnerToControlPlane
}

func (s *scriptedProtocolStream) Send(message *runnerprotocol.RunnerToControlPlane) error {
	select {
	case s.sent <- proto.Clone(message).(*runnerprotocol.RunnerToControlPlane):
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *scriptedProtocolStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	s.mu.Lock()
	if len(s.inbound) != 0 {
		message := s.inbound[0]
		s.inbound = s.inbound[1:]
		s.mu.Unlock()
		return message, nil
	}
	s.mu.Unlock()
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (s *scriptedProtocolStream) closed() bool {
	return s.ctx.Err() != nil
}
