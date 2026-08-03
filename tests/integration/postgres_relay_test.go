package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestLifecycleStopCancelsInFlightGenerationSession(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "lifecycle-stop-cancel",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-lifecycle-stop-cancel",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "lifecycle-stop-cancel-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation,
		"lifecycle-stop-cancel-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(
		t.Context(), runnercontrol.PostgresFrameRelayConfig{
			DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
			Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	session, _, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_lifecycle_stop_cancel", StreamID: "stream_lifecycle_stop_cancel",
		TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef,
		SandboxID: sandbox.ID, LeaseID: lease.ID, Generation: sandbox.Generation,
		RequestID: "request-lifecycle-stop-cancel", Kind: "exec", Operation: "exec",
		IdempotencyKey: "lifecycle-stop-cancel", RequestHash: "lifecycle-stop-cancel-hash",
		DeadlineAt: now.Add(time.Minute), MaximumResponseBytes: 1024,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "sleep 60"},
			DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 1024,
		},
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var initialFrameCount int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames WHERE session_id=$1`,
		session.ID,
	).Scan(&initialFrameCount); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := pool.QueryRow(t.Context(), `
		WITH ready_workspace AS (
			UPDATE secondbox.workspaces
			SET state='ready',generation=$2,mutation_kind='',mutation_id='',
			    mutation_effect_id='',mutation_operation_id='',
			    mutation_expected_generation=NULL,mutation_target_generation=NULL,
			    mutation_state='',updated_at=$3
			WHERE sandbox_id=$1
		)
		UPDATE secondbox.sandboxes
		SET state='draining',desired_state='stopped',
		    lifecycle_termination_reason='requested_stop',
		    reconcile_owner='lifecycle-stop-cancel-worker',
		    reconcile_claim_expires_at=$4,revision=revision+1,updated_at=$3
		WHERE id=$1
		RETURNING revision`,
		sandbox.ID, sandbox.Generation, now.Add(time.Second), now.Add(time.Minute),
	).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	assignmentScheduler, err := scheduler.NewPostgresStore(
		t.Context(), scheduler.PostgresStoreConfig{
			DatabaseURL: integrationDatabaseURL, Now: func() time.Time { return now },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assignmentScheduler.Close)
	broker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, assignmentScheduler,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute, AssignmentDeadline: time.Minute,
			HeartbeatTimeout: time.Minute, RetryLimit: 2, SerializationRetryLimit: 2,
			AssetCatalog: multirunnerAssetCatalog{}, SessionCanceller: relay,
			NewID: func(prefix string) string { return prefix + "-lifecycle-stop-cancel" },
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
			Now: func() time.Time { return now },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	if err := broker.ExecuteLifecycleEffect(
		t.Context(),
		ports.LifecycleReconcileClaim{
			SandboxID: sandbox.ID, WorkerID: "lifecycle-stop-cancel-worker", Revision: revision,
		},
		lifecycle.Decision{Action: lifecycle.ActionStopInstance},
		now.Add(2*time.Second), now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	cancelled, err := relay.GetDataPlaneSession(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
	)
	if err != nil || cancelled.State != "completed" ||
		cancelled.TerminalDetail != "Sandbox drain grace expired" {
		t.Fatalf("stopping session = %#v, %v", cancelled, err)
	}
	var sandboxState string
	if err := pool.QueryRow(t.Context(), `
		UPDATE secondbox.sandboxes
		SET reconcile_owner='lifecycle-stop-cancel-replay',
		    reconcile_claim_expires_at=$2,revision=revision+1
		WHERE id=$1
		RETURNING state,revision`,
		sandbox.ID, now.Add(time.Minute),
	).Scan(&sandboxState, &revision); err != nil {
		t.Fatal(err)
	}
	if sandboxState != contracts.SandboxStateStopping {
		t.Fatalf("Sandbox state after cancellation = %q", sandboxState)
	}
	if err := broker.ExecuteLifecycleEffect(
		t.Context(),
		ports.LifecycleReconcileClaim{
			SandboxID: sandbox.ID, WorkerID: "lifecycle-stop-cancel-replay", Revision: revision,
		},
		lifecycle.Decision{Action: lifecycle.ActionStopInstance},
		now.Add(3*time.Second), now.Add(4*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	var frameCount int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames WHERE session_id=$1`,
		session.ID,
	).Scan(&frameCount); err != nil {
		t.Fatal(err)
	}
	if frameCount != initialFrameCount {
		t.Fatalf(
			"session frame count after stop replay = %d, want unchanged initial count %d",
			frameCount, initialFrameCount,
		)
	}
}

