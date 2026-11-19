package runnercontrol

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type changingReservationBackend struct {
	recordingAssignmentBackend
	reserved       atomic.Pointer[runnerprotocol.Capacity]
	readinessCalls atomic.Uint32
}

func (backend *changingReservationBackend) Readiness(ctx context.Context) (BackendReadiness, error) {
	backend.readinessCalls.Add(1)
	return backend.recordingAssignmentBackend.Readiness(ctx)
}

func (backend *changingReservationBackend) ReservedCapacity() *runnerprotocol.Capacity {
	return backend.reserved.Load()
}

func TestHeartbeatsRefreshReservationsAfterReconnectWithoutReadinessProbes(t *testing.T) {
	initial := &runnerprotocol.Capacity{Instances: 1, VcpuCount: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30}
	backend := &changingReservationBackend{}
	backend.reserved.Store(initial)
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: &recordingProtocolStream{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stream := &blockingProtocolStream{ctx: ctx, heartbeats: make(chan *runnerprotocol.RunnerHeartbeat, 4)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.sendHeartbeats(ctx, stream, "reconnected-with-live-guest", BackendReadiness{Reserved: initial}, 5*time.Millisecond, make(chan error, 1))
	}()
	defer func() { cancel(); <-done }()
	select {
	case frame := <-stream.heartbeats:
		if frame.Reserved.GetInstances() != 1 {
			t.Fatalf("live guest reservations=%v", frame.Reserved)
		}
	case <-time.After(time.Second):
		t.Fatal("live guest heartbeat missing")
	}
	backend.reserved.Store(&runnerprotocol.Capacity{})
	deadline := time.After(time.Second)
	for {
		select {
		case frame := <-stream.heartbeats:
			if frame.Reserved != nil && frame.Reserved.GetInstances() == 0 && frame.Reserved.GetVcpuCount() == 0 && frame.Reserved.GetMemoryBytes() == 0 && frame.Reserved.GetDiskBytes() == 0 {
				if backend.readinessCalls.Load() != 0 {
					t.Fatal("periodic heartbeat reran expensive readiness probes")
				}
				return
			}
		case <-deadline:
			t.Fatal("stopped guest reservations remained frozen at reconnect")
		}
	}
}
