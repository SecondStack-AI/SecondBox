package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/config"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	processConfig, err := config.FromEnvironment()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("SecondBox configuration failed", "error", err)
		os.Exit(1)
	}
	logger, closeLog, err := newProcessLogger(processConfig.LogPath)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("SecondBox logging initialization failed", "error", err)
		os.Exit(1)
	}
	runErr := run(processConfig, logger)
	closeErr := closeLog()
	if err := errors.Join(runErr, closeErr); err != nil {
		logger.Error("SecondBox stopped", "error", err)
		os.Exit(1)
	}
}

func newProcessLogger(path string) (*slog.Logger, func() error, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, errors.New("SecondBox log path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, fmt.Errorf("SecondBox log directory creation failed: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox log file open failed: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), nil))
	return logger, file.Close, nil
}

func run(processConfig config.Config, logger *slog.Logger) error {
	processContext, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := postgresmigrations.Apply(processContext, processConfig.DatabaseURL); err != nil {
		return err
	}
	controlPlaneStore, err := store.NewPostgresControlPlaneStore(processContext, processConfig.DatabaseURL)
	if err != nil {
		return err
	}
	defer controlPlaneStore.Close()
	dataPlaneRelay, err := runnercontrol.NewPostgresFrameRelay(
		processContext,
		runnercontrol.PostgresFrameRelayConfig{
			DatabaseURL:         processConfig.DatabaseURL,
			ClaimDuration:       processConfig.DataPlaneClaimDuration,
			Retention:           processConfig.DataPlaneRetention,
			MaximumFrameBytes:   processConfig.DataPlaneMaximumFrameBytes,
			MaximumSessionBytes: processConfig.DataPlaneMaximumSessionBytes,
		},
	)
	if err != nil {
		return err
	}
	defer dataPlaneRelay.Close()
	artifactObjects, err := objectstore.NewS3Store(processContext, objectstore.S3Config{
		Endpoint: processConfig.ObjectStoreEndpoint, Region: processConfig.ObjectStoreRegion,
		Bucket: processConfig.ObjectStoreBucket, AccessKeyID: processConfig.ObjectStoreAccessKeyID,
		SecretAccessKey:  processConfig.ObjectStoreSecretAccessKey,
		UsePathStyle:     processConfig.ObjectStoreUsePathStyle,
		RetryMaxAttempts: processConfig.ObjectStoreRetryMaxAttempts,
		HTTPTimeout:      processConfig.ObjectStoreHTTPTimeout,
		TempDirectory:    processConfig.ObjectStoreTempDirectory,
		MaxObjectBytes:   processConfig.ObjectStoreMaxObjectBytes,
	})
	if err != nil {
		return err
	}
	builtInProfiles, err := service.BuildBuiltInProfiles(service.BuiltInProfileBindings{
		AgentCompartment: service.BuiltInProfileBinding{
			Pool:                  processConfig.AgentCompartmentProfile.Pool,
			RuntimeBundleDigest:   processConfig.AgentCompartmentProfile.RuntimeBundleDigest,
			ToolchainBundleDigest: processConfig.AgentCompartmentProfile.ToolchainBundleDigest,
		},
		CodingEnvironment: service.BuiltInProfileBinding{
			Pool:                  processConfig.CodingEnvironmentProfile.Pool,
			RuntimeBundleDigest:   processConfig.CodingEnvironmentProfile.RuntimeBundleDigest,
			ToolchainBundleDigest: processConfig.CodingEnvironmentProfile.ToolchainBundleDigest,
		},
	})
	if err != nil {
		return err
	}
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store:               controlPlaneStore,
		PlatformToken:       processConfig.PlatformToken,
		BuiltInProfiles:     builtInProfiles,
		DefaultSubjectQuota: processConfig.DefaultSubjectQuota,
		Now:                 service.SystemClock, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		ArtifactObjectStore:   artifactObjects,
		DataPlaneRelay:        dataPlaneRelay, DataPlanePollInterval: processConfig.DataPlanePollInterval,
		PortSessionRelay: dataPlaneRelay, PublicBaseURL: processConfig.PublicBaseURL,
	})
	if err != nil {
		return err
	}
	runnerCA, err := runnercontrol.LoadCertificateAuthority(processConfig.RunnerCACertificatePath)
	if err != nil {
		return err
	}
	runnerCredentialAuthority, err := runnercontrol.NewCredentialAuthority(
		runnercontrol.CredentialAuthorityConfig{
			Credential: processConfig.RunnerCredential, CACertificate: runnerCA,
		},
	)
	if err != nil {
		return err
	}
	runnerStateStore, err := runnercontrol.NewPostgresStateStore(
		processContext, processConfig.DatabaseURL,
	)
	if err != nil {
		return err
	}
	defer runnerStateStore.Close()
	assignmentScheduler, err := scheduler.NewPostgresStore(
		processContext,
		scheduler.PostgresStoreConfig{
			DatabaseURL: processConfig.DatabaseURL,
			Now:         service.SystemClock,
		},
	)
	if err != nil {
		return err
	}
	defer assignmentScheduler.Close()
	assignmentReconcileStore, err := reconcile.NewPostgresStore(
		processContext, processConfig.DatabaseURL,
	)
	if err != nil {
		return err
	}
	defer assignmentReconcileStore.Close()
	signedAssetCatalog, err := lifecycle.LoadFileAssetCatalog(processConfig.SignedAssetCatalogPath)
	if err != nil {
		return err
	}
	lifecycleEffects, err := lifecycle.NewPostgresEffectBroker(
		processContext,
		processConfig.DatabaseURL,
		assignmentScheduler,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: processConfig.AssignmentClaimDuration,
			AssignmentDeadline:      processConfig.AssignmentDeadline,
			HeartbeatTimeout:        processConfig.RunnerHeartbeatTimeout,
			RetryLimit:              processConfig.AssignmentRetryLimit,
			SerializationRetryLimit: processConfig.SchedulerSerializationRetryLimit,
			AssetCatalog:            signedAssetCatalog,
			SessionCanceller:        dataPlaneRelay,
			NewID:                   service.NewOpaqueID,
			NewFencingToken:         newLifecycleFencingToken,
		},
	)
	if err != nil {
		return err
	}
	defer lifecycleEffects.Close()
	runnerServerCertificate, err := tls.LoadX509KeyPair(
		processConfig.RunnerServerCertificatePath,
		processConfig.RunnerServerPrivateKeyPath,
	)
	if err != nil {
		return fmt.Errorf("SecondBox runner control server credential: %w", err)
	}
	runnerTLSConfig, err := runnerCredentialAuthority.ServerTLSConfig(runnerServerCertificate)
	if err != nil {
		return err
	}
	runnerFeatures, err := configuredRunnerFeatures(processConfig.RunnerEnabledFeatures)
	if err != nil {
		return err
	}
	workWakeups := worknotify.NewHub()
	runnerControlServer, err := runnercontrol.NewServer(runnercontrol.ServerConfig{
		CredentialVerifier: runnerCredentialAuthority, StateStore: runnerStateStore,
		SupportedVersions: runnercontrol.VersionRange{
			Minimum: processConfig.RunnerProtocolMinimum,
			Maximum: processConfig.RunnerProtocolMaximum,
		},
		EnabledFeatures:     runnerFeatures,
		HeartbeatInterval:   processConfig.RunnerHeartbeatInterval,
		CommandPollInterval: processConfig.RunnerCommandPollInterval,
		CommandBatchSize:    processConfig.RunnerCommandDeliveryBatchSize,
		EventBatchSize:      processConfig.RunnerEventPersistenceBatchSize,
		EventBatchWait:      processConfig.RunnerEventPersistenceBatchWait,
		WorkWakeups:         workWakeups,
		FrameRelay:          dataPlaneRelay,
		Now:                 service.SystemClock,
		NewConnectionID:     func() string { return service.NewOpaqueID("rconn") },
	})
	if err != nil {
		return err
	}
	runnerListener, err := net.Listen("tcp", processConfig.RunnerListenAddress)
	if err != nil {
		return fmt.Errorf("SecondBox runner control listener: %w", err)
	}
	defer runnerListener.Close()
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(runnerTLSConfig)))
	runnerv1.RegisterRunnerControlServer(grpcServer, runnerControlServer)
	httpHandler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, Logger: logger,
		PlatformToken:             processConfig.PlatformToken,
		ApplicationAuthorities:    applicationAuthorities(processConfig.ApplicationAuthorities),
		MaximumDataPlaneBodyBytes: processConfig.DataPlaneMaximumSessionBytes,
	})
	if err != nil {
		return err
	}
	workListener, err := worknotify.NewPostgresListener(
		processContext,
		processConfig.DatabaseURL,
		workWakeups,
	)
	if err != nil {
		return err
	}
	lifecycleWakeups, cancelLifecycleWakeups := workWakeups.Subscribe(
		worknotify.KindLifecycle,
		"",
	)
	defer cancelLifecycleWakeups()
	assignmentWakeups, cancelAssignmentWakeups := workWakeups.Subscribe(
		worknotify.KindAssignment,
		"",
	)
	defer cancelAssignmentWakeups()
	server := &http.Server{
		Addr: processConfig.ListenAddress, Handler: httpHandler,
		ReadHeaderTimeout: processConfig.HTTPTimeout, ReadTimeout: processConfig.HTTPTimeout,
		WriteTimeout: processConfig.HTTPTimeout, IdleTimeout: processConfig.HTTPTimeout,
	}
	serverErrors := make(chan error, 1)
	runnerServerErrors := make(chan error, 1)
	lifecycleErrors := make(chan error, 1)
	assignmentErrors := make(chan error, 1)
	snapshotRetentionErrors := make(chan error, 1)
	dataPlaneErrors := make(chan error, 1)
	workListenerErrors := make(chan error, 1)
	go func() {
		logger.Info("SecondBox listening", "address", processConfig.ListenAddress)
		serverErrors <- server.ListenAndServe()
	}()
	go func() {
		logger.Info("SecondBox runner control listening", "address", processConfig.RunnerListenAddress)
		runnerServerErrors <- grpcServer.Serve(runnerListener)
	}()
	go func() {
		lifecycleErrors <- runLifecycleReconciler(
			processContext,
			lifecycle.Reconciler{
				Store: controlPlaneStore, Effects: lifecycleEffects,
				WorkerID:      service.NewOpaqueID("lifecycle-worker"),
				ClaimDuration: processConfig.LifecycleReconcileClaimDuration,
				PollInterval:  processConfig.LifecycleReconcilePollInterval,
			},
			lifecycleWakeups,
		)
	}()
	go func() {
		assignmentErrors <- runAssignmentReconciler(
			processContext,
			reconcile.AssignmentWorker{
				Store:            assignmentReconcileStore,
				WorkerID:         service.NewOpaqueID("assignment-worker"),
				ClaimDuration:    processConfig.AssignmentClaimDuration,
				PollInterval:     processConfig.LifecycleReconcilePollInterval,
				CommandDeadline:  processConfig.AssignmentDeadline,
				HeartbeatTimeout: processConfig.RunnerHeartbeatTimeout,
				NewCommandID:     service.NewOpaqueID,
			},
			assignmentWakeups,
		)
	}()
	go func() {
		snapshotRetentionErrors <- runSnapshotRetentionWorker(
			processContext,
			lifecycle.SnapshotRetentionWorker{
				Store:           controlPlaneStore,
				PollInterval:    processConfig.LifecycleReconcilePollInterval,
				NewID:           service.NewOpaqueID,
				NewFencingToken: newLifecycleFencingToken,
			},
		)
	}()
	go func() {
		dataPlaneErrors <- runDataPlaneSweeper(
			processContext, dataPlaneRelay, processConfig.DataPlanePollInterval,
		)
	}()
	go func() {
		workListenerErrors <- workListener.Run(processContext)
	}()
	var serveErr error
	lifecycleExited := false
	assignmentExited := false
	snapshotRetentionExited := false
	dataPlaneExited := false
	workListenerExited := false
	select {
	case <-processContext.Done():
	case httpServeErr := <-serverErrors:
		if !errors.Is(httpServeErr, http.ErrServerClosed) {
			serveErr = fmt.Errorf("SecondBox HTTP server failed: %w", httpServeErr)
		}
	case runnerServeErr := <-runnerServerErrors:
		if !errors.Is(runnerServeErr, grpc.ErrServerStopped) {
			serveErr = fmt.Errorf("SecondBox runner control server failed: %w", runnerServeErr)
		}
	case lifecycleErr := <-lifecycleErrors:
		lifecycleExited = true
		if lifecycleErr != nil {
			serveErr = lifecycleErr
		} else if processContext.Err() == nil {
			serveErr = errors.New("SecondBox lifecycle reconciler stopped unexpectedly")
		}
	case assignmentErr := <-assignmentErrors:
		assignmentExited = true
		if assignmentErr != nil {
			serveErr = assignmentErr
		} else if processContext.Err() == nil {
			serveErr = errors.New("SecondBox Assignment reconciler stopped unexpectedly")
		}
	case snapshotRetentionErr := <-snapshotRetentionErrors:
		snapshotRetentionExited = true
		if snapshotRetentionErr != nil {
			serveErr = snapshotRetentionErr
		} else if processContext.Err() == nil {
			serveErr = errors.New("SecondBox Snapshot retention worker stopped unexpectedly")
		}
	case dataPlaneErr := <-dataPlaneErrors:
		dataPlaneExited = true
		if dataPlaneErr != nil {
			serveErr = dataPlaneErr
		} else if processContext.Err() == nil {
			serveErr = errors.New("SecondBox data-plane sweeper stopped unexpectedly")
		}
	case workListenerErr := <-workListenerErrors:
		workListenerExited = true
		if workListenerErr != nil {
			serveErr = workListenerErr
		} else if processContext.Err() == nil {
			serveErr = errors.New("SecondBox PostgreSQL work listener stopped unexpectedly")
		}
	}
	cancel()
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), processConfig.HTTPTimeout)
	defer shutdownCancel()
	httpShutdownErr := server.Shutdown(shutdownContext)
	grpcShutdownErr := stopGRPCServer(shutdownContext, grpcServer)
	var lifecycleShutdownErr error
	if !lifecycleExited {
		select {
		case lifecycleShutdownErr = <-lifecycleErrors:
		case <-shutdownContext.Done():
			lifecycleShutdownErr = fmt.Errorf("SecondBox lifecycle reconciler shutdown: %w", shutdownContext.Err())
		}
	}
	var assignmentShutdownErr error
	if !assignmentExited {
		select {
		case assignmentShutdownErr = <-assignmentErrors:
		case <-shutdownContext.Done():
			assignmentShutdownErr = fmt.Errorf(
				"SecondBox Assignment reconciler shutdown: %w", shutdownContext.Err(),
			)
		}
	}
	var dataPlaneShutdownErr error
	var snapshotRetentionShutdownErr error
	if !snapshotRetentionExited {
		select {
		case snapshotRetentionShutdownErr = <-snapshotRetentionErrors:
		case <-shutdownContext.Done():
			snapshotRetentionShutdownErr = fmt.Errorf(
				"SecondBox Snapshot retention worker shutdown: %w",
				shutdownContext.Err(),
			)
		}
	}
	if !dataPlaneExited {
		select {
		case dataPlaneShutdownErr = <-dataPlaneErrors:
		case <-shutdownContext.Done():
			dataPlaneShutdownErr = fmt.Errorf("SecondBox data-plane sweeper shutdown: %w", shutdownContext.Err())
		}
	}
	var workListenerShutdownErr error
	if !workListenerExited {
		select {
		case workListenerShutdownErr = <-workListenerErrors:
		case <-shutdownContext.Done():
			workListenerShutdownErr = fmt.Errorf(
				"SecondBox PostgreSQL work listener shutdown: %w",
				shutdownContext.Err(),
			)
		}
	}
	if err := errors.Join(
		serveErr, httpShutdownErr, grpcShutdownErr, lifecycleShutdownErr,
		assignmentShutdownErr, snapshotRetentionShutdownErr, dataPlaneShutdownErr,
		workListenerShutdownErr,
	); err != nil {
		return fmt.Errorf("SecondBox coordinated server shutdown: %w", err)
	}
	return nil
}

