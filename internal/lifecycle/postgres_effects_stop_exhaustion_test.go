package lifecycle

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
)

var lifecycleEffectTestDatabaseSequence atomic.Uint64

func openLifecycleEffectTestDatabase(t *testing.T) string {
	t.Helper()
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("SECONDBOX_TEST_DATABASE_URL is required for PostgreSQL lifecycle effect tests")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf(
		"secondbox_lifecycle_effect_test_%d_%d",
		os.Getpid(), lifecycleEffectTestDatabaseSequence.Add(1),
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
	databaseURL := parsed.String()
	if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
		_, _ = admin.Exec(t.Context(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close(t.Context())
		t.Fatal(err)
	}
	seedLifecycleEffectTestQuotaLedgers(t, databaseURL)
	t.Cleanup(func() {
		if _, err := admin.Exec(
			context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop lifecycle effect test database: %v", err)
		}
		if err := admin.Close(context.Background()); err != nil {
			t.Errorf("close lifecycle effect test admin connection: %v", err)
		}
	})
	return databaseURL
}

func seedLifecycleEffectTestQuotaLedgers(t *testing.T, databaseURL string) {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(t.Context())
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.tenant_quotas (
			tenant_ref,max_sandboxes,max_active_instances,max_vcpu_count,max_memory_bytes,
			max_snapshots,max_port_sessions,max_concurrent_operations,max_active_subjects,
			max_application_authorities,updated_at
		) VALUES ('tenant',100,100,100000,1099511627776,100,100,100,100,100,now());
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_vcpu_count,
			max_memory_bytes,max_snapshots,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ('tenant','subject',100,100,100000,1099511627776,100,100,100,now())`,
		pgx.QueryExecModeSimpleProtocol,
	); err != nil {
		t.Fatal(err)
	}
}

// A stop effect whose retries are exhausted must reach runner_failed even when
// its runner command is already terminal, and must release the Workspace stop
// mutation: nothing downstream can release it (finish_stop requires
// runner_succeeded), and a held slot blocks the restart that recovers a failed
// Sandbox.
func TestStopRetryExhaustionToleratesExpiredCommandAndReleasesMutation(t *testing.T) {
	databaseURL := openLifecycleEffectTestDatabase(t)
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(t.Context())
	})
	now := time.Date(2026, 8, 6, 11, 30, 0, 0, time.UTC)
	token := []byte("01234567890123456789012345678901")
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-exhaust','tenant','subject','sandbox-exhaust','runner-home','ready',
			8589934592,3,'stop','effect-exhaust','effect-exhaust','effect-exhaust',
			3,4,'stopping','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,revision,created_at,updated_at
		) VALUES (
			'sandbox-exhaust','tenant','subject','profile','revision','stopping','stopped',
			3,'workspace-exhaust','instance-exhaust','{}','{}','worker-exhaust',5,$1,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-exhaust','sandbox-exhaust',3,'stop','queued','assignment-exhaust',
			'instance-exhaust','runner-home','command-exhaust','',$2,2,2,$3,
			'',$1,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-exhaust','runner-home','assignment-exhaust','lifecycle_fence',
			$4,'expired','',1,$1,$1,NULL
		)`,
		pgx.QueryExecModeSimpleProtocol, now, token, now.Add(-time.Minute), []byte{},
	); err != nil {
		t.Fatal(err)
	}
	tx, err := connection.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	broker := &PostgresEffectBroker{}
	handled, err := broker.resumeStopEffect(
		t.Context(),
		tx,
		ports.LifecycleReconcileClaim{
			SandboxID: "sandbox-exhaust",
			WorkerID:  "worker-exhaust",
			Revision:  5,
		},
		"effect-exhaust",
		"command-exhaust",
		"runner-home",
		"assignment-exhaust",
		"workspace-exhaust",
		[]byte{},
		now.Add(time.Minute),
		now,
		now.Add(time.Second),
	)
	if err != nil || !handled {
		t.Fatalf("stop retry exhaustion = %t, %v", handled, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var (
		effectState, failureClass, commandState        string
		mutationKind, mutationState, reconcileOwner    string
		mutationID, mutationEffectID, mutationOperator string
	)
	if err := connection.QueryRow(t.Context(), `
		SELECT effect.state,effect.failure_class,command.state,
		       workspace.mutation_kind,workspace.mutation_state,workspace.mutation_id,
		       workspace.mutation_effect_id,workspace.mutation_operation_id,
		       sandbox.reconcile_owner
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.runner_commands AS command ON command.id='command-exhaust'
		JOIN secondbox.workspaces AS workspace ON workspace.id='workspace-exhaust'
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id='sandbox-exhaust'
		WHERE effect.id='effect-exhaust'`,
	).Scan(
		&effectState, &failureClass, &commandState,
		&mutationKind, &mutationState, &mutationID,
		&mutationEffectID, &mutationOperator, &reconcileOwner,
	); err != nil {
		t.Fatal(err)
	}
	if effectState != "runner_failed" ||
		failureClass != "stop_retry_exhausted" ||
		commandState != "expired" ||
		mutationKind != "" ||
		mutationState != "" ||
		mutationID != "" ||
		mutationEffectID != "" ||
		mutationOperator != "" ||
		reconcileOwner != "" {
		t.Fatalf(
			"effect=%q/%q command=%q mutation=%q/%q/%q/%q/%q owner=%q",
			effectState, failureClass, commandState,
			mutationKind, mutationState, mutationID,
			mutationEffectID, mutationOperator, reconcileOwner,
		)
	}
}
