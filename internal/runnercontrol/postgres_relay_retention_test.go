package runnercontrol

import (
	"errors"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestRelayFrameSweepBoundsRowsBytesAndFinalization(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC)
	insertRelayRetentionSession(t, store.pool, "retention-batch", now, now.Add(time.Hour))
	for sequence, size := range []int{6, 6, 20} {
		insertRelayRetentionFrame(t, store.pool, "retention-batch", int64(sequence+1), size, "delivered", nil)
	}
	relay := &PostgresFrameRelay{pool: store.pool}
	for iteration, wantRemaining := range []int64{2, 1, 0} {
		changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 10, 10)
		if err != nil || !changed {
			t.Fatalf("frame sweep %d = %t, %v", iteration+1, changed, err)
		}
		var remaining int64
		if err := store.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM secondbox.data_plane_frames
			WHERE session_id='retention-batch'`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != wantRemaining {
			t.Fatalf("frame sweep %d remaining = %d, want %d", iteration+1, remaining, wantRemaining)
		}
	}
	var cleanupCompleted *time.Time
	if err := store.pool.QueryRow(t.Context(), `
		SELECT frame_cleanup_completed_at FROM secondbox.data_plane_sessions
		WHERE id='retention-batch'`).Scan(&cleanupCompleted); err != nil {
		t.Fatal(err)
	}
	if cleanupCompleted == nil {
		t.Fatal("frame cleanup was not finalized after the final bounded batch")
	}
	if changed, err := relay.sweepDataPlaneSessions(t.Context(), now, 10); err != nil || changed {
		t.Fatalf("session cleanup before retention = %t, %v", changed, err)
	}
	if changed, err := relay.sweepDataPlaneSessions(t.Context(), now.Add(time.Hour), 10); err != nil || !changed {
		t.Fatalf("session cleanup after retention = %t, %v", changed, err)
	}
}

func TestRelayFrameSweepBoundsFrameRowsAndSessionCandidates(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 2, 15, 30, 0, 0, time.UTC)
	relay := &PostgresFrameRelay{pool: store.pool}
	insertRelayRetentionSession(t, store.pool, "retention-row-bound", now, now.Add(time.Hour))
	for sequence := int64(1); sequence <= 3; sequence++ {
		insertRelayRetentionFrame(t, store.pool, "retention-row-bound", sequence, 1, "delivered", nil)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 2, 1024); err != nil || !changed {
		t.Fatalf("row-bounded first frame sweep = %t, %v", changed, err)
	}
	var remaining int64
	if err := store.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_frames
		WHERE session_id='retention-row-bound'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("row-bounded first frame sweep remaining = %d, want 1", remaining)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 2, 1024); err != nil || !changed {
		t.Fatalf("row-bounded second frame sweep = %t, %v", changed, err)
	}

	for index := 0; index < 3; index++ {
		insertRelayRetentionSession(
			t, store.pool, fmt.Sprintf("retention-candidate-%d", index), now, now.Add(time.Hour),
		)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 2, 1024); err != nil || !changed {
		t.Fatalf("candidate-bounded first frame sweep = %t, %v", changed, err)
	}
	var finalized int64
	if err := store.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_sessions
		WHERE id LIKE 'retention-candidate-%' AND frame_cleanup_completed_at IS NOT NULL`,
	).Scan(&finalized); err != nil {
		t.Fatal(err)
	}
	if finalized != 2 {
		t.Fatalf("candidate-bounded first frame sweep finalized = %d, want 2", finalized)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 2, 1024); err != nil || !changed {
		t.Fatalf("candidate-bounded second frame sweep = %t, %v", changed, err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions SET retain_until=$1
		WHERE id LIKE 'retention-candidate-%'`, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := relay.sweepDataPlaneSessions(t.Context(), now, 2); err != nil || !changed {
		t.Fatalf("candidate-bounded first session sweep = %t, %v", changed, err)
	}
	if err := store.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_sessions
		WHERE id LIKE 'retention-candidate-%'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("candidate-bounded first session sweep remaining = %d, want 1", remaining)
	}
	if changed, err := relay.sweepDataPlaneSessions(t.Context(), now, 2); err != nil || !changed {
		t.Fatalf("candidate-bounded second session sweep = %t, %v", changed, err)
	}
}

func TestRelayFrameSweepLinearizesExpiredDeliveryClaimsOnTheSession(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 2, 16, 0, 0, 0, time.UTC)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_connections (
			id,runner_id,credential_serial,protocol_version,state,last_sequence,
			last_control_sequence,connected_at,last_seen_at,disconnected_at
		) VALUES ('retention-connection','runner-retention','serial',1,'active',0,0,$1,$1,NULL)`, now); err != nil {
		t.Fatal(err)
	}
	relay := &PostgresFrameRelay{pool: store.pool}
	insertRelayRetentionSession(t, store.pool, "retention-mark-wins", now, now.Add(time.Hour))
	activeExpiry := now.Add(time.Minute)
	insertRelayRetentionFrame(t, store.pool, "retention-mark-wins", 1, 10, "claimed", &activeExpiry)
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 10, 100); err != nil || changed {
		t.Fatalf("active claim cleanup = %t, %v", changed, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), "retention-mark-wins-frame-1", "retention-connection", 1, now,
	); err != nil {
		t.Fatalf("delivery mark before claim expiry: %v", err)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 10, 100); err != nil || !changed {
		t.Fatalf("delivered claim cleanup = %t, %v", changed, err)
	}

	insertRelayRetentionSession(t, store.pool, "retention-cleanup-wins", now, now.Add(time.Hour))
	expired := now.Add(-time.Second)
	insertRelayRetentionFrame(t, store.pool, "retention-cleanup-wins", 1, 10, "claimed", &expired)
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 10, 100); err != nil || !changed {
		t.Fatalf("expired claim cleanup = %t, %v", changed, err)
	}
	if err := relay.MarkOutboundFrameDelivered(
		t.Context(), "retention-cleanup-wins-frame-1", "retention-connection", 1, now,
	); !errors.Is(err, ErrRelayDeliveryClaim) {
		t.Fatalf("delivery mark after cleanup error = %v", err)
	}
}

