package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

func TestPartialFileUploadCancellation(t *testing.T) {
	for _, mode := range []string{"durable", "inline", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			control := &threadSafeRunnerStream{}
			service, err := NewRunnerProtocolService(testRunnerConfig(), &relayAssignmentBackend{file: func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.FileOpen, []byte) (FileOperationResult, error) {
				calls.Add(1)
				return FileOperationResult{}, nil
			}}, staticProtocolConnector{stream: control})
			if err != nil {
				t.Fatal(err)
			}
			service.directDataPlane.bindStream(control)
			fence := relayRunnerFence()
			service.recordActiveAssignment(fence, "fc-instance-1")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			enabled := map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING: true}
			frame := &runnerprotocol.FileFrame{Fence: fence, OperationId: "partial", StreamId: "partial-stream", Sequence: 1,
				Correlation: relayOperationCorrelation(fence, "partial", "request-partial", "lease-partial"),
				Payload:     &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "partial", ExpectedSize: 4, ExpectedChecksum: "checksum"}}}
			if err := service.handleFileFrame(ctx, control, frame, enabled, nil); err != nil {
				t.Fatal(err)
			}
			frame.Sequence = 2
			frame.Payload = &runnerprotocol.FileFrame_Chunk{Chunk: &runnerprotocol.FileChunk{Data: []byte("ab")}}
			if err := service.handleFileFrame(ctx, control, frame, enabled, nil); err != nil {
				t.Fatal(err)
			}
			command := &runnerprotocol.DataPlaneCancelCommand{Fence: fence, OperationId: "partial", StreamId: "partial-stream", Kind: runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE, Reason: "cancel upload"}
			switch mode {
			case "durable":
				err = service.handleDataPlaneCancel(command)
			case "inline":
				frame.Sequence = 3
				frame.Payload = &runnerprotocol.FileFrame_Cancel{Cancel: &runnerprotocol.ExecCancel{}}
				err = service.handleFileFrame(ctx, control, frame, enabled, nil)
			case "disconnect":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			key := runnerDataPlaneOperationKey(fence, "partial", "partial-stream")
			deadline := time.Now().Add(time.Second)
			for {
				service.operationMu.Lock()
				state := service.fileOperations[key]
				terminal, buffered := state.terminal, len(state.content)
				service.operationMu.Unlock()
				service.stateMu.Lock()
				active := len(service.active[fence.AssignmentId].ActiveOperationIds)
				service.stateMu.Unlock()
				if terminal && active == 0 {
					if buffered != 0 {
						t.Fatalf("cancelled upload retains %d bytes", buffered)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("partial upload never terminated and released its active operation")
				}
				time.Sleep(time.Millisecond)
			}
			frame.Sequence++
			frame.Payload = &runnerprotocol.FileFrame_Chunk{Chunk: &runnerprotocol.FileChunk{Offset: 2, Data: []byte("cd")}}
			if err := service.handleFileFrame(ctx, control, frame, enabled, nil); err != nil {
				t.Fatal(err)
			}
			if err := service.handleDataPlaneCancel(command); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("cancelled partial upload reached the backend")
			}
			messages := control.messages()
			if messages[len(messages)-1].GetFile().GetTerminal().GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED {
				t.Fatal("missing cancellation confirmation")
			}
		})
	}
}

