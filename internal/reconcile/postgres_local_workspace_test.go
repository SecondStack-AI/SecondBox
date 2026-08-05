package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	controlstore "github.com/SecondStack-AI/SecondBox/internal/store"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
)

var reconcileTestDatabaseSequence atomic.Uint64

func TestClaimNextIgnoresAssignmentWithoutCurrentSandboxAuthority(t *testing.T) {
	store := openReconcileTestDatabase(t)
	now := time.Date(2026, 7, 30, 3, 0, 0, 0, time.UTC)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sandbox-stale','tenant','subject','profile','revision','stopped','stopped',
			2,'workspace-stale','instance-current','{}','{}',1,$1,$1
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-stale','sandbox-stale','instance-stale','runner-home','revision',
			'firecracker','instance-stale',1,$2,'uncertain','{}','{}','{}','transient',
			0,8,$3,$3,'',$1,$1,1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		[]byte("01234567890123456789012345678901"),
		now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	_, found, err := store.ClaimNext(
		t.Context(),
		"reconcile-worker",
		now.Add(time.Minute),
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("stale Assignment was claimed without current Sandbox authority")
	}
}

func TestFencedRunnerLossQueuesHomeLocalAdvanceWithoutRelocation(t *testing.T) {
	store := openReconcileTestDatabase(t)
	now := time.Date(2026, 7, 29, 21, 0, 0, 0, time.UTC)
	token := []byte("01234567890123456789012345678901")
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-loss','tenant','subject','sandbox-loss','runner-home','ready',
			8589934592,3,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sandbox-loss','tenant','subject','profile','revision','ready','running',
			3,'workspace-loss','instance-loss','{}','{}',4,$1,$1
		);
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at
		) VALUES (
			'instance-loss','sandbox-loss',3,'stopped','stopped','runner_lost',$1,$1,$1
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-loss','sandbox-loss','instance-loss','runner-home','revision',
			'firecracker','instance-loss',3,$2,'fenced','{}','{}',
			'{"terminationEvidenceDigest":"sha256:proved"}','fencing',0,8,
			$3,$3,'worker',$3,$1,7,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now, token, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	nextGeneration, err := store.AdvanceFencedGeneration(
		t.Context(), "assignment-loss", 7, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if nextGeneration != 4 {
		t.Fatalf("reported next generation = %d", nextGeneration)
	}
	var (
		sandboxState, desiredState, assignmentState, homeRunnerID     string
		mutationKind, mutationState, effectRunnerID, commandRunnerID  string
		sandboxGeneration, workspaceGeneration, replacementOperations int64
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT sandbox.state,sandbox.desired_state,sandbox.generation,
		       workspace.home_runner_id,workspace.generation,
		       workspace.mutation_kind,workspace.mutation_state,
		       assignment.state,effect.runner_id,command.runner_id,
		       (SELECT count(*) FROM secondbox.operations
		        WHERE sandbox_id=sandbox.id AND request_id LIKE 'request-runner-loss-%')
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.assignments AS assignment ON assignment.sandbox_id=sandbox.id
		JOIN secondbox.lifecycle_effects AS effect ON effect.id=workspace.mutation_effect_id
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE sandbox.id='sandbox-loss'`,
	).Scan(
		&sandboxState, &desiredState, &sandboxGeneration,
		&homeRunnerID, &workspaceGeneration, &mutationKind, &mutationState,
		&assignmentState, &effectRunnerID, &commandRunnerID, &replacementOperations,
	); err != nil {
		t.Fatal(err)
	}
	if sandboxState != "stopping" ||
		desiredState != "running" ||
		sandboxGeneration != 3 ||
		workspaceGeneration != 3 ||
		homeRunnerID != "runner-home" ||
		mutationKind != "stop" ||
		mutationState != "advancing" ||
		assignmentState != "released" ||
		effectRunnerID != "runner-home" ||
		commandRunnerID != "runner-home" ||
		replacementOperations != 0 {
		t.Fatalf(
			"state=%q/%q generations=%d/%d home=%q mutation=%q/%q assignment=%q runners=%q/%q replacements=%d",
			sandboxState, desiredState, sandboxGeneration, workspaceGeneration,
			homeRunnerID, mutationKind, mutationState, assignmentState,
			effectRunnerID, commandRunnerID, replacementOperations,
		)
	}
}

func TestFencedRunnerLossAndWorkspaceMutationSerializeWithoutDeadlock(t *testing.T) {
	reconcileStore := openReconcileTestDatabase(t)
	databaseURL := reconcileStore.pool.Config().ConnConfig.ConnString()
	controlStore, err := controlstore.NewPostgresControlPlaneStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controlStore.Close)
	now := time.Date(2026, 7, 29, 21, 30, 0, 0, time.UTC)
	token := []byte("01234567890123456789012345678901")
	if _, err := reconcileStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-loss-race','tenant','subject','sandbox-loss-race','runner-home','ready',
			8589934592,3,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sandbox-loss-race','tenant','subject','profile','revision','ready','running',
			3,'workspace-loss-race','instance-loss-race','{}','{}',4,$1,$1
		);
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at
		) VALUES (
			'instance-loss-race','sandbox-loss-race',3,'stopped','stopped','runner_lost',
			$1,$1,$1
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-loss-race','sandbox-loss-race','instance-loss-race','runner-home',
			'revision','firecracker','instance-loss-race',3,$2,'fenced','{}','{}',
			'{"terminationEvidenceDigest":"sha256:proved"}','fencing',0,8,
			$3,$3,'worker',$3,$1,7,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now, token, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	go func() {
		<-start
		_, err := reconcileStore.AdvanceFencedGeneration(
			ctx, "assignment-loss-race", 7, now.Add(time.Second),
		)
		results <- err
	}()
	go func() {
		<-start
		_, _, err := controlStore.AcquireWorkspaceMutation(
			ctx,
			ports.WorkspaceMutationInput{
				TenantRef: "tenant", SubjectRef: "subject",
				SandboxID: "sandbox-loss-race", WorkspaceID: "workspace-loss-race",
				HomeRunnerID: "runner-home", Kind: "snapshot_create",
				MutationID: "snapshot-loss-race", EffectID: "snapshot-loss-race",
				OperationID: "snapshot-loss-race", ExpectedGeneration: 3,
				TargetGeneration: 3, Now: now,
			},
		)
		results <- err
	}()
	close(start)
	var successes, conflicts int
	for index := 0; index < 2; index++ {
		select {
		case err := <-results:
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ports.ErrWorkspaceMutation),
				strings.Contains(fmt.Sprint(err), "runner loss conflicts"):
				conflicts++
			default:
				t.Fatalf("runner-loss mutation race error = %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("runner-loss mutation race did not terminate: %v", ctx.Err())
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf(
			"runner-loss mutation race successes=%d conflicts=%d",
			successes, conflicts,
		)
	}
}

func openReconcileTestDatabase(t *testing.T) *PostgresStore {
	t.Helper()
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("SECONDBOX_TEST_DATABASE_URL is required for PostgreSQL reconcile tests")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf(
		"secondbox_reconcile_test_%d_%d",
		os.Getpid(), reconcileTestDatabaseSequence.Add(1),
	)
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := pgx.Connect(t.Context(), adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		admin.Close(t.Context())
		t.Fatal(err)
	}
	parsed.Path = "/" + databaseName
	testURL := parsed.String()
	if err := postgresmigrations.Apply(t.Context(), testURL); err != nil {
		_, _ = admin.Exec(t.Context(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close(t.Context())
		t.Fatal(err)
	}
	store, err := NewPostgresStore(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanupContext := context.Background()
		if _, err := admin.Exec(
			cleanupContext, "DROP DATABASE "+identifier+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop reconcile test database: %v", err)
		}
		if err := admin.Close(cleanupContext); err != nil {
			t.Errorf("close reconcile test admin connection: %v", err)
		}
	})
	return store
}
