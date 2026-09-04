package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	microvmguest "github.com/SecondStack-AI/SecondBox/runner/internal/guest"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"google.golang.org/grpc"
)

const expectedGuestFileWriteChunkSize = 64 << 10

type recordedGuestFileWriteChunk struct {
	sequence  uint64
	offset    uint64
	dataSize  int
	firstByte *byte
}

type recordingGuestProtocolStream struct {
	guestv1.GuestAgent_ConnectClient
	chunks []recordedGuestFileWriteChunk
	// onMetadata runs once, immediately after the guest's metadata frame is
	// received, so a test can cancel an operation exactly inside the window
	// where the bridge decides how to continue a read.
	onMetadata func()
}

func (stream *recordingGuestProtocolStream) Send(frame *guestv1.RunnerToGuest) error {
	if chunk := frame.GetFile().GetChunk(); chunk != nil {
		var firstByte *byte
		if len(chunk.Data) > 0 {
			firstByte = &chunk.Data[0]
		}
		stream.chunks = append(stream.chunks, recordedGuestFileWriteChunk{
			sequence:  frame.GetFile().GetBinding().GetSequence(),
			offset:    chunk.Offset,
			dataSize:  len(chunk.Data),
			firstByte: firstByte,
		})
	}
	return stream.GuestAgent_ConnectClient.Send(frame)
}

func (stream *recordingGuestProtocolStream) Recv() (*guestv1.GuestToRunner, error) {
	message, err := stream.GuestAgent_ConnectClient.Recv()
	if err == nil && stream.onMetadata != nil && message.GetFile().GetMetadata() != nil {
		hook := stream.onMetadata
		stream.onMetadata = nil
		hook()
	}
	return message, err
}

func (stream *recordingGuestProtocolStream) takeChunks() []recordedGuestFileWriteChunk {
	chunks := append([]recordedGuestFileWriteChunk(nil), stream.chunks...)
	stream.chunks = stream.chunks[:0]
	return chunks
}

func TestExecuteFileOperationChunksLargeWritesOverFirecrackerVsockTransport(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "guest-file-operation.sock")
	raw, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := &firecrackerVsockTestListener{Listener: raw, port: 4098}
	server := grpc.NewServer()
	workspace := t.TempDir()
	service, err := microvmguest.NewProtocolService(
		microvmguest.Server{
			WorkspaceDir: workspace, RuntimePrivateDir: t.TempDir(),
			InstanceID: "instance-file", SandboxID: "sandbox-file",
		},
		microvmguest.ProtocolIdentity{
			InstanceID: "instance-file", SandboxID: "sandbox-file", SandboxGeneration: 9,
			GuestBuildID: "guest-build-file", ImageManifestDigest: "sha256:image-file",
			ToolchainManifestDigest: "sha256:toolchain-file", HeartbeatInterval: time.Second,
		},
	)
	if err != nil {
		t.Fatalf("create guest protocol service: %v", err)
	}
	guestv1.RegisterGuestAgentServer(server, service)
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := NegotiateGuestProtocol(ctx, GuestProtocolNegotiation{
		UDSPath: socketPath, Port: 4098,
		InstanceID: "instance-file", SandboxID: "sandbox-file", SandboxGeneration: 9,
		ExpectedGuestBuildID: "guest-build-file", ExpectedImageManifestDigest: "sha256:image-file",
		ExpectedToolchainManifestDigest: "sha256:toolchain-file",
		RequestedFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
		},
		MandatoryFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
		},
	})
	if err != nil {
		t.Fatalf("negotiate guest file protocol: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close guest protocol session: %v", err)
		}
	})

	recordingStream := &recordingGuestProtocolStream{GuestAgent_ConnectClient: session.Stream}
	session.Stream = recordingStream

	assertGuestFileWriteChunkBoundaries(t, session, recordingStream, workspace)
	assertGuestLargeFileWrite(t, session, recordingStream, workspace)
	assertGuestFileFailureKeepsProtocolStreamUsable(t, session, recordingStream)
	assertGuestOversizedReadIsTypedAndKeepsProtocolStreamUsable(t, session, recordingStream)
	assertGuestOversizedReadRacingCancellationKeepsProtocolStreamUsable(t, session, recordingStream)
}

