package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestPostgresRelayPublicCancellationIsAtomicAndKeyScoped(t *testing.T) {
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
	if err != nil || replayed || first.State != "cancelling" {
		t.Fatalf("first public cancellation = %#v, replayed=%t, error=%v", first, replayed, err)
	}
	closingNewKey := cancellation
	closingNewKey.IdempotencyKey = "relay-public-cancel-new-key-closing"
	closingNewKey.Now = now.Add(1500 * time.Millisecond)
	stillClosing, replayed, err := relay.CancelPublicDataPlaneSession(t.Context(), closingNewKey)
	if err != nil || replayed || stillClosing.State != "cancelling" {
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

func TestPostgresRelayDurablyFencesSequencesAndReconnectDelivery(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "postgres-relay")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-postgres-relay")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "postgres-relay-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	fence := seed.Fence
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	session, replayed, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_postgres_relay", StreamID: "stream_postgres_relay",
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, Generation: sandbox.Generation,
		RequestID: "request-postgres-relay",
		Kind:      "exec", Operation: "exec", IdempotencyKey: "postgres-relay-exec",
		RequestHash: "request-hash", DeadlineAt: now.Add(time.Minute),
		MaximumResponseBytes: 1024, MaximumRequestBytes: 0,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "printf relay"},
			DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 1024,
		},
		Now: now,
	})
	if err != nil || replayed || session.AssignmentID != fence.AssignmentId {
		t.Fatalf("admission = %#v, replayed=%t, error=%v", session, replayed, err)
	}
	delivery, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now,
	)
	if err != nil || !found || delivery.Message.GetExec().GetOpen() == nil {
		t.Fatalf("first claim = %#v, found=%t, error=%v", delivery, found, err)
	}
	assertRelayCorrelation(
		t, delivery.Message.GetExec().GetCorrelation(), "request-postgres-relay",
		session.ID, sandbox.ID, "", seed,
	)
	reclaimed, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionTwo, now.Add(time.Second),
	)
	if err != nil || !found || reclaimed.ID != delivery.ID {
		t.Fatalf("reconnect claim = %#v, found=%t, error=%v", reclaimed, found, err)
	}
	if !proto.Equal(
		delivery.Message.GetExec().GetCorrelation(),
		reclaimed.Message.GetExec().GetCorrelation(),
	) {
		t.Fatalf("reconnect correlation changed: first=%v replay=%v",
			delivery.Message.GetExec().GetCorrelation(),
			reclaimed.Message.GetExec().GetCorrelation(),
		)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), reclaimed.ID, seed.ConnectionTwo,
		reclaimed.ClaimAttempt, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	output := relayExecOutput(fence, session.ID, session.StreamID, 1, []byte{0, 1, 0xff})
	inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionTwo, Message: output,
	}, now.Add(2*time.Second))
	if err != nil || !inserted {
		t.Fatalf("output persist = %t, %v", inserted, err)
	}
	inserted, err = relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionTwo, Message: output,
	}, now.Add(2*time.Second))
	if err != nil || inserted {
		t.Fatalf("duplicate output persist = %t, %v", inserted, err)
	}
	if _, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionTwo,
		Message: relayExecOutput(fence, session.ID, session.StreamID, 3, []byte("gap")),
	}, now.Add(2*time.Second)); !errors.Is(err, runnercontrol.ErrRelaySequence) {
		t.Fatalf("sequence gap error = %v", err)
	}
	stale := proto.Clone(fence).(*runnerv1.AssignmentFence)
	stale.SandboxGeneration++
	if _, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionTwo,
		Message: relayExecOutput(stale, session.ID, session.StreamID, 2, []byte("stale")),
	}, now.Add(2*time.Second)); !errors.Is(err, runnercontrol.ErrRelayFence) {
		t.Fatalf("stale fence error = %v", err)
	}
	terminal := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
			Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: 2,
			Payload: &runnerv1.ExecFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
				Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED, ExitCode: 0,
			}},
		}},
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionTwo, Message: terminal,
	}, now.Add(3*time.Second)); err != nil || !inserted {
		t.Fatalf("terminal persist = %t, %v", inserted, err)
	}
	completed, err := relay.GetDataPlaneSession(t.Context(), principal.TenantRef, principal.SubjectRef, session.ID)
	if err != nil || completed.State != "completed" ||
		!bytes.Equal(completed.Stdout, []byte{0, 1, 0xff}) {
		t.Fatalf("completed session = %#v, %v", completed, err)
	}
	var lastActivityAt time.Time
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var completedActivityState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.activity_sessions WHERE id=$1`,
		session.ID,
	).Scan(&completedActivityState); err != nil {
		t.Fatal(err)
	}
	if completedActivityState != contracts.ActivitySessionStateClosed {
		t.Fatalf("terminal relay activity state = %q", completedActivityState)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT last_activity_at FROM secondbox.sandboxes WHERE id=$1`,
		sandbox.ID,
	).Scan(&lastActivityAt); err != nil {
		t.Fatal(err)
	}
	if !lastActivityAt.Equal(now.Add(3 * time.Second)) {
		t.Fatalf("terminal activity = %s", lastActivityAt)
	}
	if _, err := relay.GetDataPlaneSession(t.Context(), principal.TenantRef, principal.SubjectRef, session.ID); err != nil {
		t.Fatal(err)
	}
	var afterPolling time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT last_activity_at FROM secondbox.sandboxes WHERE id=$1`,
		sandbox.ID,
	).Scan(&afterPolling); err != nil {
		t.Fatal(err)
	}
	if !afterPolling.Equal(lastActivityAt) {
		t.Fatalf("read polling changed activity from %s to %s", lastActivityAt, afterPolling)
	}

	limited, _, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_postgres_limited_" + sandbox.ID, StreamID: "stream_postgres_limited_" + sandbox.ID,
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, Generation: sandbox.Generation,
		RequestID: "request-postgres-limited",
		Kind:      "exec", Operation: "exec", IdempotencyKey: "postgres-relay-limited",
		RequestHash: "limited-request-hash", DeadlineAt: now.Add(time.Minute),
		MaximumResponseBytes: 2,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "printf oversized"},
			DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 2,
		},
		Now: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		delivery, found, err := relay.ClaimOutboundFrame(
			t.Context(), limited.RunnerID, seed.ConnectionOne, now.Add(4*time.Second),
		)
		if err != nil || !found {
			t.Fatalf("limited session initial delivery = %#v, %t, %v", delivery, found, err)
		}
		if err := relay.MarkOutboundFrameDelivered(
			t.Context(), delivery.ID, seed.ConnectionOne,
			delivery.ClaimAttempt, now.Add(4*time.Second),
		); err != nil {
			t.Fatal(err)
		}
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: limited.RunnerID, ConnectionID: seed.ConnectionOne,
		Message: relayExecOutput(fence, limited.ID, limited.StreamID, 1, []byte("too large")),
	}, now.Add(5*time.Second)); err != nil || inserted {
		t.Fatalf("over-limit response persistence = %t, %v", inserted, err)
	}
	limitedState, err := relay.GetDataPlaneSession(t.Context(), principal.TenantRef, principal.SubjectRef, limited.ID)
	if err != nil || limitedState.State != "cancelling" ||
		limitedState.TerminalKind != runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED.String() {
		t.Fatalf("over-limit cancellation state = %#v, %v", limitedState, err)
	}
	limitCancel, found, err := relay.ClaimOutboundFrame(
		t.Context(), limited.RunnerID, seed.ConnectionOne, now.Add(5*time.Second),
	)
	if err != nil || !found || limitCancel.Message.GetExec().GetCancel() == nil {
		t.Fatalf("over-limit cancel claim = %#v, %t, %v", limitCancel, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), limitCancel.ID, seed.ConnectionOne,
		limitCancel.ClaimAttempt, now.Add(5*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	limitTerminal := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
			Fence: fence, OperationId: limited.ID, StreamId: limited.StreamID, Sequence: 1,
			Payload: &runnerv1.ExecFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
				Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
			}},
		}},
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: limited.RunnerID, ConnectionID: seed.ConnectionOne, Message: limitTerminal,
	}, now.Add(6*time.Second)); err != nil || !inserted {
		t.Fatalf("over-limit terminal proof = %t, %v", inserted, err)
	}

	cancelling, _, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_postgres_cancel", StreamID: "stream_postgres_cancel",
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, Generation: sandbox.Generation,
		RequestID: "request-postgres-cancel",
		Kind:      "exec", Operation: "exec", IdempotencyKey: "postgres-relay-cancel",
		RequestHash: "cancel-request-hash", DeadlineAt: now.Add(30 * time.Second),
		MaximumResponseBytes: 1024,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "sleep 60"},
			DeadlineUnixMs: uint64(now.Add(30 * time.Second).UnixMilli()), OutputLimitBytes: 1024,
		},
		Now: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		delivery, found, err := relay.ClaimOutboundFrame(
			t.Context(), cancelling.RunnerID, seed.ConnectionOne, now.Add(4*time.Second),
		)
		if err != nil || !found {
			t.Fatalf("cancel session initial delivery = %#v, %t, %v", delivery, found, err)
		}
		if err := relay.MarkOutboundFrameDelivered(
			t.Context(), delivery.ID, seed.ConnectionOne,
			delivery.ClaimAttempt, now.Add(4*time.Second),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET state='draining',updated_at=$2 WHERE id=$1`,
		sandbox.ID, now.Add(5*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	cancelCount, err := relay.CancelSandboxSessions(
		t.Context(), sandbox.ID, sandbox.Generation,
		"Sandbox drain grace expired", now.Add(30*time.Second),
	)
	if err != nil || cancelCount != 1 {
		t.Fatalf("forced drain cancellation count = %d, %v", cancelCount, err)
	}
	forcedCancellation, err := relay.GetDataPlaneSession(
		t.Context(), principal.TenantRef, principal.SubjectRef, cancelling.ID,
	)
	if err != nil || forcedCancellation.State != "cancelling" ||
		forcedCancellation.CompletedAt != nil ||
		forcedCancellation.TerminalKind !=
			runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String() {
		t.Fatalf("forced drain cancellation = %#v, %v", forcedCancellation, err)
	}
	cancelDelivery, found, err := relay.ClaimOutboundFrame(
		t.Context(), cancelling.RunnerID, seed.ConnectionOne, now.Add(30*time.Second),
	)
	if err != nil || !found || cancelDelivery.Message.GetExec().GetCancel() == nil {
		t.Fatalf("cancel claim = %#v, %t, %v", cancelDelivery, found, err)
	}
	cancelReconnect, found, err := relay.ClaimOutboundFrame(
		t.Context(), cancelling.RunnerID, seed.ConnectionTwo, now.Add(31*time.Second),
	)
	if err != nil || !found || cancelReconnect.ID != cancelDelivery.ID {
		t.Fatalf("cancel reconnect claim = %#v, %t, %v", cancelReconnect, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), cancelReconnect.ID, seed.ConnectionTwo,
		cancelReconnect.ClaimAttempt, now.Add(31*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	cancelTerminal := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
			Fence: fence, OperationId: cancelling.ID, StreamId: cancelling.StreamID, Sequence: 1,
			Payload: &runnerv1.ExecFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
				Kind:                runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
				ElapsedMilliseconds: 30_000,
			}},
		}},
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: cancelling.RunnerID, ConnectionID: seed.ConnectionTwo, Message: cancelTerminal,
	}, now.Add(32*time.Second)); err != nil || !inserted {
		t.Fatalf("draining cancel terminal = %t, %v", inserted, err)
	}
	cancelled, err := relay.GetDataPlaneSession(t.Context(), principal.TenantRef, principal.SubjectRef, cancelling.ID)
	if err != nil || cancelled.State != "completed" ||
		cancelled.TerminalKind != runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String() {
		t.Fatalf("cancelled session = %#v, %v", cancelled, err)
	}
}

