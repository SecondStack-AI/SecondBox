package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	guestconformance "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol/conformance"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerconformance "github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol/conformance"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestNegotiateGuestProtocolOverFirecrackerVsockTransport(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "guest-protocol.sock")
	raw, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := &firecrackerVsockTestListener{Listener: raw, port: 4096}
	server := grpc.NewServer()
	workspace := t.TempDir()
	service, err := microvmguest.NewProtocolService(
		microvmguest.Server{WorkspaceDir: workspace, RuntimePrivateDir: t.TempDir(), InstanceID: "instance-1", SandboxID: "sandbox-1"},
		microvmguest.ProtocolIdentity{
			InstanceID:              "instance-1",
			SandboxID:               "sandbox-1",
			SandboxGeneration:       7,
			GuestBuildID:            "guest-build-1",
			ImageManifestDigest:     "sha256:image",
			ToolchainManifestDigest: "sha256:toolchain",
			HeartbeatInterval:       time.Second,
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := NegotiateGuestProtocol(ctx, GuestProtocolNegotiation{
		UDSPath:                         socketPath,
		Port:                            4096,
		InstanceID:                      "instance-1",
		SandboxID:                       "sandbox-1",
		SandboxGeneration:               7,
		ExpectedGuestBuildID:            "guest-build-1",
		ExpectedImageManifestDigest:     "sha256:image",
		ExpectedToolchainManifestDigest: "sha256:toolchain",
		RequestedFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
			guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
		},
		MandatoryFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
		},
	})
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	defer session.Close()
	cancel()
	if session.Generation != 1 ||
		session.GuestBuildID != "guest-build-1" ||
		!session.EnabledFeatures[guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM] {
		t.Fatalf("session = %#v", session)
	}
	execResult, err := session.ExecuteBuffered(context.Background(), "assignment-1", &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "printf transport-exec"},
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("execute over retained Firecracker transport: %v", err)
	}
	if execResult.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		string(execResult.Stdout) != "transport-exec" {
		t.Fatalf("transport exec result = %#v", execResult)
	}
	assertStreamingExecOverTransport(t, session)
	assertPTYOverTransport(t, session)
	fileContent := []byte{0, 1, 0xff, 'x'}
	if err := session.WriteFile(context.Background(), "assignment-1", "transport/data.bin", fileContent, 0o600); err != nil {
		t.Fatalf("write over retained Firecracker transport: %v", err)
	}
	fileResult, err := session.ReadFile(context.Background(), "assignment-1", "transport/data.bin", 1024)
	if err != nil {
		t.Fatalf("read over retained Firecracker transport: %v", err)
	}
	if !bytes.Equal(fileResult.Content, fileContent) {
		t.Fatalf("transport file content = %v, want %v", fileResult.Content, fileContent)
	}
	assertGuestFileSubsetOverTransport(t, session, workspace)
	assertAssignmentBackendBridgeOverTransport(t, session)

	guestconformance.RunV1Negotiation(t, func(ctx context.Context) (guestv1.GuestAgent_ConnectClient, io.Closer, error) {
		connection, err := grpc.NewClient(
			"passthrough:///secondbox-guest-conformance",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return dialFirecrackerVsock(ctx, socketPath, 4096)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return nil, nil, err
		}
		stream, err := guestv1.NewGuestAgentClient(connection).Connect(ctx)
		if err != nil {
			connection.Close()
			return nil, nil, err
		}
		return stream, connection, nil
	})
}

