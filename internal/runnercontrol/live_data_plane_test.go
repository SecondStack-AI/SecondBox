package runnercontrol

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestLiveDataPlaneBrokerDropsUnknownRouteAndKeepsConnectionHealthy(t *testing.T) {
	broker, detach := liveDataPlaneTestBroker(t)
	defer detach()
	server := &Server{config: ServerConfig{LiveDataPlane: broker}}

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
	if err := broker.Deliver(t.Context(), liveDataPlaneExecEvent(
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
			if err := broker.Deliver(t.Context(), liveDataPlaneExecEvent(
				"slow-operation", "slow-stream", sequence, []byte{byte(sequence)},
			)); err != nil {
				delivered <- err
				return
			}
		}
		delivered <- broker.Deliver(t.Context(), liveDataPlaneExecEvent(
			"healthy-operation", "healthy-stream", 1, []byte("b"),
		))
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
	server := &Server{config: ServerConfig{LiveDataPlane: broker}}

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