func TestPostgresLivePublicCancellationIsAtomicAndKeyScoped(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "relay-public-cancel",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-relay-public-cancel",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "relay-public-cancel-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 22, 0, 0, 0, time.UTC)
	seedRelayReadyAssignment(t, sandbox, now)
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
	session, _, err := relay.AdmitDataPlane(
		t.Context(),
		runnercontrol.DataPlaneAdmission{
			ID: "dps_relay_public_cancel", StreamID: "stream_relay_public_cancel",
			TenantRef: project.ID, SandboxID: sandbox.ID,
			SubjectRef: principal.SubjectRef, Generation: sandbox.Generation,
			RequestID: "request-relay-public-cancel",
			Kind:      "exec", Operation: "exec-stream",
			IdempotencyKey: "relay-public-cancel-create", RequestHash: "relay-public-cancel-create-hash",
			DeadlineAt: now.Add(time.Minute), MaximumResponseBytes: 1024,
			StreamWindowBytes: 4096, DeferResponseCredit: true,
			ExecOpen: &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "sleep 60"},
				DeadlineUnixMs:   uint64(now.Add(time.Minute).UnixMilli()),
				OutputLimitBytes: 1024, Streaming: true,
			},
			Now: now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	const failureTrigger = "test_fail_public_session_cancellation_record"
	const failureFunction = "secondbox.test_fail_public_session_cancellation_record"
	if _, err := pool.Exec(t.Context(), `
		CREATE OR REPLACE FUNCTION `+failureFunction+`() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.idempotency_key = 'relay-public-cancel-atomic-failure' THEN
				RAISE EXCEPTION 'intentional public session cancellation record failure';
			END IF;
			RETURN NEW;
		END
		$$;
		DROP TRIGGER IF EXISTS `+failureTrigger+` ON secondbox.idempotency_records;
		CREATE TRIGGER `+failureTrigger+`
		BEFORE INSERT ON secondbox.idempotency_records
		FOR EACH ROW EXECUTE FUNCTION `+failureFunction+`();
	`); err != nil {
		t.Fatal(err)
	}
	removeFailureTrigger := func(ctx context.Context) {
		if _, err := pool.Exec(ctx, `
			DROP TRIGGER IF EXISTS `+failureTrigger+` ON secondbox.idempotency_records;
			DROP FUNCTION IF EXISTS `+failureFunction+`();
		`); err != nil {
			t.Errorf("remove cancellation failure trigger: %v", err)
		}
	}
	t.Cleanup(func() { removeFailureTrigger(context.Background()) })

	cancellation := runnercontrol.PublicDataPlaneCancellation{
		TenantRef: project.ID, SandboxID: sandbox.ID, SessionID: session.ID,
		SubjectRef:  principal.SubjectRef,
		SessionKind: "exec", SessionOperation: "exec-stream",
		IdempotencyKey: "relay-public-cancel-atomic-failure",
		RequestHash:    "relay-public-cancel-fingerprint", Reason: "public cancellation",
		Generation: sandbox.Generation, Now: now.Add(time.Second),
		IdempotencyEnds: now.Add(24 * time.Hour),
	}
	if _, _, err := relay.CancelPublicDataPlaneSession(
		t.Context(), cancellation,
	); err == nil {
		t.Fatal("public cancellation succeeded despite idempotency record failure")
	}
	unchanged, err := relay.GetDataPlaneSession(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != session.State {
		t.Fatalf("failed cancellation changed session state from %q to %q", session.State, unchanged.State)
	}
	var cancelFrameCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND priority=-100`,
		session.ID,
	).Scan(&cancelFrameCount); err != nil {
		t.Fatal(err)
	}
	if cancelFrameCount != 0 {
		t.Fatalf("failed cancellation retained %d cancellation frames", cancelFrameCount)
	}
	var failureRecordCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2 AND operation='exec-stream-cancel'
		  AND target_id=$3 AND idempotency_key=$4`,
		principal.TenantRef, principal.SubjectRef, session.ID, cancellation.IdempotencyKey,
	).Scan(&failureRecordCount); err != nil {
		t.Fatal(err)
	}
	if failureRecordCount != 0 {
		t.Fatalf("failed cancellation retained %d idempotency records", failureRecordCount)
	}

	removeFailureTrigger(t.Context())
	cancellation.IdempotencyKey = "relay-public-cancel-durable"
	first, replayed, err := relay.CancelPublicDataPlaneSession(t.Context(), cancellation)
	if err != nil || replayed || first.State != "completed" {
		t.Fatalf("first public cancellation = %#v, replayed=%t, error=%v", first, replayed, err)
	}
	closingNewKey := cancellation
	closingNewKey.IdempotencyKey = "relay-public-cancel-new-key-closing"
	closingNewKey.Now = now.Add(1500 * time.Millisecond)
	stillClosing, replayed, err := relay.CancelPublicDataPlaneSession(t.Context(), closingNewKey)
	if err != nil || replayed || stillClosing.State != "completed" {
		t.Fatalf(
			"new-key closing cancellation = %#v, replayed=%t, error=%v",
			stillClosing, replayed, err,
		)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions
		SET state='completed',completed_at=$2,updated_at=$2
		WHERE id=$1`,
		session.ID, now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	replayedSession, replayed, err := relay.CancelPublicDataPlaneSession(t.Context(), cancellation)
	firstJSON, firstJSONError := json.Marshal(first)
	replayedJSON, replayedJSONError := json.Marshal(replayedSession)
	if err != nil || firstJSONError != nil || replayedJSONError != nil ||
		!replayed || !bytes.Equal(replayedJSON, firstJSON) {
		t.Fatalf(
			"public cancellation replay = %#v, replayed=%t, error=%v, encode errors=(%v,%v); want %#v",
			replayedSession, replayed, err, firstJSONError, replayedJSONError, first,
		)
	}
	conflicting := cancellation
	conflicting.RequestHash = "changed-public-cancel-fingerprint"
	if _, _, err := relay.CancelPublicDataPlaneSession(
		t.Context(), conflicting,
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("changed public cancellation fingerprint error = %v", err)
	}
	newKey := cancellation
	newKey.IdempotencyKey = "relay-public-cancel-new-key"
	newKey.Now = now.Add(3 * time.Second)
	current, replayed, err := relay.CancelPublicDataPlaneSession(t.Context(), newKey)
	if err != nil || replayed || current.State != "completed" {
		t.Fatalf("new-key public cancellation = %#v, replayed=%t, error=%v", current, replayed, err)
	}
}

func TestPostgresRelayRejectsAdmissionWithoutActiveHomeRunnerConnection(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t,
		controlPlane,
		admin,
		"relay-runner-offline",
	)
	profile := createGrantedProfile(
		t,
		controlPlane,
		databaseStore,
		admin,
		account,
		"profile-relay-runner-offline",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(),
		principal,
		"relay-runner-offline-create",
		contracts.CreateSandboxRequest{
			Profile:  profile.Name,
			Metadata: map[string]string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 29, 21, 10, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runner_connections
		SET state='disconnected',disconnected_at=$2,last_seen_at=$2
		WHERE runner_id=$1`,
		seed.RunnerID,
		now.Add(time.Second),
	); err != nil {
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
	_, _, err = relay.AdmitDataPlane(
		t.Context(),
		runnercontrol.DataPlaneAdmission{
			ID: "dps_relay_runner_offline", StreamID: "stream_relay_runner_offline",
			TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
			SubjectRef: principal.SubjectRef, Generation: sandbox.Generation,
			RequestID: "request-relay-runner-offline",
			Kind:      "exec", Operation: "exec", IdempotencyKey: "relay-runner-offline",
			RequestHash: "relay-runner-offline-hash", DeadlineAt: now.Add(time.Minute),
			MaximumResponseBytes: 1024,
			ExecOpen: &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "true"},
				DeadlineUnixMs:   uint64(now.Add(time.Minute).UnixMilli()),
				OutputLimitBytes: 1024,
			},
			Now: now.Add(2 * time.Second),
		},
	)
	if !errors.Is(err, ports.ErrLifecycleUnavailable) {
		t.Fatalf("offline home runner admission error = %v", err)
	}
}