func TestNegotiateGuestProtocolWaitsForFirecrackerVsockTransport(t *testing.T) {
	socketPath := shortUnixSocketPath(t, "guest-protocol-delayed.sock")
	server := grpc.NewServer()
	service, err := microvmguest.NewProtocolService(
		microvmguest.Server{
			WorkspaceDir:      t.TempDir(),
			RuntimePrivateDir: t.TempDir(),
			InstanceID:        "instance-delayed",
			SandboxID:         "sandbox-delayed",
		},
		microvmguest.ProtocolIdentity{
			InstanceID:              "instance-delayed",
			SandboxID:               "sandbox-delayed",
			SandboxGeneration:       3,
			GuestBuildID:            "guest-build-delayed",
			ImageManifestDigest:     "sha256:image-delayed",
			ToolchainManifestDigest: "sha256:toolchain-delayed",
			HeartbeatInterval:       time.Second,
		},
	)
	if err != nil {
		t.Fatalf("create delayed guest protocol service: %v", err)
	}
	guestv1.RegisterGuestAgentServer(server, service)
	listenResult := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		raw, listenErr := net.Listen("unix", socketPath)
		if listenErr != nil {
			listenResult <- listenErr
			return
		}
		listenResult <- nil
		_ = server.Serve(&firecrackerVsockTestListener{
			Listener: raw,
			port:     4097,
		})
	}()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := NegotiateGuestProtocol(ctx, GuestProtocolNegotiation{
		UDSPath:                         socketPath,
		Port:                            4097,
		InstanceID:                      "instance-delayed",
		SandboxID:                       "sandbox-delayed",
		SandboxGeneration:               3,
		ExpectedGuestBuildID:            "guest-build-delayed",
		ExpectedImageManifestDigest:     "sha256:image-delayed",
		ExpectedToolchainManifestDigest: "sha256:toolchain-delayed",
	})
	if listenErr := <-listenResult; listenErr != nil {
		t.Fatalf("listen after negotiation began: %v", listenErr)
	}
	if err != nil {
		t.Fatalf("negotiate after delayed listener readiness: %v", err)
	}
	defer session.Close()
}

