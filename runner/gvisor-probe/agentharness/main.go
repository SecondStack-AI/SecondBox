// Command secondbox-gvisor-probe-agent runs the production guest-agent
// protocol implementation inside the probe sandbox with Unix-socket
// listeners in place of the vsock listeners. It constructs the exact
// server composition of cmd/secondbox-guest-agent so the Task 0H proof
// exercises the real protocol service; only the listener transport and
// flag plumbing differ, which is precisely what Task 2H later adds to the
// production agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	microvmguest "github.com/SecondStack-AI/SecondBox/runner/internal/guest"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"

	"google.golang.org/grpc"
)

func main() {
	os.Exit(run())
}

func run() int {
	var controlSocket, protocolSocket, workspace, runtimePrivate string
	var instanceID, sandboxID, guestBuildID, imageDigest, toolchainDigest string
	var sandboxGeneration uint64
	var heartbeatInterval time.Duration
	var echoPort int
	flag.StringVar(&controlSocket, "control-socket", "", "required Unix socket path for the HTTP control service")
	flag.StringVar(&protocolSocket, "protocol-socket", "", "required Unix socket path for the gRPC guest protocol")
	flag.StringVar(&workspace, "workspace", "/workspace", "workspace mount path")
	flag.StringVar(&runtimePrivate, "runtime-private", "/runtime-private", "runtime-private path")
	flag.StringVar(&instanceID, "instance-id", "", "required immutable instance ID")
	flag.StringVar(&sandboxID, "sandbox-id", "", "required immutable sandbox ID")
	flag.Uint64Var(&sandboxGeneration, "sandbox-generation", 0, "required immutable sandbox generation")
	flag.StringVar(&guestBuildID, "guest-build-id", "", "required guest build ID")
	flag.StringVar(&imageDigest, "image-manifest-digest", "", "required image manifest digest")
	flag.StringVar(&toolchainDigest, "toolchain-manifest-digest", "", "required toolchain manifest digest")
	flag.DurationVar(&heartbeatInterval, "heartbeat-interval", 0, "required positive heartbeat interval")
	flag.IntVar(&echoPort, "echo-port", 0, "optional loopback TCP echo listener for the Port proof")
	flag.Parse()

	if controlSocket == "" || protocolSocket == "" {
		slog.Error("probe agent requires -control-socket and -protocol-socket")
		return 2
	}

	guestServer := microvmguest.Server{
		WorkspaceDir:      workspace,
		RuntimePrivateDir: runtimePrivate,
		InstanceID:        instanceID,
		SandboxID:         sandboxID,
		Freezer:           microvmguest.LinuxFreezer{},
	}
	protocolService, err := microvmguest.NewProtocolService(guestServer, microvmguest.ProtocolIdentity{
		InstanceID:              instanceID,
		SandboxID:               sandboxID,
		SandboxGeneration:       sandboxGeneration,
		GuestBuildID:            guestBuildID,
		ImageManifestDigest:     imageDigest,
		ToolchainManifestDigest: toolchainDigest,
		HeartbeatInterval:       heartbeatInterval,
	})
	if err != nil {
		slog.Error("invalid guest protocol identity", "error", err)
		return 1
	}
	protocolServer := grpc.NewServer()
	guestv1.RegisterGuestAgentServer(protocolServer, protocolService)
	controlServer := &http.Server{Handler: guestServer.Handler()}

	controlListener, err := listenUnix(controlSocket)
	if err != nil {
		slog.Error("failed to listen on control socket", "error", err)
		return 1
	}
	protocolListener, err := listenUnix(protocolSocket)
	if err != nil {
		slog.Error("failed to listen on protocol socket", "error", errors.Join(err, controlListener.Close()))
		return 1
	}

	if echoPort != 0 {
		echoListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort))
		if err != nil {
			slog.Error("failed to listen on echo port", "error", err)
			return 1
		}
		go serveEcho(echoListener)
	}

	serviceErrors := make(chan error, 2)
	go func() {
		if err := controlServer.Serve(controlListener); err != nil && err != http.ErrServerClosed {
			serviceErrors <- fmt.Errorf("control service: %w", err)
		}
	}()
	go func() {
		if err := protocolServer.Serve(protocolListener); err != nil {
			serviceErrors <- fmt.Errorf("protocol service: %w", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-serviceErrors:
		slog.Error("probe agent service failed", "error", err)
		return 1
	}
	protocolServer.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := controlServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("control shutdown failed", "error", err)
		return 1
	}
	return 0
}

func listenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return net.Listen("unix", path)
}

func serveEcho(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer connection.Close()
			_, _ = io.Copy(connection, connection)
		}()
	}
}