func TestPostgresLiveDataPlanePersistsOneBufferedExecOutcomeAndNoFrames(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "live-buffered-exec")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-live-buffered-exec")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "live-buffered-exec-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	session, replayed, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_live_buffered_exec", StreamID: "stream_live_buffered_exec",
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, Generation: sandbox.Generation,
		RequestID: "request-live-buffered-exec",
		Kind:      "exec", Operation: "exec", IdempotencyKey: "live-buffered-exec",
		RequestHash: "live-buffered-exec-hash", DeadlineAt: now.Add(time.Minute),
		MaximumResponseBytes: 1024,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "printf buffered"},
			DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 1024,
		},
		Request: map[string]any{"command": "printf buffered"}, Now: now,
	})
	if err != nil || replayed || session.Transport != contracts.DataPlaneTransportProxied {
		t.Fatalf("buffered Exec admission = %#v, replayed=%t, error=%v", session, replayed, err)
	}
	if delivery, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now,
	); err != nil || found {
		t.Fatalf("buffered Exec relay delivery = %#v, found=%t, error=%v", delivery, found, err)
	}
	completed, err := relay.CompleteDataPlaneSession(t.Context(), runnercontrol.DataPlaneCompletion{
		TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef, SessionID: session.ID,
		Exec: &runnerv1.ExecBufferedResult{
			Stdout: []byte{0, 1, 0xff}, Stderr: []byte("diagnostic"),
			Terminal: &runnerv1.ExecTerminal{
				Kind:     runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
				ExitCode: 0, ElapsedMilliseconds: 12,
			},
		},
		Now: now.Add(12 * time.Millisecond),
	})
	if err != nil || completed.State != "completed" ||
		!bytes.Equal(completed.Stdout, []byte{0, 1, 0xff}) ||
		!bytes.Equal(completed.Stderr, []byte("diagnostic")) {
		t.Fatalf("buffered Exec completion = %#v, %v", completed, err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var outcomeRows, frameRows int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_sessions WHERE id=$1`, session.ID,
	).Scan(&outcomeRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames WHERE session_id=$1`, session.ID,
	).Scan(&frameRows); err != nil {
		t.Fatal(err)
	}
	if outcomeRows != 1 || frameRows != 0 {
		t.Fatalf("buffered Exec persistence = outcome rows %d, frame rows %d", outcomeRows, frameRows)
	}
}

