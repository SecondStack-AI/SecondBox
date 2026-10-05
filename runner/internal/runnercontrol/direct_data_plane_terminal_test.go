package runnercontrol

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
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
	read := startDirectFileReadToTerminal(t, time.Now().Add(time.Minute))
	read.writeCredit(t, 3)
	read.writeCredit(t, 4)
	if err := read.connection.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := read.connection.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Runner closed the connection after its terminal instead of awaiting the peer: %v", err)
	}
	read.writeCredit(t, 5)
	if err := read.connection.Close(); err != nil {
		t.Fatal(err)
	}
	read.waitReleased(t, time.Second)
}

func TestDirectTerminalLingerClosesAConnectedPeer(t *testing.T) {
	for _, test := range []struct {
		name      string
		deadline  time.Duration
		wantClose time.Duration
	}{
		{name: "after the linger", deadline: time.Minute, wantClose: directDataPlaneTerminalLinger},
		{name: "at an earlier delivery deadline", deadline: 1500 * time.Millisecond, wantClose: 1500 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := startDirectFileReadToTerminal(t, time.Now().Add(test.deadline))
			terminalAt := time.Now()
			read.writeCredit(t, 3)
			if err := read.connection.SetReadDeadline(time.Now().Add(directDataPlaneTerminalLinger + 2*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := read.connection.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("lingering Runner connection ended with %v, want a close", err)
			}
			if elapsed := time.Since(terminalAt); elapsed > test.wantClose+time.Second {
				t.Fatalf("Runner closed %s after its terminal, want about %s", elapsed, test.wantClose)
			}
			read.waitReleased(t, time.Second)
		})
	}
}

type directFileReadAtTerminal struct {
	service    *RunnerProtocolService
	connection *tls.Conn
	fence      *runnerprotocol.AssignmentFence
	frame      func(sequence uint64, frame *runnerprotocol.FileFrame) *runnerprotocol.ControlPlaneToRunner
}

// startDirectFileReadToTerminal reads a small file over a direct connection and
// returns once the client has received the Runner's terminal.
func startDirectFileReadToTerminal(t *testing.T, deadline time.Time) *directFileReadAtTerminal {
	t.Helper()
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
	t.Cleanup(answerDirectDataPlaneAdmissions(service, control))
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
	digest := sha256.Sum256([]byte(credential))
	correlation := relayOperationCorrelation(fence, "direct-read", "request-direct-read", "lease-direct-read")
	if err := service.registerDirectDataPlaneSession(&runnerprotocol.DataPlaneDirectOpen{
		Fence: cloneRunnerFence(fence), OperationId: "direct-read", StreamId: "direct-read-stream",
		Correlation: correlation, Kind: runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE,
		DeadlineUnixMs: uint64(deadline.UnixMilli()), CredentialDigest: digest[:], StreamWindowBytes: 64,
	}); err != nil {
		t.Fatal(err)
	}
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
	read := &directFileReadAtTerminal{
		service: service, connection: connection, fence: fence,
		frame: func(sequence uint64, frame *runnerprotocol.FileFrame) *runnerprotocol.ControlPlaneToRunner {
			frame.Fence, frame.OperationId, frame.StreamId = cloneRunnerFence(fence), "direct-read", "direct-read-stream"
			frame.Sequence, frame.Correlation = sequence, correlation
			return &runnerprotocol.ControlPlaneToRunner{Message: &runnerprotocol.ControlPlaneToRunner_File{File: frame}}
		},
	}
	writeDirectDataPlaneTestMessage(t, connection, read.frame(1, &runnerprotocol.FileFrame{
		Payload: &runnerprotocol.FileFrame_Open{Open: &runnerprotocol.FileOpen{
			Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: "read.txt",
			ExpectedSize: 1024,
		}},
	}))
	read.writeCredit(t, 2)
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
	return read
}

func (read *directFileReadAtTerminal) writeCredit(t *testing.T, sequence uint64) {
	t.Helper()
	writeDirectDataPlaneTestMessage(t, read.connection, read.frame(sequence, &runnerprotocol.FileFrame{
		Payload: &runnerprotocol.FileFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: 5}},
	}))
}

func (read *directFileReadAtTerminal) waitReleased(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for read.service.directDataPlane.find("direct-read") != nil {
		if time.Now().After(deadline) {
			t.Fatal("direct File session stayed admitted after its connection closed")
		}
		time.Sleep(time.Millisecond)
	}
}
