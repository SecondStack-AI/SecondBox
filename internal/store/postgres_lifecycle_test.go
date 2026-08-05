package store

import (
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func TestPostgresMarkReadyStartsNewIdleWindow(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	previousActivity := time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)
	readyAt := previousActivity.Add(15 * time.Minute)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,
			created_at,updated_at
		) VALUES (
			'workspace-mark-ready-idle','tenant','subject','sandbox-mark-ready-idle',
			'runner-home','ready',1048576,2,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,
			compatibility_summary_json,last_activity_at,reconcile_owner,
			reconcile_claim_expires_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-mark-ready-idle','tenant','subject','profile','revision','starting','running',
			2,'workspace-mark-ready-idle','instance-mark-ready-idle','{}','{}',$1,
			'worker-mark-ready-idle',$2,7,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		previousActivity,
		readyAt.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	claim := ports.LifecycleReconcileClaim{
		SandboxID: "sandbox-mark-ready-idle", ObservedState: contracts.SandboxStateStarting,
		DesiredState: contracts.SandboxDesiredStateRunning,
		WorkerID:     "worker-mark-ready-idle", Revision: 7,
	}
	if err := controlPlaneStore.ApplyLifecycleAction(
		t.Context(), claim, "mark_ready", "", readyAt, readyAt.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	var lastActivityAt time.Time
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT last_activity_at FROM secondbox.sandboxes
		WHERE id='sandbox-mark-ready-idle'`,
	).Scan(&lastActivityAt); err != nil {
		t.Fatal(err)
	}
	if !lastActivityAt.Equal(readyAt) {
		t.Fatalf("mark-ready last activity = %s, want %s", lastActivityAt, readyAt)
	}
}

func TestPostgresLifecycleClaimRequiresExplicitIntentAfterTerminalFailure(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ('revision-terminal-failure','profile-terminal-failure',1,'{}',$1);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,
			created_at,updated_at
		) VALUES (
			'workspace-terminal-failure','tenant','subject','sandbox-terminal-failure',
			'runner-home','failed',1048576,1,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,
			compatibility_summary_json,lifecycle_failure_class,lifecycle_failure_message,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-terminal-failure','tenant','subject','profile-terminal-failure',
			'revision-terminal-failure','failed','running',1,
			'workspace-terminal-failure','','{}','{}','home_workspace_conflict',
			'home runner local Workspace evidence conflicts with durable authority',
			$1,1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, found, err := controlPlaneStore.ClaimLifecycle(
		t.Context(), "worker-terminal-failure", now, time.Minute,
	); err != nil || found {
		t.Fatalf("terminal failure claim found=%t error=%v", found, err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET lifecycle_failure_class='',lifecycle_failure_message=''
		WHERE id='sandbox-terminal-failure'`,
	); err != nil {
		t.Fatal(err)
	}
	claim, found, err := controlPlaneStore.ClaimLifecycle(
		t.Context(), "worker-explicit-retry", now, time.Minute,
	)
	if err != nil || !found || claim.SandboxID != "sandbox-terminal-failure" {
		t.Fatalf("explicit retry claim=%#v found=%t error=%v", claim, found, err)
	}
}

