package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
)

func main() {
	syscall.Umask(0o077)
	if err := run(os.Args[1:]); err != nil {
		slog.Error("SecondBox runner stopped", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string) (runErr error) {
	flags := flag.NewFlagSet("secondbox-runner", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	healthcheck := flags.Bool("healthcheck", false, "open the authenticated runner protocol stream and exit")
	healthcheckTimeout := flags.Duration("healthcheck-timeout", 10*time.Second, "maximum runner protocol probe time")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if err := validateRunnerExecutionIdentity(*healthcheck, os.Geteuid()); err != nil {
		return err
	}

	protocolConfig, connectorConfig, err := runnercontrol.LoadRunnerProtocolConfigFromEnv()
	if err != nil {
		return fmt.Errorf("load SecondBox runner protocol config: %w", err)
	}
	connector, err := runnercontrol.NewGRPCConnector(connectorConfig)
	if err != nil {
		return fmt.Errorf("load SecondBox runner mTLS credentials: %w", err)
	}
	if *healthcheck {
		ctx, cancel := context.WithTimeout(context.Background(), *healthcheckTimeout)
		defer cancel()
		if _, err := connector.Connect(ctx); err != nil {
			return fmt.Errorf("SecondBox runner protocol readiness failed: %w", err)
		}
		return connector.Close()
	}

	closeLog, err := configureRunnerLogging(os.Getenv("SECONDBOX_RUNNER_LOG_PATH"))
	if err != nil {
		return fmt.Errorf("initialize SecondBox runner logging: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, closeLog())
	}()
	firecrackerConfig, err := firecracker.LoadRunnerFirecrackerConfigFromEnv()
	if err != nil {
		return fmt.Errorf("load SecondBox Firecracker config: %w", err)
	}
	manager, err := firecracker.New(firecrackerConfig)
	if err != nil {
		return fmt.Errorf("create SecondBox Firecracker backend: %w", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		return fmt.Errorf("start SecondBox Firecracker backend: %w", err)
	}
	backend, err := firecracker.NewAssignmentBackend(manager)
	if err != nil {
		return fmt.Errorf("create SecondBox assignment backend: %w", err)
	}
	service, err := runnercontrol.NewRunnerProtocolService(protocolConfig, backend, connector)
	if err != nil {
		return fmt.Errorf("create SecondBox runner composition root: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runErr = service.Run(ctx)
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	shutdownErr := manager.Shutdown(shutdownContext)
	if runErr != nil && ctx.Err() == nil {
		runErr = fmt.Errorf("SecondBox runner protocol stopped: %w", runErr)
	} else {
		runErr = nil
	}
	return errors.Join(runErr, shutdownErr)
}

func configureRunnerLogging(path string) (func() error, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("SECONDBOX_RUNNER_LOG_PATH must be an absolute path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), nil)))
	return file.Close, nil
}

func validateRunnerExecutionIdentity(healthcheck bool, effectiveUID int) error {
	if !healthcheck && effectiveUID != 0 {
		return fmt.Errorf("SecondBox runner must run as root to own Firecracker host resources")
	}
	return nil
}