func TestPostgresRelayPreservesDistinctOperationCorrelationAcrossReconnect(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "relay-correlation")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-relay-correlation")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "relay-correlation-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	execLease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "relay-correlation-exec-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	fileLease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "relay-correlation-file-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	if execLease.ID == fileLease.ID {
		t.Fatalf("distinct Lease admissions returned %q", execLease.ID)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	execSession, _, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_correlation_exec_" + sandbox.ID, StreamID: "stream_correlation_exec_" + sandbox.ID,
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, RequestID: "request-correlation-exec",
		LeaseID: execLease.ID, Generation: sandbox.Generation,
		Kind: "exec", Operation: "exec", IdempotencyKey: "relay-correlation-exec",
		RequestHash: "relay-correlation-exec-hash", DeadlineAt: now.Add(30 * time.Second),
		MaximumResponseBytes: 1024,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "printf correlation"},
			DeadlineUnixMs: uint64(now.Add(30 * time.Second).UnixMilli()), OutputLimitBytes: 1024,
		},
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	fileSession, _, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_correlation_file_" + sandbox.ID, StreamID: "stream_correlation_file_" + sandbox.ID,
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, RequestID: "request-correlation-file",
		LeaseID: fileLease.ID, Generation: sandbox.Generation,
		Kind: "file", Operation: "mkdir", IdempotencyKey: "relay-correlation-file",
		RequestHash: "relay-correlation-file-hash", DeadlineAt: now.Add(30 * time.Second),
		FileOpen: &runnerv1.FileOpen{
			Operation:             runnerv1.FileOperation_FILE_OPERATION_MKDIR,
			WorkspaceRelativePath: "correlation", Recursive: false,
		},
		Now: now.Add(time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}

	execFirst, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(time.Second),
	)
	if err != nil || !found || execFirst.Message.GetExec().GetOpen() == nil {
		t.Fatalf("Exec initial claim = %#v, %t, %v", execFirst, found, err)
	}
	execReplay, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionTwo, now.Add(2*time.Second),
	)
	if err != nil || !found || execReplay.ID != execFirst.ID {
		t.Fatalf("Exec reconnect claim = %#v, %t, %v", execReplay, found, err)
	}
	assertRelayCorrelation(
		t, execReplay.Message.GetExec().GetCorrelation(), "request-correlation-exec",
		execSession.ID, sandbox.ID, execLease.ID, seed,
	)
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), execReplay.ID, seed.ConnectionTwo,
		execReplay.ClaimAttempt, now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	execCredit, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionTwo, now.Add(2*time.Second),
	)
	if err != nil || !found || execCredit.Message.GetExec().GetCredit() == nil {
		t.Fatalf("Exec credit claim = %#v, %t, %v", execCredit, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), execCredit.ID, seed.ConnectionTwo,
		execCredit.ClaimAttempt, now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	fileFirst, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(2*time.Second),
	)
	if err != nil || !found || fileFirst.Message.GetFile().GetOpen() == nil {
		t.Fatalf("File initial claim = %#v, %t, %v", fileFirst, found, err)
	}
	fileReplay, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionTwo, now.Add(3*time.Second),
	)
	if err != nil || !found || fileReplay.ID != fileFirst.ID {
		t.Fatalf("File reconnect claim = %#v, %t, %v", fileReplay, found, err)
	}
	assertRelayCorrelation(
		t, fileReplay.Message.GetFile().GetCorrelation(), "request-correlation-file",
		fileSession.ID, sandbox.ID, fileLease.ID, seed,
	)
}