func applicationAuthorities(configured []config.ApplicationAuthority) []api.ApplicationAuthority {
	authorities := make([]api.ApplicationAuthority, 0, len(configured))
	for _, authority := range configured {
		authorities = append(authorities, api.ApplicationAuthority{
			ID: authority.ID, Token: authority.Token,
			TenantRef: authority.TenantRef, SubjectRef: authority.SubjectRef,
			Scopes: authority.Scopes, ProfileGrants: authority.ProfileGrants,
		})
	}
	return authorities
}

func runDataPlaneSweeper(
	ctx context.Context,
	relay *runnercontrol.PostgresFrameRelay,
	pollInterval time.Duration,
) error {
	for {
		found, err := relay.SweepDataPlane(ctx, service.SystemClock(), 100)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("SecondBox data-plane sweep failed: %w", err)
		}
		if found {
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func newLifecycleFencingToken() ([]byte, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle fencing token: %w", err)
	}
	return token, nil
}

func runLifecycleReconciler(
	ctx context.Context,
	reconciler lifecycle.Reconciler,
	wakeups <-chan struct{},
) error {
	for {
		_, found, err := reconciler.RunOnce(ctx, service.SystemClock())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ports.ErrRevisionConflict) {
				continue
			}
			return fmt.Errorf("SecondBox lifecycle reconciliation failed: %w", err)
		}
		if found {
			continue
		}
		if !waitForWork(ctx, reconciler.PollInterval, wakeups) {
			return nil
		}
	}
}