func assertPTYOverTransport(t *testing.T, session *GuestProtocolSession) {
	t.Helper()
	controls := make(chan GuestPTYControl, 4)
	type ptyResult struct {
		result GuestPTYResult
		err    error
	}
	completed := make(chan ptyResult, 1)
	output := make(chan []byte, 4)
	go func() {
		result, err := session.ExecutePTY(
			t.Context(),
			"assignment-pty",
			&guestv1.ExecRequest{
				Command: &guestv1.ExecRequest_Shell{
					Shell: "stty size; stty raw -echo; dd bs=1 count=4 2>/dev/null | od -An -t x1; stty size",
				},
				OutputLimitBytes: 1024,
				Streaming:        true,
				Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
			},
			controls,
			func(data []byte) error {
				output <- bytes.Clone(data)
				return nil
			},
		)
		completed <- ptyResult{result: result, err: err}
	}()
	select {
	case value := <-output:
		t.Fatalf("Firecracker guest PTY emitted without credit: %q", value)
	case <-time.After(50 * time.Millisecond):
	}
	controls <- GuestPTYControl{Credit: 1024}
	var received bytes.Buffer
	for !strings.Contains(strings.ReplaceAll(received.String(), "\r", ""), "24 80") {
		select {
		case value := <-output:
			received.Write(value)
		case <-time.After(time.Second):
			t.Fatalf("initial Firecracker guest PTY dimensions missing from %q", received.String())
		}
	}
	controls <- GuestPTYControl{Rows: 40, Columns: 120}
	controls <- GuestPTYControl{Input: []byte{0, 1, 0xfe, 0xff}}
	close(controls)
normalPTY:
	for {
		select {
		case value := <-output:
			received.Write(value)
		case result := <-completed:
			if result.err != nil {
				t.Fatalf("execute Firecracker guest PTY: %v", result.err)
			}
			if result.result.SessionID == "" ||
				result.result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
				t.Fatalf("Firecracker guest PTY result = %#v", result.result)
			}
			normalized := strings.ReplaceAll(received.String(), "\r", "")
			if !strings.Contains(normalized, "00 01 fe ff") ||
				!strings.Contains(normalized, "40 120") {
				t.Fatalf("Firecracker guest PTY output = %q", normalized)
			}
			break normalPTY
		case <-time.After(time.Second):
			t.Fatal("Firecracker guest PTY did not terminate")
		}
	}

	cancelContext, cancel := context.WithCancel(t.Context())
	time.AfterFunc(25*time.Millisecond, cancel)
	cancelled, err := session.ExecutePTY(
		cancelContext,
		"assignment-pty-cancel",
		&guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 60"},
			OutputLimitBytes: 1024,
			Streaming:        true,
			Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
		},
		make(chan GuestPTYControl),
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatalf("cancel Firecracker guest PTY: %v", err)
	}
	if cancelled.SessionID == "" ||
		cancelled.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("cancelled Firecracker guest PTY = %#v", cancelled)
	}

	deadline, err := session.ExecutePTY(
		t.Context(),
		"assignment-pty-deadline",
		&guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 60"},
			DeadlineUnixMs:   uint64(time.Now().Add(50 * time.Millisecond).UnixMilli()),
			OutputLimitBytes: 1024,
			Streaming:        true,
			Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
		},
		make(chan GuestPTYControl),
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatalf("deadline Firecracker guest PTY: %v", err)
	}
	if deadline.SessionID == "" ||
		deadline.SessionID == cancelled.SessionID ||
		deadline.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED {
		t.Fatalf("deadline Firecracker guest PTY = %#v", deadline)
	}

	exhaustedControls := make(chan GuestPTYControl, 1)
	exhaustedControls <- GuestPTYControl{Credit: 4}
	close(exhaustedControls)
	var partial bytes.Buffer
	exhausted, err := session.ExecutePTY(
		t.Context(),
		"assignment-pty-exhausted",
		&guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "printf abcdef"},
			OutputLimitBytes: 4,
			Streaming:        true,
			Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
		},
		exhaustedControls,
		func(data []byte) error {
			_, err := partial.Write(data)
			return err
		},
	)
	if err != nil {
		t.Fatalf("output-exhausted Firecracker guest PTY: %v", err)
	}
	if partial.String() != "abcd" ||
		exhausted.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED ||
		exhausted.Terminal.GetLimitBytes() != 4 {
		t.Fatalf("output-exhausted Firecracker guest PTY = partial %q result %#v", partial.String(), exhausted)
	}

	spawnFailed, err := session.ExecutePTY(
		t.Context(),
		"assignment-pty-spawn-failed",
		&guestv1.ExecRequest{
			Command: &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{
				Argument: []string{"/missing/secondbox-pty-command"},
			}},
			OutputLimitBytes: 1024,
			Streaming:        true,
			Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
		},
		make(chan GuestPTYControl),
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatalf("spawn-failed Firecracker guest PTY: %v", err)
	}
	if spawnFailed.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED ||
		spawnFailed.Terminal.GetSpawnFailureReason() != guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND {
		t.Fatalf("spawn-failed Firecracker guest PTY = %#v", spawnFailed)
	}
}

