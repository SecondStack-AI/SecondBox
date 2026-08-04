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
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES
		(
			'sandbox','tenant','subject','profile','profile-reservation','starting','running',
			1,'workspace','instance','{}','{}',1,$1,$1
		),
		(
			'sandbox-stale','tenant','subject-stale','profile','profile-reservation','stopped','stopped',
			2,'workspace-stale','instance-current','{}','{}',1,$1,$1
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES
		(
			'assignment-reservation','sandbox','instance','runner-home',
			'profile-reservation','firecracker','',1,$2,'assigned','{}','{}','{}','',
			0,3,$3,$3,'',$3,$1,1,$1,$1
		),
		(
			'assignment-stale','sandbox-stale','instance-stale','runner-home',
			'profile-reservation','firecracker','',1,$2,'uncertain','{}','{}','{}','transient',
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

func TestHeartbeatMakesMissingActiveAssignmentUncertain(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 20, 57, 0, 0, time.UTC)
	seedRunnerConnectionForDataPlaneDisconnect(
		t,
		store,
		"connection-active-inventory",
		"connection-active-inventory",
		now,
	)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES (
			'profile-active-inventory','profile',1,
			'{"resources":{"cpuMillis":1000,"memoryBytes":536870912,"workspaceBytes":1073741824,"concurrentOperations":4}}',
			$1
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES
		(
			'assignment-retained','sandbox-retained','instance-retained','runner-home',
			'profile-active-inventory','firecracker','fc-retained',2,$2,'ready',
			'{}','{}','{}','',0,3,$3,$3,'',$1,$3,1,$1,$1
		),
		(
			'assignment-missing','sandbox-missing','instance-missing','runner-home',
			'profile-active-inventory','firecracker','fc-missing',4,$4,'ready',
			'{}','{}','{}','',0,3,$3,$3,'',$1,$3,1,$1,$1
		),
		(
			'assignment-starting','sandbox-starting','instance-starting','runner-home',
			'profile-active-inventory','firecracker','',5,$5,'starting',
			'{}','{}','{}','',0,3,$3,$3,'',$1,$3,1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		[]byte("retained-fencing-token-000000000"),
		now.Add(time.Hour),
		[]byte("missing-fencing-token-0000000000"),
		[]byte("starting-fencing-token-00000000"),
	); err != nil {
		t.Fatal(err)
	}
	heartbeatAt := now.Add(time.Second)
	if _, err := store.RecordHeartbeat(t.Context(), &runnerv1.RunnerHeartbeat{
		MessageId:        "heartbeat-active-inventory",
		Sequence:         1,
		RunnerId:         "runner-home",
		ConnectionId:     "connection-active-inventory",
		ObservedAtUnixMs: uint64(heartbeatAt.UnixMilli()),
		Allocatable: &runnerv1.Capacity{
			VcpuMillis: 8000, MemoryBytes: 4 << 30, DiskBytes: 8 << 30,
			Instances: 8, Operations: 32,
		},
		Reserved: &runnerv1.Capacity{},
		ActiveAssignments: []*runnerv1.ActiveAssignmentSummary{{
			AssignmentId:      "assignment-retained",
			SandboxId:         "sandbox-retained",
			InstanceId:        "instance-retained",
			SandboxGeneration: 2,
			FencingToken:      []byte("retained-fencing-token-000000000"),
		}},
		DrainPhase:    runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
		StartupTiming: &runnerv1.StartupTiming{},
	}, heartbeatAt); err != nil {
		t.Fatal(err)
	}

	rows, err := store.pool.Query(t.Context(), `
		SELECT id,state,failure_class,next_reconcile_at
		FROM secondbox.assignments
		ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string]struct {
		state        string
		failureClass string
		next         time.Time
	})
	for rows.Next() {
		var (
			id, state, failureClass string
			next                    time.Time
		)
		if err := rows.Scan(&id, &state, &failureClass, &next); err != nil {
			t.Fatal(err)
		}
		got[id] = struct {
			state        string
			failureClass string
			next         time.Time
		}{state: state, failureClass: failureClass, next: next}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if retained := got["assignment-retained"]; retained.state != "ready" ||
		retained.failureClass != "" ||
		!retained.next.Equal(now.Add(time.Hour)) {
		t.Fatalf("retained assignment = %#v", retained)
	}
	if missing := got["assignment-missing"]; missing.state != "uncertain" ||
		missing.failureClass != "transient" ||
		!missing.next.Equal(heartbeatAt) {
		t.Fatalf("missing assignment = %#v", missing)
	}
	if starting := got["assignment-starting"]; starting.state != "starting" ||
		starting.failureClass != "" ||
		!starting.next.Equal(now.Add(time.Hour)) {
		t.Fatalf("starting assignment = %#v", starting)
	}
}

func TestCloseCurrentConnectionDetachesProxiedTerminalAndFailsOtherDataPlaneSessions(t *testing.T) {
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
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES (
			'profile-revision','profile',1,
			'{"execution":{"dataPlaneTransport":"proxied"}}',$3
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
				completed_at,retain_until,frames_retain_until,next_outbound_sequence
		)
		SELECT
			'session-'||kind,'tenant','subject','sandbox','profile-revision',
			'assignment','instance','runner-home',1,$1,'request','',''||kind,kind,
			'stream-'||kind,'running',0,'','',$2,1024,1024,1024,1024,0,false,
			kind='terminal',CASE WHEN kind='terminal' THEN 30 ELSE 0 END,
			CASE WHEN kind='terminal' THEN 'attachment-terminal' ELSE '' END,
			CASE WHEN kind='terminal' THEN $3::timestamptz ELSE NULL END,NULL,NULL,0,0,1,'','',0,0,'',0,0,'',false,'',
				''::bytea,''::bytea,''::bytea,'{}','{}',$3,$3,NULL,$2,$2,1
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
				connected_at,closed_at,acknowledged_inbound_sequence
		) VALUES (
			'port-session','tenant','subject','sandbox','profile-revision',
			'session-port','',1,'http',8080,'tcp',1024,0,0,0,'open','','',
				$2,$3,$3,$3,NULL,0
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
		"exec": runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED.String(),
		"file": runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_FAILED.String(),
		"port": runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE.String(),
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
		if kind == "terminal" {
			if state != "running" || terminalKind != "" || infrastructureReason != "" ||
				retryable || terminalMessage != "" || completedAt != nil {
				t.Fatalf(
					"disconnected Terminal state=%q terminal=%q reason=%q retryable=%t message=%q completed=%v",
					state, terminalKind, infrastructureReason, retryable, terminalMessage, completedAt,
				)
			}
			continue
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
	var terminalAttachment string
	var terminalDetachedAt, terminalDetachExpiresAt *time.Time
	if err := store.pool.QueryRow(t.Context(), `
		SELECT attachment_id,detached_at,detach_expires_at
		FROM secondbox.data_plane_sessions WHERE id='session-terminal'`,
	).Scan(&terminalAttachment, &terminalDetachedAt, &terminalDetachExpiresAt); err != nil {
		t.Fatal(err)
	}
	if terminalAttachment != "" || terminalDetachedAt == nil ||
		!terminalDetachedAt.Equal(disconnectedAt) || terminalDetachExpiresAt == nil ||
		!terminalDetachExpiresAt.Equal(disconnectedAt.Add(30*time.Second)) {
		t.Fatalf(
			"disconnected Terminal attachment=%q detached=%v expires=%v",
			terminalAttachment, terminalDetachedAt, terminalDetachExpiresAt,
		)
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
		activeActivities != 1 ||
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
				completed_at,retain_until,frames_retain_until,next_outbound_sequence
		) VALUES (
			'session-current','tenant','subject','sandbox','profile-revision',
			'assignment','instance','runner-home',1,$2,'request','','exec','exec',
			'stream','running',0,'','',$3,1024,1024,1024,1024,0,false,false,0,'',
			NULL,NULL,NULL,0,0,1,'','',0,0,'',0,0,'',false,'',''::bytea,
				''::bytea,''::bytea,'{}','{}',$1,$1,NULL,$3,$3,1
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
