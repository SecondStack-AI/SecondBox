package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/guest"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

const (
	guestWorkspaceMountPath    = "/workspace"
	guestRuntimePrivatePath    = "/runtime-private"
	guestShutdownGraceInterval = 2 * time.Second
)

func main() {
	os.Exit(run())
}

func run() int {
	var controlVsockPort int
	var protocolVsockPort int
	var workspace string
	var runtimePrivate string
	var logPath string
	var instanceID string
	var sandboxID string
	var sandboxGeneration uint64
	var guestBuildID string
	var imageManifestDigest string
	var toolchainManifestDigest string
	var heartbeatInterval time.Duration
	var waitSecrets bool
	var secretsTimeout time.Duration
	flag.IntVar(&controlVsockPort, "control-vsock-port", 0, "required AF_VSOCK port for the HTTP control service")
	flag.IntVar(&protocolVsockPort, "protocol-vsock-port", 0, "required AF_VSOCK port for the canonical gRPC guest protocol")
	// These two paths are image ABI constants: the init script mounts the
	// workspace disk and RAM-only runtime-private tmpfs at these exact paths.
	flag.StringVar(&workspace, "workspace", guestWorkspaceMountPath, "workspace mount path")
	flag.StringVar(&runtimePrivate, "runtime-private", guestRuntimePrivatePath, "runtime-private path for vsock-delivered secrets")
	flag.StringVar(&logPath, "log", "", "runtime log path to expose")
	flag.StringVar(&instanceID, "instance-id", "", "required immutable instance ID")
	flag.StringVar(&sandboxID, "sandbox-id", "", "required immutable sandbox ID")
	flag.Uint64Var(&sandboxGeneration, "sandbox-generation", 0, "required immutable sandbox generation")
	flag.StringVar(&guestBuildID, "guest-build-id", "", "required signed guest build ID")
	flag.StringVar(&imageManifestDigest, "image-manifest-digest", "", "required signed image manifest digest")
	flag.StringVar(&toolchainManifestDigest, "toolchain-manifest-digest", "", "required signed toolchain manifest digest")
	flag.DurationVar(&heartbeatInterval, "heartbeat-interval", 0, "required negotiated heartbeat interval")
	flag.BoolVar(&waitSecrets, "wait-secrets", false, "wait for runtime-private env.json before starting the runtime command")
	flag.DurationVar(&secretsTimeout, "secrets-timeout", 0, "required positive maximum wait when --wait-secrets is enabled")
	flag.Parse()

	if controlVsockPort < 1 || controlVsockPort > 65535 ||
		protocolVsockPort < 1 || protocolVsockPort > 65535 ||
		controlVsockPort == protocolVsockPort {
		slog.Error("guest control and protocol vsock ports must be explicit, distinct values from 1 through 65535")
		return 1
	}
	if waitSecrets && secretsTimeout <= 0 {
		slog.Error("--secrets-timeout must be positive when --wait-secrets is enabled")
		return 1
	}

	guestServer := microvmguest.Server{
		WorkspaceDir:      workspace,
		RuntimePrivateDir: runtimePrivate,
		InstanceID:        instanceID,
		SandboxID:         sandboxID,
		LogPath:           logPath,
		Freezer:           microvmguest.LinuxFreezer{},
	}
	controlServer := &http.Server{Handler: guestServer.Handler()}
	protocolService, err := microvmguest.NewProtocolService(guestServer, microvmguest.ProtocolIdentity{
		InstanceID:              instanceID,
		SandboxID:               sandboxID,
		SandboxGeneration:       sandboxGeneration,
		GuestBuildID:            guestBuildID,
		ImageManifestDigest:     imageManifestDigest,
		ToolchainManifestDigest: toolchainManifestDigest,
		HeartbeatInterval:       heartbeatInterval,
	})
	if err != nil {
		slog.Error("invalid guest protocol identity", "error", err)
		return 1
	}
	protocolServer := grpc.NewServer()
	guestv1.RegisterGuestAgentServer(protocolServer, protocolService)

	controlListener, err := listenSocket(controlVsockPort)
	if err != nil {
		slog.Error("failed to listen on guest control vsock port", "error", err)
		return 1
	}
	protocolListener, err := listenSocket(protocolVsockPort)
	if err != nil {
		err = errors.Join(err, controlListener.Close())
		slog.Error("failed to listen on guest protocol vsock port", "error", err)
		return 1
	}

	serviceErrors := make(chan error, 2)
	go func() {
		slog.Info("microVM guest control service listening", "addr", controlListener.Addr().String())
		if err := controlServer.Serve(controlListener); err != nil && err != http.ErrServerClosed {
			serviceErrors <- fmt.Errorf("guest control service: %w", err)
		}
	}()
	go func() {
		slog.Info("microVM canonical guest protocol listening", "addr", protocolListener.Addr().String())
		if err := protocolServer.Serve(protocolListener); err != nil {
			serviceErrors <- fmt.Errorf("canonical guest protocol service: %w", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown := func() error {
		var joined error
		grpcStopped := make(chan struct{})
		go func() {
			protocolServer.GracefulStop()
			close(grpcStopped)
		}()
		select {
		case <-grpcStopped:
		case <-time.After(guestShutdownGraceInterval):
			protocolServer.Stop()
			joined = errors.Join(joined, fmt.Errorf("canonical guest protocol graceful shutdown exceeded %s", guestShutdownGraceInterval))
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), guestShutdownGraceInterval)
		defer cancel()
		if err := controlServer.Shutdown(shutdownCtx); err != nil {
			joined = errors.Join(joined, fmt.Errorf("shutdown guest control service: %w", err))
		}
		if err := controlListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			joined = errors.Join(joined, fmt.Errorf("close guest control listener: %w", err))
		}
		if err := protocolListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			joined = errors.Join(joined, fmt.Errorf("close guest protocol listener: %w", err))
		}
		return joined
	}
	finish := func(code int) int {
		if err := shutdown(); err != nil {
			slog.Error("guest service shutdown failed", "error", err)
			return 1
		}
		return code
	}

	runtimeArgs := flag.Args()
	if len(runtimeArgs) == 0 {
		select {
		case <-ctx.Done():
			return finish(0)
		case err := <-serviceErrors:
			slog.Error("guest service failed", "error", err)
			return finish(1)
		}
	}

	runtimeEnv := os.Environ()
	if waitSecrets {
		env, err := waitRuntimeEnv(ctx, runtimePrivate, secretsTimeout)
		if err != nil {
			slog.Error("failed to load runtime secrets before launch", "error", err)
			return finish(1)
		}
		runtimeEnv = appendEnv(runtimeEnv, env)
	}
	runtimeExit := make(chan int, 1)
	go func() {
		runtimeExit <- runRuntimeCommand(ctx, runtimeArgs, runtimeEnv)
	}()
	select {
	case code := <-runtimeExit:
		return finish(code)
	case err := <-serviceErrors:
		slog.Error("guest service failed", "error", err)
		stop()
		<-runtimeExit
		return finish(1)
	case <-ctx.Done():
		code := <-runtimeExit
		return finish(code)
	}
}

