package postgresmigrations

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRelayFrameRetentionMigrationBackfillsCompactProjections(t *testing.T) {
	writer := newRelayRetentionDatabase(t)
	insertWakeupExecSession(t, writer)
	insertRelayFrame(t, writer, relayFrame{
		id: "exec-outbound-1", sessionID: "exec", direction: "outbound", sequence: 1, state: "delivered",
	})
	insertRelayFrame(t, writer, relayFrame{
		id: "exec-outbound-2", sessionID: "exec", direction: "outbound", sequence: 2, state: "delivered",
	})
	insertRelayFrame(t, writer, relayFrame{
		id: "exec-terminal", sessionID: "exec", direction: "inbound", sequence: 1, state: "delivered",
	})
	now := time.Now().UTC()
	if _, err := writer.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions
		SET state='completed',next_inbound_sequence=2,completed_at=$2,retain_until=$3
		WHERE id=$1`, "exec", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(t.Context(), migrationSQL(t, "0007_relay_frame_retention.sql")); err != nil {
		t.Fatal(err)
	}
	var frameHorizon, sessionHorizon time.Time
	var nextOutbound int64
	var terminalSequence *int64
	var terminalHash *string
	if err := writer.QueryRow(t.Context(), `
		SELECT frames_retain_until,retain_until,next_outbound_sequence,
		       terminal_inbound_sequence,terminal_inbound_payload_hash
		FROM secondbox.data_plane_sessions WHERE id='exec'`,
	).Scan(&frameHorizon, &sessionHorizon, &nextOutbound, &terminalSequence, &terminalHash); err != nil {
		t.Fatal(err)
	}
	if !frameHorizon.Equal(sessionHorizon) || nextOutbound != 3 ||
		terminalSequence == nil || *terminalSequence != 1 ||
		terminalHash == nil || *terminalHash != "hash" {
		t.Fatalf("backfill = horizon %v/%v next %d terminal %v/%v",
			frameHorizon, sessionHorizon, nextOutbound, terminalSequence, terminalHash)
	}
	var indexDefinition string
	if err := writer.QueryRow(t.Context(), `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname='secondbox' AND indexname='data_plane_sessions_frame_retention_idx'`,
	).Scan(&indexDefinition); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"frames_retain_until", "state", "frame_cleanup_completed_at IS NULL"} {
		if !strings.Contains(indexDefinition, fragment) {
			t.Fatalf("frame retention index %q omitted %q", indexDefinition, fragment)
		}
	}
}

func TestRelayFrameRetentionMutationDoesNotChangeWakeupScope(t *testing.T) {
	writer := newRelayRetentionDatabase(t)
	insertWakeupExecSession(t, writer)
	insertRelayFrame(t, writer, relayFrame{
		id: "retained-inbound", sessionID: "exec", direction: "inbound", sequence: 1, state: "delivered",
	})
	if _, err := writer.Exec(t.Context(), migrationSQL(t, "0007_relay_frame_retention.sql")); err != nil {
		t.Fatal(err)
	}
	listener, err := pgx.Connect(t.Context(), writer.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err := listener.Exec(t.Context(), "LISTEN secondbox_work"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := writer.Exec(t.Context(), `
		UPDATE secondbox.data_plane_sessions
		SET frames_retain_until=$2,frame_cleanup_completed_at=$2,
		    next_outbound_sequence=next_outbound_sequence+1
		WHERE id=$1`, "exec", now); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(t.Context(), `
		DELETE FROM secondbox.data_plane_frames WHERE id='retained-inbound'`); err != nil {
		t.Fatal(err)
	}
	if notifications := drainWakeupNotifications(t, listener); len(notifications) != 0 {
		t.Fatalf("retention-only mutations emitted wakeups: %v", notifications)
	}
	insertRelayFrame(t, writer, relayFrame{
		id: "new-inbound", sessionID: "exec", direction: "inbound", sequence: 2, state: "delivered",
	})
	if notifications := drainWakeupNotifications(t, listener); len(notifications) != 1 {
		t.Fatalf("inbound insert wakeups = %v, want one", notifications)
	}
	insertRelayFrame(t, writer, relayFrame{
		id: "new-outbound", sessionID: "exec", direction: "outbound", sequence: 1, state: "pending",
	})
	notifications := drainWakeupNotifications(t, listener)
	if len(notifications) != 1 {
		t.Fatalf("outbound idle-to-pending wakeups = %v, want one", notifications)
	}
	assertWakeupPayload(t, notifications[0], "runner_command", "runner-exec")
}

func newRelayRetentionDatabase(t *testing.T) *pgx.Conn {
	t.Helper()
	writer := newGuardDatabase(t)
	for _, migration := range []string{
		"0004_direct_port_data_plane.sql",
		"0005_relay_data_plane_wakeups.sql",
	} {
		if _, err := writer.Exec(t.Context(), migrationSQL(t, migration)); err != nil {
			t.Fatal(err)
		}
	}
	return writer
}