func assertStreamingExecOverTransport(t *testing.T, session *GuestProtocolSession) {
	t.Helper()
	controls := make(chan GuestExecControl, 4)
	type execResult struct {
		result BufferedGuestExecResult
		err    error
	}
	result := make(chan execResult, 1)
	outputs := make(chan string, 2)
	go func() {
		value, err := session.ExecuteStreaming(
			t.Context(),
			"assignment-streaming",
			&guestv1.ExecRequest{
				Command: &guestv1.ExecRequest_Shell{
					Shell: "read first; printf 'stdout:%s' \"$first\"; read second; printf 'stderr:%s' \"$second\" >&2",
				},
				OutputLimitBytes: 1024,
				Streaming:        true,
			},
			controls,
			func(channel guestv1.ExecOutputChannel, data []byte) error {
				outputs <- channel.String() + ":" + string(data)
				return nil
			},
		)
		result <- execResult{result: value, err: err}
	}()
	controls <- GuestExecControl{Input: []byte("one\n")}
	select {
	case output := <-outputs:
		t.Fatalf("streaming guest emitted without output credit: %q", output)
	case <-time.After(50 * time.Millisecond):
	}
	controls <- GuestExecControl{Credit: 1024}
	select {
	case output := <-outputs:
		if output != "EXEC_OUTPUT_CHANNEL_STDOUT:stdout:one" {
			t.Fatalf("first streaming output = %q", output)
		}
	case <-time.After(time.Second):
		t.Fatal("streaming stdout was not emitted after credit")
	}
	controls <- GuestExecControl{Input: []byte("two\n")}
	select {
	case output := <-outputs:
		if output != "EXEC_OUTPUT_CHANNEL_STDERR:stderr:two" {
			t.Fatalf("second streaming output = %q", output)
		}
	case <-time.After(time.Second):
		t.Fatal("streaming stderr was not emitted")
	}
	close(controls)
	select {
	case completed := <-result:
		if completed.err != nil {
			t.Fatalf("streaming guest exec: %v", completed.err)
		}
		if completed.result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
			t.Fatalf("streaming guest terminal = %#v", completed.result.Terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("streaming guest exec did not terminate")
	}

	exhaustedControls := make(chan GuestExecControl, 1)
	exhaustedControls <- GuestExecControl{Credit: 4}
	close(exhaustedControls)
	var partial bytes.Buffer
	exhausted, err := session.ExecuteStreaming(
		t.Context(),
		"assignment-exhausted",
		&guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "printf abcdef"},
			OutputLimitBytes: 4,
			Streaming:        true,
		},
		exhaustedControls,
		func(_ guestv1.ExecOutputChannel, data []byte) error {
			_, err := partial.Write(data)
			return err
		},
	)
	if err != nil {
		t.Fatalf("output-exhausted guest exec: %v", err)
	}
	if partial.String() != "abcd" ||
		exhausted.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
		t.Fatalf("output-exhausted guest result = partial %q, terminal %#v", partial.String(), exhausted.Terminal)
	}

	cancelContext, cancel := context.WithCancel(t.Context())
	time.AfterFunc(25*time.Millisecond, cancel)
	cancelled, err := session.ExecuteStreaming(
		cancelContext,
		"assignment-stream-cancel",
		&guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 60"},
			OutputLimitBytes: 1024,
			Streaming:        true,
		},
		make(chan GuestExecControl),
		func(guestv1.ExecOutputChannel, []byte) error { return nil },
	)
	if err != nil {
		t.Fatalf("cancel streaming guest exec: %v", err)
	}
	if cancelled.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("cancelled streaming guest terminal = %#v", cancelled.Terminal)
	}

	eofControls := make(chan GuestExecControl, 3)
	eofResult := make(chan execResult, 1)
	eofOutputs := make(chan string, 1)
	go func() {
		value, err := session.ExecuteStreaming(
			t.Context(),
			"assignment-stream-eof",
			&guestv1.ExecRequest{
				Command:          &guestv1.ExecRequest_Shell{Shell: "wc -c"},
				OutputLimitBytes: 1024,
				Streaming:        true,
			},
			eofControls,
			func(_ guestv1.ExecOutputChannel, data []byte) error {
				eofOutputs <- string(data)
				return nil
			},
		)
		eofResult <- execResult{result: value, err: err}
	}()
	eofControls <- GuestExecControl{Input: []byte("abcdef")}
	eofControls <- GuestExecControl{Credit: 1024}
	select {
	case result := <-eofResult:
		t.Fatalf("EOF-dependent guest command completed before EOF: %#v", result)
	case <-time.After(50 * time.Millisecond):
	}
	eofControls <- GuestExecControl{EndOfInput: true}
	select {
	case output := <-eofOutputs:
		if strings.TrimSpace(output) != "6" {
			t.Fatalf("EOF-dependent guest output = %q", output)
		}
	case <-time.After(time.Second):
		t.Fatal("EOF-dependent guest output was not emitted")
	}
	select {
	case result := <-eofResult:
		if result.err != nil ||
			result.result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
			t.Fatalf("EOF-dependent guest result = %#v error=%v", result.result, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("EOF-dependent guest command did not terminate")
	}
}

