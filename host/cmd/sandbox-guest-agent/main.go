package main

import (
	"context"
	"encoding/json"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"secondstack/sandbox-host/internal/guest"

	"golang.org/x/sys/unix"
)

func main() {
	var listen string
	var vsockPort int
	var workspace string
	var runtimePrivate string
	var logPath string
	var waitSecrets bool
	var secretsTimeout time.Duration
	flag.StringVar(&listen, "listen", envOrDefault("AGENT_MANAGER_GUEST_LISTEN", "127.0.0.1:1024"), "TCP listen address; ignored when --vsock-port is set")
	flag.IntVar(&vsockPort, "vsock-port", envIntOrDefault("AGENT_MANAGER_GUEST_VSOCK_PORT", 0), "AF_VSOCK listen port")
	flag.StringVar(&workspace, "workspace", envOrDefault("AGENT_MANAGER_WORKSPACE_DIR", "/workspace"), "workspace mount path")
	flag.StringVar(&runtimePrivate, "runtime-private", envOrDefault("AGENT_MANAGER_RUNTIME_PRIVATE_DIR", "/runtime-private"), "runtime-private path for vsock-delivered secrets")
	flag.StringVar(&logPath, "log", os.Getenv("AGENT_MANAGER_GUEST_LOG_PATH"), "runtime log path to expose")
	flag.BoolVar(&waitSecrets, "wait-secrets", envBoolOrDefault("AGENT_MANAGER_WAIT_SECRETS", false), "wait for runtime-private env.json before starting the runtime command")
	flag.DurationVar(&secretsTimeout, "secrets-timeout", envDurationOrDefault("AGENT_MANAGER_SECRETS_TIMEOUT", 30*time.Second), "maximum time to wait for runtime-private env.json")
	flag.Parse()

	srv := &http.Server{
		Handler: microvmguest.Server{
			WorkspaceDir:      workspace,
			RuntimePrivateDir: runtimePrivate,
			InstanceID:        os.Getenv("AGENT_MANAGER_INSTANCE_ID"),
			AgentID:           os.Getenv("AGENT_ID"),
			LogPath:           logPath,
			Freezer:           microvmguest.LinuxFreezer{},
		}.Handler(),
	}

	ln, err := listenSocket(listen, vsockPort)
	if err != nil {
		slog.Error("failed to listen", "error", err)
		os.Exit(1)
	}
	defer ln.Close()

	go func() {
		slog.Info("microVM guest agent listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("guest agent server error", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtimeArgs := flag.Args()
	if len(runtimeArgs) == 0 {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
		return
	}

	if waitSecrets {
		env, err := waitRuntimeEnv(ctx, runtimePrivate, secretsTimeout)
		if err != nil {
			slog.Error("failed to load runtime secrets before launch", "error", err)
			_ = srv.Shutdown(context.Background())
			os.Exit(1)
		}
		runtimeArgsEnv := appendEnv(os.Environ(), env)
		os.Exit(runRuntimeCommand(ctx, srv, runtimeArgs, runtimeArgsEnv))
	}
	os.Exit(runRuntimeCommand(ctx, srv, runtimeArgs, os.Environ()))
}

func runRuntimeCommand(ctx context.Context, srv *http.Server, args []string, env []string) int {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		slog.Error("failed to start runtime command", "command", args[0], "error", err)
		_ = srv.Shutdown(context.Background())
		return 1
	}
	slog.Info("started microVM runtime command", "pid", cmd.Process.Pid, "command", args[0])
	err := cmd.Wait()
	_ = srv.Shutdown(context.Background())
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

func listenSocket(listen string, vsockPort int) (net.Listener, error) {
	if vsockPort <= 0 {
		return net.Listen("tcp", listen)
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(vsockPort)}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", vsockPort, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen vsock port %d: %w", vsockPort, err)
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
			_ = unix.Close(fd)
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

func (c *vsockConn) SetDeadline(time.Time) error { return nil }

func (c *vsockConn) SetReadDeadline(time.Time) error { return nil }

func (c *vsockConn) SetWriteDeadline(time.Time) error { return nil }

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envIntOrDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envBoolOrDefault(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

func runtimePrivateRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "/runtime-private"
	}
	return filepath.Clean(root)
}