func assertGuestFileWriteChunkBoundaries(
	t *testing.T,
	session *GuestProtocolSession,
	recordingStream *recordingGuestProtocolStream,
	workspace string,
) {
	t.Helper()
	for _, size := range []int{
		0,
		1,
		expectedGuestFileWriteChunkSize - 1,
		expectedGuestFileWriteChunkSize,
		expectedGuestFileWriteChunkSize + 1,
	} {
		t.Run(fmt.Sprintf("write-size-%d", size), func(t *testing.T) {
			content := make([]byte, size)
			for index := range content {
				content[index] = byte(index % 251)
			}
			path := fmt.Sprintf("chunk-boundary-%d.bin", size)
			result, err := session.ExecuteFileOperation(t.Context(), "assignment-chunk-boundary", &guestv1.FileRequest{
				Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
				WorkspaceRelativePath: path,
				ExpectedSize:          uint64(len(content)),
				ExpectedChecksum:      fmt.Sprintf("sha256:%x", sha256.Sum256(content)),
				CreateMode:            0o600,
			}, content)
			if err != nil {
				t.Fatalf("write %d bytes over guest protocol: %v", size, err)
			}
			if result.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
				t.Fatalf("write %d bytes terminal = %#v", size, result.Terminal)
			}
			got, err := os.ReadFile(filepath.Join(workspace, path))
			if err != nil {
				t.Fatalf("read written %d-byte file: %v", size, err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("written %d-byte file content mismatch", size)
			}

			chunks := recordingStream.takeChunks()
			wantChunkCount := (size + expectedGuestFileWriteChunkSize - 1) / expectedGuestFileWriteChunkSize
			if len(chunks) != wantChunkCount {
				t.Fatalf("write %d bytes emitted %d chunks, want %d", size, len(chunks), wantChunkCount)
			}
			for index, chunk := range chunks {
				wantOffset := index * expectedGuestFileWriteChunkSize
				wantSize := min(expectedGuestFileWriteChunkSize, size-wantOffset)
				if chunk.sequence != uint64(index+2) ||
					chunk.offset != uint64(wantOffset) ||
					chunk.dataSize != wantSize {
					t.Fatalf("write %d bytes chunk %d = %#v, want sequence=%d offset=%d size=%d", size, index, chunk, index+2, wantOffset, wantSize)
				}
				if wantSize > 0 && chunk.firstByte != &content[wantOffset] {
					t.Fatalf("write %d bytes chunk %d copied its payload before transport send", size, index)
				}
			}
		})
	}
}

func assertGuestLargeFileWrite(
	t *testing.T,
	session *GuestProtocolSession,
	recordingStream *recordingGuestProtocolStream,
	workspace string,
) {
	t.Helper()
	const regressionPayloadSize = 52_929_536
	content := make([]byte, regressionPayloadSize)
	for index := range content {
		content[index] = byte(index % 251)
	}
	checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	result, err := session.ExecuteFileOperation(t.Context(), "assignment-large-file", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
		WorkspaceRelativePath: "large-file.bin",
		ExpectedSize:          uint64(len(content)),
		ExpectedChecksum:      checksum,
		CreateMode:            0o600,
	}, content)
	if err != nil {
		t.Fatalf("write reproduction payload over guest protocol: %v", err)
	}
	if result.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("large write terminal = %#v", result.Terminal)
	}
	file, err := os.Open(filepath.Join(workspace, "large-file.bin"))
	if err != nil {
		t.Fatalf("open large written file: %v", err)
	}
	hash := sha256.New()
	writtenSize, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatalf("checksum large written file: %v", err)
	}
	if writtenSize != regressionPayloadSize || fmt.Sprintf("sha256:%x", hash.Sum(nil)) != checksum {
		t.Fatalf("large written file size=%d checksum=sha256:%x", writtenSize, hash.Sum(nil))
	}
	chunks := recordingStream.takeChunks()
	wantChunkCount := (regressionPayloadSize + expectedGuestFileWriteChunkSize - 1) / expectedGuestFileWriteChunkSize
	if len(chunks) != wantChunkCount {
		t.Fatalf("large write emitted %d chunks, want %d", len(chunks), wantChunkCount)
	}
}

func assertGuestFileFailureKeepsProtocolStreamUsable(
	t *testing.T,
	session *GuestProtocolSession,
	recordingStream *recordingGuestProtocolStream,
) {
	t.Helper()
	failedContent := []byte("checksum failure")
	_, err := session.ExecuteFileOperation(t.Context(), "assignment-size-mismatch", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
		WorkspaceRelativePath: "size-mismatch.bin",
		ExpectedSize:          uint64(len(failedContent) + 1),
		ExpectedChecksum:      fmt.Sprintf("sha256:%x", sha256.Sum256(failedContent)),
		CreateMode:            0o600,
	}, failedContent)
	if err == nil || !strings.Contains(err.Error(), "content does not match declared size") {
		t.Fatalf("declared-size mismatch error = %v", err)
	}
	if chunks := recordingStream.takeChunks(); len(chunks) != 0 {
		t.Fatalf("declared-size mismatch emitted %d file chunks", len(chunks))
	}

	failed, err := session.ExecuteFileOperation(t.Context(), "assignment-failed-file", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
		WorkspaceRelativePath: "checksum-failure.bin",
		ExpectedSize:          uint64(len(failedContent)),
		ExpectedChecksum:      "sha256:incorrect",
		CreateMode:            0o600,
	}, failedContent)
	if err != nil {
		t.Fatalf("receive failed file operation terminal: %v", err)
	}
	if failed.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH {
		t.Fatalf("failed file terminal = %#v", failed.Terminal)
	}
	recordingStream.takeChunks()

	recoveryContent := []byte("stream remains usable")
	recovered, err := session.ExecuteFileOperation(t.Context(), "assignment-recovery-file", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
		WorkspaceRelativePath: "after-failure.txt",
		ExpectedSize:          uint64(len(recoveryContent)),
		ExpectedChecksum:      fmt.Sprintf("sha256:%x", sha256.Sum256(recoveryContent)),
		CreateMode:            0o600,
	}, recoveryContent)
	if err != nil || recovered.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("file operation after failure = %#v, error=%v", recovered, err)
	}
	recordingStream.takeChunks()

	execResult, err := session.ExecuteBuffered(t.Context(), "assignment-recovery-exec", &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "printf protocol-recovered"},
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("exec after failed file operation: %v", err)
	}
	if execResult.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		string(execResult.Stdout) != "protocol-recovered" {
		t.Fatalf("exec after failed file operation = %#v", execResult)
	}
}

