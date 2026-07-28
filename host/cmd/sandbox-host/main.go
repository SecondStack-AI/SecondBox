package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"secondstack/sandbox-host/internal/firecracker"
)

func main() {
	syscall.Umask(0077)
	if len(os.Args) > 1 && os.Args[1] == "harness-exec" {
		if err := firecracker.RunHarnessExec(os.Args[2:]); err != nil {
			slog.Error("run isolated harness command", "error", err)
			os.Exit(1)
		}
		return
	}
	healthcheck := flag.Bool("healthcheck", false, "probe the running launcher protocol and exit")
	socketPath := flag.String("socket", os.Getenv("SANDBOX_HOST_LAUNCHER_SOCKET"), "launcher unix socket used by -healthcheck")
	healthcheckTimeout := flag.Duration("healthcheck-timeout", 10*time.Second, "maximum time for -healthcheck")
	flag.Parse()
	if err := validateExecutionIdentity(*healthcheck, os.Geteuid()); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	if *healthcheck {
		ctx, cancel := context.WithTimeout(context.Background(), *healthcheckTimeout)
		defer cancel()
		if err := firecracker.WaitForPrivilegedLauncher(ctx, *socketPath, 100*time.Millisecond); err != nil {
			slog.Error("privileged launcher readiness check failed", "error", err)
			os.Exit(1)
		}
		if err := probeSandboxHostHTTP(ctx, os.Getenv("SANDBOX_HOST_URL"), os.Getenv("SANDBOX_HOST_TOKEN")); err != nil {
			slog.Error("Sandbox Host HTTP readiness check failed", "error", err)
			os.Exit(1)
		}
		return
	}
	closeLog, err := configureSandboxHostLogging(os.Getenv("SANDBOX_HOST_LOG_PATH"))
	if err != nil {
		slog.Error("initialize Sandbox Host logging", "error", err)
		os.Exit(1)
	}
	defer closeLog()
	cfg, err := firecracker.LoadPrivilegedLauncherConfigFromEnv()
	if err != nil {
		slog.Error("load privileged launcher config", "error", err)
		os.Exit(1)
	}
	server, err := firecracker.NewPrivilegedLauncherServer(cfg)
	if err != nil {
		slog.Error("validate privileged launcher config", "error", err)
		os.Exit(1)
	}
	managerConfig, err := firecracker.SandboxHostManagerConfig(cfg)
	if err != nil {
		slog.Error("load Sandbox Host compute config", "error", err)
		os.Exit(1)
	}
	manager, err := firecracker.New(managerConfig)
	if err != nil {
		slog.Error("create Sandbox Host compute backend", "error", err)
		os.Exit(1)
	}
	integrationTimeoutSeconds, err := strconv.Atoi(os.Getenv("SANDBOX_HOST_INTEGRATION_HTTP_TIMEOUT_SECONDS"))
	if err != nil || integrationTimeoutSeconds < 1 {
		slog.Error("SANDBOX_HOST_INTEGRATION_HTTP_TIMEOUT_SECONDS must be a positive integer")
		os.Exit(1)
	}
	sourceBindings, err := firecracker.NewIntegrationSourceBindingClient(
		os.Getenv("INTEGRATION_SERVICE_INTERNAL_URL"),
		os.Getenv("INTEGRATION_SERVICE_INTERNAL_TOKEN"),
		&http.Client{Timeout: time.Duration(integrationTimeoutSeconds) * time.Second},
	)
	if err != nil {
		slog.Error("configure Integration source-binding client", "error", err)
		os.Exit(1)
	}
	manager.SetSourceBindingRegistrar(sourceBindings)
	runtime, err := firecracker.NewFirecrackerSandboxHostRuntime(manager, cfg.StateRoot)
	if err != nil {
		slog.Error("create Sandbox Host runtime", "error", err)
		os.Exit(1)
	}
	maxFileTransferBytes, err := firecracker.SandboxHostFileTransferMaxBytes()
	if err != nil {
		slog.Error("load Sandbox Host file transfer limit", "error", err)
		os.Exit(1)
	}
	httpHandler, err := firecracker.NewSandboxHostHTTPHandler(runtime, os.Getenv("SANDBOX_HOST_TOKEN"), int64(maxFileTransferBytes))
	if err != nil {
		slog.Error("create Sandbox Host HTTP contract", "error", err)
		os.Exit(1)
	}
	listenAddress := os.Getenv("SANDBOX_HOST_LISTEN_ADDR")
	if listenAddress == "" {
		slog.Error("SANDBOX_HOST_LISTEN_ADDR is required")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	httpServer := &http.Server{
		Addr: listenAddress, Handler: httpHandler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	failures := make(chan error, 2)
	go func() { failures <- server.Serve(ctx) }()
	go func() { failures <- httpServer.ListenAndServe() }()
	select {
	case err := <-failures:
		slog.Error("Sandbox Host boundary stopped", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			slog.Error("shutdown Sandbox Host HTTP contract", "error", err)
			os.Exit(1)
		}
	}
}

func configureSandboxHostLogging(path string) (func(), error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("SANDBOX_HOST_LOG_PATH must be an absolute path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), nil)))
	return func() { _ = file.Close() }, nil
}

func probeSandboxHostHTTP(ctx context.Context, rawURL, token string) error {
	rawURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if rawURL == "" || strings.TrimSpace(token) == "" {
		return fmt.Errorf("SANDBOX_HOST_URL and SANDBOX_HOST_TOKEN are required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL+"/v1/ready", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Sandbox Host readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}

func validateExecutionIdentity(healthcheck bool, effectiveUID int) error {
	if !healthcheck && effectiveUID != 0 {
		return fmt.Errorf("Sandbox Host server must run as root")
	}
	return nil
}
