package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestBufferedExecEnvironmentAcceptsAgentRuntimeStartupPrompt(t *testing.T) {
	_, err := validateBufferedExecRequest(contracts.BufferedExecRequest{
		Command: contracts.ExecCommand{Mode: "shell", Command: "true"},
		Environment: map[string]string{
			"SANDBOX_STARTUP_CLAUDE_MD": strings.Repeat("x", 10564),
		},
		DeadlineMilliseconds: 1,
		MaximumOutputBytes:   1,
	})
	if err != nil {
		t.Fatalf("validateBufferedExecRequest rejected Agent Runtime startup prompt: %v", err)
	}
}

func TestBufferedExecEnvironmentErrorIdentifiesOnlyVariableShape(t *testing.T) {
	_, err := validateBufferedExecRequest(contracts.BufferedExecRequest{
		Command: contracts.ExecCommand{Mode: "shell", Command: "true"},
		Environment: map[string]string{
			"AGENT_RUNTIME_BUNDLE": strings.Repeat("x", 131073),
		},
		DeadlineMilliseconds: 1,
		MaximumOutputBytes:   1,
	})
	if err == nil {
		t.Fatal("validateBufferedExecRequest accepted an oversized environment value")
	}
	if !strings.Contains(err.Error(), `"AGENT_RUNTIME_BUNDLE"`) ||
		!strings.Contains(err.Error(), "131073") ||
		!strings.Contains(err.Error(), "131072") ||
		strings.Contains(err.Error(), strings.Repeat("x", 16)) {
		t.Fatalf("environment error = %q, want variable name and byte count without value", err)
	}
}

func TestBufferedExecEnvironmentRejectsAggregateAboveOneMiB(t *testing.T) {
	environment := make(map[string]string, 9)
	for index := range 9 {
		environment[fmt.Sprintf("VALUE_%d", index)] = strings.Repeat("x", 131072)
	}
	_, err := validateBufferedExecRequest(contracts.BufferedExecRequest{
		Command:              contracts.ExecCommand{Mode: "shell", Command: "true"},
		Environment:          environment,
		DeadlineMilliseconds: 1,
		MaximumOutputBytes:   1,
	})
	if err == nil || !strings.Contains(err.Error(), "total") {
		t.Fatalf("environment aggregate error = %v, want bounded total", err)
	}
}

func TestExecOutcomeRejectsIncompleteTerminalEvidence(t *testing.T) {
	tests := []runnercontrol.DataPlaneSession{
		{
			TerminalKind:       runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED.String(),
			SpawnFailureReason: runnerv1.SpawnFailureReason_SPAWN_FAILURE_REASON_UNSPECIFIED.String(),
			TerminalMessage:    "spawn failed",
		},
		{
			TerminalKind:         runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED.String(),
			InfrastructureReason: runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_UNSPECIFIED.String(),
			TerminalMessage:      "runner failed",
		},
		{
			TerminalKind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED.String(),
			LimitBytes:   0,
		},
		{
			TerminalKind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED.String(),
			ExitCode:     256,
		},
	}
	for _, session := range tests {
		if _, err := execOutcome(session); err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Fatalf("execOutcome(%s) error = %v, want incomplete evidence", session.TerminalKind, err)
		}
	}
}

