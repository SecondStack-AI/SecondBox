package lifecycle

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A Profile revision that cannot be resolved into a valid assignment is a
// terminal compatibility failure. It must fail before assignment and release
// its Workspace mutation so an operator can delete the durable Sandbox.
func TestInvalidProfileStartFailsBeforeAssignmentAndReleasesWorkspace(t *testing.T) {
	databaseURL := openLifecycleEffectTestDatabase(t)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Date(2026, 8, 6, 13, 0, 0, 0, time.UTC)
	spec := contracts.ProfileRevisionSpec{
		Network: contracts.NetworkPolicy{Mode: "bogus"},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ('revision-bogus','profile',1,$2,$1);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-profile','tenant','subject','sandbox-profile','runner-home','ready',
			8589934592,3,'start','operation-profile','operation-profile','operation-profile',
			3,3,'queued','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,revision,created_at,updated_at
		) VALUES (
			'sandbox-profile','tenant','subject','profile','revision-bogus','stopped','running',
			3,'workspace-profile','','{}','{}','worker-profile',2,$1,$1
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES (
			'operation-profile','tenant','subject','sandbox-profile','','create','pending',
			'request-profile','{}','','',false,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now, string(specJSON),
	); err != nil {
		t.Fatal(err)
	}
	identifiers := 0
	broker := &PostgresEffectBroker{
		pool: pool,
		config: EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              2,
			NewID: func(kind string) string {
				identifiers++
				return fmt.Sprintf("%s-%d", kind, identifiers)
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
			Now: func() time.Time { return now },
		},
	}
	if err := broker.scheduleAndStart(
		t.Context(),
		ports.LifecycleReconcileClaim{
			SandboxID: "sandbox-profile",
			WorkerID:  "worker-profile",
			Revision:  2,
		},
		now,
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("invalid Profile start = %v, want terminal compatibility failure", err)
	}
	var sandboxState, failureClass, reconcileOwner string
	var nextAt *time.Time
	var mutationState, operationState, operationError string
	var assignments int64
	if err := pool.QueryRow(t.Context(), `
		SELECT sandbox.state,sandbox.lifecycle_failure_class,sandbox.reconcile_owner,
		       sandbox.next_reconcile_at,workspace.mutation_state,
		       operation.state,operation.error_code,
		       (SELECT count(*) FROM secondbox.assignments)
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.operations AS operation ON operation.id='operation-profile'
		WHERE sandbox.id='sandbox-profile'`,
	).Scan(
		&sandboxState, &failureClass, &reconcileOwner, &nextAt, &mutationState,
		&operationState, &operationError, &assignments,
	); err != nil {
		t.Fatal(err)
	}
	if sandboxState != "failed" || failureClass != "compatibility" ||
		reconcileOwner != "" || nextAt != nil || mutationState != "" ||
		operationState != "failed" || operationError != "profile_unavailable" ||
		assignments != 0 {
		t.Fatalf(
			"state=%q failure=%q owner=%q next=%v mutation=%q operation=%q/%q assignments=%d",
			sandboxState, failureClass, reconcileOwner, nextAt, mutationState,
			operationState, operationError, assignments,
		)
	}
}

// A delete effect recorded as succeeded while its Sandbox was never finalized
// is self-contradictory durable state; it must defer and log rather than end
// the reconciler.
func TestSucceededDeleteEffectWithoutFinalizedSandboxDefers(t *testing.T) {
	databaseURL := openLifecycleEffectTestDatabase(t)
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(t.Context())
	})
	now := time.Date(2026, 8, 6, 13, 30, 0, 0, time.UTC)
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-del','tenant','subject','sandbox-del','runner-home','deleting',
			8589934592,3,'workspace_delete','effect-del','effect-del','operation-del',
			3,3,'deleting','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,revision,created_at,updated_at
		) VALUES (
			'sandbox-del','tenant','subject','profile','revision','deleting','deleted',
			3,'workspace-del','','{}','{}','worker-del',6,$1,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-del','sandbox-del',3,'workspace_delete','succeeded','','','runner-home',
			'command-del','',$2,0,2,$3,'',$1,'','','{}','{}',$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now, []byte("01234567890123456789012345678901"), now.Add(-time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	tx, err := connection.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	locked, err := rowlock.SandboxWorkspaceByID(t.Context(), tx, "sandbox-del")
	if err != nil {
		t.Fatal(err)
	}
	broker := &PostgresEffectBroker{}
	handled, err := broker.resumeWorkspaceDeleteEffect(
		t.Context(),
		tx,
		ports.LifecycleReconcileClaim{
			SandboxID: "sandbox-del",
			WorkerID:  "worker-del",
			Revision:  6,
		},
		locked,
		"effect-del",
		"command-del",
		"operation-del",
		"request-del",
		now,
		now.Add(time.Second),
	)
	if err != nil || !handled {
		t.Fatalf("succeeded delete without finalization = %t, %v, want a deferral", handled, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var reconcileOwner, effectState string
	if err := connection.QueryRow(t.Context(), `
		SELECT sandbox.reconcile_owner,effect.state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.lifecycle_effects AS effect ON effect.id='effect-del'
		WHERE sandbox.id='sandbox-del'`,
	).Scan(&reconcileOwner, &effectState); err != nil {
		t.Fatal(err)
	}
	if reconcileOwner != "" || effectState != "succeeded" {
		t.Fatalf("owner=%q effect=%q, want a released claim and untouched effect", reconcileOwner, effectState)
	}
}
