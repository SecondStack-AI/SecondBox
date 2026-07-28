package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

func TestCheckpointProtocolRejectsSupersededAndRevokedConnectionsBeforeSideEffects(t *testing.T) {
	controlPlane, lifecycleStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	_, account, apiCredential := createProjectAccountAndCredential(
		t, controlPlane, admin, "checkpoint-connection-revocation",
	)
	profile := createGrantedProfile(
		t, controlPlane, lifecycleStore, admin, account, "checkpoint-connection-revocation-profile",
	)
	principal := authenticateCredential(t, controlPlane, apiCredential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "checkpoint-connection-revocation-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	runnerID := task4ID("checkpoint-revocation-runner")
	runnerPool := task4ID("checkpoint-revocation-pool")
	task4InsertRunnerPool(t, runnerPool, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	enrollment, err := authority.CreateEnrollment(t.Context(), runnercontrol.EnrollmentRequest{
		TokenID: task4ID("checkpoint-revocation-enrollment"), RunnerID: runnerID,
		PoolName: runnerPool, RunnerName: runnerID, ExpiresAt: now.Add(time.Hour),
	})
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
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id=$1`,
		sandbox.ID, now.Add(24*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	checkpointAuthority := seedRevocationCheckpointAuthority(
		t, pool, sandbox, runnerID, now,
	)
	objects := &checkpointObjectStore{objects: make(map[string][]byte)}
	spoolDirectory := t.TempDir()
	receiver, err := lifecycle.NewCheckpointReceiver(
		t.Context(),
		lifecycle.CheckpointReceiverConfig{
			DatabaseURL: integrationDatabaseURL, SpoolDirectory: spoolDirectory,
			ObjectStore: objects, LifecycleStore: lifecycleStore,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Close)
	stateStore, err := runnercontrol.NewPostgresStateStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	server, err := runnercontrol.NewServer(runnercontrol.ServerConfig{
		CredentialVerifier: authority, StateStore: stateStore,
		CheckpointReceiver: receiver, CheckpointRestore: rejectingCheckpointRestoreSender{},
		SupportedVersions: runnercontrol.VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures: []runnerv1.RunnerFeature{
			runnerv1.RunnerFeature_RUNNER_FEATURE_CHECKPOINT,
		},
		HeartbeatInterval: time.Second, CommandPollInterval: time.Hour,
		Now: func() time.Time { return now },
		NewConnectionID: func() string {
			return task4ID("checkpoint-revocation-connection")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	protocol := startRevocationProtocol(
		t, server, authority, caCertificate, caPrivateKey, clientCertificate, runnerID,
	)
	first, firstConnectionID := protocol.connect(t)
	registerRevocationProtocolRunner(t, first, runnerID, firstConnectionID, runnerPool)
	waitFreshRunnerRestoreCondition(t, func() bool {
		var state string
		err := pool.QueryRow(t.Context(), `
			SELECT state FROM secondbox.runners WHERE id=$1`, runnerID,
		).Scan(&state)
		return err == nil && state == "ready"
	})

	second, secondConnectionID := protocol.connect(t)
	waitFreshRunnerRestoreCondition(t, func() bool {
		var firstState, secondState string
		err := pool.QueryRow(t.Context(), `
			SELECT
			  (SELECT state FROM secondbox.runner_connections WHERE id=$1),
			  (SELECT state FROM secondbox.runner_connections WHERE id=$2)`,
			firstConnectionID, secondConnectionID,
		).Scan(&firstState, &secondState)
		return err == nil && firstState == "superseded" && secondState == "active"
	})
	supersededCheckpoint := checkpointAuthority.checkpoint("superseded")
	supersededCheckpoint.insertEffect(t, pool, now)
	if err := first.Send(supersededCheckpoint.chunkEvent(
		firstConnectionID, 2, 0, []byte("unauthorized"),
	).Message); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Recv(); err == nil {
		t.Fatal("superseded checkpoint connection remained open")
	}
	assertCheckpointAdmissionRejectedWithoutSideEffects(
		t, pool, spoolDirectory, objects, sandbox.Workspace.ID,
		firstConnectionID, supersededCheckpoint, nil,
	)

	registerRevocationProtocolRunner(t, second, runnerID, secondConnectionID, runnerPool)
	waitFreshRunnerRestoreCondition(t, func() bool {
		var state string
		err := pool.QueryRow(t.Context(), `
			SELECT state FROM secondbox.runners WHERE id=$1`, runnerID,
		).Scan(&state)
		return err == nil && state == "ready"
	})
	revokedCheckpointContent := []byte("revoked checkpoint bytes")
	revokedCheckpoint := checkpointAuthority.checkpoint("revoked")
	revokedCheckpoint.insertEffect(t, pool, now)
	revokedSpoolPath := filepath.Join(
		spoolDirectory, revokedCheckpoint.checkpointID+".partial",
	)
	if err := os.WriteFile(revokedSpoolPath, revokedCheckpointContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authority.RevokeCredential(
		t.Context(), issued.Identity.CredentialSerial,
	); err != nil {
		t.Fatal(err)
	}
	var credentialState, connectionState, runnerState, activeConnectionID string
	if err := pool.QueryRow(t.Context(), `
		SELECT credential.state,connection.state,runner.state,runner.active_connection_id
		FROM secondbox.runner_credentials AS credential
		JOIN secondbox.runner_connections AS connection
		  ON connection.credential_serial=credential.serial_number
		JOIN secondbox.runners AS runner ON runner.id=credential.runner_id
		WHERE credential.serial_number=$1 AND connection.id=$2`,
		issued.Identity.CredentialSerial, secondConnectionID,
	).Scan(
		&credentialState, &connectionState, &runnerState, &activeConnectionID,
	); err != nil {
		t.Fatal(err)
	}
	if credentialState != "revoked" || connectionState != "revoked" ||
		runnerState != "offline" || activeConnectionID != "" {
		t.Fatalf(
			"atomic revocation projection = credential %q connection %q runner %q active %q",
			credentialState, connectionState, runnerState, activeConnectionID,
		)
	}
	if err := second.Send(revokedCheckpoint.resultEvent(
		secondConnectionID, 2, revokedCheckpointContent,
	).Message); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Recv(); err == nil {
		t.Fatal("revoked checkpoint connection remained open")
	}
	assertCheckpointAdmissionRejectedWithoutSideEffects(
		t, pool, spoolDirectory, objects, sandbox.Workspace.ID,
		secondConnectionID, revokedCheckpoint, revokedCheckpointContent,
	)
}

func TestRunnerCredentialRevocationInvalidatesEveryActiveDataPlanePath(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "relay-credential-revocation",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "relay-credential-revocation-profile",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "relay-credential-revocation-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation,
		"relay-credential-revocation-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(
		t.Context(),
		runnercontrol.PostgresFrameRelayConfig{
			DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
			Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)

	sessions := make(map[string]runnercontrol.DataPlaneSession)
	for _, kind := range []string{"exec", "file", "terminal", "port"} {
		admissionKind := kind
		operation := kind
		admission := runnercontrol.DataPlaneAdmission{
			ID:        "revocation_" + kind + "_" + sandbox.ID,
			StreamID:  "revocation_stream_" + kind + "_" + sandbox.ID,
			ProjectID: principal.ProjectID, SandboxID: sandbox.ID,
			ServiceAccountID: principal.ServiceAccountID,
			Generation:       sandbox.Generation, Kind: admissionKind, Operation: operation,
			RequestID:      "revocation-request-" + kind,
			IdempotencyKey: "revocation-" + kind, RequestHash: "revocation-hash-" + kind,
			DeadlineAt: now.Add(30 * time.Second), MaximumResponseBytes: 1024,
			Now: now,
		}
		switch kind {
		case "exec", "port":
			admission.Kind = "exec"
			admission.ExecOpen = &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "printf revocation"},
				DeadlineUnixMs:   uint64(admission.DeadlineAt.UnixMilli()),
				OutputLimitBytes: 1024,
			}
		case "file":
			admission.FileOpen = &runnerv1.FileOpen{
				Operation:             runnerv1.FileOperation_FILE_OPERATION_STAT,
				WorkspaceRelativePath: "revocation",
			}
		case "terminal":
			admission.LeaseID = lease.ID
			admission.DeferResponseCredit = true
			admission.StreamWindowBytes = 64
			admission.ExecOpen = &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "cat"},
				DeadlineUnixMs:   uint64(admission.DeadlineAt.UnixMilli()),
				OutputLimitBytes: 1024, AllocatePty: true, Streaming: true,
				PtyRows: 24, PtyColumns: 80,
			}
		}
		session, replayed, err := relay.AdmitDataPlane(t.Context(), admission)
		if err != nil || replayed {
			t.Fatalf("%s admission = %#v, replayed=%t, error=%v", kind, session, replayed, err)
		}
		sessions[kind] = session
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id=$1`,
		sandbox.ID, now.Add(24*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.ID)
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM secondbox.data_plane_frames WHERE session_id=ANY($1)`,
			`DELETE FROM secondbox.activity_sessions WHERE id=ANY($1)`,
			`DELETE FROM secondbox.data_plane_sessions WHERE id=ANY($1)`,
		} {
			if _, err := pool.Exec(context.Background(), statement, sessionIDs); err != nil {
				t.Errorf("revocation relay fixture cleanup: %v", err)
			}
		}
	})
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions
		SET kind='port',operation='port:web'
		WHERE id=$1`, sessions["port"].ID,
	); err != nil {
		t.Fatal(err)
	}
	portSession := sessions["port"]
	portSession.Kind = "port"
	portSession.Operation = "port:web"
	sessions["port"] = portSession

	claimedBeforeRevocation, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now,
	)
	if err != nil || !found {
		t.Fatalf("pre-revocation claim = %#v, found=%t, error=%v", claimedBeforeRevocation, found, err)
	}
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	if err := authority.RevokeCredential(t.Context(), seed.CredentialSerial); err != nil {
		t.Fatal(err)
	}
	var activeConnections int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.runner_connections
		WHERE credential_serial=$1 AND state='active'`,
		seed.CredentialSerial,
	).Scan(&activeConnections); err != nil {
		t.Fatal(err)
	}
	if activeConnections != 0 {
		t.Fatalf("revoked credential retains %d active connections", activeConnections)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), claimedBeforeRevocation.ID, seed.ConnectionOne, now.Add(time.Millisecond),
	); !errors.Is(err, runnercontrol.ErrRelayDeliveryClaim) {
		t.Fatalf("revoked claimed outbound delivery error = %v", err)
	}
	if delivery, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionTwo, now.Add(time.Millisecond),
	); err != nil || found {
		t.Fatalf("revoked outbound claim = %#v, found=%t, error=%v", delivery, found, err)
	}

	inbound := map[string]*runnerv1.RunnerToControlPlane{
		"exec": relayExecOutput(
			seed.Fence, sessions["exec"].ID, sessions["exec"].StreamID, 1, []byte("exec"),
		),
		"file": {
			Message: &runnerv1.RunnerToControlPlane_File{File: &runnerv1.FileFrame{
				Fence: seed.Fence, OperationId: sessions["file"].ID,
				StreamId: sessions["file"].StreamID, Sequence: 1,
				Payload: &runnerv1.FileFrame_Terminal{Terminal: &runnerv1.FileTerminal{
					Kind: runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
				}},
			}},
		},
		"terminal": {
			Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
				Fence: seed.Fence, OperationId: sessions["terminal"].ID,
				StreamId: sessions["terminal"].StreamID, Sequence: 1,
				Payload: &runnerv1.PtyFrame_Output{Output: &runnerv1.ExecOutput{
					Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
					Data:    []byte("pty"),
				}},
			}},
		},
		"port": {
			Message: &runnerv1.RunnerToControlPlane_Port{Port: &runnerv1.PortFrame{
				Fence: seed.Fence, OperationId: sessions["port"].ID,
				StreamId: sessions["port"].StreamID, Sequence: 1,
				Correlation: &runnerv1.Correlation{
					RequestId:   sessions["port"].RequestID,
					OperationId: sessions["port"].ID, SandboxId: sandbox.ID,
					InstanceId:        seed.Fence.InstanceId,
					SandboxGeneration: seed.Fence.SandboxGeneration,
					AssignmentId:      seed.Fence.AssignmentId, RunnerId: seed.RunnerID,
				},
				Payload: &runnerv1.PortFrame_Bytes{Bytes: &runnerv1.PortBytes{
					Data: []byte("port"),
				}},
			}},
		},
	}
	for kind, message := range inbound {
		if inserted, err := relay.PersistInboundFrame(
			t.Context(),
			runnercontrol.InboundRelayFrame{
				RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionTwo, Message: message,
			},
			now.Add(2*time.Millisecond),
		); !errors.Is(err, runnercontrol.ErrRelayFence) || inserted {
			t.Fatalf("revoked %s inbound persistence = %t, error=%v", kind, inserted, err)
		}
	}
	var persistedInboundFrames int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames
		WHERE session_id=ANY($1) AND direction='inbound'`,
		sessionIDs,
	).Scan(&persistedInboundFrames); err != nil {
		t.Fatal(err)
	}
	if persistedInboundFrames != 0 {
		t.Fatalf("revoked connections persisted %d inbound frames", persistedInboundFrames)
	}
}

