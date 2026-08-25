package firecracker

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	microvmguest "github.com/SecondStack-AI/SecondBox/runner/internal/guest"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"

	"google.golang.org/grpc"
)

func startDirectUnixSocketGuest(t *testing.T) (string, string, *grpc.Server) {
	t.Helper()
	socketPath := shortUnixSocketPath(t, "guest-direct.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
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
	t.Cleanup(server.Stop)
	return socketPath, workspace, server
}

func negotiateDirectUnixSocket(t *testing.T, ctx context.Context, socketPath string) *GuestProtocolSession {
	t.Helper()
	session, err := NegotiateGuestProtocol(ctx, GuestProtocolNegotiation{
		UDSPath:                         socketPath,
		DirectUnixSocket:                true,
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
		t.Fatalf("negotiate over direct Unix socket: %v", err)
	}
	return session
}

// TestNegotiateGuestProtocolOverDirectUnixSocket proves the gVisor transport
// carries the identical negotiated protocol and complete operation surface,
// with no vsock CONNECT framing.
func TestNegotiateGuestProtocolOverDirectUnixSocket(t *testing.T) {
	socketPath, workspace, _ := startDirectUnixSocketGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := negotiateDirectUnixSocket(t, ctx, socketPath)
	defer session.Close()
	if session.Generation != 1 || session.GuestBuildID != "guest-build-1" {
		t.Fatalf("session = %#v", session)
	}

	execResult, err := session.ExecuteBuffered(context.Background(), "assignment-1", &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "printf uds-exec"},
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatalf("execute over direct Unix socket transport: %v", err)
	}
	if execResult.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		string(execResult.Stdout) != "uds-exec" {
		t.Fatalf("transport exec result = %#v", execResult)
	}
	assertStreamingExecOverTransport(t, session)
	assertPTYOverTransport(t, session)
	fileContent := []byte{0, 1, 0xff, 'u'}
	if err := session.WriteFile(context.Background(), "assignment-1", "transport/data.bin", fileContent, 0o600); err != nil {
		t.Fatalf("write over direct Unix socket transport: %v", err)
	}
	fileResult, err := session.ReadFile(context.Background(), "assignment-1", "transport/data.bin", 1024)
	if err != nil {
		t.Fatalf("read over direct Unix socket transport: %v", err)
	}
	if !bytes.Equal(fileResult.Content, fileContent) {
		t.Fatalf("transport file content = %v, want %v", fileResult.Content, fileContent)
	}
	assertGuestFileSubsetOverTransport(t, session, workspace)
	assertAssignmentBackendBridgeOverTransport(t, session)
}

// TestDirectUnixSocketSupportsIndependentConcurrentConnections proves a
// second negotiated connection operates independently over the same socket;
// the Port relay's dedicated stream depends on exactly this property.
func TestDirectUnixSocketSupportsIndependentConcurrentConnections(t *testing.T) {
	socketPath, _, _ := startDirectUnixSocketGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := negotiateDirectUnixSocket(t, ctx, socketPath)
	defer first.Close()
	second := negotiateDirectUnixSocket(t, ctx, socketPath)
	defer second.Close()
	if bytes.Equal(first.Binding.ConnectionNonce, second.Binding.ConnectionNonce) {
		t.Fatal("concurrent connections share a nonce")
	}
	for index, session := range []*GuestProtocolSession{first, second} {
		result, err := session.ExecuteBuffered(context.Background(), "assignment-1", &guestv1.ExecRequest{
			Command:          &guestv1.ExecRequest_Shell{Shell: "printf concurrent"},
			OutputLimitBytes: 1024,
		})
		if err != nil {
			t.Fatalf("connection %d exec: %v", index, err)
		}
		if string(result.Stdout) != "concurrent" {
			t.Fatalf("connection %d stdout = %q", index, result.Stdout)
		}
	}
}

// TestDirectUnixSocketTransportLossFailsOperations proves a lost transport
// surfaces as an explicit operation failure, never a hang or fabricated exit.
func TestDirectUnixSocketTransportLossFailsOperations(t *testing.T) {
	socketPath, _, server := startDirectUnixSocketGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := negotiateDirectUnixSocket(t, ctx, socketPath)
	defer session.Close()
	server.Stop()
	_, err := session.ExecuteBuffered(context.Background(), "assignment-1", &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: "printf lost"},
		OutputLimitBytes: 1024,
	})
	if err == nil {
		t.Fatal("operation succeeded over a lost transport")
	}
}

func TestDirectUnixSocketNegotiationValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	base := GuestProtocolNegotiation{
		UDSPath:                         "/run/guest.sock",
		DirectUnixSocket:                true,
		InstanceID:                      "instance-1",
		SandboxID:                       "sandbox-1",
		SandboxGeneration:               7,
		ExpectedGuestBuildID:            "guest-build-1",
		ExpectedImageManifestDigest:     "sha256:image",
		ExpectedToolchainManifestDigest: "sha256:toolchain",
	}
	withPort := base
	withPort.Port = 4096
	if _, err := NegotiateGuestProtocol(ctx, withPort); err == nil ||
		!strings.Contains(err.Error(), "takes no port") {
		t.Fatalf("direct socket with port error = %v", err)
	}
	longPath := base
	longPath.UDSPath = "/" + strings.Repeat("x", 120)
	if _, err := NegotiateGuestProtocol(ctx, longPath); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded socket path error = %v", err)
	}
}
