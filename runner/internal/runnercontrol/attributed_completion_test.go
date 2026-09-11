package runnercontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type attributedCompletionBackend struct {
	relayAssignmentBackend
	fenceEntered chan struct{}
	releaseFence <-chan struct{}
	stopped      chan struct{}
	stopErr      error
	enteredOnce  sync.Once
	stoppedOnce  sync.Once
}

func (backend *attributedCompletionBackend) FenceAssignment(ctx context.Context, _ *runnerprotocol.FenceCommand) (FenceEvidence, error) {
	backend.enteredOnce.Do(func() { close(backend.fenceEntered) })
	if backend.releaseFence != nil {
		select {
		case <-backend.releaseFence:
		case <-ctx.Done():
			return FenceEvidence{}, ctx.Err()
		}
	}
	if backend.stopErr != nil {
		return FenceEvidence{}, backend.stopErr
	}
	backend.stoppedOnce.Do(func() { close(backend.stopped) })
	return FenceEvidence{Result: runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED}, nil
}

func newAttributedCompletionService(t *testing.T, backend *attributedCompletionBackend) (*RunnerProtocolService, *runnerprotocol.AssignmentFence) {
	t.Helper()
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{})
	if err != nil {
		t.Fatal(err)
	}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "instance")
	service.armAttributedAssignmentExpiry(t.Context(), &runnerprotocol.AssignmentCommand{Fence: fence, AttributedExecution: &runnerprotocol.AttributedExecution{ExpiresAtUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli())}})
	t.Cleanup(func() { service.removeActiveAssignment(fence.AssignmentId) })
	return service, fence
}

func TestAttributedExecReportsCompletionAfterHostStop(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			name := map[bool]string{false: "buffered", true: "streaming"}[streaming] + "/" + map[bool]string{false: "stopped", true: "stop_failed"}[failed]
			t.Run(name, func(t *testing.T) {
				release := make(chan struct{})
				backend := &attributedCompletionBackend{fenceEntered: make(chan struct{}), releaseFence: release, stopped: make(chan struct{})}
				failure := errors.New("test host stop failed")
				if failed {
					backend.stopErr = failure
				}
				backend.exec = func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen) (BufferedExecResult, error) {
					return BufferedExecResult{Terminal: &runnerprotocol.ExecTerminal{Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED}}, nil
				}
				backend.streaming = func(ctx context.Context, fence *runnerprotocol.AssignmentFence, open *runnerprotocol.ExecOpen, _ <-chan ExecControl, _ func(runnerprotocol.ExecOutputChannel, []byte) error) (*runnerprotocol.ExecTerminal, error) {
					result, err := backend.exec(ctx, fence, open)
					return result.Terminal, err
				}
				service, fence := newAttributedCompletionService(t, backend)
				stream := &threadSafeRunnerStream{}
				asyncErrors := make(chan error, 1)
				open := relayExecOpen(fence, "exec", "stream", "true")
				open.GetOpen().Streaming = streaming
				open.GetOpen().DeadlineUnixMs = uint64(time.Now().Add(10 * time.Second).UnixMilli())
				if err := service.handleExecFrame(t.Context(), stream, open, map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true}, asyncErrors); err != nil {
					t.Fatal(err)
				}
				select {
				case <-backend.fenceEntered:
				case <-time.After(time.Second):
					t.Fatal("guest completion did not stop compute")
				}
				if len(stream.messages()) != 0 {
					t.Error("terminal result was sent before host stop")
				}
				close(release)
				waitRunnerMessages(t, stream, 1)
				terminal := stream.messages()[0].GetExec().GetBufferedResult().GetTerminal()
				if failed {
					if terminal.Kind != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED || terminal.Retryable {
						t.Fatalf("unconfirmed stop terminal: %v", terminal)
					}
					select {
					case err := <-asyncErrors:
						if !errors.Is(err, failure) {
							t.Fatal(err)
						}
					case <-time.After(time.Second):
						t.Fatal("stop error was not reported")
					}
				} else if terminal.Kind != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || len(service.activeAssignments()) != 0 {
					t.Fatalf("confirmed stop terminal: %v", terminal)
				}
			})
		}
	}
}

func TestAttributedCancellationStopsUnresponsiveGuest(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			started := make(chan struct{})
			backend := &attributedCompletionBackend{fenceEntered: make(chan struct{}), stopped: make(chan struct{})}
			backend.exec = func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.ExecOpen) (BufferedExecResult, error) {
				close(started)
				<-backend.stopped // Ignore guest cancellation; only host termination releases execution.
				return BufferedExecResult{Terminal: &runnerprotocol.ExecTerminal{Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED}}, nil
			}
			service, fence := newAttributedCompletionService(t, backend)
			stream := &threadSafeRunnerStream{}
			open := relayExecOpen(fence, "exec", "stream", "sleep forever")
			open.GetOpen().Streaming = false
			expiresAt := time.Now().Add(10 * time.Second)
			if deadline {
				expiresAt = time.Now().Add(200 * time.Millisecond)
			}
			open.GetOpen().DeadlineUnixMs = uint64(expiresAt.UnixMilli())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := service.handleExecFrame(ctx, stream, open, map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true}, make(chan error, 1)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("exec did not start")
			}
			if !deadline {
				cancel()
			}
			waitRunnerMessages(t, stream, 1)
			wantKind := runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED
			if deadline {
				wantKind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED
			}
			if got := stream.messages()[0].GetExec().GetBufferedResult().GetTerminal().GetKind(); got != wantKind {
				t.Fatalf("terminal = %s, want %s", got, wantKind)
			}
			if len(service.activeAssignments()) != 0 {
				t.Fatal("cancelled command retained compute")
			}
		})
	}
}
