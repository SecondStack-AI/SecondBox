package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
)

// proofAgentProtocol proves the guest-agent transport the backend design
// depends on: the production protocol service listening on gofer-passed host
// Unix sockets inside the sandbox, negotiated and driven by the production
// runner-side operation code. Only the dial and the listener plumbing are
// probe-local; Task 2H adds exactly those to the production binaries.
func proofAgentProtocol(env *probeEnv) error {
	if env.agentPath == "" {
		return fmt.Errorf("agent proof requires -agent")
	}
	base := filepath.Join(env.workDir, "agent-protocol")
	area, err := newProofArea(env, base, "session")
	if err != nil {
		return err
	}
	socketDir := filepath.Join(base, "session", "sockets")
	workspaceDir := filepath.Join(base, "session", "workspace")
	runtimePrivateDir := filepath.Join(base, "session", "runtime-private")
	for _, directory := range []string{socketDir, workspaceDir, runtimePrivateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}

	identity := agentIdentity{
		instanceID:      "probe-instance-1",
		sandboxID:       "probe-sandbox-1",
		generation:      1,
		buildID:         "secondbox-gvisor-probe-agent",
		imageDigest:     "sha256:" + repeatHex("a"),
		toolchainDigest: "sha256:" + repeatHex("b"),
	}
	const echoPort = 7777
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		Entrypoint: []string{
			"/agent",
			"-control-socket", "/probe-sockets/control.sock",
			"-protocol-socket", "/probe-sockets/protocol.sock",
			"-instance-id", identity.instanceID,
			"-sandbox-id", identity.sandboxID,
			"-sandbox-generation", strconv.FormatUint(identity.generation, 10),
			"-guest-build-id", identity.buildID,
			"-image-manifest-digest", identity.imageDigest,
			"-toolchain-manifest-digest", identity.toolchainDigest,
			"-heartbeat-interval", "5s",
			"-echo-port", strconv.Itoa(echoPort),
		},
		ExtraBinaries: map[string]string{"agent": env.agentPath},
		Binds: []bindMount{
			{Source: socketDir, Destination: "/probe-sockets", ReadOnly: false},
			{Source: workspaceDir, Destination: "/workspace", ReadOnly: false},
			{Source: runtimePrivateDir, Destination: "/runtime-private", ReadOnly: false},
		},
	}); err != nil {
		return err
	}

	command := env.runscRun(area, "run", "--host-uds=all")
	if err := command.Start(); err != nil {
		return fmt.Errorf("start agent sandbox: %w", err)
	}
	defer reapArea(env, area, command)

	protocolSocket := filepath.Join(socketDir, "protocol.sock")
	if err := waitForFile(protocolSocket, bootDeadline); err != nil {
		return fmt.Errorf("agent protocol socket never appeared: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	session, err := negotiateOverUnixSocket(ctx, protocolSocket, identity)
	if err != nil {
		return fmt.Errorf("negotiate: %w", err)
	}
	defer session.Close()
	emit(env.stdout, "agent-negotiation", "passed",
		"generation="+strconv.FormatUint(uint64(session.Generation), 10),
		"features="+strconv.Itoa(len(session.EnabledFeatures)),
		"transport=host-uds")

	const assignmentID = "probe-assignment-1"
	if err := subproofBufferedExec(env, ctx, session, assignmentID); err != nil {
		return fmt.Errorf("buffered-exec: %w", err)
	}
	if err := subproofStreamingExec(env, ctx, session, assignmentID); err != nil {
		return fmt.Errorf("streaming-exec: %w", err)
	}
	if err := subproofFileRoundtrip(env, ctx, session, assignmentID, workspaceDir); err != nil {
		return fmt.Errorf("file-roundtrip: %w", err)
	}
	if err := subproofPTY(env, ctx, session, assignmentID); err != nil {
		return fmt.Errorf("pty: %w", err)
	}
	if err := subproofPortRelay(env, ctx, session, assignmentID, echoPort); err != nil {
		return fmt.Errorf("port-relay: %w", err)
	}

	// Graceful shutdown: SIGTERM forwards to the agent, which must stop its
	// servers and exit zero.
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal agent sandbox: %w", err)
	}
	if err := waitCommand(command, gracefulDeadline); err != nil {
		return fmt.Errorf("agent sandbox did not stop: %w", err)
	}
	emit(env.stdout, "agent-shutdown", "passed", "signal=SIGTERM")
	return nil
}

type agentIdentity struct {
	instanceID      string
	sandboxID       string
	generation      uint64
	buildID         string
	imageDigest     string
	toolchainDigest string
}

