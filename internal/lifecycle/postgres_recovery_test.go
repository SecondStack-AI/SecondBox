package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	controlstore "github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type recoverySessions struct{}

func (recoverySessions) CancelSandboxSessions(context.Context, string, int64, string, time.Time) (int64, error) {
	return 0, nil
}

type recoveryFixture struct {
	pool   *pgxpool.Pool
	worker lifecycle.Reconciler
	now    time.Time
}

func newRecoveryFixture(t *testing.T, assignmentState string) *recoveryFixture {
	t.Helper()
	url := openLifecyclePostgresTestDatabase(t)
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store, err := controlstore.NewPostgresControlPlaneStore(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	// Reject before Assignment creation after cleanup, to prove the public
	// recovery Operation retains its identity even on this failure path.
	spec, err := json.Marshal(contracts.ProfileRevisionSpec{Network: contracts.NetworkPolicy{
		Mode: "invalid", RequiresTenantEgressContext: new(bool),
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (id,profile_name,revision_number,spec_json,created_at)
		VALUES ('revision','profile',1,$2,$1);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,local_receipt_json,created_at,updated_at
		) VALUES ('workspace','tenant','subject','sandbox','runner','ready',8589934592,
			3,'start','operation','operation','operation',3,3,'queued','{}',$1,$1);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,generation,
			workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			vcpu_count,memory_bytes,workspace_bytes,revision,next_reconcile_at,created_at,updated_at,lifecycle_failure_class
		) VALUES ('sandbox','tenant','subject','profile','revision','failed','running',3,
			'workspace','instance','{}','{}',1,1073741824,8589934592,1,$1,$1,$1,'');
		INSERT INTO secondbox.instances (id,sandbox_id,generation,state,guest_liveness,termination_reason,created_at,updated_at)
		VALUES ('instance','sandbox',3,
			CASE WHEN $4='ready' THEN 'ready' ELSE 'failed' END,
			CASE WHEN $4='ready' THEN 'ready' ELSE 'lost' END,
			CASE WHEN $4='ready' THEN '' ELSE 'startup_failed' END,$1,$1);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,backend_reference,
			generation,fencing_token,state,capability_snapshot_json,resolved_artifacts_json,release_proof_json,
			failure_class,retry_count,retry_limit,operation_deadline,claim_expires_at,reconcile_owner,
			reconcile_claim_expires_at,next_reconcile_at,revision,created_at,updated_at
		) VALUES ('assignment','sandbox','instance','runner','revision','firecracker','',3,$3,$4,
			'{}','{}','{}','transient',1,1,$1,$1,'',$1,$1,1,$1,$1);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES ('operation','tenant','subject','sandbox','','start','pending','request','{}','','',false,$1,$1)`,
		pgx.QueryExecModeSimpleProtocol, now, string(spec), []byte("01234567890123456789012345678901"), assignmentState)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &recoveryFixture{pool: pool, now: now}
	ids := 0
	broker, err := lifecycle.NewPostgresEffectBroker(t.Context(), url, unusedAssignmentScheduler{}, lifecycle.EffectBrokerConfig{
		AssignmentClaimDuration: time.Minute, HeartbeatTimeout: time.Minute, AssetCatalog: unusedAssetCatalog{},
		AssignmentDeadline: time.Second, RetryLimit: 1, SessionCanceller: recoverySessions{},
		NewID:           func(prefix string) string { ids++; return fmt.Sprintf("%s-%d", prefix, ids) },
		NewFencingToken: func() ([]byte, error) { return []byte("01234567890123456789012345678901"), nil },
		Now:             func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	fixture.worker = lifecycle.Reconciler{Store: store, Effects: broker, WorkerID: "worker",
		ClaimDuration: time.Minute, PollInterval: time.Second, BatchSize: 1}
	return fixture
}

func (fixture *recoveryFixture) step(t *testing.T, want lifecycle.Action) {
	t.Helper()
	decision, found, err := fixture.worker.RunOnce(t.Context(), fixture.now, ports.LifecycleWakeTriggerDeadline)
	if err != nil || !found || decision.Action != want {
		t.Fatalf("decision=%+v found=%t error=%v, want %s", decision, found, err, want)
	}
	fixture.now = fixture.now.Add(2 * time.Second)
}

func (fixture *recoveryFixture) exec(t *testing.T, query string) {
	t.Helper()
	if _, err := fixture.pool.Exec(t.Context(), query, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
}

func TestStartupFailureCleanupRetriesAndFailsWithoutRunnerReceipt(t *testing.T) {
	fixture := newRecoveryFixture(t, "failed_terminal")
	fixture.step(t, lifecycle.ActionStopInstance)
	fixture.step(t, lifecycle.ActionStopInstance)
	fixture.step(t, lifecycle.ActionStopInstance)
	fixture.step(t, lifecycle.ActionFail)
	var state, instanceID, operationState, mutation string
	var generation int64
	err := fixture.pool.QueryRow(t.Context(), `SELECT sandbox.state,sandbox.current_instance_id,sandbox.generation,
		operation.state,workspace.mutation_state FROM secondbox.sandboxes sandbox
		JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.operations operation ON operation.id='operation' WHERE sandbox.id='sandbox'`).Scan(
		&state, &instanceID, &generation, &operationState, &mutation)
	if err != nil {
		t.Fatal(err)
	}
	if state != "failed" || instanceID != "instance" || generation != 3 || operationState != "failed" || mutation != "" {
		t.Fatalf("state=%s instance=%s generation=%d operation=%s mutation=%s", state, instanceID, generation, operationState, mutation)
	}
}

func TestExhaustedStopRecoveryRebindsStartBeforeProfileRejection(t *testing.T) {
	for _, runnerLoss := range []bool{false, true} {
		t.Run(fmt.Sprintf("runner_loss=%t", runnerLoss), func(t *testing.T) {
			testExhaustedStopRecovery(t, runnerLoss)
		})
	}
}

func testExhaustedStopRecovery(t *testing.T, runnerLoss bool) {
	t.Helper()
	fixture := newRecoveryFixture(t, "ready")
	fixture.step(t, lifecycle.ActionStopInstance)
	if runnerLoss {
		fixture.exec(t, `UPDATE secondbox.lifecycle_effects SET id='runner-loss-stop-assignment' WHERE sandbox_id='sandbox';
			UPDATE secondbox.workspaces SET mutation_id='runner-loss-stop-assignment',mutation_effect_id='runner-loss-stop-assignment',
			mutation_operation_id='runner-loss-stop-assignment',mutation_state='advancing' WHERE id='workspace';
			UPDATE secondbox.assignments SET state='released' WHERE id='assignment';
			UPDATE secondbox.instances SET state='stopped',guest_liveness='lost' WHERE id='instance';
			UPDATE secondbox.runner_commands SET kind='local-workspace',assignment_id='runner-loss-stop-assignment',payload='receipt-command'::bytea;`)
	}
	fixture.step(t, lifecycle.ActionStopInstance)
	fixture.step(t, lifecycle.ActionStopInstance)
	fixture.step(t, lifecycle.ActionFail)
	fixture.exec(t, `INSERT INTO secondbox.operations (
		id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
		request_metadata_json,error_code,error_message,retryable,created_at,updated_at
	) VALUES ('operation-retry','tenant','subject','sandbox','','start','pending','request-retry','{}','','',false,now(),now());
		UPDATE secondbox.sandboxes SET lifecycle_failure_class='',next_reconcile_at=created_at WHERE id='sandbox';
		UPDATE secondbox.workspaces SET mutation_kind='start',mutation_id='operation-retry',mutation_effect_id='operation-retry',
		mutation_operation_id='operation-retry',mutation_expected_generation=3,mutation_target_generation=3,mutation_state='queued' WHERE id='workspace';`)
	fixture.step(t, lifecycle.ActionStopInstance)
	var state string
	var retries, limit int64
	if err := fixture.pool.QueryRow(t.Context(), `SELECT state,retry_count,retry_limit FROM secondbox.lifecycle_effects WHERE sandbox_id='sandbox'`).Scan(&state, &retries, &limit); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || retries != 2 || limit != 3 {
		t.Fatalf("renewal state=%s retries=%d limit=%d", state, retries, limit)
	}
	var effectCount int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.lifecycle_effects WHERE sandbox_id='sandbox'`).Scan(&effectCount); err != nil {
		t.Fatal(err)
	}
	if effectCount != 1 {
		t.Fatalf("recovery created a second stop effect: %d", effectCount)
	}
	fixture.exec(t, `UPDATE secondbox.instances SET state='stopped',guest_liveness='stopped' WHERE id='instance';
		UPDATE secondbox.assignments SET state='released' WHERE id='assignment';
		UPDATE secondbox.lifecycle_effects SET state='runner_succeeded' WHERE sandbox_id='sandbox';
		UPDATE secondbox.workspaces SET mutation_state='runner_succeeded' WHERE id='workspace';`)
	fixture.step(t, lifecycle.ActionFinishStop)
	var operationID string
	var generation int64
	if err := fixture.pool.QueryRow(t.Context(), `SELECT mutation_operation_id,mutation_expected_generation FROM secondbox.workspaces WHERE id='workspace'`).Scan(&operationID, &generation); err != nil {
		t.Fatal(err)
	}
	if operationID != "operation-retry" || generation != 4 {
		t.Fatalf("successor operation=%s generation=%d", operationID, generation)
	}
	fixture.step(t, lifecycle.ActionStartInstance)
	var code string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT state,error_code FROM secondbox.operations WHERE id='operation-retry'`).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != "profile_unavailable" {
		t.Fatalf("operation=%s error=%s", state, code)
	}
}

func TestCleanupRetriesLocalGenerationAdvanceAfterAssignmentRelease(t *testing.T) {
	fixture := newRecoveryFixture(t, "failed_terminal")
	fixture.step(t, lifecycle.ActionStopInstance)
	fixture.exec(t, `UPDATE secondbox.assignments SET state='released' WHERE id='assignment';
		UPDATE secondbox.instances SET state='stopped',guest_liveness='stopped' WHERE id='instance';
		UPDATE secondbox.workspaces SET mutation_state='advancing' WHERE id='workspace';
		UPDATE secondbox.runner_commands SET kind='local-workspace',assignment_id=(SELECT id FROM secondbox.lifecycle_effects WHERE sandbox_id='sandbox'),payload='receipt-command'::bytea;`)
	fixture.step(t, lifecycle.ActionStopInstance)
	var kind, commandOwner, effectID, mutation string
	var payload []byte
	if err := fixture.pool.QueryRow(t.Context(), `SELECT command.kind,command.assignment_id,command.payload,effect.id,workspace.mutation_state
		FROM secondbox.lifecycle_effects effect JOIN secondbox.runner_commands command ON command.id=effect.command_id
		JOIN secondbox.workspaces workspace ON workspace.mutation_effect_id=effect.id WHERE effect.sandbox_id='sandbox'`).Scan(&kind, &commandOwner, &payload, &effectID, &mutation); err != nil {
		t.Fatal(err)
	}
	if kind != "local-workspace" || commandOwner != effectID || string(payload) != "receipt-command" || mutation != "advancing" {
		t.Fatalf("kind=%s owner=%s effect=%s payload=%s mutation=%s", kind, commandOwner, effectID, payload, mutation)
	}
}
