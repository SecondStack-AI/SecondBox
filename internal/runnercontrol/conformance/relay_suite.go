package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

var (
	ErrRelayStaleFence = errors.New("SecondBox conformance relay fence is stale")
	ErrRelayReordered  = errors.New("SecondBox conformance relay frame is reordered")
)

// RelayBoundary exposes durable frame behavior without selecting PostgreSQL.
type RelayBoundary interface {
	SeedOperation(context.Context, *runnerv1.AssignmentFence) error
	SeedOutbound(context.Context, string, *runnerv1.ControlPlaneToRunner) error
	ClaimOutboundFrame(context.Context, string, string, time.Time) (string, *runnerv1.ControlPlaneToRunner, bool, error)
	MarkOutboundFrameDelivered(context.Context, string, string, int64, time.Time) error
	PersistInboundFrame(context.Context, string, string, *runnerv1.RunnerToControlPlane, time.Time) (bool, error)
}

type RelayBoundaryFactory func(*testing.T) RelayBoundary

// RunRelayConformanceSuite qualifies sequencing, fencing, reconnect, and
// idempotency required from a PostgreSQL relay implementation.
func RunRelayConformanceSuite(t *testing.T, factory RelayBoundaryFactory) {
	t.Helper()
	now := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	fence := conformanceFence()

	t.Run("outbound_reclaimed_until_delivered", func(t *testing.T) {
		boundary := factory(t)
		frame := &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: "operation-1", StreamId: "stream-1", Sequence: 1,
				Payload: &runnerv1.ExecFrame_Open{Open: &runnerv1.ExecOpen{
					Command: &runnerv1.ExecOpen_Shell{Shell: "true"}, OutputLimitBytes: 1024,
				}},
			}},
		}
		if err := boundary.SeedOutbound(t.Context(), "relay-1", frame); err != nil {
			t.Fatal(err)
		}
		id, _, found, err := boundary.ClaimOutboundFrame(t.Context(), "runner-1", "connection-1", now)
		if err != nil || !found || id != "relay-1" {
			t.Fatalf("first claim = %q, %t, %v", id, found, err)
		}
		id, _, found, err = boundary.ClaimOutboundFrame(t.Context(), "runner-1", "connection-2", now.Add(time.Second))
		if err != nil || !found || id != "relay-1" {
			t.Fatalf("reconnect claim = %q, %t, %v", id, found, err)
		}
		if err := boundary.MarkOutboundFrameDelivered(t.Context(), id, "connection-2", 2, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, _, found, err := boundary.ClaimOutboundFrame(t.Context(), "runner-1", "connection-3", now.Add(2*time.Second)); err != nil || found {
			t.Fatalf("delivered frame reclaimed = %t, %v", found, err)
		}
	})

	t.Run("inbound_fence_sequence_and_duplicate", func(t *testing.T) {
		boundary := factory(t)
		if err := boundary.SeedOperation(t.Context(), fence); err != nil {
			t.Fatal(err)
		}
		first := relayConformanceTerminalOutput(fence, 1, []byte{0, 1, 0xff})
		inserted, err := boundary.PersistInboundFrame(t.Context(), "runner-1", "connection-1", first, now)
		if err != nil || !inserted {
			t.Fatalf("first persist = %t, %v", inserted, err)
		}
		inserted, err = boundary.PersistInboundFrame(t.Context(), "runner-1", "connection-2", first, now)
		if err != nil || inserted {
			t.Fatalf("reconnected duplicate persist = %t, %v", inserted, err)
		}
		if _, err := boundary.PersistInboundFrame(
			t.Context(), "runner-1", "connection-2",
			relayConformanceTerminalOutput(fence, 3, []byte("gap")), now,
		); !errors.Is(err, ErrRelayReordered) {
			t.Fatalf("sequence gap error = %v", err)
		}
		stale := cloneConformanceFence(fence)
		stale.SandboxGeneration++
		if _, err := boundary.PersistInboundFrame(
			t.Context(), "runner-1", "connection-2",
			relayConformanceTerminalOutput(stale, 2, []byte("stale")), now,
		); !errors.Is(err, ErrRelayStaleFence) {
			t.Fatalf("stale fence error = %v", err)
		}
	})
}

func relayConformanceTerminalOutput(
	fence *runnerv1.AssignmentFence,
	sequence uint64,
	content []byte,
) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
			Fence: fence, OperationId: "operation-1", StreamId: "stream-1", Sequence: sequence,
			Payload: &runnerv1.PtyFrame_Output{Output: &runnerv1.ExecOutput{
				Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
				Data:    content,
			}},
		}},
	}
}

func cloneConformanceFence(fence *runnerv1.AssignmentFence) *runnerv1.AssignmentFence {
	return &runnerv1.AssignmentFence{
		AssignmentId:      fence.AssignmentId,
		SandboxId:         fence.SandboxId,
		InstanceId:        fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration,
		FencingToken:      append([]byte(nil), fence.FencingToken...),
	}
}