func TestWaitForFileDeadlineRequiresGuestTerminalProof(t *testing.T) {
	relay := &deadlineProofRelay{}
	service := &ControlPlaneService{
		dataPlaneStore: relay, dataPlanePollInterval: time.Millisecond,
		now: func() time.Time { return time.Unix(100, 0).UTC() },
	}
	session, err := service.waitForDataPlane(
		t.Context(), "tenant", "subject", runnercontrol.DataPlaneSession{
			ID: "session", Kind: "file", State: "running",
		}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !relay.expired || relay.getCalls == 0 {
		t.Fatalf("deadline proof calls: expired=%t get=%d", relay.expired, relay.getCalls)
	}
	if session.State != "completed" ||
		session.TerminalKind != runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String() {
		t.Fatalf("deadline proof session = %#v", session)
	}
	if err := fileTerminalError(session); err != runnercontrol.ErrDataPlaneDeadline {
		t.Fatalf("fileTerminalError = %v, want %v", err, runnercontrol.ErrDataPlaneDeadline)
	}
}

func TestStreamingExecCancellationPersistsWhenLiveRouteIsUnavailable(t *testing.T) {
	relay := &cancellationProofRelay{}
	stream := &SandboxExecStream{
		service: &ControlPlaneService{
			dataPlaneStore: relay, now: func() time.Time { return time.Unix(100, 0).UTC() },
		},
		session: runnercontrol.DataPlaneSession{
			ID: "session", StreamID: "stream", TenantRef: "tenant", SubjectRef: "subject",
		},
		stream: unavailableDataPlaneStream{},
	}
	err := stream.Cancel(t.Context(), "connection replaced")
	if !errors.Is(err, runnercontrol.ErrLiveDataPlaneUnavailable) {
		t.Fatalf("stream cancellation error = %v", err)
	}
	if !relay.cancelled {
		t.Fatal("stream cancellation was not persisted after live route loss")
	}
}

func TestTerminalFramesDoNotSynchronouslyCheckpoint(t *testing.T) {
	relay := &terminalCheckpointProofRelay{}
	session := runnercontrol.DataPlaneSession{
		ID: "terminal-session", StreamID: "terminal-stream", Kind: "terminal",
		TenantRef: "tenant", SubjectRef: "subject", SandboxID: "sandbox",
		AssignmentID: "assignment", InstanceID: "instance", RunnerID: "runner",
		LeaseID: "lease", RequestID: "request", Generation: 1,
		FencingToken: []byte("fence"), MaximumRequestBytes: 64,
		MaximumResponseBytes: 64, StreamWindowBytes: 8,
	}
	wire := &queuedDataPlaneStream{receive: []*runnerv1.RunnerToControlPlane{{
		Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
			Fence: dataPlaneFence(session), Correlation: dataPlaneCorrelation(session),
			OperationId: session.ID, StreamId: session.StreamID, Sequence: 1,
			Payload: &runnerv1.PtyFrame_Output{Output: &runnerv1.ExecOutput{
				Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, Data: []byte("x"),
			}},
		}},
	}}}
	stream := &SandboxTerminalStream{
		service: &ControlPlaneService{dataPlaneStore: relay, now: time.Now},
		session: session, attachmentID: "attachment", stream: wire,
		credit: 1, nextRecv: 0, recordedThrough: -1,
	}
	if err := stream.Send(t.Context(), runnercontrol.TerminalClientFrame{
		Sequence: 0, Input: []byte("k"),
	}); err != nil {
		t.Fatal(err)
	}
	frame, _, err := stream.Receive(t.Context())
	if err != nil || string(frame.Output) != "x" {
		t.Fatalf("Terminal output = %#v, %v", frame, err)
	}
	if relay.checkpoints != 0 {
		t.Fatalf("per-frame durable checkpoints = %d, want 0", relay.checkpoints)
	}
}

func TestTerminalAttachSequenceConvergesAcrossCheckpointGap(t *testing.T) {
	durable := runnercontrol.DataPlaneSession{State: "running", NextClientSequence: 1}
	runnerNext := uint64(9)
	next, err := terminalAttachNextClientSequence(durable, &runnerv1.PtyAttachResult{
		NextInputSequence: &runnerNext,
	})
	if err != nil || next != 7 {
		t.Fatalf("Runner-authoritative next client sequence = %d, %v", next, err)
	}
	if _, err := terminalAttachNextClientSequence(
		durable, &runnerv1.PtyAttachResult{},
	); !errors.Is(err, runnercontrol.ErrTerminalReplayEvicted) {
		t.Fatalf("older Runner reattach error = %v", err)
	}
	pending := runnercontrol.DataPlaneSession{State: "pending", NextClientSequence: 3}
	if next, err := terminalAttachNextClientSequence(pending, &runnerv1.PtyAttachResult{}); err != nil || next != 3 {
		t.Fatalf("initial older Runner attach sequence = %d, %v", next, err)
	}
	stream := &SandboxTerminalStream{
		service: &ControlPlaneService{now: time.Now},
		session: runnercontrol.DataPlaneSession{
			ID: "terminal-gap", StreamID: "stream-gap", MaximumRequestBytes: 64,
			MaximumResponseBytes: 64, StreamWindowBytes: 8,
		},
		stream: &queuedDataPlaneStream{}, nextSend: next,
		credit: 8, replayAllowance: 8,
	}
	if err := stream.Send(t.Context(), runnercontrol.TerminalClientFrame{
		Sequence: next, Credit: 8,
	}); err != nil {
		t.Fatalf("fresh credit across checkpoint gap: %v", err)
	}
	if err := stream.Send(t.Context(), runnercontrol.TerminalClientFrame{
		Sequence: next + 1, Credit: 1,
	}); !errors.Is(err, runnercontrol.ErrDataPlaneFrameLimit) {
		t.Fatalf("credit beyond one-window recovery allowance = %v", err)
	}
}

func TestAttachedTerminalCheckpointsOnDataPlanePollInterval(t *testing.T) {
	checkpointed := make(chan runnercontrol.TerminalCheckpoint, 1)
	relay := &terminalCheckpointProofRelay{checkpointed: checkpointed}
	stream := &SandboxTerminalStream{
		service: &ControlPlaneService{
			dataPlaneStore: relay, dataPlanePollInterval: time.Millisecond, now: time.Now,
		},
		session: runnercontrol.DataPlaneSession{
			ID: "terminal-periodic", TenantRef: "tenant", SubjectRef: "subject",
		},
		attachmentID: "attachment", nextSend: 4, nextRecv: 3,
		request: 2, credit: 8, emitted: 3, version: 1,
	}
	stream.startCheckpointing()
	select {
	case checkpoint := <-checkpointed:
		if checkpoint.NextClientSequence != 4 || checkpoint.NextInboundSequence != 4 ||
			checkpoint.RequestBytes != 2 || checkpoint.ResponseCredit != 8 || checkpoint.InboundBytes != 3 {
			t.Fatalf("periodic Terminal checkpoint = %#v", checkpoint)
		}
	case <-time.After(time.Second):
		t.Fatal("periodic Terminal checkpoint did not run")
	}
	stream.checkpointCancel()
	<-stream.checkpointDone
}

