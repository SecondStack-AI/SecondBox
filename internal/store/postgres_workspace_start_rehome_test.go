package store

import (
	"context"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestResidentStartOffloadRefusesSnapshotsAndUnsettledAssignments(t *testing.T) {
	storage := openStoreTest(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	storage.ConfigureWorkspaceArchives(&archiveTransferTestProvider{})
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET reserved_capacity_json=capacity_json WHERE id='runner-home'`); err != nil {
		t.Fatal(err)
	}
	for _, barrier := range []string{"snapshot", "assignment"} {
		t.Run(barrier, func(t *testing.T) {
			suffix := "start-rehome-barrier-" + barrier
			workspaceID, sandboxID := seedLocalWorkspace(t, storage, suffix, now)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(t.Context())
				for _, query := range []string{`DELETE FROM secondbox.snapshots WHERE sandbox_id=$1`, `DELETE FROM secondbox.assignments WHERE sandbox_id=$1`, `DELETE FROM secondbox.operation_stage_timings WHERE sandbox_id=$1`, `DELETE FROM secondbox.operations WHERE sandbox_id=$1`, `DELETE FROM secondbox.sandboxes WHERE id=$1`, `DELETE FROM secondbox.workspaces WHERE sandbox_id=$1`} {
					if _, err := storage.pool.Exec(ctx, query, sandboxID); err != nil {
						t.Error(err)
					}
				}
			})
			if barrier == "snapshot" {
				seedReadyLocalSnapshot(t, storage, suffix, sandboxID, workspaceID, now)
			} else {
				if _, err := storage.pool.Exec(t.Context(), `INSERT INTO secondbox.assignments(id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,backend_reference,generation,fencing_token,state,capability_snapshot_json,resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,next_reconcile_at,revision,created_at,updated_at) VALUES($1,$2,$1,'runner-home','revision-local','firecracker','test',3,'fence','uncertain','{}','{}','{}','',0,8,$3,$3,'',$3,$3,1,$3,$3)`, "assignment-"+suffix, sandboxID, now); err != nil {
					t.Fatal(err)
				}
			}
			operation := localTestOperation("operation-"+suffix, "start", now)
			if _, err := storage.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateRunning, Operation: operation, AuditEvent: testLifecycleAudit(), ExpectedRevision: 1, IdempotencyKey: operation.ID, RequestHash: operation.ID, IdempotencyEnds: now.Add(time.Hour), Now: now}); err != nil {
				t.Fatal(err)
			}
			if err := storage.prepareResidentWorkspaceStart(t.Context(), sandboxID, now); err != nil {
				t.Fatal(err)
			}
			var home, mutation, state string
			var archives int
			if err := storage.pool.QueryRow(t.Context(), `SELECT home_runner_id,mutation_kind,state,(SELECT count(*) FROM secondbox.workspace_archives WHERE sandbox_id=$2) FROM secondbox.workspaces WHERE id=$1`, workspaceID, sandboxID).Scan(&home, &mutation, &state, &archives); err != nil {
				t.Fatal(err)
			}
			if home != "runner-home" || mutation != "start" || state != "ready" || archives != 0 {
				t.Fatalf("%s failed to prevent offload: home=%q mutation=%q state=%q archives=%d", barrier, home, mutation, state, archives)
			}
			tx, err := storage.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var retained int
			query := `SELECT count(*) FROM secondbox.assignments WHERE sandbox_id=$1 AND state='uncertain'`
			if barrier == "snapshot" {
				query = `SELECT count(*) FROM secondbox.snapshots WHERE sandbox_id=$1 AND state='ready'`
			}
			err = tx.QueryRow(t.Context(), query, sandboxID).Scan(&retained)
			_ = tx.Rollback(t.Context())
			if err != nil || retained != 1 {
				t.Fatalf("%s authority was removed: retained=%d err=%v", barrier, retained, err)
			}
		})
	}
}
