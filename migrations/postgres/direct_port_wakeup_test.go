package postgresmigrations

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestDirectPortAdmissionWakeupIsScopedToTheDirectTransport pins the notify
// scope. NOTIFY has no server-side filtering, so every payload reaches every
// listening replica; a wakeup on each relay frame would broadcast once per Port
// message, per Exec and PTY stdin chunk, and per File chunk. This plan moves
// only the Port byte path, so exactly one notification per direct PortSession
// admission is the whole contract.
func TestDirectPortAdmissionWakeupIsScopedToTheDirectTransport(t *testing.T) {
	writer := newGuardDatabase(t)
	if _, err := writer.Exec(
		t.Context(),
		migrationSQL(t, "0004_direct_port_data_plane.sql"),
	); err != nil {
		t.Fatal(err)
	}
	listener, err := pgx.Connect(t.Context(), writer.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close(context.Background())
	})
	if _, err := listener.Exec(t.Context(), "LISTEN secondbox_work"); err != nil {
		t.Fatal(err)
	}

	for _, transport := range []string{"relay", "direct"} {
		insertWakeupPortSession(t, writer, transport)
	}
	// Every frame kind the relay still carries, on both a relay Port session and
	// the direct one, must emit nothing.
	for _, session := range []string{"relay", "direct"} {
		for sequence := 1; sequence <= 3; sequence++ {
			insertWakeupOutboundFrame(t, writer, session, sequence)
		}
	}
	insertWakeupExecSession(t, writer)
	for sequence := 1; sequence <= 3; sequence++ {
		insertWakeupOutboundFrame(t, writer, "exec", sequence)
	}

	payloads := drainWakeupNotifications(t, listener)
	if len(payloads) != 1 {
		t.Fatalf("wakeup notifications = %d (%v), want exactly one", len(payloads), payloads)
	}
	var payload struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal([]byte(payloads[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "runner_command" || payload.Key != "runner-direct" {
		t.Fatalf("wakeup payload = %#v", payload)
	}
}

func TestDirectPortAdmissionWakeupIsDeliveredOnlyAfterCommit(t *testing.T) {
	writer := newGuardDatabase(t)
	if _, err := writer.Exec(
		t.Context(),
		migrationSQL(t, "0004_direct_port_data_plane.sql"),
	); err != nil {
		t.Fatal(err)
	}
	listener, err := pgx.Connect(t.Context(), writer.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close(context.Background())
	})
	if _, err := listener.Exec(t.Context(), "LISTEN secondbox_work"); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	insertWakeupPortSessionTx(t, tx, "direct")
	notifications := make(chan string, 1)
	waitContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	go func() {
		notification, err := listener.WaitForNotification(waitContext)
		if err != nil {
			return
		}
		notifications <- notification.Payload
	}()
	select {
	case payload := <-notifications:
		t.Fatalf("wakeup before commit = %q", payload)
	case <-time.After(25 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("committed direct PortSession admission emitted no wakeup")
	}
}

// insertWakeupPortSession admits one PortSession on the named transport with its
// data-plane session, which is where the home Runner identity lives.
func insertWakeupPortSession(t *testing.T, connection *pgx.Conn, transport string) {
	t.Helper()
	tx, err := connection.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	insertWakeupPortSessionTx(t, tx, transport)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func insertWakeupPortSessionTx(t *testing.T, tx pgx.Tx, transport string) {
	t.Helper()
	insertWakeupDataPlaneSession(t, tx, transport, "port")
	now := time.Now().UTC()
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO secondbox.port_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,data_plane_session_id,
			lease_id,generation,name,guest_port,protocol,transport,credential_digest,
			stream_window_bytes,client_credit_bytes,client_bytes,runner_bytes,state,
			idempotency_key,request_hash,expires_at,created_at,updated_at,connected_at,closed_at
		) VALUES (
			$1,'tenant','subject','sandbox','revision',$1,'lease',1,'web',8080,'tcp',$2,''::bytea,
			65536,0,0,0,'open',$1,'hash',$3,$4,$4,NULL,NULL
		)`,
		transport, transport, now.Add(time.Minute), now,
	); err != nil {
		t.Fatal(err)
	}
}

func insertWakeupExecSession(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	tx, err := connection.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	insertWakeupDataPlaneSession(t, tx, "exec", "exec")
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func insertWakeupDataPlaneSession(t *testing.T, tx pgx.Tx, id string, kind string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := tx.Exec(t.Context(), `
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
			created_at,updated_at,completed_at,retain_until
		) VALUES (
			$1,'tenant','subject','sandbox','revision','assignment','instance',
			$2,1,'fence'::bytea,'request','lease',$3,'operation',$1,'pending',
			0,$1,'hash',$4,1048576,1048576,65536,0,0,
			false,false,0,'',NULL,NULL,NULL,0,0,1,
			'','',0,0,'',0,0,'',false,'',''::bytea,''::bytea,''::bytea,'{}','{}',$5,$5,NULL,$4
		)`,
		id, "runner-"+id, kind, now.Add(time.Hour), now,
	); err != nil {
		t.Fatal(err)
	}
}

func insertWakeupOutboundFrame(t *testing.T, connection *pgx.Conn, sessionID string, sequence int) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at
		) VALUES ($1,$2,'outbound',$3,'hash',''::bytea,0,0,'pending','',NULL,0,$4,$4,NULL)`,
		sessionID+"-out-"+time.Now().Format("150405.000000000"), sessionID, sequence, now,
	); err != nil {
		t.Fatal(err)
	}
}

// drainWakeupNotifications collects every notification already queued, then
// stops at the first quiet interval.
func drainWakeupNotifications(t *testing.T, listener *pgx.Conn) []string {
	t.Helper()
	var payloads []string
	for {
		waitContext, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		notification, err := listener.WaitForNotification(waitContext)
		cancel()
		if err != nil {
			return payloads
		}
		payloads = append(payloads, notification.Payload)
	}
}
