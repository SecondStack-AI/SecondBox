package runnercontrol

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

func TestDirectDataPlaneCarriesTypedExecFileAndPTYMessages(t *testing.T) {
	ptyResize := make(chan string, 1)
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
		pty: func(
			_ context.Context,
			_ *runnerprotocol.AssignmentFence,
			_ *runnerprotocol.ExecOpen,
			controls <-chan PTYControl,
			emit func([]byte) error,
		) (*runnerprotocol.ExecTerminal, error) {
			for control := range controls {
				switch {
				case control.Credit > 0:
					if err := emit([]byte("direct pty")); err != nil {
						return nil, err
					}
				case control.Rows > 0:
					ptyResize <- fmt.Sprintf("%dx%d", control.Rows, control.Columns)
				case control.Input != nil:
					if err := emit([]byte("done")); err != nil {
						return nil, err
					}
					return &runnerprotocol.ExecTerminal{
						Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
					}, nil
				}
			}
			return nil, context.Canceled
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

	ptyCredential := "direct-pty-credential-00000000000000000"
	ptySession := registerDirectDataPlaneTestSession(
		t, service, fence, "direct-pty", "direct-pty-stream",
		runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY, ptyCredential,
	)
	ptyConnection, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := portdirect.WriteCredential(ptyConnection, portdirect.SessionKindPTY, ptyCredential); err != nil {
		t.Fatal(err)
	}
	if verdict, detail, err := portdirect.ReadVerdict(ptyConnection); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("direct PTY verdict = %d/%q, %v", verdict, detail, err)
	}
	correlation := relayOperationCorrelation(fence, "direct-pty", "request-direct-pty", "lease-direct-pty")
	writeDirectDataPlaneTestMessage(t, ptyConnection, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Exec{Exec: &runnerprotocol.ExecFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-pty", StreamId: "direct-pty-stream",
			Sequence: 1, Correlation: correlation,
			Payload: &runnerprotocol.ExecFrame_Open{Open: &runnerprotocol.ExecOpen{
				Command: &runnerprotocol.ExecOpen_Shell{Shell: "cat"}, OutputLimitBytes: 1024,
				AllocatePty: true, PtyRows: 24, PtyColumns: 80, Streaming: true,
			}},
		}},
	})
	writeDirectDataPlaneTestMessage(t, ptyConnection, directPTYAttachTestMessage(
		fence, correlation, "attachment-one", -1, 2,
	))
	if result := readDirectDataPlaneTestMessage(t, ptyConnection).GetPty().GetAttachResult(); result.GetKind() != runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED {
		t.Fatalf("direct PTY attachment = %#v", result)
	}
	writeDirectDataPlaneTestMessage(t, ptyConnection, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Pty{Pty: &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-pty", StreamId: "direct-pty-stream",
			Sequence: 2, Correlation: correlation,
			Payload: &runnerprotocol.PtyFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 64}},
		}},
	})
	if output := readDirectDataPlaneTestMessage(t, ptyConnection).GetPty().GetOutput(); string(output.GetData()) != "direct pty" {
		t.Fatalf("direct PTY output = %#v", output)
	}
	writeDirectDataPlaneTestMessage(t, ptyConnection, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Pty{Pty: &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-pty", StreamId: "direct-pty-stream",
			Sequence: 3, Correlation: correlation,
			Payload: &runnerprotocol.PtyFrame_Detach{Detach: &runnerprotocol.PtyDetach{ReconnectId: "attachment-one"}},
		}},
	})
	if err := ptyConnection.Close(); err != nil {
		t.Fatal(err)
	}
	waitDirectDataPlaneSessionReleased(t, ptySession)

	ptyReconnect, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ptyReconnect.Close() })
	if err := portdirect.WriteCredential(ptyReconnect, portdirect.SessionKindPTY, ptyCredential); err != nil {
		t.Fatal(err)
	}
	if verdict, detail, err := portdirect.ReadVerdict(ptyReconnect); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("direct PTY reconnect verdict = %d/%q, %v", verdict, detail, err)
	}
	writeDirectDataPlaneTestMessage(t, ptyReconnect, directPTYAttachTestMessage(
		fence, correlation, "attachment-two", 0, 3,
	))
	if result := readDirectDataPlaneTestMessage(t, ptyReconnect).GetPty().GetAttachResult(); result.GetKind() != runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED {
		t.Fatalf("direct PTY reattachment = %#v", result)
	}
	writeDirectDataPlaneTestMessage(t, ptyReconnect, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Pty{Pty: &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-pty", StreamId: "direct-pty-stream",
			Sequence: 3, Correlation: correlation,
			Payload: &runnerprotocol.PtyFrame_Resize{Resize: &runnerprotocol.PtyResize{Rows: 40, Columns: 120}},
		}},
	})
	select {
	case resize := <-ptyResize:
		if resize != "40x120" {
			t.Fatalf("direct PTY resize = %q", resize)
		}
	case <-time.After(time.Second):
		t.Fatal("direct PTY resize was not forwarded")
	}
	writeDirectDataPlaneTestMessage(t, ptyReconnect, &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Pty{Pty: &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-pty", StreamId: "direct-pty-stream",
			Sequence: 4, Correlation: correlation,
			Payload: &runnerprotocol.PtyFrame_Input{Input: &runnerprotocol.PtyInput{Data: []byte("exit")}},
		}},
	})
	ptyOutput := readDirectDataPlaneTestMessage(t, ptyReconnect).GetPty().GetOutput()
	ptyTerminal := readDirectDataPlaneTestMessage(t, ptyReconnect).GetPty().GetTerminal()
	if string(ptyOutput.GetData()) != "done" ||
		ptyTerminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("direct PTY completion = %#v/%#v", ptyOutput, ptyTerminal)
	}
	waitDirectDataPlaneSessionReleased(t, ptySession)
	ptyTerminalReplay, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ptyTerminalReplay.Close() })
	if err := portdirect.WriteCredential(ptyTerminalReplay, portdirect.SessionKindPTY, ptyCredential); err != nil {
		t.Fatal(err)
	}
	if verdict, detail, err := portdirect.ReadVerdict(ptyTerminalReplay); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("direct PTY terminal replay verdict = %d/%q, %v", verdict, detail, err)
	}
	writeDirectDataPlaneTestMessage(t, ptyTerminalReplay, directPTYAttachTestMessage(
		fence, correlation, "attachment-three", 1, 5,
	))
	if result := readDirectDataPlaneTestMessage(t, ptyTerminalReplay).GetPty().GetAttachResult(); result.GetKind() != runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED {
		t.Fatalf("direct PTY terminal replay attachment = %#v", result)
	}
	if terminal := readDirectDataPlaneTestMessage(t, ptyTerminalReplay).GetPty().GetTerminal(); terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("direct PTY replayed terminal = %#v", terminal)
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

