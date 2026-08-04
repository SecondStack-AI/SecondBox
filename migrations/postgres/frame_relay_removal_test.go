package postgresmigrations

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestFrameRelayRemovalDropsPayloadSchemaAndConvergesWithFreshDatabase(t *testing.T) {
	upgraded := newGuardDatabase(t)
	applyMigrations(t, upgraded,
		"0002_sandbox_name_index.sql",
		"0003_control_plane_wakeups.sql",
		"0004_direct_port_data_plane.sql",
		"0005_relay_data_plane_wakeups.sql",
		"0006_eager_assignment_dispatch.sql",
		"0007_relay_frame_retention.sql",
		"0008_workspace_relocations.sql",
	)
	seedFrameRelayRemoval(t, upgraded)
	applyMigrations(t, upgraded, "0009_remove_frame_relay.sql")

	var resultJSON []byte
	if err := upgraded.QueryRow(t.Context(), `
		SELECT result_json FROM secondbox.data_plane_sessions WHERE id='removal-session'`,
	).Scan(&resultJSON); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Stdout  []byte `json:"stdout"`
		Stderr  []byte `json:"stderr"`
		Content []byte `json:"content"`
	}
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Stdout, []byte{0, 1, 0xff}) ||
		!bytes.Equal(result.Stderr, []byte("diagnostic")) ||
		!bytes.Equal(result.Content, []byte("content")) {
		t.Fatalf("migrated result = %#v", result)
	}

	var table, outboundFunction, inboundFunction *string
	if err := upgraded.QueryRow(t.Context(), `
		SELECT to_regclass('secondbox.data_plane_frames')::text,
		       to_regprocedure('secondbox.notify_outbound_relay_work()')::text,
		       to_regprocedure('secondbox.notify_inbound_relay_work()')::text`,
	).Scan(&table, &outboundFunction, &inboundFunction); err != nil {
		t.Fatal(err)
	}
	if table != nil || outboundFunction != nil || inboundFunction != nil {
		t.Fatalf("relay schema survived: table=%v outbound=%v inbound=%v", table, outboundFunction, inboundFunction)
	}
	var removedColumns int
	if err := upgraded.QueryRow(t.Context(), `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema='secondbox' AND table_name='data_plane_sessions'
		  AND column_name=ANY($1)`, []string{
		"stdout_bytes", "stderr_bytes", "content_bytes", "frames_retain_until",
		"frame_cleanup_completed_at", "terminal_inbound_sequence",
		"terminal_inbound_payload_hash",
	}).Scan(&removedColumns); err != nil {
		t.Fatal(err)
	}
	if removedColumns != 0 {
		t.Fatalf("removed data-plane columns still present = %d", removedColumns)
	}

	fresh := newGuardDatabase(t)
	applyMigrations(t, fresh,
		"0002_sandbox_name_index.sql",
		"0003_control_plane_wakeups.sql",
		"0004_direct_port_data_plane.sql",
		"0005_relay_data_plane_wakeups.sql",
		"0006_eager_assignment_dispatch.sql",
		"0007_relay_frame_retention.sql",
		"0008_workspace_relocations.sql",
		"0009_remove_frame_relay.sql",
	)
	if upgradedShape, freshShape := secondboxSchemaShape(t, upgraded), secondboxSchemaShape(t, fresh); upgradedShape != freshShape {
		t.Fatalf("upgraded and fresh schema shapes differ:\nupgraded:\n%s\nfresh:\n%s", upgradedShape, freshShape)
	}
}

func applyMigrations(t *testing.T, connection *pgx.Conn, filenames ...string) {
	t.Helper()
	for _, filename := range filenames {
		if _, err := connection.Exec(t.Context(), migrationSQL(t, filename)); err != nil {
			t.Fatalf("apply %s: %v", filename, err)
		}
	}
}

func seedFrameRelayRemoval(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := connection.Exec(t.Context(), `
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
			created_at,updated_at,completed_at,retain_until,frames_retain_until,
			frame_cleanup_completed_at,next_outbound_sequence,terminal_inbound_sequence,
			terminal_inbound_payload_hash
		) VALUES (
			'removal-session','tenant','subject','sandbox','revision','assignment','instance',
			'runner',1,'fence'::bytea,'request','','exec','exec','stream','completed',
			0,'key','hash',$1,1048576,1048576,65536,0,0,false,false,0,'',NULL,NULL,NULL,
			0,0,2,'EXEC_TERMINAL_KIND_EXITED','',0,0,'',0,0,'',false,'',
			$2,$3,$4,'{}','{}',$5,$5,$5,$1,$1,NULL,1,1,'hash'
		)`, now.Add(time.Hour), []byte{0, 1, 0xff}, []byte("diagnostic"), []byte("content"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at,consumed_at
		) VALUES ('removal-frame','removal-session','inbound',1,'hash',$1,3,0,
			'delivered','',NULL,0,$2,$2,$2,NULL)`, []byte{0, 1, 0xff}, now); err != nil {
		t.Fatal(err)
	}
}

func secondboxSchemaShape(t *testing.T, connection *pgx.Conn) string {
	t.Helper()
	var shape string
	if err := connection.QueryRow(t.Context(), `
		SELECT string_agg(item,E'\n' ORDER BY item)
		FROM (
		  SELECT 'relation:'||class.relname||':'||class.relkind::text AS item
		  FROM pg_class AS class
		  JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
		  WHERE namespace.nspname='secondbox'
		  UNION ALL
		  SELECT 'column:'||class.relname||':'||attribute.attnum||':'||attribute.attname||':'||
		         attribute.atttypid::regtype::text||':'||attribute.attnotnull
		  FROM pg_attribute AS attribute
		  JOIN pg_class AS class ON class.oid=attribute.attrelid
		  JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
		  WHERE namespace.nspname='secondbox' AND attribute.attnum>0 AND NOT attribute.attisdropped
		  UNION ALL
		  SELECT 'function:'||procedure.proname
		  FROM pg_proc AS procedure
		  JOIN pg_namespace AS namespace ON namespace.oid=procedure.pronamespace
		  WHERE namespace.nspname='secondbox'
		  UNION ALL
		  SELECT 'trigger:'||class.relname||':'||trigger.tgname
		  FROM pg_trigger AS trigger
		  JOIN pg_class AS class ON class.oid=trigger.tgrelid
		  JOIN pg_namespace AS namespace ON namespace.oid=class.relnamespace
		  WHERE namespace.nspname='secondbox' AND NOT trigger.tgisinternal
		) AS shape`,
	).Scan(&shape); err != nil {
		t.Fatal(err)
	}
	return shape
}
