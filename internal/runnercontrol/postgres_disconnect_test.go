package runnercontrol

import (
	"encoding/json"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
)

func TestHeartbeatPreservesAndReleasesDurableAssignmentReservations(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 20, 55, 0, 0, time.UTC)
	seedRunnerConnectionForDataPlaneDisconnect(
		t,
		store,
		"connection-reservation",
		"connection-reservation",
		now,
	)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES (
			'profile-reservation','profile',1,
			'{"resources":{"cpuMillis":1000,"memoryBytes":536870912,"workspaceBytes":1073741824,"concurrentOperations":4}}',
			$1
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-reservation','sandbox','instance','runner-home',
			'profile-reservation','firecracker','',1,$2,'assigned','{}','{}','{}','',
			0,3,$3,$3,'',$3,$1,1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		[]byte("01234567890123456789012345678901"),
		now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	recordHeartbeat := func(sequence uint64) {
		t.Helper()
		if _, err := store.RecordHeartbeat(t.Context(), &runnerv1.RunnerHeartbeat{
			MessageId: "heartbeat-reservation-" + time.Duration(sequence).String(),
			Sequence:  sequence, RunnerId: "runner-home",
			ConnectionId:     "connection-reservation",
			ObservedAtUnixMs: uint64(now.Add(time.Duration(sequence) * time.Second).UnixMilli()),
			Allocatable: &runnerv1.Capacity{
				VcpuMillis: 8000, MemoryBytes: 4 << 30, DiskBytes: 8 << 30,
				Instances: 8, Operations: 32,
			},
			Reserved:      &runnerv1.Capacity{},
			DrainPhase:    runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
			StartupTiming: &runnerv1.StartupTiming{},
		}, now.Add(time.Duration(sequence)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	readReservation := func() runnerCapacity {
		t.Helper()
		var encoded []byte
		if err := store.pool.QueryRow(t.Context(), `
			SELECT reserved_capacity_json FROM secondbox.runners
			WHERE id='runner-home'`,
		).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var capacity runnerCapacity
		if err := json.Unmarshal(encoded, &capacity); err != nil {
			t.Fatal(err)
		}
		return capacity
	}
	recordHeartbeat(1)
	if got := readReservation(); got != (runnerCapacity{
		CPUMillis: 1000, MemoryBytes: 536870912, DiskBytes: 1073741824,
		Instances: 1, Operations: 4,
	}) {
		t.Fatalf("durable reservation = %#v", got)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.assignments SET state='fenced'
		WHERE id='assignment-reservation'`,
	); err != nil {
		t.Fatal(err)
	}
	recordHeartbeat(2)
	if got := readReservation(); got != (runnerCapacity{}) {
		t.Fatalf("released reservation = %#v", got)
	}
}

func TestCloseCurrentConnectionFailsActiveDataPlaneSessionsWithoutPrematureFencing(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 21, 0, 0, 0, time.UTC)
	seedRunnerConnectionForDataPlaneDisconnect(
		t,
		store,
		"connection-current",
		"connection-current",
		now,
	)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.data_plane_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,
			instance_id,runner_id,generation,fencing_token,request_id,lease_id,kind,
			operation,stream_id,state,priority,idempotency_key,request_hash,deadline_at,
			maximum_response_bytes,maximum_request_bytes,stream_window_bytes,
			response_credit_bytes,request_stream_bytes,request_stream_closed,detachable,
			terminal_detach_seconds,attachment_id,attached_at,detached_at,detach_expires_at,
			outbound_bytes,inbound_bytes,next_inbound_sequence,terminal_kind,terminal_detail,
			exit_code,signal,spawn_failure_reason,elapsed_milliseconds,limit_bytes,
			infrastructure_failure_reason,retryable,terminal_message,stdout_bytes,
			stderr_bytes,content_bytes,metadata_json,request_json,created_at,updated_at,
			completed_at,retain_until
		)
		SELECT
			'session-'||kind,'tenant','subject','sandbox','profile-revision',
			'assignment','instance','runner-home',1,$1,'request','',''||kind,kind,
			'stream-'||kind,'running',0,'','',$2,1024,1024,1024,1024,0,false,
			false,0,'',NULL,NULL,NULL,0,0,1,'','',0,0,'',0,0,'',false,'',
			''::bytea,''::bytea,''::bytea,'{}','{}',$3,$3,NULL,$2
		FROM unnest(ARRAY['exec','terminal','file','port']) AS kind;

		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment','sandbox','instance','runner-home','profile-revision',
			'firecracker','fc-instance',1,$1,'ready','{}','{}','{}','',0,3,
			$2,$2,'',$2,$2,1,$3,$3
		);

		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,
			last_activity_at,created_at,updated_at,closed_at
		)
		SELECT
			'session-'||kind,'tenant','subject','sandbox',1,kind,'active','',
			$3,$3,$3,NULL
		FROM unnest(ARRAY['exec','terminal','file','port']) AS kind;

		INSERT INTO secondbox.port_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,
			data_plane_session_id,lease_id,generation,name,guest_port,protocol,
			stream_window_bytes,client_credit_bytes,client_bytes,runner_bytes,state,
			idempotency_key,request_hash,expires_at,created_at,updated_at,
			connected_at,closed_at
		) VALUES (
			'port-session','tenant','subject','sandbox','profile-revision',
			'session-port','',1,'http',8080,'tcp',1024,0,0,0,'open','','',
			$2,$3,$3,$3,NULL
		)`,
		pgx.QueryExecModeSimpleProtocol,
		[]byte("01234567890123456789012345678901"),
		now.Add(time.Hour),
		now,
	); err != nil {
		t.Fatal(err)
	}

	disconnectedAt := now.Add(time.Second)
	if err := store.CloseConnection(
		t.Context(),
		"runner-home",
		"connection-current",
		disconnectedAt,
	); err != nil {
		t.Fatal(err)
	}

	rows, err := store.pool.Query(t.Context(), `
		SELECT kind,state,terminal_kind,infrastructure_failure_reason,retryable,
		       terminal_message,completed_at
		FROM secondbox.data_plane_sessions
		ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantTerminal := map[string]string{
		"exec":     runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED.String(),
		"terminal": runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED.String(),
		"file":     runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_FAILED.String(),
		"port":     runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE.String(),
	}
	seen := make(map[string]bool, len(wantTerminal))
	for rows.Next() {
		var (
			kind, state, terminalKind, infrastructureReason, terminalMessage string
			retryable                                                        bool
			completedAt                                                      *time.Time
		)
		if err := rows.Scan(
			&kind,
			&state,
			&terminalKind,
			&infrastructureReason,
			&retryable,
			&terminalMessage,
			&completedAt,
		); err != nil {
			t.Fatal(err)
		}
		if state != "failed" ||
			terminalKind != wantTerminal[kind] ||
			infrastructureReason !=
				runnerv1.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_EXECUTION_NODE.String() ||
			!retryable ||
			terminalMessage == "" ||
			completedAt == nil ||
			!completedAt.Equal(disconnectedAt) {
			t.Fatalf(
				"disconnected %s session state=%q terminal=%q reason=%q retryable=%t message=%q completed=%v",
				kind,
				state,
				terminalKind,
				infrastructureReason,
				retryable,
				terminalMessage,
				completedAt,
			)
		}
		seen[kind] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(wantTerminal) {
		t.Fatalf("disconnected session kinds = %v, want %v", seen, wantTerminal)
	}

	var assignmentState, assignmentFailureClass string
	var nextReconcileAt time.Time
	if err := store.pool.QueryRow(t.Context(), `
		SELECT state,failure_class,next_reconcile_at
		FROM secondbox.assignments WHERE id='assignment'`,
	).Scan(
		&assignmentState,
		&assignmentFailureClass,
		&nextReconcileAt,
	); err != nil {
		t.Fatal(err)
	}
	var activeActivities int
	if err := store.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.activity_sessions WHERE state='active'`,
	).Scan(&activeActivities); err != nil {
		t.Fatal(err)
	}
	var portState string
	var portClosedAt *time.Time
	if err := store.pool.QueryRow(t.Context(), `
		SELECT state,closed_at FROM secondbox.port_sessions WHERE id='port-session'`,
	).Scan(&portState, &portClosedAt); err != nil {
		t.Fatal(err)
	}
	if assignmentState != "ready" ||
		assignmentFailureClass != "" ||
		!nextReconcileAt.Equal(now.Add(time.Hour)) ||
		activeActivities != 0 ||
		portState != "closed" ||
		portClosedAt == nil ||
		!portClosedAt.Equal(disconnectedAt) {
		t.Fatalf(
			"disconnect cleanup assignment=%q/%q/%s activities=%d port=%q/%v",
			assignmentState,
			assignmentFailureClass,
			nextReconcileAt,
			activeActivities,
			portState,
			portClosedAt,
		)
	}
}

func TestCloseSupersededConnectionDoesNotFailCurrentDataPlaneSessions(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 21, 5, 0, 0, time.UTC)
	seedRunnerConnectionForDataPlaneDisconnect(
		t,
		store,
		"connection-current",
		"connection-old",
		now,
	)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_connections (
			id,runner_id,credential_serial,protocol_version,state,last_sequence,
			last_control_sequence,connected_at,last_seen_at,disconnected_at
		) VALUES (
			'connection-current','runner-home','credential-current',1,'active',
			0,0,$1,$1,NULL
		);
		INSERT INTO secondbox.data_plane_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,
			instance_id,runner_id,generation,fencing_token,request_id,lease_id,kind,
			operation,stream_id,state,priority,idempotency_key,request_hash,deadline_at,
			maximum_response_bytes,maximum_request_bytes,stream_window_bytes,
			response_credit_bytes,request_stream_bytes,request_stream_closed,detachable,
			terminal_detach_seconds,attachment_id,attached_at,detached_at,detach_expires_at,
			outbound_bytes,inbound_bytes,next_inbound_sequence,terminal_kind,terminal_detail,
			exit_code,signal,spawn_failure_reason,elapsed_milliseconds,limit_bytes,
			infrastructure_failure_reason,retryable,terminal_message,stdout_bytes,
			stderr_bytes,content_bytes,metadata_json,request_json,created_at,updated_at,
			completed_at,retain_until
		) VALUES (
			'session-current','tenant','subject','sandbox','profile-revision',
			'assignment','instance','runner-home',1,$2,'request','','exec','exec',
			'stream','running',0,'','',$3,1024,1024,1024,1024,0,false,false,0,'',
			NULL,NULL,NULL,0,0,1,'','',0,0,'',0,0,'',false,'',''::bytea,
			''::bytea,''::bytea,'{}','{}',$1,$1,NULL,$3
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		[]byte("01234567890123456789012345678901"),
		now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	if err := store.CloseConnection(
		t.Context(),
		"runner-home",
		"connection-old",
		now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	var runnerState, activeConnectionID, sessionState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT runner.state,runner.active_connection_id,session.state
		FROM secondbox.runners AS runner
		JOIN secondbox.data_plane_sessions AS session ON session.id='session-current'
		WHERE runner.id='runner-home'`,
	).Scan(&runnerState, &activeConnectionID, &sessionState); err != nil {
		t.Fatal(err)
	}
	if runnerState != "ready" ||
		activeConnectionID != "connection-current" ||
		sessionState != "running" {
		t.Fatalf(
			"superseded close runner=%q connection=%q session=%q",
			runnerState,
			activeConnectionID,
			sessionState,
		)
	}
}

func seedRunnerConnectionForDataPlaneDisconnect(
	t *testing.T,
	store *PostgresStateStore,
	activeConnectionID string,
	connectionID string,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_pools (
			name,state,architectures_json,capabilities_json,capacity_policy_json,
			ready_runner_count,revision,created_at,updated_at
		) VALUES ('pool','active','[]','[]','{}',1,1,$1,$1);
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			'runner-home','pool','runner-home','ready','[]','[]','{}','[]',
			1,1,'test',$2,0,'active','{}','{}',0,0,$1,1,$1,$1
		);
		INSERT INTO secondbox.runner_connections (
			id,runner_id,credential_serial,protocol_version,state,last_sequence,
			last_control_sequence,connected_at,last_seen_at,disconnected_at
		) VALUES ($3,'runner-home','credential',1,'active',0,0,$1,$1,NULL)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		activeConnectionID,
		connectionID,
	); err != nil {
		t.Fatal(err)
	}
}