func TestRelayPortTerminalAcknowledgementSurvivesFrameCleanup(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 2, 17, 0, 0, 0, time.UTC)
	seedRunnerConnectionForDataPlaneDisconnect(
		t, store, "retention-port-connection", "retention-port-connection", now,
	)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sandbox-port-retention','tenant','subject','profile','revision','ready','running',
			1,'workspace','instance','{}','{}',1,$1,$1
		)`, now); err != nil {
		t.Fatal(err)
	}
	insertRelayRetentionSession(
		t, store.pool, "retention-port-ack", now.Add(time.Hour), now.Add(2*time.Hour),
	)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions
		SET kind='port',operation='port',sandbox_id='sandbox-port-retention',
		    runner_id='runner-home',next_inbound_sequence=2
		WHERE id='retention-port-ack'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.port_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,data_plane_session_id,
			lease_id,generation,name,guest_port,protocol,transport,credential_digest,
			stream_window_bytes,client_credit_bytes,client_bytes,runner_bytes,state,
			idempotency_key,request_hash,expires_at,created_at,updated_at,connected_at,closed_at,
			acknowledged_inbound_sequence
		) VALUES (
			'retention-port-ack','tenant','subject','sandbox-port-retention','revision',
			'retention-port-ack','',1,'web',8080,'tcp','relay',''::bytea,1024,0,0,0,'closed',
			'retention-port-ack','hash',$2,$1,$1,$1,$1,0
		)`, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	message := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Port{Port: &runnerv1.PortFrame{
			Sequence: 1,
			Payload: &runnerv1.PortFrame_Terminal{Terminal: &runnerv1.PortTerminal{
				Kind: runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED,
			}},
		}},
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,priority,
			state,claim_owner,claim_expires_at,delivery_count,created_at,updated_at,delivered_at,consumed_at
		) VALUES (
			'retention-port-terminal','retention-port-ack','inbound',1,'hash',$1,$2,0,
			'delivered','retention-port-connection',NULL,1,$3,$3,$3,NULL
		)`, payload, len(payload), now); err != nil {
		t.Fatal(err)
	}
	relay := &PostgresFrameRelay{pool: store.pool}
	if err := relay.AcknowledgePortTunnelEvent(
		t.Context(), "tenant", "subject", "retention-port-ack", 1, now,
	); err != nil {
		t.Fatal(err)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 10, 1024); err != nil || !changed {
		t.Fatalf("Port terminal frame cleanup = %t, %v", changed, err)
	}
	if err := relay.AcknowledgePortTunnelEvent(
		t.Context(), "tenant", "subject", "retention-port-ack", 1, now.Add(time.Second),
	); err != nil {
		t.Fatalf("exact Port acknowledgement after cleanup: %v", err)
	}
}

func TestRelayPortByteAcknowledgementAndRunnerDisconnectBothSerializations(t *testing.T) {
	for _, acknowledgeFirst := range []bool{true, false} {
		name := map[bool]string{true: "acknowledgement-first", false: "disconnect-first"}[acknowledgeFirst]
		t.Run(name, func(t *testing.T) {
			store := openRunnerControlDatabase(t)
			now := time.Date(2026, 8, 2, 17, 30, 0, 0, time.UTC)
			seedRelayRetentionPortByte(t, store, now)
			relay := &PostgresFrameRelay{pool: store.pool, maximumFrameBytes: 1 << 20}
			acknowledge := func() {
				t.Helper()
				if err := relay.AcknowledgePortTunnelEvent(
					t.Context(), "tenant", "subject", "retention-port-race", 1, now.Add(time.Second),
				); err != nil {
					t.Fatal(err)
				}
			}
			disconnect := func() {
				t.Helper()
				if err := store.CloseConnection(
					t.Context(), "runner-home", "retention-port-race-connection", now.Add(time.Second),
				); err != nil {
					t.Fatal(err)
				}
			}
			if acknowledgeFirst {
				acknowledge()
				disconnect()
			} else {
				disconnect()
				acknowledge()
			}
			var dataState, portState, creditState string
			var acknowledged, nextOutbound int64
			var frameHorizon time.Time
			if err := store.pool.QueryRow(t.Context(), `
				SELECT session.state,port.state,port.acknowledged_inbound_sequence,
				       session.next_outbound_sequence,session.frames_retain_until,frame.state
				FROM secondbox.data_plane_sessions AS session
				JOIN secondbox.port_sessions AS port ON port.data_plane_session_id=session.id
				JOIN secondbox.data_plane_frames AS frame
				  ON frame.session_id=session.id AND frame.direction='outbound' AND frame.sequence=2
				WHERE session.id='retention-port-race'`,
			).Scan(&dataState, &portState, &acknowledged, &nextOutbound, &frameHorizon, &creditState); err != nil {
				t.Fatal(err)
			}
			if dataState != "failed" || portState != "closed" || acknowledged != 1 ||
				nextOutbound != 3 || creditState != "pending" {
				t.Fatalf("Port disconnect/acknowledgement projections = data %q port %q ack %d next %d credit %q",
					dataState, portState, acknowledged, nextOutbound, creditState)
			}
			if changed, err := relay.sweepDataPlaneFrames(
				t.Context(), frameHorizon, 10, 1<<20,
			); err != nil || !changed {
				t.Fatalf("late Port credit cleanup = %t, %v", changed, err)
			}
		})
	}
}

func TestTerminalExecAppendAfterCleanupUsesProjectionAndReopensCleanup(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 2, 18, 0, 0, 0, time.UTC)
	retainUntil := now.Add(time.Hour)
	insertRelayRetentionSession(t, store.pool, "retention-late-exec", now, retainUntil)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions
		SET operation='exec-stream',frame_cleanup_completed_at=$2
		WHERE id=$1`, "retention-late-exec", now); err != nil {
		t.Fatal(err)
	}
	relay := &PostgresFrameRelay{pool: store.pool, maximumFrameBytes: 1 << 20}
	inserted, err := relay.AppendExecClientFrame(
		t.Context(), "tenant", "subject", "retention-late-exec",
		ExecClientFrame{Sequence: 0, Input: []byte("late")}, now.Add(time.Second),
	)
	if err != nil || !inserted {
		t.Fatalf("late terminal Exec append = %t, %v", inserted, err)
	}
	var nextSequence int64
	var frameHorizon time.Time
	var cleanupCompleted *time.Time
	var frameState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT session.next_outbound_sequence,session.frames_retain_until,
		       session.frame_cleanup_completed_at,frame.state
		FROM secondbox.data_plane_sessions AS session
		JOIN secondbox.data_plane_frames AS frame ON frame.session_id=session.id
		WHERE session.id=$1 AND frame.direction='outbound' AND frame.sequence=2`,
		"retention-late-exec",
	).Scan(&nextSequence, &frameHorizon, &cleanupCompleted, &frameState); err != nil {
		t.Fatal(err)
	}
	if nextSequence != 3 || !frameHorizon.Equal(retainUntil) ||
		cleanupCompleted != nil || frameState != "discarded" {
		t.Fatalf("late terminal Exec projection = next %d horizon %v cleanup %v frame %q",
			nextSequence, frameHorizon, cleanupCompleted, frameState)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), now, 10, 1024); err != nil || changed {
		t.Fatalf("late frame cleanup before fallback = %t, %v", changed, err)
	}
	if changed, err := relay.sweepDataPlaneFrames(t.Context(), retainUntil, 10, 1024); err != nil || !changed {
		t.Fatalf("late frame cleanup at fallback = %t, %v", changed, err)
	}
}

func insertRelayRetentionSession(
	t *testing.T,
	pool *pgxpool.Pool,
	id string,
	frameHorizon time.Time,
	retainUntil time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,instance_id,
			runner_id,generation,fencing_token,request_id,lease_id,kind,operation,stream_id,state,
			priority,idempotency_key,request_hash,deadline_at,maximum_response_bytes,
			maximum_request_bytes,stream_window_bytes,response_credit_bytes,request_stream_bytes,
			request_stream_closed,detachable,terminal_detach_seconds,attachment_id,attached_at,
			detached_at,detach_expires_at,outbound_bytes,inbound_bytes,next_inbound_sequence,
			terminal_kind,terminal_detail,exit_code,signal,spawn_failure_reason,
			elapsed_milliseconds,limit_bytes,infrastructure_failure_reason,retryable,
			terminal_message,stdout_bytes,stderr_bytes,content_bytes,metadata_json,request_json,
			created_at,updated_at,completed_at,retain_until,frames_retain_until,next_outbound_sequence
		) VALUES (
			$1,'tenant','subject','sandbox','revision','assignment','instance','runner-retention',
			1,'fence'::bytea,'request','','exec','exec',$1,'completed',0,$1,'hash',$2,
			1024,1024,1024,0,0,false,false,0,'',NULL,NULL,NULL,0,0,1,
			'EXEC_TERMINAL_KIND_EXITED','',0,0,'',0,0,'',false,'',''::bytea,''::bytea,
			''::bytea,'{}','{}',$3,$3,$3,$4,$2,2
		)`, id, frameHorizon, frameHorizon.Add(-time.Second), retainUntil); err != nil {
		t.Fatal(err)
	}
}

