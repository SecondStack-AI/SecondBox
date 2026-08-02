package postgresmigrations

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestControlPlaneWakeupIsDeliveredOnlyAfterCommit(t *testing.T) {
	for _, state := range []string{"pending", "delivering"} {
		t.Run(state, func(t *testing.T) {
			testControlPlaneWakeupIsDeliveredOnlyAfterCommit(t, state)
		})
	}
}

func testControlPlaneWakeupIsDeliveredOnlyAfterCommit(t *testing.T, state string) {
	writer := newGuardDatabase(t)
	for _, migration := range []string{
		"0003_control_plane_wakeups.sql",
		"0006_eager_assignment_dispatch.sql",
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
	tx, err := writer.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	now := time.Now().UTC()
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-notify','runner-notify','','local-workspace',$1,
			$2,'connection-notify',0,$3,$3,NULL
		)`,
		[]byte{},
		state,
		now,
	); err != nil {
		t.Fatal(err)
	}
	type notificationResult struct {
		payload string
		err     error
	}
	notifications := make(chan notificationResult, 1)
	waitContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	go func() {
		notification, err := listener.WaitForNotification(waitContext)
		if err != nil {
			notifications <- notificationResult{err: err}
			return
		}
		notifications <- notificationResult{payload: notification.Payload}
	}()
	select {
	case result := <-notifications:
		t.Fatalf("notification before commit = %#v", result)
	case <-time.After(25 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-notifications:
		if result.err != nil {
			t.Fatal(result.err)
		}
		var payload struct {
			Kind string `json:"kind"`
			Key  string `json:"key"`
		}
		if err := json.Unmarshal([]byte(result.payload), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Kind != "runner_command" || payload.Key != "runner-notify" {
			t.Fatalf("notification payload = %#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("committed runner command emitted no notification")
	}
}