func assertGuestFileSubsetOverTransport(
	t *testing.T,
	session *GuestProtocolSession,
	workspace string,
) {
	t.Helper()
	run := func(request *guestv1.FileRequest, content []byte) GuestFileOperationResult {
		t.Helper()
		result, err := session.ExecuteFileOperation(t.Context(), "assignment-1", request, content)
		if err != nil {
			t.Fatal(err)
		}
		if result.Terminal.GetKind() != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
			t.Fatalf("file operation %s terminal = %#v", request.Operation, result.Terminal)
		}
		return result
	}
	run(&guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_MKDIR, WorkspaceRelativePath: "subset",
	}, nil)
	content := []byte{0, 2, 0xfe, 'z'}
	checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	run(&guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "subset/data.bin",
		ExpectedSize: uint64(len(content)), ExpectedChecksum: checksum, CreateMode: 0o600,
	}, content)
	stat := run(&guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_STAT, WorkspaceRelativePath: "subset/data.bin",
	}, nil)
	if stat.Metadata.GetSize() != uint64(len(content)) {
		t.Fatalf("stat metadata = %#v", stat.Metadata)
	}
	if stat.Metadata.GetKind() != guestv1.FileKind_FILE_KIND_FILE ||
		stat.Metadata.GetModifiedAtUnixMs() == 0 {
		t.Fatalf("stat typed metadata = %#v", stat.Metadata)
	}
	list := run(&guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_LIST_DIRECT_CHILDREN, WorkspaceRelativePath: "subset",
	}, nil)
	if len(list.Metadata.GetDirectChildren()) != 1 || list.Metadata.GetDirectChildren()[0] != "data.bin" {
		t.Fatalf("list metadata = %#v", list.Metadata)
	}
	entries := list.Metadata.GetDirectChildEntries()
	if len(entries) != 1 || entries[0].GetPath() != "subset/data.bin" ||
		entries[0].GetKind() != guestv1.FileKind_FILE_KIND_FILE ||
		entries[0].GetSize() != uint64(len(content)) ||
		entries[0].GetModifiedAtUnixMs() == 0 {
		t.Fatalf("list typed metadata = %#v", list.Metadata)
	}
	exists := run(&guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_EXISTS, WorkspaceRelativePath: "subset/data.bin",
	}, nil)
	if !exists.Metadata.GetExists() {
		t.Fatalf("exists metadata = %#v", exists.Metadata)
	}
	read := run(&guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: "subset/data.bin",
		ExpectedSize: 1024,
	}, nil)
	if !bytes.Equal(read.Content, content) {
		t.Fatalf("generic read content = %v, want %v", read.Content, content)
	}
	nonRecursiveMkdir, err := session.ExecuteFileOperation(t.Context(), "assignment-1", &guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_MKDIR, WorkspaceRelativePath: "missing/child",
		Recursive: false,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nonRecursiveMkdir.Terminal.GetKind() == guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("non-recursive mkdir unexpectedly completed: %#v", nonRecursiveMkdir)
	}
	nonRecursiveRemove, err := session.ExecuteFileOperation(t.Context(), "assignment-1", &guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_REMOVE, WorkspaceRelativePath: "subset",
		Recursive: false, Force: false,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nonRecursiveRemove.Terminal.GetKind() == guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("non-recursive remove unexpectedly completed: %#v", nonRecursiveRemove)
	}
	run(&guestv1.FileRequest{
		Operation: guestv1.FileOperation_FILE_OPERATION_REMOVE, WorkspaceRelativePath: "subset",
		Recursive: true, Force: false,
	}, nil)
	if _, err := os.Stat(filepath.Join(workspace, "subset")); !os.IsNotExist(err) {
		t.Fatalf("removed subset stat error = %v", err)
	}
}

