package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func cleanupArchiveLivenessSandboxes(t *testing.T, storage *PostgresControlPlaneStore, sandboxIDs *[]string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_, err := storage.pool.Exec(ctx, `
 DELETE FROM secondbox.runner_commands WHERE assignment_id IN (SELECT id FROM secondbox.workspace_archives WHERE sandbox_id=ANY($1::text[]));
 DELETE FROM secondbox.audit_events WHERE resource_id=ANY($1::text[]);
 DELETE FROM secondbox.idempotency_records WHERE target_id=ANY($1::text[]);
 DELETE FROM secondbox.operation_stage_timings WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.workspace_archives WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.operations WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.workspaces WHERE sandbox_id=ANY($1::text[]);
 DELETE FROM secondbox.sandboxes WHERE id=ANY($1::text[]);`, pgx.QueryExecModeSimpleProtocol, *sandboxIDs)
		if err != nil {
			t.Errorf("archive liveness fixture cleanup: %v", err)
		}
	})
}

func TestFleetKeepsRestoreDemandAndProtectsItsTarget(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	const pool, runnerID, revision = "archive-restore-busy-pool", "archive-restore-busy-runner", "archive-restore-busy-revision"
	if _, err := storage.CreateRunnerPool(t.Context(), contracts.RunnerPool{Name: pool, State: "ready", Architectures: []string{"amd64"}, Capabilities: []string{"compute"}, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	seedPlacementRunner(t, storage, pool, runnerID, now.Add(-time.Hour))
	seedPlacementProfileRevision(t, storage, revision, contracts.ProfileRevisionSpec{Pool: pool}, now)
	workspaceID, sandboxID := seedLocalWorkspace(t, storage, "archive-restore-busy", now)
	sandboxIDs := []string{sandboxID}
	cleanupArchiveLivenessSandboxes(t, storage, &sandboxIDs)
	tx, err := storage.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	operation := localTestOperation("archive-restore-busy-start", "start", now)
	operation.SandboxID = sandboxID
	if err := insertOperation(t.Context(), tx, "tenant-local", "subject-local", operation); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE secondbox.sandboxes SET desired_state='running',lifecycle_failure_class='',profile_revision_id=$2 WHERE id=$1`, sandboxID, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='restoring',home_runner_id='',mutation_kind='archive_restore',mutation_state='queued',mutation_operation_id=$2 WHERE id=$1`, workspaceID, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO secondbox.workspace_archives(id,operation_id,sandbox_id,workspace_id,source_runner_id,target_runner_id,generation,logical_capacity_bytes,archive_key,state,command_id,fencing_token,created_at,updated_at) VALUES('archive-restore-busy',$1,$2,$3,'',$4,3,8589934592,'fixture/archive','restore_queued_sent','archive-restore-busy-command','fixture-fence',$5,$5)`, operation.ID, sandboxID, workspaceID, runnerID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"restore_queued", "restore_queued_sent"} {
		if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspace_archives SET state=$1 WHERE id='archive-restore-busy'`, phase); err != nil {
			t.Fatal(err)
		}
		observed, err := storage.ReadRunnerFleet(t.Context(), pool, now)
		if err != nil {
			t.Fatal(err)
		}
		if observed.Demand.Instances != 1 || len(observed.Runners) != 1 || observed.Runners[0].PendingMutations != 1 {
			t.Fatalf("%s lost restore capacity: demand=%+v runners=%+v", phase, observed.Demand, observed.Runners)
		}
		runner := observed.Runners[0]
		if _, err := storage.ChangeRunnerFleetState(t.Context(), pool, runnerID, "evacuate", runner.Revision, now); !errors.Is(err, ports.ErrInvalidLifecycleTransition) {
			t.Fatalf("%s allowed evacuation during restore: %v", phase, err)
		}
	}
}

func TestIdleArchiveSkipsOfflineHomesBeforeItsCandidateLimit(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	storage.ConfigureWorkspaceArchives(&archiveTransferTestProvider{})
	const offline = "archive-idle-offline-runner"
	seedPlacementRunner(t, storage, "pool-local", offline, now)
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET state='offline',active_connection_id='' WHERE id=$1`, offline); err != nil {
		t.Fatal(err)
	}
	var sandboxIDs []string
	cleanupArchiveLivenessSandboxes(t, storage, &sandboxIDs)
	for index := range 16 {
		workspaceID, sandboxID := seedLocalWorkspace(t, storage, fmt.Sprintf("archive-idle-offline-%02d", index), now.Add(-3*time.Hour+time.Duration(index)*time.Second))
		sandboxIDs = append(sandboxIDs, sandboxID)
		if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET home_runner_id=$2 WHERE id=$1`, workspaceID, offline); err != nil {
			t.Fatal(err)
		}
	}
	workspaceID, sandboxID := seedLocalWorkspace(t, storage, "archive-idle-available", now.Add(-2*time.Hour))
	sandboxIDs = append(sandboxIDs, sandboxID)
	if err := storage.archiveIdleWorkspaces(t.Context(), now, map[string]int64{"profile-local": 3600}); err != nil {
		t.Fatal(err)
	}
	var mutation string
	if err := storage.pool.QueryRow(t.Context(), `SELECT mutation_kind FROM secondbox.workspaces WHERE id=$1`, workspaceID).Scan(&mutation); err != nil {
		t.Fatal(err)
	}
	if mutation != "archive" {
		t.Fatalf("offline homes exhausted the candidate page: available mutation=%q", mutation)
	}
}
