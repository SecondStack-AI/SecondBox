package conformance

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/protobuf/proto"
)

func TestInMemoryRelayConformance(t *testing.T) {
	RunRelayConformanceSuite(t, func(*testing.T) RelayBoundary {
		return &memoryRelayBoundary{
			outbound: make(map[string]*memoryRelayDelivery),
			inbound:  make(map[string][]byte),
		}
	})
}

type memoryRelayBoundary struct {
	mu       sync.Mutex
	fence    *runnerv1.AssignmentFence
	outbound map[string]*memoryRelayDelivery
	inbound  map[string][]byte
	last     uint64
}

type memoryRelayDelivery struct {
	message    *runnerv1.ControlPlaneToRunner
	connection string
	delivered  bool
}

func (relay *memoryRelayBoundary) SeedOperation(_ context.Context, fence *runnerv1.AssignmentFence) error {
	relay.mu.Lock()
	relay.fence = cloneConformanceFence(fence)
	relay.mu.Unlock()
	return nil
}

func (relay *memoryRelayBoundary) SeedOutbound(
	_ context.Context,
	id string,
	message *runnerv1.ControlPlaneToRunner,
) error {
	relay.mu.Lock()
	relay.outbound[id] = &memoryRelayDelivery{message: proto.Clone(message).(*runnerv1.ControlPlaneToRunner)}
	relay.mu.Unlock()
	return nil
}

func (relay *memoryRelayBoundary) ClaimOutboundFrame(
	_ context.Context,
	_ string,
	connectionID string,
	_ time.Time,
) (string, *runnerv1.ControlPlaneToRunner, bool, error) {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	for id, delivery := range relay.outbound {
		if !delivery.delivered {
			delivery.connection = connectionID
			return id, proto.Clone(delivery.message).(*runnerv1.ControlPlaneToRunner), true, nil
		}
	}
	return "", nil, false, nil
}

func (relay *memoryRelayBoundary) MarkOutboundFrameDelivered(
	_ context.Context,
	id string,
	connectionID string,
	_ time.Time,
) error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	delivery := relay.outbound[id]
	if delivery == nil || delivery.connection != connectionID {
		return ErrInactiveConnection
	}
	delivery.delivered = true
	return nil
}

func (relay *memoryRelayBoundary) PersistInboundFrame(
	_ context.Context,
	_ string,
	_ string,
	message *runnerv1.RunnerToControlPlane,
	_ time.Time,
) (bool, error) {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	frame := message.GetExec()
	if frame == nil || !sameConformanceFence(relay.fence, frame.Fence) {
		return false, ErrRelayStaleFence
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return false, err
	}
	key := frame.OperationId + "\x00" + frame.StreamId + "\x00" + string(rune(frame.Sequence))
	if previous, exists := relay.inbound[key]; exists {
		if bytes.Equal(previous, encoded) {
			return false, nil
		}
		return false, ErrRelayReordered
	}
	if frame.Sequence != relay.last+1 {
		return false, ErrRelayReordered
	}
	relay.inbound[key] = append([]byte(nil), encoded...)
	relay.last = frame.Sequence
	return true, nil
}

func sameConformanceFence(left, right *runnerv1.AssignmentFence) bool {
	return left != nil &&
		right != nil &&
		left.AssignmentId == right.AssignmentId &&
		left.SandboxId == right.SandboxId &&
		left.InstanceId == right.InstanceId &&
		left.SandboxGeneration == right.SandboxGeneration &&
		bytes.Equal(left.FencingToken, right.FencingToken)
}