func runAssignmentReconciler(
	ctx context.Context,
	worker reconcile.AssignmentWorker,
	wakeups <-chan struct{},
) error {
	for {
		_, found, err := worker.RunOnce(ctx, service.SystemClock())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, reconcile.ErrClaimLost) {
				continue
			}
			return fmt.Errorf("SecondBox Assignment reconciliation failed: %w", err)
		}
		if found {
			continue
		}
		if !waitForWork(ctx, worker.PollInterval, wakeups) {
			return nil
		}
	}
}

func waitForWork(
	ctx context.Context,
	fallbackInterval time.Duration,
	wakeups <-chan struct{},
) bool {
	timer := time.NewTimer(fallbackInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-wakeups:
		return true
	}
}

func runSnapshotRetentionWorker(
	ctx context.Context,
	worker lifecycle.SnapshotRetentionWorker,
) error {
	for {
		queued, err := worker.RunOnce(ctx, service.SystemClock())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("SecondBox Snapshot retention failed: %w", err)
		}
		if queued {
			continue
		}
		timer := time.NewTimer(worker.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func stopGRPCServer(ctx context.Context, server *grpc.Server) error {
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		server.Stop()
		<-stopped
		return fmt.Errorf("SecondBox runner control graceful shutdown: %w", ctx.Err())
	}
}

func configuredRunnerFeatures(names []string) ([]runnerv1.RunnerFeature, error) {
	known := map[string]runnerv1.RunnerFeature{
		"exec-streaming":  runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
		"file-streaming":  runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING,
		"pty":             runnerv1.RunnerFeature_RUNNER_FEATURE_PTY,
		"port-proxy":      runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
		"evidence":        runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		"local-workspace": runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	}
	features := make([]runnerv1.RunnerFeature, 0, len(names))
	for _, name := range names {
		feature, exists := known[name]
		if !exists {
			return nil, fmt.Errorf("SecondBox runner feature %q is unsupported", name)
		}
		features = append(features, feature)
	}
	if !slices.Contains(
		features,
		runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	) {
		return nil, errors.New("SecondBox runner features require local-workspace")
	}
	return features, nil
}
