package runnercontrol

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestLiveDataPlaneBrokerDropsUnknownRouteAndKeepsConnectionHealthy(t *testing.T) {
	broker, detach := liveDataPlaneTestBroker(t)
	defer detach()
	server := &Server{config: ServerConfig{
		LiveDataPlane: broker, CancelConfirmations: &recordingCancellationConfirmer{},
	}}

	if err := server.persistEvent(
		t.Context(),
		liveDataPlaneExecEvent("missing-operation", "missing-stream", 1, []byte("stale")),
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if got := broker.MetricsSnapshot().DroppedRouteNotFoundFrames; got != 1 {
		t.Fatalf("dropped route-not-found frames = %d, want 1", got)
	}

	stream := openCreditedExecRoute(t, broker, "healthy-operation", "healthy-stream", 8)
	defer stream.Close()
	if _, err := broker.Deliver(t.Context(), liveDataPlaneExecEvent(
		"healthy-operation", "healthy-stream", 1, []byte("healthy"),
	)); err != nil {
		t.Fatal(err)
	}
	message, err := stream.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(message.GetExec().GetOutput().Data); got != "healthy" {
		t.Fatalf("healthy route payload = %q", got)
	}
}

func TestLiveDataPlaneBrokerSlowConsumerDoesNotBlockOtherRoutes(t *testing.T) {
	broker, detach := liveDataPlaneTestBroker(t)
	defer detach()

	slow := openCreditedExecRoute(t, broker, "slow-operation", "slow-stream", 64)
	defer slow.Close()
	healthy := openCreditedExecRoute(t, broker, "healthy-operation", "healthy-stream", 1)
	defer healthy.Close()

	delivered := make(chan error, 1)
	go func() {
		for sequence := uint64(1); sequence <= 64; sequence++ {
			if _, err := broker.Deliver(t.Context(), liveDataPlaneExecEvent(
				"slow-operation", "slow-stream", sequence, []byte{byte(sequence)},
			)); err != nil {
				delivered <- err
				return
			}
		}
		_, err := broker.Deliver(t.Context(), liveDataPlaneExecEvent(
			"healthy-operation", "healthy-stream", 1, []byte("b"),
		))
		delivered <- err
	}()

	select {
	case err := <-delivered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow route blocked broker delivery")
	}
	message, err := healthy.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(message.GetExec().GetOutput().Data); got != "b" {
		t.Fatalf("healthy route payload = %q", got)
	}
}

func TestLiveDataPlaneBrokerCreditViolationFailsOnlyOneRoute(t *testing.T) {
	broker, detach := liveDataPlaneTestBroker(t)
	defer detach()
	server := &Server{config: ServerConfig{
		LiveDataPlane: broker, CancelConfirmations: &recordingCancellationConfirmer{},
	}}

	violating := openCreditedExecRoute(t, broker, "violating-operation", "violating-stream", 2)
	defer violating.Close()
	healthy := openCreditedExecRoute(t, broker, "healthy-operation", "healthy-stream", 4)
	defer healthy.Close()

	if err := server.persistEvent(
		t.Context(),
		liveDataPlaneExecEvent("violating-operation", "violating-stream", 1, []byte("too large")),
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("route-local credit violation reached connection: %v", err)
	}
	if _, err := violating.Receive(t.Context()); !errors.Is(err, ErrLiveDataPlaneCreditViolation) {
		t.Fatalf("violating route error = %v, want credit violation", err)
	}
	if err := server.persistEvent(
		t.Context(),
		liveDataPlaneExecEvent("healthy-operation", "healthy-stream", 1, []byte("live")),
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	message, err := healthy.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(message.GetExec().GetOutput().Data); got != "live" {
		t.Fatalf("healthy route payload = %q", got)
	}
}

func liveDataPlaneTestBroker(t *testing.T) (*LiveDataPlaneBroker, func()) {
	t.Helper()
	session := negotiatedDataPlaneSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	broker := NewLiveDataPlaneBroker()
	detach, err := broker.AttachConnection(
		"runner-1", "connection-1", &recordingControlPlaneSender{}, session,
	)
	if err != nil {
		t.Fatal(err)
	}
	return broker, detach
}

func openCreditedExecRoute(
	t *testing.T,
	broker *LiveDataPlaneBroker,
	operationID string,
	streamID string,
	credit int64,
) *LiveDataPlaneStream {
	t.Helper()
	stream, err := broker.Open("runner-1", "exec", operationID, streamID, credit, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
			Fence: dataPlaneTestFence(), OperationId: operationID, StreamId: streamID,
			Sequence: 1,
			Payload: &runnerv1.ExecFrame_Credit{Credit: &runnerv1.StreamCredit{
				ByteCount: uint64(credit),
			}},
		}},
	}); err != nil {
		stream.Close()
		t.Fatal(err)
	}
	return stream
}

