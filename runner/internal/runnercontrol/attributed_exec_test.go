package runnercontrol

import (
	"context"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

func testExecAttribution() *runnerprotocol.AttributedExecution {
	return &runnerprotocol.AttributedExecution{
		TenantRef: "tenant", SubjectRef: "subject", AuthorizationRef: "command",
		ExpiresAtUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
	}
}

// The backend owns the attributed window, so the service forwards the
// admitted attribution unchanged and keeps the assignment active afterwards.
func TestAttributedExecForwardsAttributionWithoutRetiringAssignment(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[streaming], func(t *testing.T) {
			received := make(chan *runnerprotocol.ExecOpen, 1)
			backend := &relayAssignmentBackend{}
			backend.exec = func(_ context.Context, _ *runnerprotocol.AssignmentFence, open *runnerprotocol.ExecOpen) (BufferedExecResult, error) {
				received <- open
				return BufferedExecResult{Terminal: &runnerprotocol.ExecTerminal{Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED}}, nil
			}
			backend.streaming = func(ctx context.Context, fence *runnerprotocol.AssignmentFence, open *runnerprotocol.ExecOpen, _ <-chan ExecControl, _ func(runnerprotocol.ExecOutputChannel, []byte) error) (*runnerprotocol.ExecTerminal, error) {
				result, err := backend.exec(ctx, fence, open)
				return result.Terminal, err
			}
			service := newRelayRunnerService(t, backend)
			fence := relayRunnerFence()
			service.recordActiveAssignment(fence, "instance")
			stream := &threadSafeRunnerStream{}
			frame := relayExecOpen(fence, "attributed", "attributed-stream", "true")
			frame.GetOpen().Streaming = streaming
			frame.GetOpen().AttributedExecution = testExecAttribution()
			if err := service.handleExecFrame(t.Context(), stream, frame, map[runnerprotocol.RunnerFeature]bool{
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
			}, make(chan error, 1)); err != nil {
				t.Fatal(err)
			}
			select {
			case open := <-received:
				if !proto.Equal(open.AttributedExecution, frame.GetOpen().AttributedExecution) {
					t.Fatalf("backend attribution = %v", open.AttributedExecution)
				}
			case <-time.After(time.Second):
				t.Fatal("backend received no exec")
			}
			waitRunnerMessages(t, stream, 1)
			if len(service.activeAssignments()) != 1 {
				t.Fatalf("attributed exec retired its assignment: %v", service.activeAssignments())
			}
		})
	}
}

func TestAttributedExecRefusesPTY(t *testing.T) {
	backend := &relayAssignmentBackend{}
	backend.pty = func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen, <-chan PTYControl, func([]byte) error) (*runnerprotocol.ExecTerminal, error) {
		t.Error("attributed PTY reached the backend")
		return nil, nil
	}
	service := newRelayRunnerService(t, backend)
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "instance")
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY:            true,
	}
	stream := &threadSafeRunnerStream{}
	open := relayExecOpen(fence, "attributed-terminal", "attributed-terminal-stream", "sh")
	open.GetOpen().AllocatePty = true
	open.GetOpen().PtyRows, open.GetOpen().PtyColumns = 24, 80
	open.GetOpen().AttributedExecution = testExecAttribution()
	if err := service.handleExecFrame(t.Context(), stream, open, enabled, make(chan error, 1)); err != nil {
		t.Fatal(err)
	}
	if err := service.handlePTYFrame(t.Context(), stream, &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(fence), OperationId: "attributed-terminal", StreamId: "attributed-terminal-stream", Sequence: 2,
		Payload: &runnerprotocol.PtyFrame_Attach{Attach: &runnerprotocol.PtyAttach{
			ReconnectId: "attachment", AfterSequence: -1, StreamWindowBytes: 1024,
		}},
	}, enabled); err != nil {
		t.Fatal(err)
	}
	waitRunnerMessages(t, stream, 1)
	terminal := stream.messages()[len(stream.messages())-1].GetPty().GetTerminal()
	if terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED ||
		terminal.GetInfrastructureFailureReason() != runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_ADMISSION {
		t.Fatalf("attributed PTY terminal = %v", terminal)
	}
}

func TestDirectExecStreamRefusesAttribution(t *testing.T) {
	fence := relayRunnerFence()
	session := &directDataPlaneSession{
		fence: fence, operationID: "direct", streamID: "direct-stream",
		kind: runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC,
	}
	frame := relayExecOpen(fence, "direct", "direct-stream", "true")
	frame.GetOpen().AttributedExecution = testExecAttribution()
	message := &runnerprotocol.ControlPlaneToRunner{Message: &runnerprotocol.ControlPlaneToRunner_Exec{Exec: frame}}
	if err := validateDirectFirstMessage(session, message); err == nil {
		t.Fatal("direct Exec stream accepted client-supplied attribution")
	}
	frame.GetOpen().AttributedExecution = nil
	if err := validateDirectFirstMessage(session, message); err != nil {
		t.Fatalf("ordinary direct Exec stream = %v", err)
	}
}