func repeatHex(digit string) string {
	value := ""
	for i := 0; i < 64; i++ {
		value += digit
	}
	return value
}

// negotiateOverUnixSocket mirrors the production NegotiateGuestProtocol flow
// with a filesystem Unix-socket dialer in place of the Firecracker vsock
// CONNECT framing, then hands the negotiated stream to the production
// operation drivers through the exported session fields.
func negotiateOverUnixSocket(
	ctx context.Context,
	socketPath string,
	identity agentIdentity,
) (*firecracker.GuestProtocolSession, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	binding := &guestv1.ConnectionBinding{
		InstanceId:        identity.instanceID,
		SandboxId:         identity.sandboxID,
		SandboxGeneration: identity.generation,
		ConnectionNonce:   nonce,
	}
	connection, err := grpc.NewClient(
		"passthrough:///secondbox-gvisor-probe-guest",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  10 * time.Millisecond,
				Multiplier: 1.5,
				Jitter:     0.2,
				MaxDelay:   250 * time.Millisecond,
			},
			MinConnectTimeout: 20 * time.Second,
		}),
	)
	if err != nil {
		return nil, err
	}
	requested := []guestv1.GuestFeature{
		guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
		guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
		guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
		guestv1.GuestFeature_GUEST_FEATURE_ACTIVITY_EVENTS,
		guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
	}
	stream, err := guestv1.NewGuestAgentClient(connection).Connect(ctx, grpc.WaitForReady(true))
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
			Binding:              binding,
			SupportedGenerations: &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			RequestedFeatures:    requested,
			MandatoryFeatures: []guestv1.GuestFeature{
				guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
				guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
				guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
			},
			ExpectedImageManifestDigest:     identity.imageDigest,
			ExpectedToolchainManifestDigest: identity.toolchainDigest,
		}},
	}); err != nil {
		_ = connection.Close()
		return nil, err
	}
	first, err := stream.Recv()
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if rejection := first.GetRejection(); rejection != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("negotiation rejected (%s): %s", rejection.Kind, rejection.SafeDetail)
	}
	welcome := first.GetWelcome()
	if welcome == nil || welcome.SelectedGeneration != 1 ||
		welcome.GuestBuildId != identity.buildID ||
		welcome.ImageManifestDigest != identity.imageDigest ||
		welcome.ToolchainManifestDigest != identity.toolchainDigest {
		_ = connection.Close()
		return nil, fmt.Errorf("welcome identity mismatch: %+v", welcome)
	}
	enabled := make(map[guestv1.GuestFeature]bool, len(welcome.EnabledFeatures))
	for _, feature := range welcome.EnabledFeatures {
		enabled[feature] = true
	}
	return &firecracker.GuestProtocolSession{
		Connection:              connection,
		Stream:                  stream,
		Binding:                 binding,
		Generation:              welcome.SelectedGeneration,
		EnabledFeatures:         enabled,
		GuestBuildID:            welcome.GuestBuildId,
		ImageManifestDigest:     welcome.ImageManifestDigest,
		ToolchainManifestDigest: welcome.ToolchainManifestDigest,
	}, nil
}

func execDeadline() uint64 {
	return uint64(time.Now().Add(60 * time.Second).UnixMilli())
}

func subproofBufferedExec(
	env *probeEnv, ctx context.Context,
	session *firecracker.GuestProtocolSession, assignmentID string,
) error {
	result, err := session.ExecuteBuffered(ctx, assignmentID, &guestv1.ExecRequest{
		Command: &guestv1.ExecRequest_Argv{
			Argv: &guestv1.ArgvCommand{Argument: []string{"/guest", "print", "buffered-exec-ok"}},
		},
		Cwd:              ".",
		DeadlineUnixMs:   execDeadline(),
		OutputLimitBytes: 1 << 20,
	})
	if err != nil {
		return err
	}
	if !bytes.Contains(result.Stdout, []byte("buffered-exec-ok")) {
		return fmt.Errorf("stdout %q lacks expected output (stderr %q, terminal %v, admission %v)",
			result.Stdout, result.Stderr, result.Terminal, result.Admission)
	}
	if result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		result.Terminal.GetExitCode() != 0 {
		return fmt.Errorf("unexpected terminal: %v", result.Terminal)
	}
	emit(env.stdout, "agent-buffered-exec", "passed",
		"stdout_bytes="+strconv.Itoa(len(result.Stdout)),
		"exit_code=0")
	return nil
}

