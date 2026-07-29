package service

import (
	"context"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
)

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

func (relay *deadlineProofRelay) ExpireDataPlaneSession(context.Context, string, string, string, time.Time) (runnercontrol.DataPlaneSession, error) {
	relay.expired = true
	return runnercontrol.DataPlaneSession{
		ID: "session", Kind: "file", State: "cancelling",
		TerminalKind: runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String(),
	}, nil
}

func (*deadlineProofRelay) AppendExecClientFrame(
	context.Context, string, string, string, runnercontrol.ExecClientFrame, time.Time,
) (bool, error) {
	panic("unexpected streaming Exec frame")
}

func (*deadlineProofRelay) ListExecServerFrames(
	context.Context, string, string, string, int64, int,
) ([]runnercontrol.ExecServerFrame, error) {
	panic("unexpected streaming Exec frame lookup")
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