func TestCancellationAfterTerminalEviction(t *testing.T) {
	for _, kind := range []runnerprotocol.DataPlaneSessionKind{runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC, runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE, runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY} {
		t.Run(kind.String(), func(t *testing.T) {
			control := &threadSafeRunnerStream{}
			service, err := NewRunnerProtocolService(testRunnerConfig(), &relayAssignmentBackend{}, staticProtocolConnector{stream: control})
			if err != nil {
				t.Fatal(err)
			}
			fence := relayRunnerFence()
			service.recordActiveAssignment(fence, "fc-instance-1")
			// Lose the original proof, evict it, then reconnect and replay cancellation.
			for i := 0; i <= maxRunnerDataPlaneTerminalTombstones; i++ {
				id := fmt.Sprintf("evicted-%d", i)
				_, err := service.retainUnopenedDirectOperation(&directDataPlaneSession{fence: fence, operationID: id, streamID: id, kind: kind, correlation: relayOperationCorrelation(fence, id, "request-"+id, "lease-"+id)})
				if err != nil {
					t.Fatal(err)
				}
			}
			key := runnerDataPlaneOperationKey(fence, "evicted-0", "evicted-0")
			if service.execOperations[key] != nil || service.fileOperations[key] != nil {
				t.Fatal("test did not evict terminal")
			}
			service.directDataPlane.bindStream(control)
			command := &runnerprotocol.DataPlaneCancelCommand{Fence: fence, OperationId: "evicted-0", StreamId: "evicted-0", Kind: kind, Reason: "cancel after reconnect"}
			if err := service.handleDataPlaneCancel(command); err != nil {
				t.Fatal(err)
			}
			if len(control.messages()) != 1 {
				t.Fatal("replayed cancellation did not confirm absent operation")
			}
			correlation := relayOperationCorrelation(fence, "evicted-0", "request-evicted-0", "lease-evicted-0")
			enabled := map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true, runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING: true, runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY: true}
			if kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE {
				err = service.handleFileFrame(t.Context(), control, &runnerprotocol.FileFrame{Fence: fence, OperationId: command.OperationId, StreamId: command.StreamId, Sequence: 1, Correlation: correlation, Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_MKDIR, WorkspaceRelativePath: "late"}}}, enabled, nil)
			} else {
				err = service.handleExecFrame(t.Context(), control, &runnerprotocol.ExecFrame{Fence: fence, OperationId: command.OperationId, StreamId: command.StreamId, Sequence: 1, Correlation: correlation, Payload: &runnerprotocol.ExecFrame_Open{Open: &runnerprotocol.ExecOpen{Command: &runnerprotocol.ExecOpen_Shell{Shell: "true"}, AllocatePty: kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY}}}, enabled, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := service.handleDataPlaneCancel(command); err != nil {
				t.Fatal(err)
			}
			messages := control.messages()
			if len(messages) != 2 || !proto.Equal(messages[0], messages[1]) {
				t.Fatal("cancellation replay changed its proof")
			}
		})
	}
}

func TestDisconnectedDirectPartialUploadConfirmsCancellation(t *testing.T) {
	control := &threadSafeRunnerStream{}
	var calls atomic.Int32
	service, err := NewRunnerProtocolService(testRunnerConfig(), &relayAssignmentBackend{file: func(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.FileOpen, []byte) (FileOperationResult, error) {
		calls.Add(1)
		return FileOperationResult{}, nil
	}}, staticProtocolConnector{stream: control})
	if err != nil {
		t.Fatal(err)
	}
	service.directDataPlane.bindStream(control)
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	credential := "partial-upload-credential"
	session := registerDirectDataPlaneTestSession(t, service, fence, "partial-direct", "partial-direct-stream", runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE, credential)
	session.consumed = true
	client, runner := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = runner.Close() })
	if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- service.serveDirectTypedConnection(t.Context(), runner, portdirect.Credential{SessionKind: portdirect.SessionKindFile, Value: credential})
	}()
	if verdict, _, err := portdirect.ReadVerdict(client); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("admission = %v, %v", verdict, err)
	}
	frame := &runnerprotocol.FileFrame{Fence: fence, OperationId: session.operationID, StreamId: session.streamID, Sequence: 1, Correlation: session.correlation,
		Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "partial", ExpectedSize: 4, ExpectedChecksum: "checksum"}}}
	writeDirectDataPlaneTestMessage(t, client, &runnerprotocol.ControlPlaneToRunner{Message: &runnerprotocol.ControlPlaneToRunner_File{File: frame}})
	frame.Sequence = 2
	frame.Payload = &runnerprotocol.FileFrame_Chunk{Chunk: &runnerprotocol.FileChunk{Data: []byte("ab")}}
	writeDirectDataPlaneTestMessage(t, client, &runnerprotocol.ControlPlaneToRunner{Message: &runnerprotocol.ControlPlaneToRunner_File{File: frame}})
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("direct connection did not close")
	}
	if err := service.handleDataPlaneCancel(&runnerprotocol.DataPlaneCancelCommand{Fence: fence, OperationId: session.operationID, StreamId: session.streamID, Kind: session.kind, Reason: "disconnected upload"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		messages := control.messages()
		if len(messages) > 0 && messages[len(messages)-1].GetFile().GetTerminal().GetKind() == runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnected partial upload never confirmed cancellation")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 0 {
		t.Fatal("partial upload reached backend")
	}
}

