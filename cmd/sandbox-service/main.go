package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"secondstack/sandbox-service/internal/agentservice"
	"secondstack/sandbox-service/internal/api"
	"secondstack/sandbox-service/internal/compute"
	"secondstack/sandbox-service/internal/config"
	"secondstack/sandbox-service/internal/service"
	"secondstack/sandbox-service/internal/store"
)

func main() {
	logger, closeLog, err := newProcessLogger(os.Getenv("SANDBOX_SERVICE_LOG_PATH"))
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("initialize Sandbox Service logging", "error", err)
		os.Exit(1)
	}
	runErr := run(logger)
	closeErr := closeLog()
	if err := errors.Join(runErr, closeErr); err != nil {
		logger.Error("Sandbox Service stopped", "error", err)
		os.Exit(1)
	}
}

func newProcessLogger(path string) (*slog.Logger, func() error, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("SANDBOX_SERVICE_LOG_PATH must be an absolute path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), nil))
	return logger, file.Close, nil
}

func run(logger *slog.Logger) error {
	processConfig, err := config.FromEnvironment()
	if err != nil {
		return err
	}
	processContext, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	environmentStore, err := store.NewPostgresEnvironmentStore(processContext, processConfig.DatabaseURL)
	if err != nil {
		return err
	}
	defer environmentStore.Close()

	computeBackend, err := compute.NewSandboxHostClient(
		processConfig.SandboxHostURL,
		processConfig.SandboxHostToken,
		&http.Client{Timeout: processConfig.HTTPTimeout},
	)
	if err != nil {
		return err
	}
	executionRevoker, err := agentservice.NewExecutionRevoker(
		processConfig.AgentServiceURL,
		processConfig.AgentServiceToken,
		&http.Client{Timeout: processConfig.HTTPTimeout},
	)
	if err != nil {
		return err
	}
	coordinator, err := service.NewSandboxService(service.Config{
		Store: environmentStore, Compute: computeBackend, LeaseTTL: processConfig.LeaseTTL,
		ExecutionRevoker:     executionRevoker,
		MaxFileTransferBytes: processConfig.FileTransferMaxBytes,
		Now:                  time.Now, NewID: service.NewOpaqueID,
	})
	if err != nil {
		return err
	}
	httpHandler, err := api.NewHandler(api.HandlerConfig{
		Service: coordinator, InternalToken: processConfig.InternalToken, Logger: logger,
		MaxFileTransferBytes: processConfig.FileTransferMaxBytes,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              processConfig.ListenAddress,
		Handler:           httpHandler,
		ReadHeaderTimeout: processConfig.HTTPTimeout,
		ReadTimeout:       processConfig.HTTPTimeout,
		WriteTimeout:      processConfig.HTTPTimeout,
		IdleTimeout:       processConfig.HTTPTimeout,
	}
	errs := make(chan error, 2)
	go runLifecycleReconciler(processContext, coordinator, processConfig.ReconcileInterval, processConfig.ReconcileBatch, errs)
	go func() {
		logger.Info("Sandbox Service listening", "address", processConfig.ListenAddress)
		errs <- server.ListenAndServe()
	}()

	select {
	case <-processContext.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			cancel()
			return err
		}
	}
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), processConfig.HTTPTimeout)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return err
	}
	return nil
}

func runLifecycleReconciler(
	ctx context.Context,
	coordinator *service.SandboxService,
	interval time.Duration,
	batchSize int,
	errs chan<- error,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := coordinator.ReconcileLifecycle(ctx, batchSize); err != nil {
				errs <- err
				return
			}
		}
	}
}