func TestPostgresLifecycleBatchClaimsOrderedCohortAndClosesExpiredActivity(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2025, 1, 2, 12, 0, 0, 0, time.UTC)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ('revision-batch','profile-batch',1,'{}',$1);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,
			created_at,updated_at
		) VALUES
			('workspace-batch-a','tenant','subject','sandbox-batch-a','runner-home',
			 'ready',1048576,1,'','','','',NULL,NULL,'','{}',$1,$1),
			('workspace-batch-b','tenant','subject','sandbox-batch-b','runner-home',
			 'ready',1048576,1,'','','','',NULL,NULL,'','{}',$1,$1);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,
			compatibility_summary_json,next_reconcile_at,revision,created_at,updated_at
		) VALUES
			('sandbox-batch-a','tenant','subject','profile-batch','revision-batch','creating','stopped',
			 1,'workspace-batch-a','','{}','{}',$1,1,$1,$1),
			('sandbox-batch-b','tenant','subject','profile-batch','revision-batch','creating','stopped',
			 1,'workspace-batch-b','','{}','{}',$1,1,$1,$1);
		INSERT INTO secondbox.leases (
			id,tenant_ref,subject_ref,sandbox_id,generation,state,
			expires_at,revision,created_at,updated_at
		) VALUES (
			'lease-batch-expired','tenant','subject','sandbox-batch-a',1,'active',
			$2,1,$2,$2
		);
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,
			last_activity_at,created_at,updated_at
		) VALUES (
			'activity-batch-expired','tenant','subject','sandbox-batch-a',1,
			'terminal','active','lease-batch-expired',$2,$2,$2
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		now.Add(-time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	var due int
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.id IN ('sandbox-batch-a','sandbox-batch-b')
		  AND sandbox.state<>'deleted' AND sandbox.next_reconcile_at<=$1
		  AND NOT (sandbox.state='failed' AND sandbox.lifecycle_failure_class<>'')
		  AND NOT (
		    sandbox.state IN ('stopped','failed') AND sandbox.desired_state='stopped'
		  )
		  AND (
		    sandbox.reconcile_claim_expires_at IS NULL
		    OR sandbox.reconcile_claim_expires_at<=$1
		    OR sandbox.reconcile_owner=$2
		  )`,
		now, "worker-batch",
	).Scan(&due); err != nil || due != 2 {
		t.Fatalf("due batch Sandboxes = %d, %v", due, err)
	}
	claimAt := now.Add(2 * time.Minute)
	claims, err := controlPlaneStore.ClaimLifecycleBatch(
		t.Context(), "worker-batch", claimAt, time.Minute, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 ||
		claims[0].SandboxID != "sandbox-batch-a" ||
		claims[1].SandboxID != "sandbox-batch-b" ||
		claims[0].Revision != 1 || claims[1].Revision != 1 ||
		claims[0].WorkerID != "worker-batch" || claims[1].WorkerID != "worker-batch" ||
		claims[0].ActiveSessions != 0 {
		t.Fatalf("batch claims = %#v", claims)
	}
	var leaseState, sessionState string
	var sandboxRevision int64
	var sandboxUpdatedAt time.Time
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT lease.state,session.state,sandbox.revision,sandbox.updated_at
		FROM secondbox.leases AS lease
		JOIN secondbox.activity_sessions AS session ON session.lease_id=lease.id
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=lease.sandbox_id
		WHERE lease.id='lease-batch-expired'`,
	).Scan(&leaseState, &sessionState, &sandboxRevision, &sandboxUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if leaseState != contracts.LeaseStateExpired || sessionState != "closed" ||
		sandboxRevision != 1 || !sandboxUpdatedAt.Equal(now) {
		t.Fatalf(
			"claim side effects = lease/session %q/%q Sandbox revision/updated %d/%s",
			leaseState, sessionState, sandboxRevision, sandboxUpdatedAt,
		)
	}
	if later, err := controlPlaneStore.ClaimLifecycleBatch(
		t.Context(), "worker-batch-other", claimAt, time.Minute, 2,
	); err != nil || len(later) != 0 {
		t.Fatalf("claimed fenced cohort = %#v, %v", later, err)
	}
}

func TestPostgresAutomaticRetirementStopsDesiredCompute(t *testing.T) {
	for _, terminationReason := range []string{
		contracts.TerminationReasonIdleTimeout,
		contracts.TerminationReasonMaximumDuration,
	} {
		t.Run(terminationReason, func(t *testing.T) {
			controlPlaneStore := openStoreTest(t)
			now := time.Date(2026, 7, 30, 3, 10, 0, 0, time.UTC)
			sandboxID := "sandbox-automatic-retirement-" + terminationReason
			workspaceID := "workspace-automatic-retirement-" + terminationReason
			if _, err := controlPlaneStore.pool.Exec(t.Context(), `
				INSERT INTO secondbox.workspaces (
					id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
					logical_capacity_bytes,generation,mutation_kind,mutation_id,
					mutation_effect_id,mutation_operation_id,mutation_expected_generation,
					mutation_target_generation,mutation_state,local_receipt_json,
					created_at,updated_at
				) VALUES (
					$2,'tenant','subject',
					$3,'runner-home','ready',1048576,1,
					'','','','',NULL,NULL,'','{}',$1,$1
				);
				INSERT INTO secondbox.sandboxes (
					id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
					generation,workspace_id,current_instance_id,metadata_json,
					compatibility_summary_json,reconcile_owner,reconcile_claim_expires_at,
					revision,created_at,updated_at
				) VALUES (
					$3,'tenant','subject','profile','revision',
					'ready','running',1,$2,'instance',
					'{}','{}','lifecycle-worker',$4,7,$1,$1
				)`,
				pgx.QueryExecModeSimpleProtocol,
				now,
				workspaceID,
				sandboxID,
				now.Add(time.Minute),
			); err != nil {
				t.Fatal(err)
			}
			claim := ports.LifecycleReconcileClaim{
				SandboxID:     sandboxID,
				ObservedState: contracts.SandboxStateReady,
				DesiredState:  contracts.SandboxDesiredStateRunning,
				WorkerID:      "lifecycle-worker",
				Revision:      7,
			}
			if err := controlPlaneStore.ApplyLifecycleAction(
				t.Context(),
				claim,
				"drain",
				terminationReason,
				now,
				now.Add(time.Second),
			); err != nil {
				t.Fatal(err)
			}
			var state, desiredState string
			if err := controlPlaneStore.pool.QueryRow(t.Context(), `
				SELECT state,desired_state
				FROM secondbox.sandboxes
				WHERE id=$1`,
				sandboxID,
			).Scan(&state, &desiredState); err != nil {
				t.Fatal(err)
			}
			if state != contracts.SandboxStateDraining ||
				desiredState != contracts.SandboxDesiredStateStopped {
				t.Fatalf("automatic retirement state=%q desired=%q", state, desiredState)
			}
		})
	}
}

func TestPostgresFinishStopAdvancesGenerationAndFencesActivity(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,
			effect_deadline,claim_owner,claim_expires_at,failure_class,failure_message,
			payload_json,evidence_json,created_at,updated_at
		) VALUES (
			'effect_store_stop','sbx_store_stop',3,'stop','runner_succeeded','','',
			'runner-store','command_store_stop','',$1,0,8,$2,'',$2,'','','{}','{}',$3,$3
		)`,
		[]byte("01234567890123456789012345678901"), now.Add(time.Minute), now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,
			mutation_state,local_receipt_json,created_at,updated_at
		) VALUES (
			'wrk_store_stop','tenant','subject','sbx_store_stop','runner-store','ready',1048576,
			3,'stop','effect_store_stop','effect_store_stop','effect_store_stop',
			3,4,'runner_succeeded','{}',$1,$1
		)`,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,reconcile_claim_expires_at,revision,created_at,updated_at
		) VALUES (
			'sbx_store_stop','tenant','subject','profile','revision','stopping','stopped',
			3,'wrk_store_stop','ins_store_stop','{}','{}','store-worker',$2,7,$1,$1
		)`,
		now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.leases (
			id,tenant_ref,subject_ref,sandbox_id,generation,state,
			expires_at,revision,created_at,updated_at
		) VALUES ('lease_store_stop','tenant','subject','sbx_store_stop',3,'active',$2,1,$1,$1)`,
		now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,
			last_activity_at,created_at,updated_at
		) VALUES ('activity_store_stop','tenant','subject','sbx_store_stop',3,'terminal','active',
		          'lease_store_stop',$1,$1,$1)`, now,
	); err != nil {
		t.Fatal(err)
	}
	claim := ports.LifecycleReconcileClaim{
		SandboxID: "sbx_store_stop", ObservedState: contracts.SandboxStateStopping,
		DesiredState: contracts.SandboxDesiredStateStopped,
		WorkerID:     "store-worker", Revision: 7,
	}
	if err := controlPlaneStore.ApplyLifecycleAction(
		t.Context(), claim, "finish_stop", contracts.TerminationReasonRequestedStop,
		now, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	var sandboxGeneration, workspaceGeneration int64
	var sandboxState, leaseState, activityState string
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT sandbox.generation,workspace.generation,sandbox.state,lease.state,activity.state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.leases AS lease ON lease.sandbox_id=sandbox.id
		JOIN secondbox.activity_sessions AS activity ON activity.sandbox_id=sandbox.id
		WHERE sandbox.id='sbx_store_stop'`,
	).Scan(
		&sandboxGeneration, &workspaceGeneration, &sandboxState, &leaseState, &activityState,
	); err != nil {
		t.Fatal(err)
	}
	if sandboxGeneration != 4 || workspaceGeneration != 4 ||
		sandboxState != contracts.SandboxStateStopped ||
		leaseState != contracts.LeaseStateFenced || activityState != "closed" {
		t.Fatalf(
			"finish_stop generations=%d/%d states=%s/%s/%s",
			sandboxGeneration, workspaceGeneration, sandboxState, leaseState, activityState,
		)
	}
}
