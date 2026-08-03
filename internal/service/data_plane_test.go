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
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
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
		dataPlaneRelay: relay, dataPlanePollInterval: time.Millisecond,
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
			dataPlaneRelay: relay, now: func() time.Time { return time.Unix(100, 0).UTC() },
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

// TestSubscribeDataPlaneSessionFallsBackToPollingWithoutASource proves a
// deployment that has not enabled notifications keeps working. A nil channel
// blocks forever in a select, so the caller stays on its poll interval, which is
// the recovery bound in both cases.
func TestSubscribeDataPlaneSessionFallsBackToPollingWithoutASource(t *testing.T) {
	service := &ControlPlaneService{}
	wakeups, cancel := service.SubscribeDataPlaneSession("dps_1")
	if wakeups != nil {
		t.Error("a service without a wakeup source must yield no wakeup channel")
	}
	if cancel == nil {
		t.Fatal("cancellation must always be callable")
	}
	cancel()
	cancel()
}

// TestSubscribeDataPlaneSessionIsKeyedBySession proves one session's frames
// never wake another session's loop.
func TestSubscribeDataPlaneSessionIsKeyedBySession(t *testing.T) {
	hub := worknotify.NewHub()
	service := &ControlPlaneService{dataPlaneWakeups: hub}
	wakeups, cancel := service.SubscribeDataPlaneSession("dps_1")
	defer cancel()
	if wakeups == nil {
		t.Fatal("a configured wakeup source must yield a wakeup channel")
	}
	hub.Publish(worknotify.KindDataPlaneSession, "dps_2")
	select {
	case <-wakeups:
		t.Fatal("another session's frame woke this loop")
	default:
	}
	hub.Publish(worknotify.KindDataPlaneSession, "dps_1")
	select {
	case <-wakeups:
	default:
		t.Fatal("this session's frame did not wake its loop")
	}
	// An empty session cannot be subscribed, so it must not consume the source.
	empty, cancelEmpty := service.SubscribeDataPlaneSession("")
	defer cancelEmpty()
	if empty != nil {
		t.Error("an empty session ID must not produce a subscription")
	}
}
