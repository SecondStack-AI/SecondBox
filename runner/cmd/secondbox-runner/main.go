package main

import (
	"context"
	"encoding/json"
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
	"github.com/SecondStack-AI/SecondBox/runner/internal/jailersupervisor"
	"github.com/SecondStack-AI/SecondBox/runner/internal/microsandbox"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtimeconfig"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

var (
	releaseVersion = "0.0.0-development"
	sourceCommit   = "development"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"version": releaseVersion, "sourceCommit": sourceCommit}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if handled, err := jailersupervisor.RunInvocation(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "SecondBox jailer supervisor failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
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

	composition, err := runtimeconfig.LoadFromEnvironment(*healthcheck)
	if err != nil {
		return err
	}
	protocolConfig, connector := composition.Protocol, composition.Connector
	if *healthcheck {
		ctx, cancel := context.WithTimeout(context.Background(), *healthcheckTimeout)
		defer cancel()
		if _, err := connector.Connect(ctx); err != nil {
			return fmt.Errorf("SecondBox runner protocol readiness failed: %w", err)
		}
		return connector.Close()
	}

	closeLog, err := configureRunnerLogging(composition.RunnerLogPath)
	if err != nil {
		return fmt.Errorf("initialize SecondBox runner logging: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, closeLog())
	}()
	workspaceStore, err := workspacestore.New(
		context.Background(),
		workspacestore.Config{
			Root:                         composition.WorkspaceRoot,
			TemplateCapacityBytes:        composition.WorkspaceTemplateCapacityBytes,
			MicrosandboxHelperExecutable: strings.TrimSpace(os.Getenv("SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE")),
		},
	)
	if err != nil {
		return fmt.Errorf("initialize SecondBox runner WorkspaceStore: %w", err)
	}
	var backend runnercontrol.AssignmentBackend
	var shutdownBackend func(context.Context) error
	switch composition.BackendKind {
	case "firecracker":
		manager, createErr := firecracker.New(composition.Firecracker)
		if createErr != nil {
			return fmt.Errorf("create SecondBox Firecracker backend: %w", createErr)
		}
		if createErr = manager.SetWorkspaceStore(workspaceStore); createErr != nil {
			return fmt.Errorf("bind SecondBox runner WorkspaceStore: %w", createErr)
		}
		if createErr = manager.Start(context.Background()); createErr != nil {
			return fmt.Errorf("start SecondBox Firecracker backend: %w", createErr)
		}
		backend, createErr = firecracker.NewAssignmentBackend(manager)
		if createErr != nil {
			return fmt.Errorf("create SecondBox Firecracker assignment backend: %w", createErr)
		}
		shutdownBackend = manager.Shutdown
	case "microsandbox":
		settings := composition.Microsandbox
		microsandboxBackend, createErr := microsandbox.NewAssignmentBackend(microsandbox.Config{
			HelperExecutable:      settings.HelperExecutable,
			LibkrunfwPath:         settings.LibkrunfwPath,
			AgentdPath:            settings.AgentdPath,
			FlatRootPath:          settings.FlatRootPath,
			MaterializationPath:   settings.MaterializationPath,
			MaterializationDigest: settings.MaterializationDigest,
			MaximumVCPUs:          settings.MaximumVCPUs,
			MaximumMemoryBytes:    settings.MaximumMemoryBytes,
			MaximumDiskBytes:      settings.MaximumDiskBytes,
			MaximumInstances:      settings.MaximumInstances,
			MaximumOperations:     settings.MaximumOperations,
			WorkspaceStore:        workspaceStore,
		})
		if createErr != nil {
			return fmt.Errorf("create SecondBox Microsandbox assignment backend: %w", createErr)
		}
		backend = microsandboxBackend
		shutdownBackend = microsandboxBackend.Shutdown
	default:
		return fmt.Errorf("SecondBox runner compute backend selection is invalid")
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
	shutdownErr := shutdownBackend(shutdownContext)
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
		return fmt.Errorf("SecondBox runner must run as root to own local compute host resources")
	}
	return nil
}