type rejectingCheckpointRestoreSender struct{}

func (rejectingCheckpointRestoreSender) StreamRestore(
	context.Context,
	*runnerv1.AssignmentCommand,
	func(*runnerv1.ControlPlaneToRunner) error,
) error {
	return errors.New("revocation test does not restore checkpoints")
}

type revocationProtocol struct {
	listener          *bufconn.Listener
	certificatePool   *x509.CertPool
	clientCertificate tls.Certificate
	runnerID          string
}

func startRevocationProtocol(
	t *testing.T,
	server *runnercontrol.Server,
	authority *runnercontrol.CredentialAuthority,
	caCertificate *x509.Certificate,
	caPrivateKey ed25519.PrivateKey,
	clientCertificate tls.Certificate,
	runnerID string,
) *revocationProtocol {
	t.Helper()
	serverCertificate := task4ServerCertificate(
		t, caCertificate, caPrivateKey, time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC),
	)
	serverTLS, err := authority.ServerTLSConfig(serverCertificate)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	runnerv1.RegisterRunnerControlServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	certificatePool := x509.NewCertPool()
	certificatePool.AddCert(caCertificate)
	return &revocationProtocol{
		listener: listener, certificatePool: certificatePool,
		clientCertificate: clientCertificate, runnerID: runnerID,
	}
}