func runRuntimeCommand(ctx context.Context, args []string, env []string) int {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		slog.Error("failed to start runtime command", "command", args[0], "error", err)
		return 1
	}
	slog.Info("started microVM runtime command", "pid", cmd.Process.Pid, "command", args[0])
	err := cmd.Wait()
	if err != nil {
		slog.Warn("microVM runtime command exited with error", "error", err)
		return processExitCode(err)
	}
	return 0
}

func waitRuntimeEnv(ctx context.Context, runtimePrivate string, timeout time.Duration) (map[string]string, error) {
	path := filepath.Join(runtimePrivateRoot(runtimePrivate), "env.json")
	waitCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var env map[string]string
			if err := json.Unmarshal(data, &env); err != nil {
				return nil, fmt.Errorf("decode %s: %w", path, err)
			}
			return env, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("wait for %s: %w", path, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func appendEnv(base []string, overlay map[string]string) []string {
	env := map[string]string{}
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	for key, value := range overlay {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		env[key] = value
	}
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

func processExitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Exited() {
				return status.ExitStatus()
			}
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
		}
	}
	return 1
}

func listenSocket(vsockPort int) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(vsockPort)}
	if err := unix.Bind(fd, sa); err != nil {
		return nil, errors.Join(fmt.Errorf("bind vsock port %d: %w", vsockPort, err), unix.Close(fd))
	}
	if err := unix.Listen(fd, 128); err != nil {
		return nil, errors.Join(fmt.Errorf("listen vsock port %d: %w", vsockPort, err), unix.Close(fd))
	}
	return &vsockListener{fd: fd, addr: vsockAddr{cid: unix.VMADDR_CID_ANY, port: uint32(vsockPort)}}, nil
}

