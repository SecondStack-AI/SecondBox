package postgresmigrations

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestRelayOutboundWakeupCollapsesABurstToOneNotification pins the outbound
// rule. Consumers drain until the authoritative query reports no work, so only
// the idle-to-pending transition needs a wakeup. A per-frame trigger would emit
// three here and fail this test, which is the point of asserting the count.
func TestRelayOutboundWakeupCollapsesABurstToOneNotification(t *testing.T) {
	writer, listener := newRelayWakeupDatabase(t)
	insertWakeupExecSession(t, writer)
	for sequence := 1; sequence <= 3; sequence++ {
		insertRelayFrame(t, writer, relayFrame{
			id:        fmt.Sprintf("burst-%d", sequence),
			sessionID: "exec",
			direction: "outbound",
			sequence:  sequence,
			state:     "pending",
		})
	}
	payloads := drainWakeupNotifications(t, listener)
	if len(payloads) != 1 {
		t.Fatalf("outbound wakeups = %d (%v), want exactly one for the burst",
			len(payloads), payloads)
	}
	assertWakeupPayload(t, payloads[0], "runner_command", "runner-exec")
}

// TestRelayOutboundWakeupSkipsDiscardedAndResumesOnceDrained proves the rule is
// a transition rather than a one-shot: a session that has been drained wakes
// again on its next frame.
func TestRelayOutboundWakeupSkipsDiscardedAndResumesOnceDrained(t *testing.T) {
	writer, listener := newRelayWakeupDatabase(t)
	insertWakeupExecSession(t, writer)

	// A frame that will never be delivered must not wake anything.
	insertRelayFrame(t, writer, relayFrame{
		id: "discarded-1", sessionID: "exec", direction: "outbound",
		sequence: 1, state: "discarded",
	})
	if payloads := drainWakeupNotifications(t, listener); len(payloads) != 0 {
		t.Fatalf("discarded frame wakeups = %v, want none", payloads)
	}

	insertRelayFrame(t, writer, relayFrame{
		id: "pending-1", sessionID: "exec", direction: "outbound",
		sequence: 2, state: "pending",
	})
	if payloads := drainWakeupNotifications(t, listener); len(payloads) != 1 {
		t.Fatalf("first pending frame wakeups = %v, want one", payloads)
	}

	// Draining the queue returns the session to idle.
	if _, err := writer.Exec(t.Context(),
		`UPDATE secondbox.data_plane_frames SET state='delivered' WHERE id='pending-1'`,
	); err != nil {
		t.Fatal(err)
	}
	insertRelayFrame(t, writer, relayFrame{
		id: "pending-2", sessionID: "exec", direction: "outbound",
		sequence: 3, state: "pending",
	})
	payloads := drainWakeupNotifications(t, listener)
	if len(payloads) != 1 {
		t.Fatalf("wakeups after draining = %v, want one", payloads)
	}
	assertWakeupPayload(t, payloads[0], "runner_command", "runner-exec")
}

// TestRelayInboundWakeupNotifiesEveryFrame pins the deliberate asymmetry.
// Inbound frames are inserted already delivered and are read by cursor, so no
// durable row records how far a caller has read and there is nothing to collapse
// against. Every inbound frame is output an attached caller is already waiting
// for.
func TestRelayInboundWakeupNotifiesEveryFrame(t *testing.T) {
	writer, listener := newRelayWakeupDatabase(t)
	insertWakeupExecSession(t, writer)
	for sequence := 1; sequence <= 3; sequence++ {
		insertRelayFrame(t, writer, relayFrame{
			id:        fmt.Sprintf("inbound-%d", sequence),
			sessionID: "exec",
			direction: "inbound",
			sequence:  sequence,
			state:     "delivered",
		})
	}
	payloads := drainWakeupNotifications(t, listener)
	if len(payloads) != 3 {
		t.Fatalf("inbound wakeups = %d (%v), want one per frame", len(payloads), payloads)
	}
	for _, payload := range payloads {
		assertWakeupPayload(t, payload, "data_plane_session", "exec")
	}
}

func newRelayWakeupDatabase(t *testing.T) (*pgx.Conn, *pgx.Conn) {
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
	return writer, listener
}

type relayFrame struct {
	id        string
	sessionID string
	direction string
	sequence  int
	state     string
}

func insertRelayFrame(t *testing.T, connection *pgx.Conn, frame relayFrame) {
	t.Helper()
	now := time.Now().UTC()
	var deliveredAt *time.Time
	if frame.direction == "inbound" {
		deliveredAt = &now
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,$4,'hash',''::bytea,0,0,$5,'',NULL,0,$6,$6,$7)`,
		frame.id, frame.sessionID, frame.direction, frame.sequence,
		frame.state, now, deliveredAt,
	); err != nil {
		t.Fatal(err)
	}
}

func assertWakeupPayload(t *testing.T, encoded string, kind string, key string) {
	t.Helper()
	var payload struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != kind || payload.Key != key {
		t.Errorf("wakeup payload = %#v, want kind %q key %q", payload, kind, key)
	}
}
