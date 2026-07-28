package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const (
	freshRunnerRestoreHelperEnvironment = "SECONDBOX_FRESH_RUNNER_RESTORE_HELPER"
	freshRunnerRestoreAPIHashSecret     = "fresh-runner-restore-api-hash-secret-000000000000"
	freshRunnerRestoreBootstrapToken    = "fresh-runner-restore-bootstrap-token"
)

// TestBackupRestoreDrillMaterializesCheckpointOnFreshRunner proves the portable recovery boundary.
func TestBackupRestoreDrillMaterializesCheckpointOnFreshRunner(t *testing.T) {
	requireBackupRestoreCommands(t)
	sourceDatabaseName := "secondbox_backup_source_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	sourceDatabaseURL := backupRestoreDatabaseURL(t, integrationDatabaseURL, sourceDatabaseName)
	runBackupRestoreCommand(
		t,
		exec.Command("createdb", "--maintenance-db="+integrationDatabaseURL, sourceDatabaseName),
		"create isolated backup source database",
	)
	t.Cleanup(func() {
		command := exec.Command(
			"dropdb", "--if-exists", "--maintenance-db="+integrationDatabaseURL,
			sourceDatabaseName,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("drop isolated backup source database: %v\n%s", err, output)
		}
	})
	applyBackupRestoreMigration(t, sourceDatabaseURL)

	checkpointBytes := []byte(
		"SecondBox portable backup checkpoint bytes restored on a fresh Runner authority\n",
	)
	checkpointSum := sha256.Sum256(checkpointBytes)
	checkpointSHA256 := hex.EncodeToString(checkpointSum[:])
	objectExport := t.TempDir()
	checkpointStorageKey := "checkpoints/fresh-runner-restore.ext4"
	checkpointPath := filepath.Join(objectExport, filepath.FromSlash(checkpointStorageKey))
	if err := os.MkdirAll(filepath.Dir(checkpointPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath, checkpointBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	sandboxID, credential := seedBackupRestoreSource(
		t, sourceDatabaseURL, checkpointStorageKey, checkpointSHA256, int64(len(checkpointBytes)),
	)
	backupDirectory := t.TempDir()
	backup := exec.Command(repositoryScriptPath(t, "backup.sh"))
	backup.Env = append(os.Environ(),
		"SECONDBOX_BACKUP_DATABASE_URL="+sourceDatabaseURL,
		"SECONDBOX_BACKUP_DIR="+backupDirectory,
		"SECONDBOX_BACKUP_RECOVERY_POINT_ID=fresh-runner-portability",
		"SECONDBOX_BACKUP_OBJECT_EXPORT="+objectExport,
	)
	runBackupRestoreCommand(t, backup, "capture coordinated recovery bundle")
	bundles, err := filepath.Glob(filepath.Join(backupDirectory, "secondbox-backup-*.tar"))
	if err != nil || len(bundles) != 1 {
		t.Fatalf("recovery bundles = %v, error = %v", bundles, err)
	}

	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	verifierWrapper := filepath.Join(t.TempDir(), "fresh-runner-restore-verifier")
	wrapper := `#!/bin/sh
set -eu
export SECONDBOX_FRESH_RUNNER_RESTORE_RESULT="$1"
exec "$SECONDBOX_FRESH_RUNNER_RESTORE_TEST_BINARY" -test.run '^TestFreshRunnerRestoreVerifierProcess$' -test.count=1
`
	if err := os.WriteFile(verifierWrapper, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	listenAddress := unusedBackupRestoreListenAddress(t)
	controlPlaneURL := "http://" + listenAddress
	restoreObjectParent := t.TempDir()
	restoreObjectTarget := filepath.Join(restoreObjectParent, "objects")
	freshRunnerResult := filepath.Join(t.TempDir(), "fresh-runner-result.json")
	restore := exec.Command(repositoryScriptPath(t, "restore-drill.sh"))
	restore.Env = append(os.Environ(),
		freshRunnerRestoreHelperEnvironment+"=1",
		"SECONDBOX_FRESH_RUNNER_RESTORE_TEST_BINARY="+testExecutable,
		"SECONDBOX_FRESH_RUNNER_RESTORE_LISTEN_ADDRESS="+listenAddress,
		"SECONDBOX_FRESH_RUNNER_RESTORE_API_HASH_SECRET="+freshRunnerRestoreAPIHashSecret,
		"SECONDBOX_FRESH_RUNNER_RESTORE_BOOTSTRAP_TOKEN="+freshRunnerRestoreBootstrapToken,
		"SECONDBOX_RESTORE_DATABASE_URL="+integrationDatabaseURL,
		"SECONDBOX_RESTORE_BUNDLE="+bundles[0],
		"SECONDBOX_RESTORE_STAGE_DIR="+t.TempDir(),
		"SECONDBOX_RESTORE_OBJECT_TARGET="+restoreObjectTarget,
		"SECONDBOX_RESTORE_CONTROL_PLANE_URL="+controlPlaneURL,
		"SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN="+credential,
		"SECONDBOX_RESTORE_FRESH_RUNNER_RESULT="+freshRunnerResult,
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND="+verifierWrapper,
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS=30",
	)
	runBackupRestoreCommand(t, restore, "restore into fresh control-plane and Runner authority")

	var result freshRunnerRestoreIdentity
	resultBytes, err := os.ReadFile(freshRunnerResult)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatal(err)
	}
	if result.ContractVersion != "secondbox-fresh-runner-identity/v1" ||
		result.Restoration.SandboxID != sandboxID ||
		result.Restoration.CheckpointSHA256 != checkpointSHA256 ||
		result.Restoration.Generation != 2 {
		t.Fatalf("fresh-Runner restore identity = %#v", result)
	}
	restoredBytes, err := os.ReadFile(filepath.Join(
		restoreObjectTarget, filepath.FromSlash(checkpointStorageKey),
	))
	if err != nil {
		t.Fatal(err)
	}
	if string(restoredBytes) != string(checkpointBytes) {
		t.Fatalf("restored checkpoint bytes = %q, want %q", restoredBytes, checkpointBytes)
	}
}

// TestFreshRunnerRestoreVerifierProcess runs only as the supervised restore verifier.
func TestFreshRunnerRestoreVerifierProcess(t *testing.T) {
	if os.Getenv(freshRunnerRestoreHelperEnvironment) != "1" {
		return
	}
	runFreshRunnerRestoreVerifier(t)
}

func runFreshRunnerRestoreVerifier(t *testing.T) {
	databaseURL := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_RESTORE_VERIFICATION_DATABASE_URL",
	)
	objectRoot := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_RESTORE_VERIFICATION_OBJECT_STATE",
	)
	resultPath := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_FRESH_RUNNER_RESTORE_RESULT",
	)
	listenAddress := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_FRESH_RUNNER_RESTORE_LISTEN_ADDRESS",
	)
	recoveryPointID := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_RESTORE_VERIFICATION_RECOVERY_POINT_ID",
	)
	controlPlaneToken := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN",
	)
	apiHashSecret := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_FRESH_RUNNER_RESTORE_API_HASH_SECRET",
	)
	bootstrapToken := requiredBackupRestoreEnvironment(
		t, "SECONDBOX_FRESH_RUNNER_RESTORE_BOOTSTRAP_TOKEN",
	)

	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer databaseStore.Close()
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, BootstrapAdminToken: bootstrapToken,
		APIKeyHashSecret: []byte(apiHashSecret), DefaultProjectQuota: generousQuota(),
		DefaultProfileQuota: generousQuota(), Now: service.SystemClock,
		NewID: service.NewOpaqueID, NewCredentialMaterial: service.NewCredentialMaterial,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controlPlane.InitializeBootstrapAdmin(t.Context()); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(listener)
	}()

	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var sandboxID string
	var sandboxRevision int64
	if err := pool.QueryRow(t.Context(), `
		SELECT id,revision
		FROM secondbox.sandboxes
		WHERE state='stopped' AND desired_state='stopped'
		  AND deleted_at IS NULL
		ORDER BY created_at,id
		LIMIT 1`,
	).Scan(&sandboxID, &sandboxRevision); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	const runnerID = "runner-fresh-after-restore"
	objects := &freshRestoreFilesystemStore{root: objectRoot}
	restoreSender, err := lifecycle.NewCheckpointRestoreSender(
		t.Context(), lifecycle.CheckpointRestoreSenderConfig{
			DatabaseURL: databaseURL, ObjectStore: objects,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreSender.Close()
	runnerStream, credentialSerial := startFreshRunnerRestoreProtocol(
		t, databaseURL, stateStore, restoreSender, runnerID, now,
	)

	principal, err := controlPlane.AuthenticateCredential(t.Context(), controlPlaneToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StartSandbox(
		t.Context(), principal, sandboxID, "fresh-runner-restore-start", sandboxRevision,
	); err != nil {
		t.Fatal(err)
	}
	schedulerStore, err := scheduler.NewPostgresStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer schedulerStore.Close()
	idSequence := 0
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), databaseURL, schedulerStore,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute, AssignmentDeadline: time.Minute,
			HeartbeatTimeout: time.Minute, RetryLimit: 1, SerializationRetryLimit: 1,
			AssetCatalog:     profileLifecycleAssetCatalog{},
			SessionCanceller: profileLifecycleSessionCanceller{},
			NewID: func(prefix string) string {
				idSequence++
				return fmt.Sprintf("%s-fresh-restore-%d", prefix, idSequence)
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("fresh-restore-fencing-token-0001"), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer effectBroker.Close()
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker, WorkerID: "fresh-restore-lifecycle",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandboxID, now.Add(time.Millisecond), lifecycle.ActionMaterialize,
	)
	var restoreFrames []*runnerv1.ControlPlaneToRunner
	var assignment *runnerv1.AssignmentCommand
	for assignment == nil {
		frame, err := runnerStream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case frame.GetRestoreBegin() != nil, frame.GetRestoreChunk() != nil:
			restoreFrames = append(restoreFrames, frame)
		case frame.GetAssignment() != nil:
			assignment = frame.GetAssignment()
		default:
			t.Fatalf("fresh-Runner restore received unexpected frame %#v", frame)
		}
	}
	if assignment.SourceCheckpointId == "" {
		t.Fatalf("fresh-Runner restore Assignment = %#v", assignment)
	}
	restoredCheckpointBytes := collectCrossRunnerRestoreBytes(
		t, restoreFrames, assignment,
	)
	if err := runnerStream.Send(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentAck{
			AssignmentAck: &runnerv1.AssignmentAck{
				MessageId: "fresh-restore-assignment-ack", Sequence: 2,
				Fence:    assignment.Fence,
				Decision: runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runnerStream.Send(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId: "fresh-restore-assignment-ready", Sequence: 3,
				Fence:       assignment.Fence,
				Terminal:    runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind: "firecracker", BackendReference: "fresh-runner-restored-instance",
				Correlation: proto.Clone(assignment.Correlation).(*runnerv1.Correlation),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	waitFreshRunnerRestoreCondition(t, func() bool {
		var state string
		err := pool.QueryRow(t.Context(), `
			SELECT state FROM secondbox.assignments WHERE id=$1`,
			assignment.Fence.AssignmentId,
		).Scan(&state)
		return err == nil && state == "ready"
	})
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandboxID, now.Add(4*time.Millisecond), lifecycle.ActionMarkReady,
	)
	if _, err := databaseStore.PingGuest(t.Context(), ports.GenerationInput{
		ProjectID: principal.ProjectID, SandboxID: sandboxID,
		Generation: int64(assignment.Fence.SandboxGeneration),
		Now:        now.Add(5 * time.Millisecond),
	}, contracts.GuestLivenessReady); err != nil {
		t.Fatal(err)
	}

	var identity freshRunnerRestoreIdentity
	identity.ContractVersion = "secondbox-fresh-runner-identity/v1"
	identity.RecoveryPointID = recoveryPointID
	identity.Runner.ID = runnerID
	identity.Runner.CredentialSerial = credentialSerial
	if err := pool.QueryRow(t.Context(), `
		SELECT sandbox.id,sandbox.workspace_id,assignment.id,materialization.id,
		       checkpoint.id,checkpoint.sha256,sandbox.generation
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.assignments AS assignment
		  ON assignment.instance_id=sandbox.current_instance_id
		JOIN secondbox.workspace_materializations AS materialization
		  ON materialization.assignment_id=assignment.id
		JOIN secondbox.workspaces AS workspace
		  ON workspace.id=sandbox.workspace_id
		JOIN secondbox.workspace_checkpoints AS checkpoint
		  ON checkpoint.id=workspace.current_checkpoint_id
		WHERE sandbox.id=$1
		  AND assignment.runner_id=$2
		  AND assignment.state='ready'
		  AND materialization.state='ready'`,
		sandboxID, runnerID,
	).Scan(
		&identity.Restoration.SandboxID, &identity.Restoration.WorkspaceID,
		&identity.Restoration.AssignmentID, &identity.Restoration.MaterializationID,
		&identity.Restoration.CheckpointID, &identity.Restoration.CheckpointSHA256,
		&identity.Restoration.Generation,
	); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(restoredCheckpointBytes)
	if hex.EncodeToString(sum[:]) != identity.Restoration.CheckpointSHA256 {
		t.Fatal("fresh Runner received checkpoint bytes outside restored database authority")
	}
	writeFreshRunnerRestoreIdentity(t, resultPath, identity)

	shutdownContext, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM,
	)
	defer stop()
	select {
	case <-shutdownContext.Done():
	case serveErr := <-serverErrors:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			t.Fatal(serveErr)
		}
	}
	closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(closeContext); err != nil {
		t.Fatal(err)
	}
}

type freshRunnerRestoreIdentity struct {
	ContractVersion string `json:"contractVersion"`
	RecoveryPointID string `json:"recoveryPointId"`
	Runner          struct {
		ID               string `json:"id"`
		CredentialSerial string `json:"credentialSerial"`
	} `json:"runner"`
	Restoration struct {
		SandboxID         string `json:"sandboxId"`
		WorkspaceID       string `json:"workspaceId"`
		AssignmentID      string `json:"assignmentId"`
		MaterializationID string `json:"materializationId"`
		CheckpointID      string `json:"checkpointId"`
		CheckpointSHA256  string `json:"checkpointSHA256"`
		Generation        int64  `json:"generation"`
	} `json:"restoration"`
}

func startFreshRunnerRestoreProtocol(
	t *testing.T,
	databaseURL string,
	stateStore *runnercontrol.PostgresStateStore,
	restoreSender *lifecycle.CheckpointRestoreSender,
	runnerID string,
	now time.Time,
) (runnerv1.RunnerControl_ConnectClient, string) {
	t.Helper()
	if integrationDatabaseURL != databaseURL {
		t.Fatal("fresh-Runner verifier protocol is not bound to the restored database")
	}
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	enrollment, err := authority.CreateEnrollment(
		t.Context(), runnercontrol.EnrollmentRequest{
			TokenID: "enrollment-" + runnerID, RunnerID: runnerID,
			PoolName: "default-pool", RunnerName: runnerID, ExpiresAt: now.Add(time.Hour),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	certificateRequest, runnerPrivateKey := freshRunnerRestoreCertificateRequest(t)
	issued, err := authority.RedeemEnrollment(
		t.Context(), enrollment.Token, certificateRequest,
	)
	if err != nil {
		t.Fatal(err)
	}
	clientCertificate := freshRunnerRestoreTLSCertificate(
		t, issued.CertificatePEM, runnerPrivateKey,
	)
	serverCertificate := task4ServerCertificate(t, caCertificate, caPrivateKey, now)
	serverTLS, err := authority.ServerTLSConfig(serverCertificate)
	if err != nil {
		t.Fatal(err)
	}
	runnerServer, err := runnercontrol.NewServer(runnercontrol.ServerConfig{
		CredentialVerifier: authority, StateStore: stateStore,
		CheckpointReceiver: freshRestoreCheckpointReceiver{},
		CheckpointRestore:  restoreSender,
		SupportedVersions:  runnercontrol.VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures: []runnerv1.RunnerFeature{
			runnerv1.RunnerFeature_RUNNER_FEATURE_CHECKPOINT,
		},
		HeartbeatInterval: 100 * time.Millisecond, CommandPollInterval: 5 * time.Millisecond,
		Now: time.Now, NewConnectionID: func() string {
			return "connection-fresh-after-restore"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	protocolListener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	runnerv1.RegisterRunnerControlServer(grpcServer, runnerServer)
	go func() {
		_ = grpcServer.Serve(protocolListener)
	}()
	t.Cleanup(grpcServer.Stop)
	t.Cleanup(func() {
		_ = protocolListener.Close()
	})

	certificatePool := x509.NewCertPool()
	certificatePool.AddCert(caCertificate)
	clientTLS := &tls.Config{
		MinVersion: tls.VersionTLS13, ServerName: "control.secondbox.test",
		RootCAs: certificatePool, Certificates: []tls.Certificate{clientCertificate},
	}
	connection, err := grpc.NewClient(
		"passthrough:///fresh-runner-restore",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return protocolListener.Dial()
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close()
	})
	stream, err := runnerv1.NewRunnerControlClient(connection).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	connectionNonce := make([]byte, 32)
	if _, err := rand.Read(connectionNonce); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Hello{
			Hello: &runnerv1.RunnerHello{
				RunnerId: runnerID, ConnectionNonce: connectionNonce,
				SupportedVersions: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
				RequestedFeatures: []runnerv1.RunnerFeature{
					runnerv1.RunnerFeature_RUNNER_FEATURE_CHECKPOINT,
				},
				MandatoryFeatures: []runnerv1.RunnerFeature{
					runnerv1.RunnerFeature_RUNNER_FEATURE_CHECKPOINT,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	welcomeFrame, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	welcome := welcomeFrame.GetWelcome()
	if welcome == nil || welcome.ConnectionId == "" {
		t.Fatalf("fresh-Runner protocol welcome = %#v", welcomeFrame)
	}
	registration := task4Registration(
		runnerID, welcome.ConnectionId, "default-pool",
	)
	registration.ArtifactCache = []*runnerv1.ArtifactCacheEvidence{
		{
			ArtifactId:       "runtime",
			ManifestDigest:   "sha256:" + strings.Repeat("a", 64),
			VerifiedAtUnixMs: uint64(now.UnixMilli()),
		},
		{
			ArtifactId:       "toolchain",
			ManifestDigest:   "sha256:" + strings.Repeat("b", 64),
			VerifiedAtUnixMs: uint64(now.UnixMilli()),
		},
	}
	if err := stream.Send(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Registration{
			Registration: registration,
		},
	}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	waitFreshRunnerRestoreCondition(t, func() bool {
		var state string
		err := pool.QueryRow(t.Context(), `
			SELECT state FROM secondbox.runners WHERE id=$1`, runnerID,
		).Scan(&state)
		return err == nil && state == "ready"
	})
	return stream, issued.Identity.CredentialSerial
}

func freshRunnerRestoreCertificateRequest(
	t *testing.T,
) ([]byte, ed25519.PrivateKey) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	requestDER, err := x509.CreateCertificateRequest(
		rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "fresh restore runner"}},
		privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER},
	), privateKey
}

func freshRunnerRestoreTLSCertificate(
	t *testing.T,
	certificatePEM []byte,
	privateKey ed25519.PrivateKey,
) tls.Certificate {
	t.Helper()
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		certificatePEM,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

type freshRestoreCheckpointReceiver struct{}

func (freshRestoreCheckpointReceiver) ReceiveCheckpoint(
	context.Context,
	runnercontrol.Event,
	time.Time,
) error {
	return errors.New("Fresh restore verification Runner cannot publish a checkpoint")
}

func waitFreshRunnerRestoreCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("fresh-Runner restore condition did not become true")
		case <-ticker.C:
		}
	}
}

type freshRestoreFilesystemStore struct {
	root string
}

func (*freshRestoreFilesystemStore) PutImmutable(
	context.Context, string, io.Reader, int64, string,
) (objectstore.Evidence, error) {
	return objectstore.Evidence{}, errors.New("Fresh restore verification object store is read-only")
}

func (store *freshRestoreFilesystemStore) HeadVerified(
	_ context.Context,
	key string,
	expected objectstore.Evidence,
) (objectstore.Evidence, error) {
	path, err := store.verifiedObjectPath(key)
	if err != nil {
		return objectstore.Evidence{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return objectstore.Evidence{}, err
	}
	sum := sha256.Sum256(content)
	actual := objectstore.Evidence{
		SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(content)),
		ETag: "fresh-restore-filesystem",
	}
	if actual.SHA256 != expected.SHA256 || actual.SizeBytes != expected.SizeBytes {
		return objectstore.Evidence{}, errors.New(
			"Fresh restore verification object evidence mismatch",
		)
	}
	return actual, nil
}

func (store *freshRestoreFilesystemStore) GetVerified(
	ctx context.Context,
	key string,
	expected objectstore.Evidence,
) (io.ReadCloser, objectstore.Evidence, error) {
	actual, err := store.HeadVerified(ctx, key, expected)
	if err != nil {
		return nil, objectstore.Evidence{}, err
	}
	path, err := store.verifiedObjectPath(key)
	if err != nil {
		return nil, objectstore.Evidence{}, err
	}
	body, err := os.Open(path)
	if err != nil {
		return nil, objectstore.Evidence{}, err
	}
	return body, actual, nil
}

func (*freshRestoreFilesystemStore) Delete(context.Context, string) error {
	return errors.New("Fresh restore verification object store is read-only")
}

func (store *freshRestoreFilesystemStore) verifiedObjectPath(key string) (string, error) {
	if strings.TrimSpace(key) == "" || filepath.IsAbs(key) {
		return "", errors.New("Fresh restore verification object key is invalid")
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("Fresh restore verification object key escapes its root")
	}
	path := filepath.Join(store.root, clean)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("Fresh restore verification object is not a regular file")
	}
	return path, nil
}

func seedBackupRestoreSource(
	t *testing.T,
	databaseURL string,
	storageKey string,
	checkpointSHA256 string,
	checkpointSize int64,
) (string, string) {
	t.Helper()
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, BootstrapAdminToken: freshRunnerRestoreBootstrapToken,
		APIKeyHashSecret:    []byte(freshRunnerRestoreAPIHashSecret),
		DefaultProjectQuota: generousQuota(), DefaultProfileQuota: generousQuota(),
		Now: service.SystemClock, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controlPlane.InitializeBootstrapAdmin(t.Context()); err != nil {
		t.Fatal(err)
	}
	admin := controlPlane.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "backup-restore-source",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "backup-restore-profile",
	)
	principal, err := controlPlane.AuthenticateCredential(t.Context(), credential)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "backup-restore-sandbox",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	databaseStore.Close()

	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	checkpointID := "checkpoint-backup-restore"
	now := time.Now().UTC()
	compatibility, err := json.Marshal(map[string]string{
		"architecture": "amd64", "backend": "firecracker",
		"profileRevisionId": sandbox.ProfileRevisionID, "workspaceFormat": "ext4",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO secondbox.workspace_checkpoints (
			id,project_id,sandbox_id,workspace_id,source_generation,state,
			sha256,size_bytes,compatibility_json,storage_key,retain_until,
			created_at,verified_at,published_at
		) VALUES ($1,$2,$3,$4,1,'published',$5,$6,$7,$8,$9,$10,$10,$10)`,
		checkpointID, sandbox.ProjectID, sandbox.ID, sandbox.Workspace.ID,
		checkpointSHA256, checkpointSize, compatibility, storageKey,
		now.Add(24*time.Hour), now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE secondbox.workspaces
		SET generation=2,current_checkpoint_id=$2,current_checkpoint_sha256=$3,
		    current_checkpoint_size_bytes=$4,retained_bytes=$4,
		    retention_state='retained',garbage_collection_state='reachable',
		    updated_at=$5
		WHERE id=$1`,
		sandbox.Workspace.ID, checkpointID, checkpointSHA256, checkpointSize, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET generation=2,state='stopped',desired_state='stopped',
		    next_reconcile_at='2999-01-01 00:00:00+00',updated_at=$2
		WHERE id=$1`, sandbox.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return sandbox.ID, credential
}

func applyBackupRestoreMigration(t *testing.T, databaseURL string) {
	t.Helper()
	migration, err := os.ReadFile(filepath.Join(
		repositoryRootForBackupRestore(t),
		"migrations", "postgres", "0001_secondbox.sql",
	))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
}

func writeFreshRunnerRestoreIdentity(
	t *testing.T,
	resultPath string,
	identity freshRunnerRestoreIdentity,
) {
	t.Helper()
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := resultPath + ".tmp"
	if err := os.WriteFile(temporaryPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporaryPath, resultPath); err != nil {
		t.Fatal(err)
	}
}

func backupRestoreDatabaseURL(t *testing.T, databaseURL string, databaseName string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + databaseName
	return parsed.String()
}

func unusedBackupRestoreListenAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func requiredBackupRestoreEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("Fresh restore verifier requires %s", name)
	}
	return value
}

func requireBackupRestoreCommands(t *testing.T) {
	t.Helper()
	for _, command := range []string{
		"createdb", "curl", "dropdb", "jq", "pg_dump", "pg_restore", "psql", "tar",
	} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("fresh backup/restore drill requires %s: %v", command, err)
		}
	}
}

func runBackupRestoreCommand(t *testing.T, command *exec.Cmd, action string) {
	t.Helper()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", action, err, output)
	}
}

func repositoryScriptPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repositoryRootForBackupRestore(t), "scripts", name)
}

func repositoryRootForBackupRestore(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("locate SecondBox repository root")
		}
		directory = parent
	}
}
