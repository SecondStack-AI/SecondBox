package runnercontrol

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// A full direct socket must neither stop control traffic nor keep a cancelled
// operation alive until its original execution deadline.
func TestCancelledDirectPeerDoesNotBlockControlConnection(t *testing.T) {
	for _, blockedOutput := range []bool{false, true} {
		name := "terminal"
		if blockedOutput {
			name = "output"
		}
		t.Run(name, func(t *testing.T) {
			control := &threadSafeRunnerStream{}
			started := make(chan struct{})
			backend := &relayAssignmentBackend{streaming: func(
				ctx context.Context, _ *runnerprotocol.AssignmentFence, _ *runnerprotocol.ExecOpen,
				_ <-chan ExecControl, emit func(runnerprotocol.ExecOutputChannel, []byte) error,
			) (*runnerprotocol.ExecTerminal, error) {
				close(started)
				if blockedOutput {
					return nil, emit(runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, []byte("output"))
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: control})
			if err != nil {
				t.Fatal(err)
			}
			service.directDataPlane.bindStream(control)
			fence := relayRunnerFence()
			service.recordActiveAssignment(fence, "fc-instance-1")
			credential := "stalled-direct-credential-000000000000"
			session := registerDirectDataPlaneTestSession(t, service, fence, "stalled", "stalled-stream",
				runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC, credential)
			session.consumed = true
			client, connection := net.Pipe()
			runner := &observedDirectWriteConnection{Conn: connection, writing: make(chan struct{})}
			served := make(chan error, 1)
			t.Cleanup(func() {
				_ = client.Close()
				_ = runner.Close()
				select {
				case <-served:
				case <-time.After(time.Second):
					t.Error("direct connection did not stop")
				}
			})
			go func() {
				served <- service.serveDirectTypedConnection(t.Context(), runner, portdirect.Credential{
					SessionKind: portdirect.SessionKindExec, Value: credential,
				})
			}()
			if verdict, detail, err := portdirect.ReadVerdict(client); err != nil || verdict != portdirect.VerdictAdmitted {
				t.Fatalf("direct admission = %d/%q: %v", verdict, detail, err)
			}
			// Observe writes only after the admission verdict. The peer never
			// reads again, so the next frame must block inside net.Pipe.Write.
			runner.observe()
			writeDirectDataPlaneTestMessage(t, client, &runnerprotocol.ControlPlaneToRunner{
				Message: &runnerprotocol.ControlPlaneToRunner_Exec{Exec: &runnerprotocol.ExecFrame{
					Fence: cloneRunnerFence(fence), OperationId: session.operationID, StreamId: session.streamID,
					Sequence: 1, Correlation: session.correlation,
					Payload: &runnerprotocol.ExecFrame_Open{Open: &runnerprotocol.ExecOpen{
						Command: &runnerprotocol.ExecOpen_Shell{Shell: "sleep 30"}, OutputLimitBytes: 1024, Streaming: true,
					}},
				}},
			})
			<-started
			if blockedOutput {
				writeDirectDataPlaneTestMessage(t, client, &runnerprotocol.ControlPlaneToRunner{
					Message: &runnerprotocol.ControlPlaneToRunner_Exec{Exec: &runnerprotocol.ExecFrame{
						Fence: cloneRunnerFence(fence), OperationId: session.operationID, StreamId: session.streamID,
						Sequence: 2, Correlation: session.correlation,
						Payload: &runnerprotocol.ExecFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 1024}},
					}},
				})
				waitForDirectWrite(t, runner.writing)
			}
			if err := service.handleDataPlaneCancel(&runnerprotocol.DataPlaneCancelCommand{
				Fence: cloneRunnerFence(fence), OperationId: session.operationID, StreamId: session.streamID,
				Kind: session.kind, Reason: "test cancellation",
			}); err != nil {
				t.Fatal(err)
			}
			waitForDirectWrite(t, runner.writing)
			controlDone := make(chan error, 1)
			go func() { controlDone <- service.sendHeartbeat(t.Context(), control, "connection-1", BackendReadiness{}) }()
			select {
			case err := <-controlDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("stalled direct peer blocked the control heartbeat")
			}
			// Both output and terminal writes must unwind within the transport
			// timeout so the control connection receives the cancellation proof.
			deadline := time.Now().Add(7 * time.Second)
			for {
				for _, message := range control.messages() {
					if terminal := message.GetExec().GetBufferedResult().GetTerminal(); terminal != nil {
						if terminal.Kind != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
							t.Fatalf("control confirmation = %v", terminal)
						}
						return
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("stalled direct peer blocked cancellation confirmation")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

type observedDirectWriteConnection struct {
	net.Conn
	mu      sync.Mutex
	armed   bool
	writing chan struct{}
}

func (connection *observedDirectWriteConnection) observe() {
	connection.mu.Lock()
	connection.armed = true
	connection.mu.Unlock()
}

func (connection *observedDirectWriteConnection) Write(data []byte) (int, error) {
	connection.mu.Lock()
	if connection.armed {
		close(connection.writing)
		connection.armed = false
	}
	connection.mu.Unlock()
	return connection.Conn.Write(data)
}

func waitForDirectWrite(t *testing.T, writing <-chan struct{}) {
	t.Helper()
	select {
	case <-writing:
	case <-time.After(time.Second):
		t.Fatal("direct write did not start")
	}
}
