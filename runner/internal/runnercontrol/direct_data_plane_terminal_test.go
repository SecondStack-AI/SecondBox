package runnercontrol

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// The control plane grants credit for each received chunk, so a credit can
// reach the Runner after its terminal. Closing at once would reset the
// connection over that unread credit and could discard the unread terminal.
func TestDirectFileTerminalKeepsConnectionReadableUntilPeerCloses(t *testing.T) {
	backend := &relayAssignmentBackend{file: func(
		context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.FileOpen, []byte,
	) (FileOperationResult, error) {
		return FileOperationResult{
			Metadata: &runnerprotocol.FileMetadata{Size: 5},
			Content:  []byte("hello"),
			Terminal: &runnerprotocol.FileTerminal{Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED},
		}, nil
	}}
	control := &threadSafeRunnerStream{}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: control})
	if err != nil {
		t.Fatal(err)
	}
	service.directDataPlane.bindStream(control)
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	stopAdmissions := answerDirectDataPlaneAdmissions(service, control)
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
	credential := "direct-read-credential-0000000000000000"
	registerDirectDataPlaneTestSession(
		t, service, fence, "direct-read", "direct-read-stream",
		runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE, credential,
	)
	connection, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := portdirect.WriteCredential(connection, portdirect.SessionKindFile, credential); err != nil {
		t.Fatal(err)
	}
	if verdict, detail, err := portdirect.ReadVerdict(connection); err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("direct File verdict = %d/%q, %v", verdict, detail, err)
	}
	correlation := relayOperationCorrelation(fence, "direct-read", "request-direct-read", "lease-direct-read")
	fileFrame := func(sequence uint64, frame *runnerprotocol.FileFrame) *runnerprotocol.ControlPlaneToRunner {
		frame.Fence, frame.OperationId, frame.StreamId = cloneRunnerFence(fence), "direct-read", "direct-read-stream"
		frame.Sequence, frame.Correlation = sequence, correlation
		return &runnerprotocol.ControlPlaneToRunner{Message: &runnerprotocol.ControlPlaneToRunner_File{File: frame}}
	}
	credit := func(sequence uint64) *runnerprotocol.ControlPlaneToRunner {
		return fileFrame(sequence, &runnerprotocol.FileFrame{
			Payload: &runnerprotocol.FileFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 5}},
		})
	}
	writeDirectDataPlaneTestMessage(t, connection, fileFrame(1, &runnerprotocol.FileFrame{
		Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{
			Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: "read.txt",
			ExpectedSize: 1024,
		}},
	}))
	writeDirectDataPlaneTestMessage(t, connection, credit(2))
	if readDirectDataPlaneTestMessage(t, connection).GetFile().GetMetadata() == nil {
		t.Fatal("direct File read did not deliver metadata first")
	}
	if string(readDirectDataPlaneTestMessage(t, connection).GetFile().GetChunk().GetData()) != "hello" {
		t.Fatal("direct File read did not deliver its chunk")
	}
	terminal := readDirectDataPlaneTestMessage(t, connection).GetFile().GetTerminal()
	if terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("direct File terminal = %#v", terminal)
	}
	writeDirectDataPlaneTestMessage(t, connection, credit(3))
	writeDirectDataPlaneTestMessage(t, connection, credit(4))
	if err := connection.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Runner closed the connection after its terminal instead of awaiting the peer: %v", err)
	}
	writeDirectDataPlaneTestMessage(t, connection, credit(5))
}
