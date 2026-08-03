package store

import (
	"errors"
	"math"
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
		INSERT INTO secondbox.operation_stage_timings (
			operation_id,sandbox_id,stage,observed_at
		) VALUES
			('op_timing','sbox_timing','durable_admission',$1),
			('op_timing','sbox_timing','workspace_ready',$16),
			('op_timing','sbox_timing','placement_reconcile_started',$19),
			('op_timing','sbox_timing','placement_effect_started',$20),
			('op_timing','sbox_timing','placement_plan_ready',$21),
			('op_timing','sbox_timing','placement_schedule_started',$22),
			('op_timing','sbox_timing','placement_attempt_started',$23),
			('op_timing','sbox_timing','placement_sandbox_locked',$24),
			('op_timing','sbox_timing','placement_assignment_checked',$25),
			('op_timing','sbox_timing','placement_candidates_locked',$26),
			('op_timing','sbox_timing','placement_candidate_selected',$27),
			('op_timing','sbox_timing','placement_ready',$17),
			('op_timing','sbox_timing','startup_dispatched',$18),
			('op_timing','sbox_timing','ready_projected',$3);
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
			('assignment-timing','op_timing','sbox_timing','runner_admission',$15,$15),
			('assignment-timing','op_timing','sbox_timing','artifact_verify',$6,$6),
			('assignment-timing','op_timing','sbox_timing','workspace_attach',$7,$7),
			('assignment-timing','op_timing','sbox_timing','network_setup',$8,$8),
			('assignment-timing','op_timing','sbox_timing','compute_launch',$12,$12),
			('assignment-timing','op_timing','sbox_timing','guest_negotiation',$13,$13),
			('assignment-timing','op_timing','sbox_timing','ready',$14,$14);
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
			'dps_timing','tenant-timing','subject-timing','sbox_timing','profile-revision',
			'assignment-timing','instance-timing','runner-timing',1,$4,
			'request-exec-timing','','exec','exec','stream-timing','completed',0,
			'exec-timing-key','exec-timing-hash',$5,1024,1024,1024,0,0,true,false,30,
			'',NULL,NULL,NULL,0,0,1,'exited','',0,0,'',40,0,'',false,'',
				$9,$9,$9,'{}','{}',$10,$11,$11,$5,$5,1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		base,
		base.Add(10*time.Millisecond),
		base.Add(2636*time.Millisecond),
		[]byte("01234567890123456789012345678901"),
		base.Add(time.Hour),
		base.Add(25*time.Millisecond),
		base.Add(25*time.Millisecond+125*time.Microsecond),
		base.Add(35*time.Millisecond+625*time.Microsecond),
		[]byte{},
		base.Add(time.Minute),
		base.Add(time.Minute+40*time.Millisecond),
		base.Add(35*time.Millisecond+875*time.Microsecond),
		base.Add(2635*time.Millisecond+875*time.Microsecond),
		base.Add(2636*time.Millisecond),
		base.Add(24*time.Millisecond+750*time.Microsecond),
		base.Add(12*time.Millisecond),
		base.Add(19*time.Millisecond+500*time.Microsecond),
		base.Add(22*time.Millisecond),
		base.Add(13*time.Millisecond),
		base.Add(14*time.Millisecond),
		base.Add(15*time.Millisecond),
		base.Add(15*time.Millisecond+250*time.Microsecond),
		base.Add(15*time.Millisecond+500*time.Microsecond),
		base.Add(16*time.Millisecond),
		base.Add(16*time.Millisecond+500*time.Microsecond),
		base.Add(18*time.Millisecond),
		base.Add(18*time.Millisecond+250*time.Microsecond),
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
		operation.ExecutionMilliseconds == nil || *operation.ExecutionMilliseconds != 2626 ||
		operation.TotalMilliseconds == nil || *operation.TotalMilliseconds != 2636 {
		t.Fatalf("Operation timing = %#v", operation)
	}
	if len(operation.Boots) != 1 || len(operation.Boots[0].Stages) != 7 ||
		operation.Boots[0].DurationMilliseconds != 2636 ||
		operation.Boots[0].Stages[0].Stage != "runner_admission" ||
		math.Abs(operation.Boots[0].Stages[0].ElapsedMilliseconds-24.75) > 0.001 ||
		operation.Boots[0].Stages[1].Stage != "artifact_verify" ||
		math.Abs(operation.Boots[0].Stages[1].ElapsedMilliseconds-0.25) > 0.001 ||
		operation.Boots[0].Stages[3].Stage != "network_setup" ||
		math.Abs(operation.Boots[0].Stages[3].ElapsedMilliseconds-10.5) > 0.001 ||
		operation.Boots[0].Stages[4].Stage != "compute_launch" ||
		math.Abs(operation.Boots[0].Stages[4].ElapsedMilliseconds-0.25) > 0.001 ||
		operation.Boots[0].Stages[5].Stage != "guest_negotiation" ||
		math.Abs(operation.Boots[0].Stages[5].ElapsedMilliseconds-2600) > 0.001 {
		t.Fatalf("boot timing = %#v", operation.Boots)
	}
	if len(operation.Orchestration) != 14 ||
		operation.Orchestration[0].Stage != "durable_admission" ||
		operation.Orchestration[1].Stage != "workspace_ready" ||
		math.Abs(operation.Orchestration[1].ElapsedMilliseconds-12) > 0.001 ||
		operation.Orchestration[2].Stage != "placement_reconcile_started" ||
		math.Abs(operation.Orchestration[2].ElapsedMilliseconds-1) > 0.001 ||
		operation.Orchestration[4].Stage != "placement_plan_ready" ||
		math.Abs(operation.Orchestration[4].CumulativeMilliseconds-15) > 0.001 ||
		operation.Orchestration[9].Stage != "placement_candidates_locked" ||
		math.Abs(operation.Orchestration[9].ElapsedMilliseconds-1.5) > 0.001 ||
		operation.Orchestration[11].Stage != "placement_ready" ||
		math.Abs(operation.Orchestration[11].ElapsedMilliseconds-1.25) > 0.001 ||
		operation.Orchestration[12].Stage != "startup_dispatched" ||
		math.Abs(operation.Orchestration[12].CumulativeMilliseconds-22) > 0.001 ||
		operation.Orchestration[13].Stage != "ready_projected" {
		t.Fatalf("Operation orchestration timing = %#v", operation.Orchestration)
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
		*summary.Boot.P95Milliseconds != 2636 ||
		summary.Exec.Count != 1 || summary.Exec.P95Milliseconds == nil ||
		*summary.Exec.P95Milliseconds != 40 {
		t.Fatalf("deployment timing summary = %#v", summary)
	}
	if summary.DominantBootStage == nil ||
		summary.DominantBootStage.Stage != "guest_negotiation" ||
		summary.DominantBootStage.Duration.P95Milliseconds == nil ||
		*summary.DominantBootStage.Duration.P95Milliseconds != 2600 {
		t.Fatalf("dominant boot stage = %#v", summary.DominantBootStage)
	}

	metrics, err := controlPlaneStore.ReadMetricsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.BootDuration.Count < 1 || len(metrics.BootStageDurations) != 7 ||
		len(metrics.ExecDurations) != 1 ||
		metrics.ExecDurations[0].Mode != "buffered" ||
		metrics.ExecDurations[0].Histogram.Count != 1 {
		t.Fatalf("timing metrics = %#v", metrics)
	}
}