func subproofStreamingExec(
	env *probeEnv, ctx context.Context,
	session *firecracker.GuestProtocolSession, assignmentID string,
) error {
	payload := []byte("stream-payload-0123456789")
	controls := make(chan firecracker.GuestExecControl, 3)
	controls <- firecracker.GuestExecControl{Credit: 1 << 20}
	controls <- firecracker.GuestExecControl{Input: payload}
	controls <- firecracker.GuestExecControl{EndOfInput: true}
	close(controls)
	var stdout bytes.Buffer
	result, err := session.ExecuteStreaming(ctx, assignmentID, &guestv1.ExecRequest{
		Command: &guestv1.ExecRequest_Argv{
			Argv: &guestv1.ArgvCommand{Argument: []string{"/guest", "cat", "-"}},
		},
		Cwd:              ".",
		DeadlineUnixMs:   execDeadline(),
		OutputLimitBytes: 1 << 20,
		Streaming:        true,
	}, controls, func(channel guestv1.ExecOutputChannel, data []byte) error {
		if channel == guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT {
			stdout.Write(data)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !bytes.Equal(stdout.Bytes(), payload) {
		return fmt.Errorf("echoed stream %q does not match %q", stdout.Bytes(), payload)
	}
	if result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		result.Terminal.GetExitCode() != 0 {
		return fmt.Errorf("unexpected terminal: %v", result.Terminal)
	}
	emit(env.stdout, "agent-streaming-exec", "passed",
		"echoed_bytes="+strconv.Itoa(stdout.Len()),
		"exit_code=0")
	return nil
}

func subproofFileRoundtrip(
	env *probeEnv, ctx context.Context,
	session *firecracker.GuestProtocolSession, assignmentID, workspaceDir string,
) error {
	content := make([]byte, 4096)
	for i := range content {
		content[i] = byte(i % 251)
	}
	const relativePath = "probe/file.bin"
	if err := session.WriteFile(ctx, assignmentID, relativePath, content, 0o600); err != nil {
		return err
	}
	read, err := session.ReadFile(ctx, assignmentID, relativePath, uint64(len(content))+1)
	if err != nil {
		return err
	}
	if !bytes.Equal(read.Content, content) {
		return fmt.Errorf("read content differs from written content")
	}
	// The gofer must have written through to the host-side workspace bind.
	hostVisible, err := os.ReadFile(filepath.Join(workspaceDir, relativePath))
	if err != nil {
		return fmt.Errorf("host-side workspace file missing: %w", err)
	}
	if !bytes.Equal(hostVisible, content) {
		return fmt.Errorf("host-side workspace content differs")
	}
	emit(env.stdout, "agent-file-roundtrip", "passed",
		"bytes="+strconv.Itoa(len(content)),
		"host_visible=true")
	return nil
}

func subproofPTY(
	env *probeEnv, ctx context.Context,
	session *firecracker.GuestProtocolSession, assignmentID string,
) error {
	controls := make(chan firecracker.GuestPTYControl, 4)
	controls <- firecracker.GuestPTYControl{Credit: 1 << 20}
	controls <- firecracker.GuestPTYControl{Input: []byte("pty-echo\n")}
	controls <- firecracker.GuestPTYControl{Rows: 30, Columns: 100}
	controls <- firecracker.GuestPTYControl{Input: []byte{0x04}}
	close(controls)
	var output bytes.Buffer
	result, err := session.ExecutePTY(ctx, assignmentID, &guestv1.ExecRequest{
		Command: &guestv1.ExecRequest_Argv{
			Argv: &guestv1.ArgvCommand{Argument: []string{"/guest", "cat", "-"}},
		},
		Cwd:              ".",
		DeadlineUnixMs:   execDeadline(),
		OutputLimitBytes: 1 << 20,
		Streaming:        true,
		Pty:              &guestv1.PtyDimensions{Rows: 24, Columns: 80},
	}, controls, func(data []byte) error {
		output.Write(data)
		return nil
	})
	if err != nil {
		return err
	}
	if !bytes.Contains(output.Bytes(), []byte("pty-echo")) {
		return fmt.Errorf("pty output %q lacks echoed input", output.Bytes())
	}
	if result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		result.Terminal.GetExitCode() != 0 {
		return fmt.Errorf("unexpected terminal: %v", result.Terminal)
	}
	emit(env.stdout, "agent-pty", "passed",
		"output_bytes="+strconv.Itoa(output.Len()),
		"resize=sent",
		"exit_code=0")
	return nil
}

// subproofPortRelay mirrors the production Port frame flow on a second
// negotiated stream: request, initial guest credit, runner credit grant,
// bounded bytes both ways against the sandbox loopback echo listener.
func subproofPortRelay(
	env *probeEnv, ctx context.Context,
	session *firecracker.GuestProtocolSession, assignmentID string, echoPort int,
) error {
	portCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream, binding, err := openProbePortStream(portCtx, session)
	if err != nil {
		return err
	}
	operationBinding := &guestv1.OperationBinding{
		Connection:   binding,
		AssignmentId: assignmentID,
		OperationId:  "probe-port-op",
		StreamId:     "probe-port-op-port",
	}
	sequence := uint64(1)
	sendFrame := func(payload func(*guestv1.PortFrame)) error {
		frame := &guestv1.PortFrame{Binding: &guestv1.OperationBinding{
			Connection:   binding,
			AssignmentId: operationBinding.AssignmentId,
			OperationId:  operationBinding.OperationId,
			StreamId:     operationBinding.StreamId,
			Sequence:     sequence,
		}}
		sequence++
		payload(frame)
		return stream.Send(&guestv1.RunnerToGuest{
			Message: &guestv1.RunnerToGuest_Port{Port: frame},
		})
	}
	if err := sendFrame(func(frame *guestv1.PortFrame) {
		frame.Payload = &guestv1.PortFrame_Request{Request: &guestv1.PortRequest{
			GuestPort:     uint32(echoPort),
			Protocol:      "tcp",
			IdleTimeoutMs: 30_000,
		}}
	}); err != nil {
		return fmt.Errorf("send port request: %w", err)
	}

	payload := []byte("port-relay-payload")
	granted := false
	sent := false
	var echoed bytes.Buffer
	for echoed.Len() < len(payload) {
		message, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("port stream receive: %w", err)
		}
		frame := message.GetPort()
		if frame == nil {
			continue
		}
		if terminal := frame.GetTerminal(); terminal != nil {
			return fmt.Errorf("port terminal before echo completed: %s", terminal.Kind)
		}
		if frame.GetCredit() != nil && !granted {
			granted = true
			if err := sendFrame(func(out *guestv1.PortFrame) {
				out.Payload = &guestv1.PortFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 1 << 20}}
			}); err != nil {
				return fmt.Errorf("send port credit: %w", err)
			}
			if !sent {
				sent = true
				if err := sendFrame(func(out *guestv1.PortFrame) {
					out.Payload = &guestv1.PortFrame_Bytes{Bytes: &guestv1.PortBytes{Data: payload}}
				}); err != nil {
					return fmt.Errorf("send port bytes: %w", err)
				}
			}
		}
		if portBytes := frame.GetBytes(); portBytes != nil {
			echoed.Write(portBytes.Data)
		}
	}
	if !bytes.Equal(echoed.Bytes(), payload) {
		return fmt.Errorf("echoed %q does not match %q", echoed.Bytes(), payload)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("close port stream: %w", err)
	}
	emit(env.stdout, "agent-port-relay", "passed",
		"guest_port="+strconv.Itoa(echoPort),
		"relayed_bytes="+strconv.Itoa(echoed.Len()))
	return nil
}