func TestPostgresDirectFileAdmissionIsDurableAndPayloadFree(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "direct-file")
	profile := createGrantedProfileWithDataPlaneTransport(
		t, controlPlane, databaseStore, admin, account, "profile-direct-file",
		contracts.DataPlaneTransportDirect,
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "direct-file-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	seedFixtureHomeRunner(t, "default-pool", seed.RunnerID)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners SET data_plane_address=$2 WHERE id=$1`, seed.RunnerID,
		`{"address":"10.9.8.7:7443","certificateSpkiSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	); err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	session, replayed, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_direct_file_" + sandbox.ID, StreamID: "stream_direct_file_" + sandbox.ID,
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, RequestID: "request-direct-file",
		Generation: sandbox.Generation, Kind: "file", Operation: "mkdir",
		IdempotencyKey: "direct-file", RequestHash: "direct-file-hash",
		DeadlineAt: now.Add(30 * time.Second),
		FileOpen: &runnerv1.FileOpen{
			Operation:             runnerv1.FileOperation_FILE_OPERATION_MKDIR,
			WorkspaceRelativePath: "direct", Recursive: true,
		},
		CredentialDigest: bytes.Repeat([]byte{0x42}, 32),
		Request:          map[string]any{"path": "direct"}, Now: now,
	})
	if err != nil || replayed || session.Transport != contracts.DataPlaneTransportDirect ||
		session.DataPlaneAddress != "10.9.8.7:7443" {
		t.Fatalf("direct File admission = %#v, replayed=%t, error=%v", session, replayed, err)
	}
	var commandPayload []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT payload FROM secondbox.runner_commands WHERE id=$1`, session.ID+"_direct_open",
	).Scan(&commandPayload); err != nil {
		t.Fatal(err)
	}
	var command runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(commandPayload, &command); err != nil {
		t.Fatal(err)
	}
	open := command.GetDataPlaneDirectOpen()
	if open == nil || open.Kind != runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE ||
		open.OperationId != session.ID || open.StreamId != session.StreamID ||
		!bytes.Equal(open.CredentialDigest, bytes.Repeat([]byte{0x42}, 32)) {
		t.Fatalf("direct File command = %#v", open)
	}
	var frameRows int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames WHERE session_id=$1`, session.ID,
	).Scan(&frameRows); err != nil {
		t.Fatal(err)
	}
	if frameRows != 0 {
		t.Fatalf("direct File frame rows = %d, want zero", frameRows)
	}
}

func TestPostgresLiveStreamingExecTerminalOutcomesWriteNoFrames(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "live-stream-outcomes")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-live-stream-outcomes")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "live-stream-outcomes-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seedRelayReadyAssignment(t, sandbox, now)
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second, Retention: time.Hour,
		MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tests := []struct {
		name         string
		terminalKind string
		finish       func(runnercontrol.DataPlaneSession) (runnercontrol.DataPlaneSession, error)
	}{
		{
			name:         "output-exhausted",
			terminalKind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED.String(),
			finish: func(session runnercontrol.DataPlaneSession) (runnercontrol.DataPlaneSession, error) {
				return relay.CompleteDataPlaneSession(t.Context(), runnercontrol.DataPlaneCompletion{
					TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef, SessionID: session.ID,
					Exec: &runnerv1.ExecBufferedResult{Terminal: &runnerv1.ExecTerminal{
						Kind:       runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED,
						LimitBytes: 12,
					}},
					Now: now.Add(time.Second),
				})
			},
		},
		{
			name:         "cancelled",
			terminalKind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(),
			finish: func(session runnercontrol.DataPlaneSession) (runnercontrol.DataPlaneSession, error) {
				if changed, err := relay.CancelDataPlaneSession(
					t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
					"public streaming client cancellation", now.Add(time.Second),
				); err != nil || !changed {
					return runnercontrol.DataPlaneSession{}, errors.New("stream cancellation was not recorded")
				}
				return relay.CompleteDataPlaneSession(t.Context(), runnercontrol.DataPlaneCompletion{
					TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef, SessionID: session.ID,
					Exec: &runnerv1.ExecBufferedResult{Terminal: &runnerv1.ExecTerminal{
						Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
					}},
					Now: now.Add(2 * time.Second),
				})
			},
		},
		{
			name:         "deadline-exceeded",
			terminalKind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED.String(),
			finish: func(session runnercontrol.DataPlaneSession) (runnercontrol.DataPlaneSession, error) {
				return relay.ExpireDataPlaneSession(
					t.Context(), principal.TenantRef, principal.SubjectRef,
					session.ID, now.Add(31*time.Second),
				)
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			session, replayed, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
				ID:        "dps_stream_" + testCase.name + "_" + sandbox.ID,
				StreamID:  "stream_" + testCase.name + "_" + sandbox.ID,
				TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
				SubjectRef: principal.SubjectRef, RequestID: "request-" + testCase.name,
				Generation: sandbox.Generation, Kind: "exec", Operation: "exec-stream",
				IdempotencyKey: "stream-" + testCase.name,
				RequestHash:    "stream-" + testCase.name + "-hash",
				DeadlineAt:     now.Add(30 * time.Second), MaximumResponseBytes: 12,
				StreamWindowBytes: 12, UseProfileRequestLimit: true,
				DeferResponseCredit: true,
				ExecOpen: &runnerv1.ExecOpen{
					Command: &runnerv1.ExecOpen_Shell{Shell: "true"}, Streaming: true,
					DeadlineUnixMs:   uint64(now.Add(30 * time.Second).UnixMilli()),
					OutputLimitBytes: 12,
				},
				Request: map[string]any{"kind": testCase.name}, Now: now,
			})
			if err != nil || replayed {
				t.Fatalf("stream admission = %#v, replayed=%t, error=%v", session, replayed, err)
			}
			terminal, err := testCase.finish(session)
			if err != nil || terminal.TerminalKind != testCase.terminalKind {
				t.Fatalf("stream terminal = %#v, error=%v", terminal, err)
			}
			var frameRows int64
			if err := pool.QueryRow(t.Context(), `
				SELECT count(*) FROM secondbox.data_plane_frames WHERE session_id=$1`, session.ID,
			).Scan(&frameRows); err != nil {
				t.Fatal(err)
			}
			if frameRows != 0 {
				t.Fatalf("stream frame rows = %d, want zero", frameRows)
			}
		})
	}
}

type relayReadySeed struct {
	Fence            *runnerv1.AssignmentFence
	RunnerID         string
	CredentialSerial string
	ConnectionOne    string
	ConnectionTwo    string
}

func assertRelayCorrelation(
	t *testing.T,
	correlation *runnerv1.Correlation,
	requestID string,
	operationID string,
	sandboxID string,
	leaseID string,
	seed relayReadySeed,
) {
	t.Helper()
	if correlation == nil ||
		correlation.RequestId != requestID ||
		correlation.OperationId != operationID ||
		correlation.SandboxId != sandboxID ||
		correlation.InstanceId != seed.Fence.InstanceId ||
		correlation.SandboxGeneration != seed.Fence.SandboxGeneration ||
		correlation.AssignmentId != seed.Fence.AssignmentId ||
		correlation.LeaseId != leaseID ||
		correlation.RunnerId != seed.RunnerID {
		t.Fatalf("relay correlation = %#v", correlation)
	}
}

func seedRelayReadyAssignment(
	t *testing.T,
	sandbox contracts.Sandbox,
	now time.Time,
) relayReadySeed {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runnerID := "run_relay_" + sandbox.ID
	credentialSerial := "serial_relay_" + sandbox.ID
	connectionOne := "connection_relay_1_" + sandbox.ID
	connectionTwo := "connection_relay_2_" + sandbox.ID
	fence := &runnerv1.AssignmentFence{
		AssignmentId: "asn_relay_" + sandbox.ID, SandboxId: sandbox.ID,
		InstanceId: "ins_relay_" + sandbox.ID, SandboxGeneration: uint64(sandbox.Generation),
		FencingToken: []byte("postgres-relay-fence"),
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES ($1,$2,$3,'ready','ready','',$4,$4,$4,$4,$5,NULL)`,
		fence.InstanceId, sandbox.ID, sandbox.Generation, now, now.Add(time.Hour),
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
		) VALUES ($1,$2,$3,$4,$5,'firecracker','fc-relay',$6,$7,'ready',
		          '{}','{}','{}','',0,3,$8,$8,'',$8,$8,1,$9,$9)`,
		fence.AssignmentId, sandbox.ID, fence.InstanceId, runnerID, sandbox.ProfileRevisionID,
		sandbox.Generation, fence.FencingToken, now.Add(time.Hour), now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',desired_state='running',current_instance_id=$2,updated_at=$3
		WHERE id=$1`,
		sandbox.ID, fence.InstanceId, now,
	); err != nil {
		t.Fatal(err)
	}
	for _, connectionID := range []string{connectionOne, connectionTwo} {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO secondbox.runner_connections (
				id,runner_id,credential_serial,protocol_version,state,last_sequence,
				last_control_sequence,connected_at,last_seen_at,disconnected_at
			) VALUES ($1,$2,$3,1,'active',0,0,$4,$4,NULL)`,
			connectionID, runnerID, credentialSerial, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	return relayReadySeed{
		Fence: fence, RunnerID: runnerID, CredentialSerial: credentialSerial,
		ConnectionOne: connectionOne, ConnectionTwo: connectionTwo,
	}
}

func relayExecOutput(
	fence *runnerv1.AssignmentFence,
	operationID string,
	streamID string,
	sequence uint64,
	content []byte,
) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
			Fence: fence, OperationId: operationID, StreamId: streamID, Sequence: sequence,
			Payload: &runnerv1.ExecFrame_Output{Output: &runnerv1.ExecOutput{
				Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, Data: content,
			}},
		}},
	}
}

func TestPostgresRelayPrunesTerminalFramesWithoutPruningReplay(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "relay-frame-retention",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-relay-frame-retention",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "relay-frame-retention-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation,
		"relay-frame-retention-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(
		t.Context(), runnercontrol.PostgresFrameRelayConfig{
			DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
			Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	admission := runnercontrol.DataPlaneAdmission{
		ID: "dps_relay_frame_retention", StreamID: "stream_relay_frame_retention",
		TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef,
		SandboxID: sandbox.ID, Generation: sandbox.Generation, LeaseID: lease.ID,
		RequestID: "request-relay-frame-retention", Kind: "terminal", Operation: "terminal",
		IdempotencyKey: "relay-frame-retention", RequestHash: "relay-frame-retention-hash",
		DeadlineAt: now.Add(time.Minute), MaximumResponseBytes: 1024,
		UseProfileStreamWindow: true, DeferResponseCredit: true,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "printf retained"},
			DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 1024,
			AllocatePty: true, Streaming: true, PtyRows: 24, PtyColumns: 80,
		},
		Request: map[string]any{"command": "printf retained"}, Now: now,
	}
	session, replayed, err := relay.AdmitDataPlane(t.Context(), admission)
	if err != nil || replayed {
		t.Fatalf("admission = %#v replayed=%t error=%v", session, replayed, err)
	}
	for {
		delivery, found, err := relay.ClaimOutboundFrame(
			t.Context(), seed.RunnerID, seed.ConnectionOne, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if err := relay.MarkOutboundFrameDelivered(
			t.Context(), delivery.ID, seed.ConnectionOne, delivery.ClaimAttempt, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	terminal := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
			Fence: seed.Fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: 1,
			Payload: &runnerv1.PtyFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
				Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
			}},
		}},
	}
	completedAt := now.Add(2 * time.Second)
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne, Message: terminal,
	}, completedAt); err != nil || !inserted {
		t.Fatalf("terminal persistence = %t, %v", inserted, err)
	}
	if changed, err := relay.SweepDataPlane(t.Context(), completedAt, 100); err != nil || !changed {
		t.Fatalf("frame cleanup = %t, %v", changed, err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var frames int
	var cleanupCompleted *time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT count(frame.id),max(session.frame_cleanup_completed_at)
		FROM secondbox.data_plane_sessions AS session
		LEFT JOIN secondbox.data_plane_frames AS frame ON frame.session_id=session.id
		WHERE session.id=$1`, session.ID).Scan(&frames, &cleanupCompleted); err != nil {
		t.Fatal(err)
	}
	if frames != 0 || cleanupCompleted == nil {
		t.Fatalf("post-cleanup rows=%d cleanup=%v", frames, cleanupCompleted)
	}
	current, err := relay.GetDataPlaneSession(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
	)
	if err != nil || len(current.Stdout) != 0 || current.NextClientSequence != 0 {
		t.Fatalf("materialised result after cleanup = %#v error=%v", current, err)
	}
	replayedSession, replayed, err := relay.AdmitDataPlane(t.Context(), admission)
	if err != nil || !replayed || len(replayedSession.Stdout) != 0 ||
		replayedSession.NextClientSequence != current.NextClientSequence {
		t.Fatalf("admission replay after cleanup = %#v replayed=%t error=%v", replayedSession, replayed, err)
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne, Message: terminal,
	}, completedAt.Add(time.Second)); err != nil || inserted {
		t.Fatalf("exact terminal retransmission = %t, %v", inserted, err)
	}
	changedTerminal := proto.Clone(terminal).(*runnerv1.RunnerToControlPlane)
	changedTerminal.GetPty().GetTerminal().SafeDetail = "changed"
	if _, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne, Message: changedTerminal,
	}, completedAt.Add(time.Second)); !errors.Is(err, runnercontrol.ErrRelaySequence) {
		t.Fatalf("changed terminal retransmission error = %v", err)
	}
}
