package runnercontrol

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type storageProbeBackend struct {
	recordingAssignmentBackend
	probe func(context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error)
}

func (backend *storageProbeBackend) ObserveWorkspaceStorage(ctx context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error) {
	return backend.probe(ctx)
}

type storageProbeStream struct {
	ctx      context.Context
	incoming chan *runnerprotocol.ControlPlaneToRunner
	outgoing chan *runnerprotocol.RunnerToControlPlane
}

func (stream *storageProbeStream) Send(frame *runnerprotocol.RunnerToControlPlane) error {
	select {
	case stream.outgoing <- frame:
		return nil
	case <-stream.ctx.Done():
		return stream.ctx.Err()
	}
}
func (stream *storageProbeStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	select {
	case frame := <-stream.incoming:
		return frame, nil
	case <-stream.ctx.Done():
		return nil, stream.ctx.Err()
	}
}

func TestStorageProbeCannotBlockHeartbeatCommandsOrReconnect(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseProbe()
	started := make(chan struct{})
	var calls atomic.Int32
	observedAt := uint64(time.Now().Add(-time.Minute).UnixMilli())
	backend := &storageProbeBackend{recordingAssignmentBackend: recordingAssignmentBackend{readiness: BackendReadiness{Capacity: &runnerprotocol.Capacity{}, Capabilities: &runnerprotocol.RunnerCapabilities{}}}}
	backend.probe = func(ctx context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release // Simulate an ioctl which ignores cancellation.
			allocated, exclusive := uint64(4096), uint64(1024)
			return []*runnerprotocol.WorkspaceStorageObservation{{WorkspaceId: "workspace-observed", Generation: 1, ObservedAtUnixMs: observedAt, AllocatedBytes: &allocated, ExclusiveBytes: &exclusive}}, nil, nil
		}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{})
	if err != nil {
		t.Fatal(err)
	}
	startSession := func(connection string) (*storageProbeStream, context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(t.Context())
		stream := &storageProbeStream{ctx: ctx, incoming: make(chan *runnerprotocol.ControlPlaneToRunner, 2), outgoing: make(chan *runnerprotocol.RunnerToControlPlane, 32)}
		welcome := runnerWelcomeFrame(connection)
		welcome.GetWelcome().HeartbeatIntervalMs = 5
		stream.incoming <- welcome
		service.connector = staticProtocolConnector{stream: stream}
		result := make(chan error, 1)
		go func() { _, err := service.runProtocolSession(ctx); result <- err }()
		return stream, cancel, result
	}
	waitFrame := func(stream *storageProbeStream, accept func(*runnerprotocol.RunnerToControlPlane) bool) *runnerprotocol.RunnerToControlPlane {
		t.Helper()
		timeout := time.NewTimer(time.Second)
		defer timeout.Stop()
		for {
			select {
			case frame := <-stream.outgoing:
				if accept(frame) {
					return frame
				}
			case <-timeout.C:
				t.Fatal("blocked storage probe delayed protocol traffic")
				return nil
			}
		}
	}
	stopSession := func(cancel context.CancelFunc, result <-chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("session cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("session teardown waited for storage ioctl")
		}
	}
	heartbeat := func(frame *runnerprotocol.RunnerToControlPlane) bool { return frame.GetHeartbeat() != nil }
	first, cancelFirst, firstResult := startSession("storage-first")
	defer cancelFirst()
	waitFrame(first, heartbeat)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("storage probe did not start")
	}
	waitFrame(first, heartbeat)
	first.incoming <- &runnerprotocol.ControlPlaneToRunner{Message: &runnerprotocol.ControlPlaneToRunner_Drain{Drain: &runnerprotocol.DrainCommand{MessageId: "storage-drain", Sequence: 1, Mode: runnerprotocol.DrainMode_DRAIN_MODE_GRACEFUL, DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli())}}}
	waitFrame(first, func(frame *runnerprotocol.RunnerToControlPlane) bool { return frame.GetDrainState() != nil })
	stopSession(cancelFirst, firstResult)
	second, cancelSecond, secondResult := startSession("storage-second")
	defer cancelSecond()
	waitFrame(second, heartbeat)
	waitFrame(second, heartbeat)
	if calls.Load() != 1 {
		t.Fatalf("reconnect started %d probes while one was blocked", calls.Load())
	}
	releaseProbe()
	frame := waitFrame(second, func(frame *runnerprotocol.RunnerToControlPlane) bool {
		return frame.GetHeartbeat() != nil && len(frame.GetHeartbeat().WorkspaceStorage) > 0
	})
	observation := frame.GetHeartbeat().WorkspaceStorage[0]
	if observation.ObservedAtUnixMs != observedAt || observation.GetAllocatedBytes() != 4096 || observation.GetExclusiveBytes() != 1024 {
		t.Fatalf("completed storage observation changed: %+v", observation)
	}
	stopSession(cancelSecond, secondResult)
}

func TestCompletedStorageProbeErrorsArePropagated(t *testing.T) {
	want := errors.New("storage probe failed")
	backend := &storageProbeBackend{probe: func(context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error) {
		return nil, nil, want
	}}
	service := &RunnerProtocolService{backend: backend}
	if result := service.pollWorkspaceStorage(t.Context()); result.err != nil {
		t.Fatal(result.err)
	}
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			result := service.pollWorkspaceStorage(t.Context())
			if result.err != nil {
				if !errors.Is(result.err, want) {
					t.Fatal(result.err)
				}
				return
			}
		case <-timeout.C:
			t.Fatal("completed probe failure was lost")
		}
	}
}
