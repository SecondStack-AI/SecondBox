package runnercontrol

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

func TestDirectDataPlaneCarriesTypedExecAndFileMessages(t *testing.T) {
	backend := &relayAssignmentBackend{
		exec: func(
			_ context.Context,
			_ *runnerprotocol.AssignmentFence,
			_ *runnerprotocol.ExecOpen,
		) (BufferedExecResult, error) {
			return BufferedExecResult{
				Stdout: []byte("direct stdout"), Stderr: []byte("direct stderr"),
				Terminal: &runnerprotocol.ExecTerminal{
					Kind:     runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
					ExitCode: 0,
				},
			}, nil
		},
		file: func(
			_ context.Context,
			_ *runnerprotocol.AssignmentFence,
			_ *runnerprotocol.FileOpen,
			_ []byte,
		) (FileOperationResult, error) {
			return FileOperationResult{Terminal: &runnerprotocol.FileTerminal{
				Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
			}}, nil
		},
	}
	stream := &threadSafeRunnerStream{}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(), backend, staticProtocolConnector{stream: stream},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.directDataPlane.bindStream(stream)
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	stopAdmissions := answerDirectDataPlaneAdmissions(service, stream)
	defer stopAdmissions()
	stopListener, err := service.startDataPlaneListener(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stopListener(); err != nil {
			t.Errorf("stop data-plane listener: %v", err)
		}
	})
	tlsConfig, err := portdirect.TLSConfigForSPKIPin(service.dataPlaneSPKIPin)
	if err != nil {
		t.Fatal(err)
	}

	execCredential := "direct-exec-credential-0000000000000000"
	registerDirectDataPlaneTestSession(
		t, service, fence, "direct-exec", "direct-exec-stream",
		runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC, execCredential,
	)
	execConnection, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = execConnection.Close() })
	if err := portdirect.WriteCredential(execConnection, portdirect.SessionKindExec, execCredential); err != nil {
		t.Fatal(err)
	}
	if verdict, detail, err := portdirect.ReadVerdict(execConnection); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("direct Exec verdict = %d/%q, %v", verdict, detail, err)
	}
	writeDirectDataPlaneTestMessage(t, execConnection, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Exec{Exec: &runnerprotocol.ExecFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-exec", StreamId: "direct-exec-stream",
			Sequence:    1,
			Correlation: relayOperationCorrelation(fence, "direct-exec", "request-direct-exec", "lease-direct-exec"),
			Payload: &runnerprotocol.ExecFrame_Open{Open: &runnerprotocol.ExecOpen{
				Command: &runnerprotocol.ExecOpen_Shell{Shell: "true"}, OutputLimitBytes: 1024,
				Streaming: true,
			}},
		}},
	})
	writeDirectDataPlaneTestMessage(t, execConnection, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Exec{Exec: &runnerprotocol.ExecFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-exec", StreamId: "direct-exec-stream",
			Sequence:    2,
			Correlation: relayOperationCorrelation(fence, "direct-exec", "request-direct-exec", "lease-direct-exec"),
			Payload: &runnerprotocol.ExecFrame_Credit{Credit: &runnerprotocol.StreamCredit{
				ByteCount: 1024,
			}},
		}},
	})
	stdout := readDirectDataPlaneTestMessage(t, execConnection).GetExec().GetOutput()
	stderr := readDirectDataPlaneTestMessage(t, execConnection).GetExec().GetOutput()
	terminal := readDirectDataPlaneTestMessage(t, execConnection).GetExec().GetTerminal()
	if stdout.GetChannel() != runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT ||
		string(stdout.GetData()) != "direct stdout" ||
		stderr.GetChannel() != runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR ||
		string(stderr.GetData()) != "direct stderr" ||
		terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("direct streaming Exec result = %#v/%#v/%#v", stdout, stderr, terminal)
	}

	fileCredential := "direct-file-credential-0000000000000000"
	registerDirectDataPlaneTestSession(
		t, service, fence, "direct-file", "direct-file-stream",
		runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE, fileCredential,
	)
	fileConnection, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fileConnection.Close() })
	if err := portdirect.WriteCredential(fileConnection, portdirect.SessionKindFile, fileCredential); err != nil {
		t.Fatal(err)
	}
	if verdict, detail, err := portdirect.ReadVerdict(fileConnection); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("direct File verdict = %d/%q, %v", verdict, detail, err)
	}
	writeDirectDataPlaneTestMessage(t, fileConnection, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_File{File: &runnerprotocol.FileFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-file", StreamId: "direct-file-stream",
			Sequence:    1,
			Correlation: relayOperationCorrelation(fence, "direct-file", "request-direct-file", "lease-direct-file"),
			Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{
				Operation:             runnerprotocol.FileOperation_FILE_OPERATION_MKDIR,
				WorkspaceRelativePath: "direct", Recursive: true,
			}},
		}},
	})
	fileTerminal := readDirectDataPlaneTestMessage(t, fileConnection).GetFile().GetTerminal()
	if fileTerminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("direct File terminal = %#v", fileTerminal)
	}

	cancelCredential := "direct-cancel-credential-00000000000000"
	registerDirectDataPlaneTestSession(
		t, service, fence, "direct-cancel", "direct-cancel-stream",
		runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC, cancelCredential,
	)
	cancelConnection, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cancelConnection.Close() })
	if err := portdirect.WriteCredential(cancelConnection, portdirect.SessionKindExec, cancelCredential); err != nil {
		t.Fatal(err)
	}
	if verdict, detail, err := portdirect.ReadVerdict(cancelConnection); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("direct cancellation verdict = %d/%q, %v", verdict, detail, err)
	}
	if err := service.handleDataPlaneCancel(&runnerprotocol.DataPlaneCancelCommand{
		Fence: cloneRunnerFence(fence), OperationId: "direct-cancel", StreamId: "direct-cancel-stream",
		Kind: runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC, Reason: "test cancellation",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cancelConnection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := portdirect.ReadTypedMessage(cancelConnection); err == nil {
		t.Fatal("cancelled direct session remained connected before Open")
	}
}

func registerDirectDataPlaneTestSession(
	t *testing.T,
	service *RunnerProtocolService,
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	streamID string,
	kind runnerprotocol.DataPlaneSessionKind,
	credential string,
) {
	t.Helper()
	digest := sha256.Sum256([]byte(credential))
	if err := service.registerDirectDataPlaneSession(&runnerprotocol.DataPlaneDirectOpen{
		Fence: cloneRunnerFence(fence), OperationId: operationID, StreamId: streamID,
		Correlation: relayOperationCorrelation(fence, operationID, "request-"+operationID, "lease-"+operationID),
		Kind:        kind, DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		CredentialDigest: digest[:],
	}); err != nil {
		t.Fatal(err)
	}
}

func answerDirectDataPlaneAdmissions(
	service *RunnerProtocolService,
	stream *threadSafeRunnerStream,
) func() {
	stop := make(chan struct{})
	answered := map[string]bool{}
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, message := range stream.messages() {
				consume := message.GetDataPlaneDirectConsume()
				if consume == nil || answered[consume.MessageId] {
					continue
				}
				answered[consume.MessageId] = true
				_ = service.directDataPlane.deliverAdmission(&runnerprotocol.DataPlaneDirectAdmission{
					MessageId: consume.MessageId, Fence: consume.Fence,
					OperationId: consume.OperationId, StreamId: consume.StreamId,
					Kind:       consume.Kind,
					Admission:  runnerprotocol.DataPlaneDirectAdmissionKind_DATA_PLANE_DIRECT_ADMISSION_KIND_ADMITTED,
					SafeDetail: "test verdict",
				})
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { close(stop) }
}

func writeDirectDataPlaneTestMessage(
	t *testing.T,
	connection net.Conn,
	message *runnerprotocol.ControlPlaneToRunner,
) {
	t.Helper()
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := portdirect.WriteTypedMessage(connection, payload); err != nil {
		t.Fatal(err)
	}
}

func readDirectDataPlaneTestMessage(
	t *testing.T,
	connection net.Conn,
) *runnerprotocol.RunnerToControlPlane {
	t.Helper()
	payload, err := portdirect.ReadTypedMessage(connection)
	if err != nil {
		t.Fatal(err)
	}
	message := &runnerprotocol.RunnerToControlPlane{}
	if err := proto.Unmarshal(payload, message); err != nil {
		t.Fatal(err)
	}
	return message
}