// openProbePortStream opens the second Connect stream the Port relay uses,
// negotiating only the port-proxy feature, exactly as production does.
func openProbePortStream(
	ctx context.Context,
	session *firecracker.GuestProtocolSession,
) (guestv1.GuestAgent_ConnectClient, *guestv1.ConnectionBinding, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	binding := &guestv1.ConnectionBinding{
		InstanceId:        session.Binding.InstanceId,
		SandboxId:         session.Binding.SandboxId,
		SandboxGeneration: session.Binding.SandboxGeneration,
		ConnectionNonce:   nonce,
	}
	stream, err := guestv1.NewGuestAgentClient(session.Connection).Connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
			Binding:              binding,
			SupportedGenerations: &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
			RequestedFeatures: []guestv1.GuestFeature{
				guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
			},
			MandatoryFeatures: []guestv1.GuestFeature{
				guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
			},
			ExpectedImageManifestDigest:     session.ImageManifestDigest,
			ExpectedToolchainManifestDigest: session.ToolchainManifestDigest,
		}},
	}); err != nil {
		return nil, nil, err
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, nil, err
	}
	welcome := first.GetWelcome()
	if welcome == nil || welcome.SelectedGeneration != 1 {
		return nil, nil, fmt.Errorf("port stream welcome is invalid: %v", first)
	}
	return stream, binding, nil
}