func liveDataPlaneExecEvent(
	operationID string,
	streamID string,
	sequence uint64,
	payload []byte,
) Event {
	return Event{
		Kind: EventExec, RunnerID: "runner-1", ConnectionID: "connection-1",
		Message: runnerExecFrame(
			dataPlaneTestFence(), operationID, streamID, sequence,
			&runnerv1.ExecFrame_Output{Output: &runnerv1.ExecOutput{
				Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
				Data:    payload,
			}},
		),
	}
}

type recordingCancellationConfirmer struct {
	frames []*runnerv1.RunnerToControlPlane
}

func (confirmer *recordingCancellationConfirmer) ConfirmDataPlaneCancellation(
	_ context.Context,
	frame RunnerDataPlaneFrame,
	_ time.Time,
) error {
	confirmer.frames = append(confirmer.frames, frame.Message)
	return nil
}

// A terminal that no live route takes is the only proof that a cancelled
// operation stopped after its request ended, so it reaches the cancellation
// confirmer, as does a repeat of a terminal a route already holds. A first
// routed terminal and route-less output never touch PostgreSQL.
func TestRouteLessOperationTerminalConfirmsTheCancellation(t *testing.T) {
	broker, detach := liveDataPlaneTestBroker(t)
	defer detach()
	confirmer := &recordingCancellationConfirmer{}
	server := &Server{config: ServerConfig{LiveDataPlane: broker, CancelConfirmations: confirmer}}
	fence := dataPlaneTestFence()
	cancelled := &runnerv1.ExecTerminal{Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED, ExitCode: -1}
	routed := openCreditedExecRoute(t, broker, "routed-operation", "routed-stream", 8)
	defer routed.Close()
	for _, event := range []Event{
		liveDataPlaneExecEvent("orphan-output", "orphan-stream", 1, []byte("late")),
		{Kind: EventExec, Message: &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_Exec{
			Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: "routed-operation", StreamId: "routed-stream", Sequence: 1,
				Payload: &runnerv1.ExecFrame_BufferedResult{BufferedResult: &runnerv1.ExecBufferedResult{Terminal: cancelled}},
			},
		}}},
		{Kind: EventExec, Message: &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_Exec{
			Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: "exec", StreamId: "exec-stream", Sequence: 3,
				Payload: &runnerv1.ExecFrame_BufferedResult{BufferedResult: &runnerv1.ExecBufferedResult{Terminal: cancelled}},
			},
		}}},
		{Kind: EventFile, Message: &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_File{
			File: &runnerv1.FileFrame{
				Fence: fence, OperationId: "file", StreamId: "file-stream", Sequence: 1,
				Payload: &runnerv1.FileFrame_Terminal{Terminal: &runnerv1.FileTerminal{
					Kind: runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED,
				}},
			},
		}}},
		{Kind: EventPty, Message: &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_Pty{
			Pty: &runnerv1.PtyFrame{
				Fence: fence, OperationId: "terminal", StreamId: "terminal-stream", Sequence: 7,
				Payload: &runnerv1.PtyFrame_Terminal{Terminal: cancelled},
			},
		}}},
		// The Runner's confirmation repeats a terminal its live route already
		// holds; the route's owner may abandon it unread.
		{Kind: EventExec, Message: &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_Exec{
			Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: "routed-operation", StreamId: "routed-stream", Sequence: 1,
				Payload: &runnerv1.ExecFrame_BufferedResult{BufferedResult: &runnerv1.ExecBufferedResult{Terminal: cancelled}},
			},
		}}},
	} {
		event.RunnerID, event.ConnectionID = "runner-1", "connection-1"
		if err := server.persistEvent(t.Context(), event, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	var confirmed []string
	for _, frame := range confirmer.frames {
		confirmed = append(confirmed, frame.GetExec().GetOperationId()+frame.GetFile().GetOperationId()+frame.GetPty().GetOperationId())
	}
	if strings.Join(confirmed, ",") != "exec,file,terminal,routed-operation" {
		t.Fatalf("confirmed cancellations = %v, want exec,file,terminal,routed-operation", confirmed)
	}
	if message, err := routed.Receive(t.Context()); err != nil || message.GetExec().GetBufferedResult() == nil {
		t.Fatalf("routed terminal = %v, %v", message, err)
	}
}
