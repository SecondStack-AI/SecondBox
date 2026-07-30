package runnercontrol

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// concurrentAssignmentBackend blocks every start until released, so the number
// of assignments inside StartAssignment at once is directly observable.
type concurrentAssignmentBackend struct {
	recordingAssignmentBackend
	entered     chan struct{}
	release     chan struct{}
	inside      atomic.Int32
	maxTogether atomic.Int32
}

func (backend *concurrentAssignmentBackend) StartAssignment(
	_ context.Context,
	_ *runnerprotocol.AssignmentCommand,
	_ func(runnerprotocol.AssignmentProgressStage) error,
) (BackendInstance, error) {
	current := backend.inside.Add(1)
	for {
		observed := backend.maxTogether.Load()
		if current <= observed || backend.maxTogether.CompareAndSwap(observed, current) {
			break
		}
	}
	backend.entered <- struct{}{}
	<-backend.release
	backend.inside.Add(-1)
	return BackendInstance{BackendKind: "firecracker", BackendReference: "fc-instance"}, nil
}

func assignmentFrameAt(sequence int) *runnerprotocol.ControlPlaneToRunner {
	assignment := resolvedAssignmentCommand()
	assignment.MessageId = fmt.Sprintf("message-%d", sequence)
	assignment.Sequence = uint64(sequence)
	assignment.Fence.AssignmentId = fmt.Sprintf("assignment-%d", sequence)
	assignment.Fence.SandboxId = fmt.Sprintf("sandbox-%d", sequence)
	assignment.Fence.InstanceId = fmt.Sprintf("instance-%d", sequence)
	return &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Assignment{Assignment: assignment},
	}
}

// Handling assignments inline admitted exactly one at a time, so a burst queued
// behind a full microVM start each. Starts now run off the receive loop, bounded
// by the instance capacity the runner advertises.
func TestAssignmentsAreAdmittedConcurrentlyUpToAdvertisedCapacity(t *testing.T) {
	const capacity = 4
	backend := &concurrentAssignmentBackend{
		recordingAssignmentBackend: recordingAssignmentBackend{
			readiness: BackendReadiness{
				Capacity:     &runnerprotocol.Capacity{Instances: capacity},
				Reserved:     &runnerprotocol.Capacity{},
				Capabilities: &runnerprotocol.RunnerCapabilities{},
			},
		},
		entered: make(chan struct{}, capacity),
		release: make(chan struct{}),
	}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(), backend, staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	inbound := make([]*runnerprotocol.ControlPlaneToRunner, 0, capacity)
	for sequence := 1; sequence <= capacity; sequence++ {
		inbound = append(inbound, assignmentFrameAt(sequence))
	}
	stream := &blockingProtocolStream{
		ctx: runContext, inbound: inbound,
		heartbeats: make(chan *runnerprotocol.RunnerHeartbeat, capacity*4),
	}
	welcome := runnerWelcomeFrame("connection-concurrent")
	welcome.GetWelcome().HeartbeatIntervalMs = 50
	var consumed sync.WaitGroup
	consumed.Add(1)
	go func() {
		defer consumed.Done()
		_ = service.consumeCommands(runContext, stream, welcome.GetWelcome(), backend.readiness)
	}()

	for admitted := 0; admitted < capacity; admitted++ {
		select {
		case <-backend.entered:
		case <-time.After(3 * time.Second):
			t.Fatalf(
				"only %d of %d assignments were admitted concurrently; starts are serialised",
				admitted, capacity,
			)
		}
	}
	if got := backend.maxTogether.Load(); got != capacity {
		t.Fatalf("peak concurrent assignment starts = %d, want %d", got, capacity)
	}
	close(backend.release)
	cancelRun()
	consumed.Wait()
}

func TestConcurrentAssignmentLimitFollowsAdvertisedInstances(t *testing.T) {
	if got := concurrentAssignmentLimit(BackendReadiness{
		Capacity: &runnerprotocol.Capacity{Instances: 32},
	}); got != 32 {
		t.Fatalf("limit = %d, want the advertised 32", got)
	}
	// A runner that advertises no instance capacity must not assume one.
	if got := concurrentAssignmentLimit(BackendReadiness{
		Capacity: &runnerprotocol.Capacity{},
	}); got != 1 {
		t.Fatalf("limit without advertised capacity = %d, want 1", got)
	}
	if got := concurrentAssignmentLimit(BackendReadiness{}); got != 1 {
		t.Fatalf("limit without capacity evidence = %d, want 1", got)
	}
}