func assertAssignmentBackendBridgeOverTransport(
	t *testing.T,
	session *GuestProtocolSession,
) {
	t.Helper()
	fence := &runnerprotocol.AssignmentFence{
		AssignmentId:      "assignment-bridge",
		SandboxId:         "sandbox-1",
		InstanceId:        "instance-1",
		SandboxGeneration: 7,
		FencingToken:      []byte("bridge-fencing-token"),
	}
	manager := &Manager{
		instances: map[string]*instance{
			"fc-bridge": {
				id: "fc-bridge", sandboxID: "sandbox-1", compartmentID: "instance-1",
				sandboxGeneration: 7, guestProtocolSession: session,
			},
		},
	}
	backend := &AssignmentBackend{
		manager: manager,
		assignments: map[string]activeRunnerAssignment{
			fence.AssignmentId: {
				fence: cloneRunnerProtocolFence(fence), backendReference: "fc-bridge",
			},
		},
	}
	execResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf bridge"}}},
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("assignment backend argv Exec: %v", err)
	}
	if string(execResult.Stdout) != "bridge" ||
		execResult.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("assignment backend Exec = %#v", execResult)
	}
	stdin := []byte{0, 1, 0xff, 'x'}
	stdinResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/cat"}}},
		Stdin:            stdin,
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("assignment backend binary stdin Exec: %v", err)
	}
	if !bytes.Equal(stdinResult.Stdout, stdin) ||
		stdinResult.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("assignment backend binary stdin Exec = %#v", stdinResult)
	}
	shellResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Shell{Shell: "printf shell-bridge"},
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("assignment backend shell Exec: %v", err)
	}
	if string(shellResult.Stdout) != "shell-bridge" ||
		shellResult.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("assignment backend shell Exec = %#v", shellResult)
	}
	spawnResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/definitely/missing"}}},
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("assignment backend spawn Exec: %v", err)
	}
	if spawnResult.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED ||
		spawnResult.Terminal.GetSpawnFailureReason() != runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND ||
		spawnResult.Terminal.GetMessage() == "" {
		t.Fatalf("assignment backend spawn terminal = %#v", spawnResult.Terminal)
	}
	deadlineResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Shell{Shell: "sleep 60"},
		DeadlineUnixMs:   uint64(time.Now().Add(25 * time.Millisecond).UnixMilli()),
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("assignment backend deadline Exec: %v", err)
	}
	if deadlineResult.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED ||
		deadlineResult.Terminal.GetElapsedMilliseconds() == 0 {
		t.Fatalf("assignment backend deadline terminal = %#v", deadlineResult.Terminal)
	}
	cancelCtx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(25*time.Millisecond, cancel)
	cancelResult, err := backend.ExecuteBuffered(cancelCtx, fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Shell{Shell: "sleep 60"},
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("assignment backend cancel Exec: %v", err)
	}
	if cancelResult.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("assignment backend cancel terminal = %#v", cancelResult.Terminal)
	}
	exhaustedResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Shell{Shell: "printf output-too-large"},
		OutputLimitBytes: 2,
	})
	if err != nil {
		t.Fatalf("assignment backend exhausted Exec: %v", err)
	}
	if exhaustedResult.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED ||
		exhaustedResult.Terminal.GetLimitBytes() != 2 ||
		string(exhaustedResult.Stdout) != "ou" {
		t.Fatalf("assignment backend output-exhausted result = %#v", exhaustedResult)
	}
	streamControls := make(chan runnercontrol.ExecControl, 2)
	streamOutputs := make(chan string, 1)
	type streamingResult struct {
		terminal *runnerprotocol.ExecTerminal
		err      error
	}
	streamResult := make(chan streamingResult, 1)
	go func() {
		terminal, err := backend.ExecuteStreaming(
			t.Context(),
			fence,
			&runnerprotocol.ExecOpen{
				Command: &runnerprotocol.ExecOpen_Shell{
					Shell: "value=$(cat); printf 'live:%s' \"$value\"",
				},
				OutputLimitBytes: 1024,
				Streaming:        true,
			},
			streamControls,
			func(channel runnerprotocol.ExecOutputChannel, data []byte) error {
				streamOutputs <- channel.String() + ":" + string(data)
				return nil
			},
		)
		streamResult <- streamingResult{terminal: terminal, err: err}
	}()
	streamControls <- runnercontrol.ExecControl{
		Input: &runnerprotocol.ExecInput{Data: []byte("bridge")},
	}
	select {
	case output := <-streamOutputs:
		t.Fatalf("assignment backend emitted streaming output without credit: %q", output)
	case <-time.After(50 * time.Millisecond):
	}
	streamControls <- runnercontrol.ExecControl{
		Input: &runnerprotocol.ExecInput{EndOfInput: true},
	}
	streamControls <- runnercontrol.ExecControl{Credit: 1024}
	select {
	case output := <-streamOutputs:
		if output != "EXEC_OUTPUT_CHANNEL_STDOUT:live:bridge" {
			t.Fatalf("assignment backend streaming output = %q", output)
		}
	case <-time.After(time.Second):
		t.Fatal("assignment backend streaming output was not emitted")
	}
	select {
	case result := <-streamResult:
		if result.err != nil ||
			result.terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
			t.Fatalf("assignment backend streaming terminal = %#v error=%v", result.terminal, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("assignment backend streaming Exec did not terminate")
	}
	ptyCancelContext, cancelPTY := context.WithCancel(t.Context())
	time.AfterFunc(25*time.Millisecond, cancelPTY)
	ptyTerminal, err := backend.ExecutePTY(
		ptyCancelContext,
		fence,
		&runnerprotocol.ExecOpen{
			Command:          &runnerprotocol.ExecOpen_Shell{Shell: "sleep 60"},
			OutputLimitBytes: 1024,
			Streaming:        true,
			AllocatePty:      true,
			PtyRows:          24,
			PtyColumns:       80,
		},
		make(chan runnercontrol.PTYControl),
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatalf("assignment backend PTY cancellation: %v", err)
	}
	if ptyTerminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("assignment backend PTY cancellation terminal = %#v", ptyTerminal)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	echoErrors := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			echoErrors <- err
			return
		}
		defer connection.Close()
		value := make([]byte, 4)
		if _, err := io.ReadFull(connection, value); err != nil {
			echoErrors <- err
			return
		}
		_, err = connection.Write(value)
		echoErrors <- err
	}()
	portConnection, err := backend.OpenPort(t.Context(), fence, &runnerprotocol.PortOpen{
		GuestPort: uint32(listener.Addr().(*net.TCPAddr).Port),
		Protocol:  "tcp", IdleTimeoutMs: 30_000,
	})
	if err != nil {
		t.Fatalf("assignment backend Port open: %v", err)
	}
	portContent := []byte{0, 1, 0xfe, 2}
	if err := portConnection.Write(t.Context(), portContent); err != nil {
		t.Fatalf("assignment backend Port write: %v", err)
	}
	portResponse, err := portConnection.Read(t.Context(), len(portContent))
	if err != nil || !bytes.Equal(portResponse, portContent) {
		t.Fatalf("assignment backend Port response = %v error=%v", portResponse, err)
	}
	if err := portConnection.Close(); err != nil {
		t.Fatalf("assignment backend Port close: %v", err)
	}
	if err := <-echoErrors; err != nil {
		t.Fatal(err)
	}
	runnerconformance.RunDataPlane(t, runnerconformance.DataPlaneFixture{
		Backend: backend, PTY: backend, Port: backend, Fence: fence,
	})
}

func cloneRunnerProtocolFence(fence *runnerprotocol.AssignmentFence) *runnerprotocol.AssignmentFence {
	return &runnerprotocol.AssignmentFence{
		AssignmentId:      fence.AssignmentId,
		SandboxId:         fence.SandboxId,
		InstanceId:        fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration,
		FencingToken:      bytes.Clone(fence.FencingToken),
	}
}

type firecrackerVsockTestListener struct {
	net.Listener
	port uint32
}

func (l *firecrackerVsockTestListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	line := make([]byte, 0, 32)
	var one [1]byte
	for len(line) < cap(line) {
		if _, err := conn.Read(one[:]); err != nil {
			conn.Close()
			return nil, err
		}
		if one[0] == '\n' {
			break
		}
		line = append(line, one[0])
	}
	if got, want := strings.TrimSpace(string(line)), fmt.Sprintf("CONNECT %d", l.port); got != want {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake = %q, want %q", got, want)
	}
	if _, err := fmt.Fprintf(conn, "OK %d\n", l.port); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
