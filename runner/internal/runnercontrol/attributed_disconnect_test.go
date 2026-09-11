package runnercontrol

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type attributedDisconnectBackend struct {
	recordingAssignmentBackend
	fenced   atomic.Bool
	fenceErr error
}

type disconnectAfterStartStream struct {
	recordingProtocolStream
	started          <-chan struct{}
	beforeDisconnect func()
}

func (stream *disconnectAfterStartStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	frame, err := stream.recordingProtocolStream.Recv()
	if errors.Is(err, io.EOF) {
		<-stream.started
		stream.beforeDisconnect()
	}
	return frame, err
}

func TestRunnerDisconnectCancelsPendingAttributedStart(t *testing.T) {
	backend := &blockingAssignmentBackend{
		recordingAssignmentBackend: recordingAssignmentBackend{readiness: BackendReadiness{
			Capacity: &runnerprotocol.Capacity{}, Reserved: &runnerprotocol.Capacity{}, Capabilities: &runnerprotocol.RunnerCapabilities{},
		}},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	assignment := resolvedAssignmentCommand()
	assignment.AttributedExecution = &runnerprotocol.AttributedExecution{AuthorizationRef: "pending", ExpiresAtUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli())}
	assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "attributed-execution")
	stream := &disconnectAfterStartStream{started: backend.started, recordingProtocolStream: recordingProtocolStream{inbound: []*runnerprotocol.ControlPlaneToRunner{
		runnerWelcomeFrame("pending"), {Message: &runnerprotocol.ControlPlaneToRunner_Assignment{Assignment: assignment}},
	}}}
	stream.beforeDisconnect = func() {
		if err := backend.startContext.Err(); err != nil {
			t.Errorf("startup expired before disconnect: %v", err)
		}
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := service.runProtocolSession(ctx); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("disconnect result: %v", err)
		}
	case <-time.After(time.Second):
		cancel()
		<-result
		t.Fatal("disconnect waited for startup before cancelling it")
	}
}

func (backend *attributedDisconnectBackend) RecoveredAssignments() []*runnerprotocol.ActiveAssignmentSummary {
	if backend.started == nil || backend.fenced.Load() {
		return nil
	}
	return []*runnerprotocol.ActiveAssignmentSummary{runnerprotocol.RecoveredAssignmentSummary(backend.started.Fence, backend.started.EgressContext)}
}

func (backend *attributedDisconnectBackend) AttributedAssignmentFences() []*runnerprotocol.AssignmentFence {
	if backend.started == nil || backend.fenced.Load() {
		return nil
	}
	return []*runnerprotocol.AssignmentFence{proto.CloneOf(backend.started.Fence)}
}

func (backend *attributedDisconnectBackend) FenceAssignment(ctx context.Context, command *runnerprotocol.FenceCommand) (FenceEvidence, error) {
	backend.fenceCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return FenceEvidence{}, err
	}
	if backend.fenceErr != nil {
		return FenceEvidence{}, backend.fenceErr
	}
	backend.fenced.Store(true)
	return FenceEvidence{Result: runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED}, nil
}

func TestRunnerProtocolDisconnectFencesAttributedAssignment(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminated", true: "termination_failed"}[failed], func(t *testing.T) {
			assignment := resolvedAssignmentCommand()
			assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "attributed-execution")
			assignment.AttributedExecution = &runnerprotocol.AttributedExecution{AuthorizationRef: "authorization", ExpiresAtUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli())}
			first := &recordingProtocolStream{inbound: []*runnerprotocol.ControlPlaneToRunner{
				runnerWelcomeFrame("first"), {Message: &runnerprotocol.ControlPlaneToRunner_Assignment{Assignment: assignment}},
			}}
			second := &blockingProtocolStream{inbound: []*runnerprotocol.ControlPlaneToRunner{runnerWelcomeFrame("second")}, heartbeats: make(chan *runnerprotocol.RunnerHeartbeat, 1)}
			connector := &sequenceProtocolConnector{streams: []RunnerProtocolStream{first, second}}
			backend := &attributedDisconnectBackend{recordingAssignmentBackend: recordingAssignmentBackend{
				readiness: BackendReadiness{Capacity: &runnerprotocol.Capacity{}, Reserved: &runnerprotocol.Capacity{}, Capabilities: &runnerprotocol.RunnerCapabilities{}},
				instance:  BackendInstance{BackendKind: "firecracker", BackendReference: "instance"},
			}}
			failure := errors.New("test unconfirmed termination")
			if failed {
				backend.fenceErr = failure
			}
			service, err := NewRunnerProtocolService(testRunnerConfig(), backend, connector)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() { defer close(done); result <- service.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("Runner did not stop after test cancellation")
				}
			})
			select {
			case err := <-result:
				if !failed || !errors.Is(err, failure) {
					t.Fatalf("Run ended: %v", err)
				}
				if connector.connectCalls.Load() != 1 {
					t.Fatal("reconnected after unconfirmed termination")
				}
			case heartbeat := <-second.heartbeats:
				if failed {
					t.Fatal("reconnected despite failed termination")
				}
				if len(heartbeat.ActiveAssignments) != 0 || backend.fenceCalls.Load() != 1 {
					t.Fatal("attributed assignment survived reconnect")
				}
				cancel()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("Run cancellation: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("attributed disconnect did not settle")
			}
		})
	}
}

func TestRunnerStartupFencesRecoveredAttributionWithCancelledContext(t *testing.T) {
	assignment := resolvedAssignmentCommand()
	assignment.AttributedExecution = &runnerprotocol.AttributedExecution{AuthorizationRef: "recovered"}
	assignment.Requirements.RequiredCapabilities = append(assignment.Requirements.RequiredCapabilities, "attributed-execution")
	backend := &attributedDisconnectBackend{recordingAssignmentBackend: recordingAssignmentBackend{started: assignment}}
	connector := &sequenceProtocolConnector{}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, connector)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := service.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancellation: %v", err)
	}
	if backend.fenceCalls.Load() != 1 || len(service.activeAssignments()) != 0 {
		t.Fatal("cancelled startup retained attributed compute")
	}
	if connector.connectCalls.Load() != 0 {
		t.Fatal("cancelled Runner established a control-plane connection")
	}
}