func (protocol *revocationProtocol) connect(
	t *testing.T,
) (runnerv1.RunnerControl_ConnectClient, string) {
	t.Helper()
	connection, err := grpc.NewClient(
		"passthrough:///runner-revocation",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return protocol.listener.Dial()
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13, ServerName: "control.secondbox.test",
			RootCAs:      protocol.certificatePool,
			Certificates: []tls.Certificate{protocol.clientCertificate},
		})),
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
	if err := stream.Send(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Hello{Hello: &runnerv1.RunnerHello{
			RunnerId: protocol.runnerID, ConnectionNonce: bytes.Repeat([]byte{1}, 32),
			SupportedVersions: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
			RequestedFeatures: []runnerv1.RunnerFeature{
				runnerv1.RunnerFeature_RUNNER_FEATURE_CHECKPOINT,
			},
			MandatoryFeatures: []runnerv1.RunnerFeature{
				runnerv1.RunnerFeature_RUNNER_FEATURE_CHECKPOINT,
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	welcome := response.GetWelcome()
	if welcome == nil || welcome.ConnectionId == "" {
		t.Fatalf("revocation protocol welcome = %#v", response)
	}
	return stream, welcome.ConnectionId
}

func registerRevocationProtocolRunner(
	t *testing.T,
	stream runnerv1.RunnerControl_ConnectClient,
	runnerID string,
	connectionID string,
	poolName string,
) {
	t.Helper()
	if err := stream.Send(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Registration{
			Registration: task4Registration(runnerID, connectionID, poolName),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

type revocationCheckpointAuthority struct {
	sandbox        contracts.Sandbox
	runnerID       string
	assignmentID   string
	instanceID     string
	fencingToken   []byte
	maximumBytes   int64
	retainUntil    time.Time
	effectDeadline time.Time
}

type revocationCheckpoint struct {
	authority       revocationCheckpointAuthority
	checkpointID    string
	storageObjectID string
}

func seedRevocationCheckpointAuthority(
	t *testing.T,
	pool *pgxpool.Pool,
	sandbox contracts.Sandbox,
	runnerID string,
	now time.Time,
) revocationCheckpointAuthority {
	t.Helper()
	authority := revocationCheckpointAuthority{
		sandbox: sandbox, runnerID: runnerID,
		assignmentID: "assignment_revocation_" + sandbox.ID,
		instanceID:   "instance_revocation_" + sandbox.ID,
		fencingToken: bytes.Repeat([]byte{7}, 32), maximumBytes: 4096,
		retainUntil: now.Add(24 * time.Hour), effectDeadline: now.Add(time.Hour),
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES ($1,$2,$3,'ready','ready','',$4,$4,$4,$4,$5,NULL)`,
		authority.instanceID, sandbox.ID, sandbox.Generation, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,'firecracker','revocation',$6,$7,'ready',
		          '{}','{}','{}','',0,3,$8,$8,'',$8,$8,1,$9,$9)`,
		authority.assignmentID, sandbox.ID, authority.instanceID, runnerID,
		sandbox.ProfileRevisionID, sandbox.Generation, authority.fencingToken,
		now.Add(time.Hour), now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',desired_state='running',current_instance_id=$2,updated_at=$3
		WHERE id=$1`,
		sandbox.ID, authority.instanceID, now,
	); err != nil {
		t.Fatal(err)
	}
	return authority
}

func (authority revocationCheckpointAuthority) checkpoint(
	suffix string,
) revocationCheckpoint {
	checkpoint := revocationCheckpoint{
		authority:       authority,
		checkpointID:    "checkpoint_revocation_" + suffix + "_" + authority.sandbox.ID,
		storageObjectID: "checkpoints/revocation_" + suffix + "_" + authority.sandbox.ID + ".ext4",
	}
	return checkpoint
}

func (checkpoint revocationCheckpoint) insertEffect(
	t *testing.T,
	pool *pgxpool.Pool,
	now time.Time,
) {
	t.Helper()
	authority := checkpoint.authority
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,checkpoint_id,storage_object_id,fencing_token,retry_count,retry_limit,
			effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,
			payload_json,evidence_json,created_at,updated_at
		) VALUES (
			$1,$2,$3,'checkpoint','queued',$4,$5,$6,$7,$8,$9,$10,0,2,$11,'',$12,'','',
			jsonb_build_object(
				'workspaceId',$13::text,
				'retainUntil',$14::timestamptz,
				'maximumSizeBytes',$15::bigint
			),
			'{}',$16,$16
		)`,
		"effect_"+checkpoint.checkpointID, authority.sandbox.ID, authority.sandbox.Generation,
		authority.assignmentID, authority.instanceID, authority.runnerID,
		"command_"+checkpoint.checkpointID, checkpoint.checkpointID,
		checkpoint.storageObjectID, authority.fencingToken, authority.effectDeadline,
		now, authority.sandbox.Workspace.ID, authority.retainUntil, authority.maximumBytes, now,
	); err != nil {
		t.Fatal(err)
	}
}

func (checkpoint revocationCheckpoint) fence() *runnerv1.AssignmentFence {
	authority := checkpoint.authority
	return &runnerv1.AssignmentFence{
		AssignmentId: authority.assignmentID, SandboxId: authority.sandbox.ID,
		InstanceId:        authority.instanceID,
		SandboxGeneration: uint64(authority.sandbox.Generation),
		FencingToken:      authority.fencingToken,
	}
}

func (checkpoint revocationCheckpoint) chunkEvent(
	connectionID string,
	sequence uint64,
	offset uint64,
	content []byte,
) runnercontrol.Event {
	return runnercontrol.Event{
		Kind: runnercontrol.EventCheckpoint, RunnerID: checkpoint.authority.runnerID,
		ConnectionID: connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_CheckpointChunk{
				CheckpointChunk: &runnerv1.CheckpointChunk{
					MessageId: "message_" + checkpoint.checkpointID, Sequence: sequence,
					Fence: checkpoint.fence(), CheckpointId: checkpoint.checkpointID,
					StorageObjectId: checkpoint.storageObjectID, Offset: offset, Data: content,
				},
			},
		},
	}
}

func (checkpoint revocationCheckpoint) resultEvent(
	connectionID string,
	sequence uint64,
	content []byte,
) runnercontrol.Event {
	sum := sha256.Sum256(content)
	return runnercontrol.Event{
		Kind: runnercontrol.EventCheckpoint, RunnerID: checkpoint.authority.runnerID,
		ConnectionID: connectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_CheckpointResult{
				CheckpointResult: &runnerv1.CheckpointResult{
					MessageId: "message_" + checkpoint.checkpointID, Sequence: sequence,
					Fence: checkpoint.fence(), CheckpointId: checkpoint.checkpointID,
					StorageObjectId: checkpoint.storageObjectID,
					Terminal:        runnerv1.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_CREATED,
					Sha256:          hex.EncodeToString(sum[:]), SizeBytes: uint64(len(content)),
					Compatibility: map[string]string{
						"architecture": "amd64", "backend": "firecracker",
						"profileRevisionId":       checkpoint.authority.sandbox.ProfileRevisionID,
						"workspaceFormat":         "ext4",
						"runtimeManifestDigest":   "sha256:" + strings.Repeat("a", 64),
						"toolchainManifestDigest": "sha256:" + strings.Repeat("b", 64),
						"guestProtocolGeneration": "1", "mandatoryGuestFeatures": "",
					},
					Correlation: &runnerv1.Correlation{
						RequestId:         "request_" + checkpoint.checkpointID,
						OperationId:       "operation_" + checkpoint.checkpointID,
						SandboxId:         checkpoint.authority.sandbox.ID,
						InstanceId:        checkpoint.authority.instanceID,
						SandboxGeneration: uint64(checkpoint.authority.sandbox.Generation),
						AssignmentId:      checkpoint.authority.assignmentID,
						RunnerId:          checkpoint.authority.runnerID,
					},
				},
			},
		},
	}
}

func assertCheckpointAdmissionRejectedWithoutSideEffects(
	t *testing.T,
	pool *pgxpool.Pool,
	spoolDirectory string,
	objects *checkpointObjectStore,
	workspaceID string,
	connectionID string,
	checkpoint revocationCheckpoint,
	expectedSpool []byte,
) {
	t.Helper()
	var effectState, currentCheckpointID string
	var checkpointCount, messageCount int64
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,workspace.current_checkpoint_id,
		       (SELECT count(*) FROM secondbox.workspace_checkpoints WHERE id=$1),
		       (SELECT count(*) FROM secondbox.runner_messages
		        WHERE connection_id=$2 AND kind='checkpoint')
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.workspaces AS workspace ON workspace.id=$3
		WHERE effect.checkpoint_id=$1`,
		checkpoint.checkpointID, connectionID, workspaceID,
	).Scan(&effectState, &currentCheckpointID, &checkpointCount, &messageCount); err != nil {
		t.Fatal(err)
	}
	if effectState != "queued" || currentCheckpointID != "" ||
		checkpointCount != 0 || messageCount != 0 ||
		len(objects.objects) != 0 {
		t.Fatalf(
			"rejected checkpoint side effects: state=%q current=%q checkpoints=%d messages=%d objects=%d",
			effectState, currentCheckpointID, checkpointCount, messageCount, len(objects.objects),
		)
	}
	spoolPath := filepath.Join(spoolDirectory, checkpoint.checkpointID+".partial")
	actualSpool, err := os.ReadFile(spoolPath)
	if expectedSpool == nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected checkpoint created spool %q: %v", string(actualSpool), err)
		}
		return
	}
	if err != nil || !bytes.Equal(actualSpool, expectedSpool) {
		t.Fatalf("rejected checkpoint changed spool to %q: %v", string(actualSpool), err)
	}
}
