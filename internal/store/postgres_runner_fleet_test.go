package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestRunnerRetirementRequiresEvacuationAndAcknowledgedDrain(t *testing.T) {
	store := openStoreTest(t)
	now := time.Now().UTC()
	const pool, id = "fleet-retirement-pool", "fleet-retirement-runner"
	if _, err := store.CreateRunnerPool(t.Context(), contracts.RunnerPool{Name: pool, State: "ready", Architectures: []string{"amd64"}, Capabilities: []string{"compute"}, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	seedPlacementRunner(t, store, pool, id, now)
	if _, err := store.ChangeRunnerFleetState(t.Context(), pool, id, "retire", 1, now); !errors.Is(err, ports.ErrInvalidLifecycleTransition) {
		t.Fatalf("retirement without evacuation error=%v", err)
	}
	runner, err := store.ChangeRunnerFleetState(t.Context(), pool, id, "evacuate", 1, now)
	if err != nil || runner.FleetState != "evacuating" || runner.PendingCommands != 1 {
		t.Fatalf("runner=%+v err=%v", runner, err)
	}
	if _, err := store.ChangeRunnerFleetState(t.Context(), pool, id, "retire", runner.Revision, now); !errors.Is(err, ports.ErrInvalidLifecycleTransition) {
		t.Fatalf("retirement before Runner drain acknowledgment error=%v", err)
	}
	if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.runner_commands SET state='acknowledged' WHERE id=$1`, "fleet-drain-"+id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `INSERT INTO secondbox.workspaces(id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,mutation_state,local_receipt_json,created_at,updated_at) VALUES('fleet-resident','tenant','subject','sandbox',$1,'ready',67108864,1,'','','','','','{}',$2,$2)`, id, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ChangeRunnerFleetState(t.Context(), pool, id, "retire", runner.Revision, now); !errors.Is(err, ports.ErrInvalidLifecycleTransition) {
		t.Fatalf("retirement with local-only Workspace error=%v", err)
	}
	if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='archived',home_runner_id='' WHERE id='fleet-resident'`); err != nil {
		t.Fatal(err)
	}
	runner, err = store.ChangeRunnerFleetState(t.Context(), pool, id, "retire", runner.Revision, now)
	if err != nil || runner.FleetState != "retired" || runner.Connected {
		t.Fatalf("retired Runner=%+v err=%v", runner, err)
	}
}

func TestFleetObservationWorksWithZeroRunners(t *testing.T) {
	store := openStoreTest(t)
	now := time.Now().UTC()
	const pool = "fleet-empty-pool"
	if _, err := store.CreateRunnerPool(t.Context(), contracts.RunnerPool{Name: pool, State: "ready", Architectures: []string{"amd64"}, Capabilities: []string{"compute"}, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	observation, err := store.ReadRunnerFleet(t.Context(), pool, now)
	if err != nil || observation.PoolName != pool || len(observation.Runners) != 0 || observation.Demand.Instances != 0 {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
}

func TestFleetDemandIncludesUnhomedInitiallyStoppedCreate(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	const pool = "fleet-stopped-create-pool"
	if _, err := storage.CreateRunnerPool(t.Context(), contracts.RunnerPool{Name: pool, State: "ready", Architectures: []string{"amd64"}, Capabilities: []string{"compute"}, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	seedPlacementProfileRevision(t, storage, "fleet-stopped-create-revision", contracts.ProfileRevisionSpec{Pool: pool}, now)
	workspaceID, sandboxID := seedLocalWorkspace(t, storage, "fleet-stopped-create", now)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := storage.pool.Exec(ctx, `DELETE FROM secondbox.operation_stage_timings WHERE sandbox_id=$1`, sandboxID); err != nil {
			t.Error(err)
		}
		if _, err := storage.pool.Exec(ctx, `DELETE FROM secondbox.operations WHERE sandbox_id=$1`, sandboxID); err != nil {
			t.Error(err)
		}
		if _, err := storage.pool.Exec(ctx, `DELETE FROM secondbox.sandboxes WHERE id=$1`, sandboxID); err != nil {
			t.Error(err)
		}
		if _, err := storage.pool.Exec(ctx, `DELETE FROM secondbox.workspaces WHERE id=$1`, workspaceID); err != nil {
			t.Error(err)
		}
	})
	tx, err := storage.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	operation := localTestOperation("fleet-stopped-create-operation", "create", now)
	operation.SandboxID = sandboxID
	deadline := now.Add(time.Minute)
	operation.CapacityWaitDeadline = &deadline
	if err := insertOperation(t.Context(), tx, "tenant-local", "subject-local", operation); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE secondbox.workspaces SET home_runner_id='',state='creating',mutation_kind='create',mutation_state='queued',mutation_operation_id=$2 WHERE id=$1`, workspaceID, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE secondbox.sandboxes SET state='creating',desired_state='stopped',lifecycle_failure_class='',profile_revision_id='fleet-stopped-create-revision' WHERE id=$1`, sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	observation, err := storage.ReadRunnerFleet(t.Context(), pool, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Runners) != 0 || observation.Demand.Instances != 1 || observation.Demand.VCPUCount != 1 || observation.Demand.MemoryBytes != 1<<30 || observation.Demand.WorkspaceBytes != 8<<30 {
		t.Fatalf("unhomed stopped create demand=%+v runners=%v", observation.Demand, observation.Runners)
	}
	expired, err := storage.ReadRunnerFleet(t.Context(), pool, deadline.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if expired.Demand.Instances != 0 {
		t.Fatalf("expired create still demands capacity: %+v", expired.Demand)
	}
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET desired_state='deleted' WHERE id=$1`, sandboxID); err != nil {
		t.Fatal(err)
	}
	deleted, err := storage.ReadRunnerFleet(t.Context(), pool, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Demand.Instances != 0 {
		t.Fatalf("deleted create still demands capacity: %+v", deleted.Demand)
	}
}

func TestFleetExpiresOnlyCancellationsForExactTerminalAssignments(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	const pool = "fleet-cancel-proof-pool"
	const runner = "fleet-cancel-proof-runner"
	if _, err := storage.CreateRunnerPool(t.Context(), contracts.RunnerPool{Name: pool, State: "ready", Architectures: []string{"amd64"}, Capabilities: []string{"compute"}, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	seedPlacementRunner(t, storage, pool, runner, now)
	states := []string{"released", "fenced", "failed_terminal", "ready", "uncertain", "missing", "other-runner"}
	for _, state := range states {
		if state != "missing" {
			owner := runner
			if state == "other-runner" {
				owner = "unrelated-runner"
			}
			assignmentState := state
			if state == "other-runner" {
				assignmentState = "released"
			}
			if _, err := storage.pool.Exec(t.Context(), `INSERT INTO secondbox.assignments(id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,backend_reference,generation,fencing_token,state,capability_snapshot_json,resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,next_reconcile_at,revision,created_at,updated_at) VALUES($1,$1,$1,$2,'profile','gvisor','test',1,'fence',$3,'{}','{}','{}','',0,8,$4,$4,'',$4,$4,1,$4,$4)`, "fleet-cancel-assignment-"+state, owner, assignmentState, now); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := storage.pool.Exec(t.Context(), `INSERT INTO secondbox.runner_commands(id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at) VALUES($1,$2,$3,'data-plane-cancel','','delivered','connection',1,$4,$4)`, "fleet-cancel-command-"+state, runner, "fleet-cancel-assignment-"+state, now); err != nil {
			t.Fatal(err)
		}
	}
	observation, err := storage.ReadRunnerFleet(t.Context(), pool, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Runners) != 1 || observation.Runners[0].PendingCommands != 4 {
		t.Fatalf("live/unproven cancels were ignored: %+v", observation.Runners)
	}
	for _, state := range states {
		var actual string
		if err := storage.pool.QueryRow(t.Context(), `SELECT state FROM secondbox.runner_commands WHERE id=$1`, "fleet-cancel-command-"+state).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		expected := "delivered"
		if state == "released" || state == "fenced" || state == "failed_terminal" {
			expected = "expired"
		}
		if actual != expected {
			t.Fatalf("assignment %s command=%s want=%s", state, actual, expected)
		}
	}
}