func assertGuestOversizedReadIsTypedAndKeepsProtocolStreamUsable(
	t *testing.T,
	session *GuestProtocolSession,
	recordingStream *recordingGuestProtocolStream,
) {
	t.Helper()
	content := make([]byte, 8<<10)
	for index := range content {
		content[index] = byte(index % 251)
	}
	written, err := session.ExecuteFileOperation(t.Context(), "assignment-oversized-read", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
		WorkspaceRelativePath: "oversized-read.bin",
		ExpectedSize:          uint64(len(content)),
		ExpectedChecksum:      fmt.Sprintf("sha256:%x", sha256.Sum256(content)),
		CreateMode:            0o600,
	}, content)
	if err != nil || written.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("oversized read fixture write = %#v, error=%v", written, err)
	}
	recordingStream.takeChunks()

	declined, err := session.ExecuteFileOperation(t.Context(), "assignment-oversized-read", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
		WorkspaceRelativePath: "oversized-read.bin",
		ExpectedSize:          uint64(len(content) / 2),
	}, nil)
	if err != nil {
		t.Fatalf("oversized read returned a bridge error instead of a typed terminal: %v", err)
	}
	if declined.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED ||
		len(declined.Content) != 0 || declined.Metadata.GetSize() != uint64(len(content)) {
		t.Fatalf("oversized read = %#v", declined)
	}

	read, err := session.ExecuteFileOperation(t.Context(), "assignment-oversized-read", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
		WorkspaceRelativePath: "oversized-read.bin",
		ExpectedSize:          uint64(len(content)),
	}, nil)
	if err != nil || read.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED ||
		!bytes.Equal(read.Content, content) {
		t.Fatalf("read after declined oversized read = %#v, error=%v", read, err)
	}
}

func assertGuestOversizedReadRacingCancellationKeepsProtocolStreamUsable(
	t *testing.T,
	session *GuestProtocolSession,
	recordingStream *recordingGuestProtocolStream,
) {
	t.Helper()
	content := make([]byte, 8<<10)
	for index := range content {
		content[index] = byte(index % 251)
	}
	written, err := session.ExecuteFileOperation(t.Context(), "assignment-oversized-read-race", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
		WorkspaceRelativePath: "oversized-read-race.bin",
		ExpectedSize:          uint64(len(content)),
		ExpectedChecksum:      fmt.Sprintf("sha256:%x", sha256.Sum256(content)),
		CreateMode:            0o600,
	}, content)
	if err != nil || written.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("racing oversized read fixture write = %#v, error=%v", written, err)
	}
	recordingStream.takeChunks()

	// Cancelling as the metadata frame lands puts the caller's cancellation and
	// the bridge's own refusal in the same window. Exactly one cancellation
	// frame may reach the guest, or it retires the operation and fails the
	// shared connection for every later operation.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	recordingStream.onMetadata = cancel
	raced, err := session.ExecuteFileOperation(ctx, "assignment-oversized-read-race", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
		WorkspaceRelativePath: "oversized-read-race.bin",
		ExpectedSize:          uint64(len(content) / 2),
	}, nil)
	recordingStream.onMetadata = nil
	if err != nil {
		t.Fatalf("oversized read racing cancellation error = %v", err)
	}
	if raced.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED ||
		len(raced.Content) != 0 {
		t.Fatalf("oversized read racing cancellation = %#v, want the caller cancellation preserved", raced)
	}

	read, err := session.ExecuteFileOperation(t.Context(), "assignment-oversized-read-race", &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
		WorkspaceRelativePath: "oversized-read-race.bin",
		ExpectedSize:          uint64(len(content)),
	}, nil)
	if err != nil || read.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED ||
		!bytes.Equal(read.Content, content) {
		t.Fatalf("read after a racing oversized read = %#v, error=%v", read, err)
	}
}
