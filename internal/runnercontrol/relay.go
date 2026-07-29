package runnercontrol

import (
	"context"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

// RelayDelivery is one durably claimed root-to-runner data-plane frame.
type RelayDelivery struct {
	ID           string
	ClaimAttempt int64
	Message      *runnerv1.ControlPlaneToRunner
}

// InboundRelayFrame binds runner output to the authenticated connection that
// delivered it. Implementations enforce the authoritative Assignment fence and
// persist stream sequencing idempotently.
type InboundRelayFrame struct {
	RunnerID     string
	ConnectionID string
	Message      *runnerv1.RunnerToControlPlane
}

// ProtocolFrameRelay is the provider-neutral durable data-plane queue.
//
// ClaimOutboundFrame atomically records its delivery attempt before returning.
// MarkOutboundFrameDelivered commits a successful transport send for that exact
// connection. Failed sends remain reclaimable after reconnect. Implementations
// prioritize cancellation and other control frames ahead of byte frames.
// PersistInboundFrame returns false for an exact durable duplicate and rejects
// stale fences, conflicting duplicates, and sequence gaps.
type ProtocolFrameRelay interface {
	ClaimOutboundFrame(
		context.Context,
		string,
		string,
		time.Time,
	) (RelayDelivery, bool, error)
	MarkOutboundFrameDelivered(context.Context, string, string, int64, time.Time) error
	PersistInboundFrame(context.Context, InboundRelayFrame, time.Time) (bool, error)
}