func TestFileFinalChunkRacesCancellation(t *testing.T) {
	for i := range 50 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var calls atomic.Int32
			control := &threadSafeRunnerStream{}
			service, err := NewRunnerProtocolService(testRunnerConfig(), &relayAssignmentBackend{file: func(ctx context.Context, _ *runnerprotocol.AssignmentFence, _ *runnerprotocol.FileOpen, content []byte) (FileOperationResult, error) {
				calls.Add(1)
				if string(content) != "data" {
					return FileOperationResult{}, fmt.Errorf("unexpected upload: %q", content)
				}
				<-ctx.Done()
				return FileOperationResult{}, ctx.Err()
			}}, staticProtocolConnector{stream: control})
			if err != nil {
				t.Fatal(err)
			}
			fence := relayRunnerFence()
			service.recordActiveAssignment(fence, "fc-instance-1")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			enabled := map[runnerprotocol.RunnerFeature]bool{runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING: true}
			frame := &runnerprotocol.FileFrame{Fence: fence, OperationId: "race", StreamId: "race-stream", Sequence: 1, Correlation: relayOperationCorrelation(fence, "race", "request-race", "lease-race"), Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "file", ExpectedSize: 4, ExpectedChecksum: "checksum"}}}
			asyncErrors := make(chan error, 1)
			if err := service.handleFileFrame(ctx, control, frame, enabled, asyncErrors); err != nil {
				t.Fatal(err)
			}
			frame.Sequence = 2
			frame.Payload = &runnerprotocol.FileFrame_Chunk{Chunk: &runnerprotocol.FileChunk{Data: []byte("data")}}
			done := make(chan error, 1)
			go func() { done <- service.handleFileFrame(ctx, control, frame, enabled, asyncErrors) }()
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				select {
				case err := <-asyncErrors:
					t.Fatal(err)
				default:
				}
				messages := control.messages()
				service.stateMu.Lock()
				active := len(service.active[fence.AssignmentId].ActiveOperationIds)
				service.stateMu.Unlock()
				if len(messages) > 0 && active == 0 {
					if len(messages) != 1 || messages[0].GetFile().GetTerminal().GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED || calls.Load() > 1 {
						t.Fatalf("upload had multiple owners or wrong terminal: calls=%d messages=%v", calls.Load(), messages)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("racing cancellation never completed")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

type cancellationEvidenceSink func(context.Context, runnerevidence.Record) error

func (sink cancellationEvidenceSink) Emit(ctx context.Context, record runnerevidence.Record) error {
	return sink(ctx, record)
}

func TestAbsentCancellationRequiresEvidenceBeforeEveryConfirmation(t *testing.T) {
	for _, kind := range []runnerprotocol.DataPlaneSessionKind{runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC, runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE, runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY} {
		t.Run(kind.String(), func(t *testing.T) {
			control := &threadSafeRunnerStream{}
			service, err := NewRunnerProtocolService(testRunnerConfig(), &relayAssignmentBackend{}, staticProtocolConnector{stream: control})
			if err != nil {
				t.Fatal(err)
			}
			service.directDataPlane.bindStream(control)
			fence := relayRunnerFence()
			command := &runnerprotocol.DataPlaneCancelCommand{Fence: fence, OperationId: "absent", StreamId: "absent-stream", Kind: kind, Reason: "cancel before Open"}
			sinkErr := errors.New("audit unavailable")
			attempts := 0
			service.evidence = cancellationEvidenceSink(func(_ context.Context, record runnerevidence.Record) error {
				attempts++
				if err := record.Validate(); err != nil {
					t.Fatal(err)
				}
				if record.Event != runnerevidence.EventOperationAbsent || record.RequestID != "" || record.LeaseID != "" || record.OperationID != command.OperationId || record.AssignmentID != fence.AssignmentId || record.RunnerID != "runner-1" {
					t.Fatalf("absence evidence = %+v", record)
				}
				if len(control.messages()) != 0 {
					t.Fatal("confirmation preceded required evidence")
				}
				return sinkErr
			})
			for range 2 {
				if err := service.handleDataPlaneCancel(command); !errors.Is(err, sinkErr) {
					t.Fatalf("audit failure = %v", err)
				}
				if len(control.messages()) != 0 {
					t.Fatal("failed audit still confirmed cancellation")
				}
			}
			if attempts != 2 {
				t.Fatal("replay bypassed failed audit")
			}
			sinkErr = nil
			if err := service.handleDataPlaneCancel(command); err != nil {
				t.Fatal(err)
			}
			if len(control.messages()) != 1 {
				t.Fatal("successful audit did not permit confirmation")
			}
		})
	}
}
