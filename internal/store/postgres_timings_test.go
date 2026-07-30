package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func TestTimingProjectionsJoinLifecycleBootAndExecEvidence(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	base := time.Date(2026, 7, 29, 21, 0, 0, 0, time.UTC)
	if _, err := controlPlaneStore.pool.Exec(
		t.Context(),
		`
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sbox_timing','tenant-timing','subject-timing','profile','profile-revision',
			'ready','running',1,'workspace-timing','instance-timing','{}','{}',1,$1,$1
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,
			created_at,started_at,completed_at,updated_at
		) VALUES (
			'op_timing','tenant-timing','subject-timing','sbox_timing','',
			'create','succeeded','request-timing','{}','','',false,
			$1,$2,$3,$3
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-timing','sbox_timing','instance-timing','runner-timing',
			'profile-revision','compute','',1,$4,'ready','{}','[]','{}','',0,3,
			$5,$5,'',$1,$5,1,$1,$3
		);
		INSERT INTO secondbox.assignment_stage_timings (
			assignment_id,operation_id,sandbox_id,stage,observed_at,received_at
		) VALUES
			('assignment-timing','op_timing','sbox_timing','artifact_verify',$6,$6),
			('assignment-timing','op_timing','sbox_timing','compute_launch',$7,$7),
			('assignment-timing','op_timing','sbox_timing','ready',$8,$8);
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
			'dps_timing','tenant-timing','subject-timing','sbox_timing','profile-revision',
			'assignment-timing','instance-timing','runner-timing',1,$4,
			'request-exec-timing','','exec','exec','stream-timing','completed',0,
			'exec-timing-key','exec-timing-hash',$5,1024,1024,1024,0,0,true,false,30,
			'',NULL,NULL,NULL,0,0,1,'exited','',0,0,'',40,0,'',false,'',
			$9,$9,$9,'{}','{}',$10,$11,$11,$5
		)`,
		pgx.QueryExecModeSimpleProtocol,
		base,
		base.Add(10*time.Millisecond),
		base.Add(110*time.Millisecond),
		[]byte("01234567890123456789012345678901"),
		base.Add(time.Hour),
		base.Add(25*time.Millisecond),
		base.Add(75*time.Millisecond),
		base.Add(100*time.Millisecond),
		[]byte{},
		base.Add(time.Minute),
		base.Add(time.Minute+40*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}

	sandboxTiming, err := controlPlaneStore.ReadSandboxTiming(
		t.Context(), "tenant-timing", "subject-timing", "sbox_timing", 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxTiming.Operations) != 1 || len(sandboxTiming.Execs) != 1 {
		t.Fatalf("Sandbox timing = %#v", sandboxTiming)
	}
	operation := sandboxTiming.Operations[0]
	if operation.QueueMilliseconds == nil || *operation.QueueMilliseconds != 10 ||
		operation.ExecutionMilliseconds == nil || *operation.ExecutionMilliseconds != 100 ||
		operation.TotalMilliseconds == nil || *operation.TotalMilliseconds != 110 {
		t.Fatalf("Operation timing = %#v", operation)
	}
	if len(operation.Boots) != 1 || len(operation.Boots[0].Stages) != 3 ||
		operation.Boots[0].DurationMilliseconds != 100 ||
		operation.Boots[0].Stages[1].Stage != "compute_launch" ||
		operation.Boots[0].Stages[1].ElapsedMilliseconds != 50 {
		t.Fatalf("boot timing = %#v", operation.Boots)
	}
	if exec := sandboxTiming.Execs[0]; exec.Mode != "buffered" ||
		exec.Outcome != "exited" || exec.ElapsedMilliseconds != 40 {
		t.Fatalf("Exec timing = %#v", exec)
	}

	operationTiming, err := controlPlaneStore.ReadOperationTiming(
		t.Context(), "tenant-timing", "subject-timing", "op_timing",
	)
	if err != nil || operationTiming.OperationID != "op_timing" ||
		len(operationTiming.Boots) != 1 {
		t.Fatalf("single Operation timing = %#v error=%v", operationTiming, err)
	}
	if _, err := controlPlaneStore.ReadSandboxTiming(
		t.Context(), "other-tenant", "other-subject", "sbox_timing", 10,
	); !errors.Is(err, ports.ErrSandboxNotFound) {
		t.Fatalf("cross-subject timing error = %v", err)
	}

	summary, err := controlPlaneStore.ReadDeploymentTiming(
		t.Context(), base.Add(-time.Minute), base.Add(10*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Boot.Count != 1 || summary.Boot.P95Milliseconds == nil ||
		*summary.Boot.P95Milliseconds != 100 ||
		summary.Exec.Count != 1 || summary.Exec.P95Milliseconds == nil ||
		*summary.Exec.P95Milliseconds != 40 {
		t.Fatalf("deployment timing summary = %#v", summary)
	}
	if summary.DominantBootStage == nil ||
		summary.DominantBootStage.Stage != "compute_launch" {
		t.Fatalf("dominant boot stage = %#v", summary.DominantBootStage)
	}

	metrics, err := controlPlaneStore.ReadMetricsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.BootDuration.Count < 1 || len(metrics.BootStageDurations) != 3 ||
		len(metrics.ExecDurations) != 1 ||
		metrics.ExecDurations[0].Mode != "buffered" ||
		metrics.ExecDurations[0].Histogram.Count != 1 {
		t.Fatalf("timing metrics = %#v", metrics)
	}
}
