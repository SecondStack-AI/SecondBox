package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-manager/internal/microvm"
)

func main() {
	syscall.Umask(0077)
	healthcheck := flag.Bool("healthcheck", false, "probe the running launcher protocol and exit")
	socketPath := flag.String("socket", os.Getenv("AGENT_MANAGER_MICROVM_LAUNCHER_SOCKET"), "launcher unix socket used by -healthcheck")
	healthcheckTimeout := flag.Duration("healthcheck-timeout", 10*time.Second, "maximum time for -healthcheck")
	flag.Parse()
	if err := validateExecutionIdentity(*healthcheck, os.Geteuid()); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	if *healthcheck {
		ctx, cancel := context.WithTimeout(context.Background(), *healthcheckTimeout)
		defer cancel()
		if err := microvm.WaitForPrivilegedLauncher(ctx, *socketPath, 100*time.Millisecond); err != nil {
			slog.Error("privileged launcher readiness check failed", "error", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := microvm.LoadPrivilegedLauncherConfigFromEnv()
	if err != nil {
		slog.Error("load privileged launcher config", "error", err)
		os.Exit(1)
	}
	server, err := microvm.NewPrivilegedLauncherServer(cfg)
	if err != nil {
		slog.Error("validate privileged launcher config", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx); err != nil {
		slog.Error("privileged launcher stopped", "error", err)
		os.Exit(1)
	}
}

func validateExecutionIdentity(healthcheck bool, effectiveUID int) error {
	if !healthcheck && effectiveUID != 0 {
		return fmt.Errorf("agent-manager-vmlauncher server must run as root")
	}
	return nil
}