type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }

func (a vsockAddr) String() string {
	return fmt.Sprintf("%d:%d", a.cid, a.port)
}

type vsockListener struct {
	fd     int
	addr   vsockAddr
	closed bool
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		fd, sa, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			if l.closed {
				return nil, net.ErrClosed
			}
			return nil, err
		}
		// Only the host control plane (CID 2) may use the control surface.
		// Reject connections originating inside the guest (vsock loopback /
		// local CID) so untrusted in-guest code cannot reach the control RPCs.
		vm, ok := sa.(*unix.SockaddrVM)
		if !ok || vm.CID != unix.VMADDR_CID_HOST {
			if err := unix.Close(fd); err != nil {
				return nil, fmt.Errorf("close rejected non-host vsock connection: %w", err)
			}
			continue
		}
		remote := vsockAddr{cid: vm.CID, port: vm.Port}
		return &vsockConn{fd: fd, local: l.addr, remote: remote}, nil
	}
}

func (l *vsockListener) Close() error {
	l.closed = true
	return unix.Close(l.fd)
}

func (l *vsockListener) Addr() net.Addr { return l.addr }

type vsockConn struct {
	fd     int
	local  vsockAddr
	remote vsockAddr
}

func (c *vsockConn) Read(p []byte) (int, error) {
	for {
		n, err := unix.Read(c.fd, p)
		if err == unix.EINTR {
			continue
		}
		if n < 0 {
			return 0, err
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return n, err
	}
}

func (c *vsockConn) Write(p []byte) (int, error) {
	for {
		n, err := unix.Write(c.fd, p)
		if err == unix.EINTR {
			continue
		}
		if n < 0 {
			return 0, err
		}
		return n, err
	}
}

func (c *vsockConn) Close() error { return unix.Close(c.fd) }

func (c *vsockConn) LocalAddr() net.Addr { return c.local }

func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

func (c *vsockConn) SetDeadline(deadline time.Time) error {
	return errors.Join(c.SetReadDeadline(deadline), c.SetWriteDeadline(deadline))
}

func (c *vsockConn) SetReadDeadline(deadline time.Time) error {
	return setSocketTimeout(c.fd, unix.SO_RCVTIMEO, deadline)
}

func (c *vsockConn) SetWriteDeadline(deadline time.Time) error {
	return setSocketTimeout(c.fd, unix.SO_SNDTIMEO, deadline)
}

func setSocketTimeout(fd, option int, deadline time.Time) error {
	timeout := unix.Timeval{}
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = time.Microsecond
		}
		timeout = unix.NsecToTimeval(remaining.Nanoseconds())
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, option, &timeout); err != nil {
		return fmt.Errorf("set socket deadline: %w", err)
	}
	return nil
}

func runtimePrivateRoot(root string) string {
	return filepath.Clean(strings.TrimSpace(root))
}