func TestDirectPortAdmissionAndCancellationUseDataPlaneCommands(t *testing.T) {
	service, err := NewRunnerProtocolService(
		testRunnerConfig(), &relayAssignmentBackend{}, staticProtocolConnector{stream: &threadSafeRunnerStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	credential := "direct-port-command-credential"
	digest := sha256.Sum256([]byte(credential))
	deadline := time.Now().Add(time.Minute).UTC()
	if err := service.registerDirectDataPlaneSession(&runnerprotocol.DataPlaneDirectOpen{
		Fence: cloneRunnerFence(fence), OperationId: "direct-port", StreamId: "direct-port-stream",
		Correlation:    relayOperationCorrelation(fence, "direct-port", "request-direct-port", "lease-direct-port"),
		Kind:           runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT,
		DeadlineUnixMs: uint64(deadline.UnixMilli()), CredentialDigest: digest[:], StreamWindowBytes: 64,
		Port: &runnerprotocol.PortDirectOpen{
			GuestPort: 8080, Protocol: "tcp", PortName: "web",
			DeadlineUnixMs: uint64(deadline.UnixMilli()), CredentialDigest: digest[:],
			LeaseId: "lease-direct-port",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !service.directPorts.hasSession("direct-port") {
		t.Fatal("direct Port command did not register the session")
	}
	if err := service.handleDataPlaneCancel(&runnerprotocol.DataPlaneCancelCommand{
		Fence: cloneRunnerFence(fence), OperationId: "direct-port", StreamId: "direct-port-stream",
		Kind:   runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT,
		Reason: "test Port cancellation",
	}); err != nil {
		t.Fatal(err)
	}
	if service.directPorts.hasSession("direct-port") {
		t.Fatal("direct Port command cancellation retained the session")
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
) *directDataPlaneSession {
	t.Helper()
	digest := sha256.Sum256([]byte(credential))
	if err := service.registerDirectDataPlaneSession(&runnerprotocol.DataPlaneDirectOpen{
		Fence: cloneRunnerFence(fence), OperationId: operationID, StreamId: streamID,
		Correlation: relayOperationCorrelation(fence, operationID, "request-"+operationID, "lease-"+operationID),
		Kind:        kind, DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		CredentialDigest: digest[:], StreamWindowBytes: 64,
	}); err != nil {
		t.Fatal(err)
	}
	return service.directDataPlane.await(t.Context(), digest)
}

func directPTYAttachTestMessage(
	fence *runnerprotocol.AssignmentFence,
	correlation *runnerprotocol.Correlation,
	attachmentID string,
	afterSequence int64,
	sequence uint64,
) *runnerprotocol.ControlPlaneToRunner {
	return &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Pty{Pty: &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: "direct-pty", StreamId: "direct-pty-stream",
			Sequence: sequence, Correlation: correlation,
			Payload: &runnerprotocol.PtyFrame_Attach{Attach: &runnerprotocol.PtyAttach{
				ReconnectId: attachmentID, AfterSequence: afterSequence, StreamWindowBytes: 64,
			}},
		}},
	}
}

func waitDirectDataPlaneSessionReleased(t *testing.T, session *directDataPlaneSession) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		claimed := session.claimed
		session.mu.Unlock()
		if !claimed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("direct PTY session remained claimed after disconnect")
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