func TestPostgresRelayDurablySequencesPublicStreamingExecFrames(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "public-stream")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-public-stream")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "public-stream-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second, Retention: time.Hour,
		MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	session, _, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_stream_" + sandbox.ID, StreamID: "stream_public_" + sandbox.ID,
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, RequestID: "request-public-stream",
		Generation: sandbox.Generation, Kind: "exec", Operation: "exec-stream",
		RequestHash: "public-stream-hash", DeadlineAt: now.Add(time.Minute),
		MaximumResponseBytes: 12, StreamWindowBytes: 12,
		UseProfileRequestLimit: true, DeferResponseCredit: true,
		ExecOpen: &runnerv1.ExecOpen{
			DeadlineUnixMs:   uint64(now.Add(time.Minute).UnixMilli()),
			OutputLimitBytes: 12,
			Command:          &runnerv1.ExecOpen_Shell{Shell: "read value; printf %s \"$value\""},
		},
		Request: map[string]any{"kind": "public-stream"}, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	open, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(time.Second),
	)
	if err != nil || !found || open.Message.GetExec().GetOpen() == nil {
		t.Fatalf("stream open claim = %#v, %t, %v", open, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), open.ID, seed.ConnectionOne, open.ClaimAttempt, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if unexpected, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(time.Second),
	); err != nil || found {
		t.Fatalf("stream received ungranted output credit = %#v, %t, %v", unexpected, found, err)
	}
	if inserted, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{Sequence: 0, Input: []byte("hello")}, now.Add(2*time.Second),
	); err != nil || !inserted {
		t.Fatalf("append stdin = %t, %v", inserted, err)
	}
	if inserted, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{Sequence: 0, Input: []byte("hello")}, now.Add(2*time.Second),
	); err != nil || inserted {
		t.Fatalf("duplicate stdin = %t, %v", inserted, err)
	}
	if _, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{Sequence: 3, Credit: 12}, now.Add(2*time.Second),
	); !errors.Is(err, runnercontrol.ErrRelaySequence) {
		t.Fatalf("gapped public sequence error = %v", err)
	}
	if inserted, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{
			Sequence: 1, Input: []byte{}, EndInput: true,
		}, now.Add(2*time.Second),
	); err != nil || !inserted {
		t.Fatalf("append stdin EOF = %t, %v", inserted, err)
	}
	if inserted, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{
			Sequence: 1, Input: []byte{}, EndInput: true,
		}, now.Add(2*time.Second),
	); err != nil || inserted {
		t.Fatalf("duplicate stdin EOF = %t, %v", inserted, err)
	}
	if _, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{Sequence: 2, Input: []byte("after EOF")},
		now.Add(2*time.Second),
	); !errors.Is(err, runnercontrol.ErrRelaySequence) {
		t.Fatalf("stdin after EOF error = %v", err)
	}
	if _, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{Sequence: 2, Credit: 13}, now.Add(2*time.Second),
	); !errors.Is(err, runnercontrol.ErrRelayFrameLimit) {
		t.Fatalf("credit beyond the negotiated slow-client window error = %v", err)
	}
	if inserted, err := relay.AppendExecClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID,
		runnercontrol.ExecClientFrame{Sequence: 2, Credit: 12}, now.Add(2*time.Second),
	); err != nil || !inserted {
		t.Fatalf("append credit = %t, %v", inserted, err)
	}
	input, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(3*time.Second),
	)
	if err != nil || !found || string(input.Message.GetExec().GetInput().GetData()) != "hello" ||
		input.Message.GetExec().GetInput().GetEndOfInput() {
		t.Fatalf("stream stdin claim = %#v, %t, %v", input, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), input.ID, seed.ConnectionOne, input.ClaimAttempt, now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	endInput, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(3*time.Second),
	)
	if err != nil || !found || endInput.Message.GetExec().GetInput() == nil ||
		!endInput.Message.GetExec().GetInput().GetEndOfInput() {
		t.Fatalf("stream stdin EOF claim = %#v, %t, %v", endInput, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), endInput.ID, seed.ConnectionOne, endInput.ClaimAttempt,
		now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	credit, found, err := relay.ClaimOutboundFrame(
		t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(3*time.Second),
	)
	if err != nil || !found || credit.Message.GetExec().GetCredit().GetByteCount() != 12 {
		t.Fatalf("stream credit claim = %#v, %t, %v", credit, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), credit.ID, seed.ConnectionOne, credit.ClaimAttempt, now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	for index, output := range []*runnerv1.ExecOutput{
		{Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, Data: []byte("hello")},
		{Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR, Data: []byte("!")},
	} {
		message := &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
				Fence: seed.Fence, OperationId: session.ID, StreamId: session.StreamID,
				Sequence: uint64(index + 1), Payload: &runnerv1.ExecFrame_Output{Output: output},
			}},
		}
		if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
			RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne, Message: message,
		}, now.Add(4*time.Second)); err != nil || !inserted {
			t.Fatalf("persist stream output %d = %t, %v", index, inserted, err)
		}
	}
	terminal := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
			Fence: seed.Fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: 3,
			Payload: &runnerv1.ExecFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
				Kind:       runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED,
				LimitBytes: 12,
			}},
		}},
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne, Message: terminal,
	}, now.Add(5*time.Second)); err != nil || !inserted {
		t.Fatalf("persist stream terminal = %t, %v", inserted, err)
	}
	frames, err := relay.ListExecServerFrames(t.Context(), principal.TenantRef, principal.SubjectRef, session.ID, -1, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 ||
		string(frames[0].Output.GetData()) != "hello" ||
		string(frames[1].Output.GetData()) != "!" ||
		frames[2].Terminal.GetKind() != runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
		t.Fatalf("public stream frames = %#v", frames)
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
