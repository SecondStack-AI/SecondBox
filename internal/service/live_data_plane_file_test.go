package service

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
)

// The Runner may close after its terminal while a per-chunk credit is still on
// the way, so that credit fails to send although the terminal is readable.
func TestFileReadCompletesWhenCreditSendFailsAfterRunnerTerminal(t *testing.T) {
	store := &fileCompletionRelay{}
	service := &ControlPlaneService{dataPlaneStore: store, now: func() time.Time { return time.Unix(100, 0).UTC() }}
	session := runnercontrol.DataPlaneSession{
		ID: "session", StreamID: "stream", Kind: "file", TenantRef: "tenant", SubjectRef: "subject",
		MaximumResponseBytes: 1024, StreamWindowBytes: 64,
	}
	frame := func(sequence uint64, file *runnerv1.FileFrame) *runnerv1.RunnerToControlPlane {
		file.OperationId, file.StreamId, file.Sequence = session.ID, session.StreamID, sequence
		return &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_File{File: file}}
	}
	stream := &closedAfterTerminalDataPlaneStream{received: []*runnerv1.RunnerToControlPlane{
		frame(1, &runnerv1.FileFrame{Payload: &runnerv1.FileFrame_Metadata{Metadata: &runnerv1.FileMetadata{Size: 5}}}),
		frame(2, &runnerv1.FileFrame{Payload: &runnerv1.FileFrame_Chunk{Chunk: &runnerv1.FileChunk{Data: []byte("hello")}}}),
		frame(3, &runnerv1.FileFrame{Payload: &runnerv1.FileFrame_Terminal{Terminal: &runnerv1.FileTerminal{
			Kind: runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
		}}}),
	}}
	completed, err := service.exchangeFileDataPlane(
		t.Context(), t.Context(), session, stream,
		&runnerv1.FileOpen{Operation: runnerv1.FileOperation_FILE_OPERATION_READ}, nil,
	)
	if err != nil {
		t.Fatalf("File read after a failed credit send: %v", err)
	}
	if completed.ID != session.ID || store.completion == nil || string(store.completion.Content) != "hello" ||
		store.completion.Terminal.GetKind() != runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("File completion = %#v", store.completion)
	}
	if stream.sends != 2 {
		t.Fatalf("credit sends = %d, want the initial grant and one failed per-chunk grant", stream.sends)
	}
}

func TestFileExchangeFailsAtOnceOnNonTransportSendError(t *testing.T) {
	violation := errors.New("credit violation")
	service := &ControlPlaneService{dataPlaneStore: &fileCompletionRelay{}}
	_, err := service.exchangeFileDataPlane(
		t.Context(), t.Context(), runnercontrol.DataPlaneSession{MaximumResponseBytes: 1024, StreamWindowBytes: 64},
		&failingDataPlaneStream{err: violation},
		&runnerv1.FileOpen{Operation: runnerv1.FileOperation_FILE_OPERATION_READ}, nil,
	)
	if !errors.Is(err, violation) {
		t.Fatalf("File exchange error = %v, want %v", err, violation)
	}
}

type closedAfterTerminalDataPlaneStream struct {
	received []*runnerv1.RunnerToControlPlane
	sends    int
}

func (stream *closedAfterTerminalDataPlaneStream) Send(*runnerv1.ControlPlaneToRunner) error {
	stream.sends++
	if stream.sends > 1 {
		return errors.Join(runnercontrol.ErrLiveDataPlaneUnavailable, errors.New("write: broken pipe"))
	}
	return nil
}

func (stream *closedAfterTerminalDataPlaneStream) Receive(context.Context) (*runnerv1.RunnerToControlPlane, error) {
	if len(stream.received) == 0 {
		return nil, errors.New("connection reset by peer")
	}
	message := stream.received[0]
	stream.received = stream.received[1:]
	return message, nil
}

func (*closedAfterTerminalDataPlaneStream) Close() error { return nil }

type failingDataPlaneStream struct{ err error }

func (stream *failingDataPlaneStream) Send(*runnerv1.ControlPlaneToRunner) error { return stream.err }

func (*failingDataPlaneStream) Receive(context.Context) (*runnerv1.RunnerToControlPlane, error) {
	panic("File exchange received after a non-transport send failure")
}

func (*failingDataPlaneStream) Close() error { return nil }

type fileCompletionRelay struct {
	deadlineProofRelay
	completion *runnercontrol.FileCompletion
}

func (relay *fileCompletionRelay) CompleteDataPlaneSession(
	_ context.Context,
	completion runnercontrol.DataPlaneCompletion,
) (runnercontrol.DataPlaneSession, error) {
	relay.completion = completion.File
	return runnercontrol.DataPlaneSession{ID: completion.SessionID, State: "completed"}, nil
}