type queuedDataPlaneStream struct {
	receive []*runnerv1.RunnerToControlPlane
}

func (*queuedDataPlaneStream) Send(*runnerv1.ControlPlaneToRunner) error { return nil }

func (stream *queuedDataPlaneStream) Receive(context.Context) (*runnerv1.RunnerToControlPlane, error) {
	if len(stream.receive) == 0 {
		return nil, errors.New("SecondBox test data-plane stream is empty")
	}
	message := stream.receive[0]
	stream.receive = stream.receive[1:]
	return message, nil
}

func (*queuedDataPlaneStream) Close() error { return nil }

type terminalCheckpointProofRelay struct {
	deadlineProofRelay
	checkpoints  int
	checkpointed chan<- runnercontrol.TerminalCheckpoint
}

func (*terminalCheckpointProofRelay) AcquireTerminalAttachment(
	context.Context, string, string, string, string, int64, string, time.Time,
) (runnercontrol.DataPlaneSession, error) {
	panic("unexpected Terminal attachment")
}

func (*terminalCheckpointProofRelay) DetachTerminalAttachment(
	context.Context, string, string, string, string, time.Time,
) (bool, error) {
	panic("unexpected Terminal detach")
}

func (relay *terminalCheckpointProofRelay) CheckpointTerminal(
	_ context.Context, _, _, _ string, checkpoint runnercontrol.TerminalCheckpoint, _ time.Time,
) (runnercontrol.DataPlaneSession, error) {
	relay.checkpoints++
	if relay.checkpointed != nil {
		relay.checkpointed <- checkpoint
	}
	return runnercontrol.DataPlaneSession{ID: "terminal-periodic", TenantRef: "tenant", SubjectRef: "subject"}, nil
}

type unavailableDataPlaneStream struct{}

func (unavailableDataPlaneStream) Send(*runnerv1.ControlPlaneToRunner) error {
	return runnercontrol.ErrLiveDataPlaneUnavailable
}

func (unavailableDataPlaneStream) Receive(context.Context) (*runnerv1.RunnerToControlPlane, error) {
	return nil, runnercontrol.ErrLiveDataPlaneUnavailable
}

func (unavailableDataPlaneStream) Close() error { return nil }

type cancellationProofRelay struct {
	deadlineProofRelay
	cancelled bool
}

func (relay *cancellationProofRelay) CancelDataPlaneSession(
	context.Context, string, string, string, string, time.Time,
) (bool, error) {
	relay.cancelled = true
	return true, nil
}

type deadlineProofRelay struct {
	expired  bool
	getCalls int
}

func (*deadlineProofRelay) AdmitDataPlane(context.Context, runnercontrol.DataPlaneAdmission) (runnercontrol.DataPlaneSession, bool, error) {
	panic("unexpected admission")
}

func (relay *deadlineProofRelay) GetDataPlaneSession(context.Context, string, string, string) (runnercontrol.DataPlaneSession, error) {
	relay.getCalls++
	if !relay.expired {
		return runnercontrol.DataPlaneSession{ID: "session", Kind: "file", State: "running"}, nil
	}
	return runnercontrol.DataPlaneSession{
		ID: "session", Kind: "file", State: "completed",
		TerminalKind: runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String(),
	}, nil
}

func (*deadlineProofRelay) StartDataPlaneSession(
	context.Context, string, string, string, time.Time,
) (runnercontrol.DataPlaneSession, error) {
	panic("unexpected live data-plane start")
}

func (relay *deadlineProofRelay) ExpireDataPlaneSession(context.Context, string, string, string, time.Time) (runnercontrol.DataPlaneSession, error) {
	relay.expired = true
	return runnercontrol.DataPlaneSession{
		ID: "session", Kind: "file", State: "cancelling",
		TerminalKind: runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String(),
	}, nil
}

func (*deadlineProofRelay) CompleteDataPlaneSession(
	context.Context, runnercontrol.DataPlaneCompletion,
) (runnercontrol.DataPlaneSession, error) {
	panic("unexpected live data-plane completion")
}

func (*deadlineProofRelay) ConsumeDirectDataPlaneSession(
	context.Context, runnercontrol.DirectDataPlaneConsumption,
) error {
	panic("unexpected direct data-plane consumption")
}

func (*deadlineProofRelay) CancelDataPlaneSession(
	context.Context, string, string, string, string, time.Time,
) (bool, error) {
	panic("unexpected streaming Exec cancellation")
}

func (*deadlineProofRelay) CancelPublicDataPlaneSession(
	context.Context,
	runnercontrol.PublicDataPlaneCancellation,
) (runnercontrol.DataPlaneSession, bool, error) {
	panic("unexpected public streaming session cancellation")
}