func seedRelayRetentionPortByte(t *testing.T, store *PostgresStateStore, now time.Time) {
	t.Helper()
	seedRunnerConnectionForDataPlaneDisconnect(
		t, store, "retention-port-race-connection", "retention-port-race-connection", now,
	)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sandbox-port-race','tenant','subject','profile','revision','ready','running',
			1,'workspace','instance','{}','{}',1,$1,$1
		)`, now); err != nil {
		t.Fatal(err)
	}
	insertRelayRetentionSession(
		t, store.pool, "retention-port-race", now.Add(time.Hour), now.Add(2*time.Hour),
	)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions
		SET kind='port',operation='port',sandbox_id='sandbox-port-race',runner_id='runner-home',
		    state='running',next_inbound_sequence=2
		WHERE id='retention-port-race';
		INSERT INTO secondbox.port_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,data_plane_session_id,
			lease_id,generation,name,guest_port,protocol,transport,credential_digest,
			stream_window_bytes,client_credit_bytes,client_bytes,runner_bytes,state,
			idempotency_key,request_hash,expires_at,created_at,updated_at,connected_at,closed_at,
			acknowledged_inbound_sequence
		) VALUES (
			'retention-port-race','tenant','subject','sandbox-port-race','revision',
			'retention-port-race','',1,'web',8080,'tcp','relay',''::bytea,1024,0,0,4,'open',
			'retention-port-race','hash',$2,$1,$1,$1,NULL,0
		)`, pgx.QueryExecModeSimpleProtocol, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	message := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Port{Port: &runnerv1.PortFrame{
			Sequence: 1,
			Payload:  &runnerv1.PortFrame_Bytes{Bytes: &runnerv1.PortBytes{Data: []byte("race")}},
		}},
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,priority,
			state,claim_owner,claim_expires_at,delivery_count,created_at,updated_at,delivered_at,consumed_at
		) VALUES (
			'retention-port-race-in-1','retention-port-race','inbound',1,'hash',$1,$2,0,
			'delivered','retention-port-race-connection',NULL,1,$3,$3,$3,NULL
		)`, payload, len(payload), now); err != nil {
		t.Fatal(err)
	}
}

func insertRelayRetentionFrame(
	t *testing.T,
	pool *pgxpool.Pool,
	sessionID string,
	sequence int64,
	size int,
	state string,
	claimExpiry *time.Time,
) {
	t.Helper()
	claimOwner := ""
	deliveryCount := int64(0)
	if state == "claimed" {
		claimOwner = "retention-connection\x1f1"
		deliveryCount = 1
	}
	createdAt := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,created_at,updated_at
		) VALUES ($1,$2,'outbound',$3,'hash',repeat('x',$4)::bytea,$4,0,$5,$6,$7,$8,$9,$9)`,
		fmt.Sprintf("%s-frame-%d", sessionID, sequence), sessionID, sequence, size,
		state, claimOwner, claimExpiry, deliveryCount, createdAt); err != nil {
		t.Fatal(err)
	}
}
