package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/lifecycleprojection"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func seedArchivedWorkspaceDelete(t *testing.T, storage *PostgresControlPlaneStore, suffix string, now time.Time) (string, string, string) {
	t.Helper()
	workspaceID, sandboxID := seedLocalWorkspace(t, storage, suffix, now)
	key := "workspaces/" + suffix
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='archived',home_runner_id='',archive_key=$2,archive_descriptor_json='{"archive_bytes":4096}' WHERE id=$1`, workspaceID, key); err != nil {
		t.Fatal(err)
	}
	operation := localTestOperation("delete-"+suffix, "delete", now)
	if _, err := storage.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateDeleted, Operation: operation, AuditEvent: testLifecycleAudit(), ExpectedRevision: 1, IdempotencyKey: operation.ID, RequestHash: operation.ID, IdempotencyEnds: now.Add(time.Hour), Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET reconcile_owner='cancelled-worker',reconcile_claim_expires_at=$2,next_reconcile_at=$2 WHERE id=$1`, sandboxID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return workspaceID, sandboxID, operation.ID
}

func TestArchivedWorkspaceDeletionAccountsTeardownAndClearsCancelledClaims(t *testing.T) {
	storage := openArchiveConfigurationTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	if err := storage.ConfigureWorkspaceArchives(t.Context(), &archiveTransferTestProvider{}, nil); err != nil {
		t.Fatal(err)
	}
	workspaceID, sandboxID, operationID := seedArchivedWorkspaceDelete(t, storage, "delete-accounting", now)
	claim := ports.LifecycleReconcileClaim{SandboxID: sandboxID, WorkerID: "cancelled-worker", Revision: 2, ObservedState: contracts.SandboxStateStopped, DesiredState: contracts.SandboxDesiredStateDeleted}
	// The ordinary deletion path establishes deleting before terminalization.
	if _, err := storage.pool.Exec(t.Context(), `CREATE FUNCTION secondbox.assert_archive_delete_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='deleted' AND OLD.state<>'deleting' THEN RAISE EXCEPTION 'archive delete skipped deleting barrier'; END IF; RETURN NEW; END $$; CREATE TRIGGER archive_delete_barrier BEFORE UPDATE ON secondbox.sandboxes FOR EACH ROW EXECUTE FUNCTION secondbox.assert_archive_delete_barrier()`); err != nil {
		t.Fatal(err)
	}
	if err := storage.deleteArchivedWorkspaces(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	var finalized bool
	if err := storage.pool.QueryRow(t.Context(), `SELECT s.state='deleted' AND s.current_instance_id='' AND s.deleted_at=$3 AND s.next_reconcile_at IS NULL AND s.reconcile_owner='' AND s.reconcile_claim_expires_at IS NULL AND w.state='deleted' AND w.archive_key='' AND w.archive_descriptor_json IS NULL AND w.mutation_state='' AND o.state='succeeded' AND o.started_at IS NOT NULL AND o.completed_at=$3 FROM secondbox.sandboxes s JOIN secondbox.workspaces w ON w.id=s.workspace_id JOIN secondbox.operations o ON o.id=$2 WHERE s.id=$1`, sandboxID, operationID, now).Scan(&finalized); err != nil || !finalized {
		t.Fatalf("delete finalization=%t err=%v", finalized, err)
	}
	var stages []string
	if err := storage.pool.QueryRow(t.Context(), `SELECT array_agg(stage ORDER BY stage) FROM secondbox.operation_stage_timings WHERE operation_id=$1 AND stage LIKE 'teardown_%'`, operationID).Scan(&stages); err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 || stages[0] != lifecycleprojection.StageTeardownFinalized || stages[1] != lifecycleprojection.StageTeardownWorkspaceDeleteDispatched {
		t.Fatalf("delete teardown stages=%v", stages)
	}
	var queued int
	if err := storage.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.archive_cleanup WHERE archive_key='workspaces/delete-accounting'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued object deletion=%d err=%v", queued, err)
	}
	if err := storage.deleteArchivedWorkspaces(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.ApplyLifecycleAction(t.Context(), claim, "wait", "", now.Add(time.Second), now.Add(time.Minute)); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("cancelled delete claim can reschedule a deleted Sandbox: %v", err)
	}
	var revision int64
	if err := storage.pool.QueryRow(t.Context(), `SELECT revision FROM secondbox.sandboxes WHERE id=$1`, sandboxID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != claim.Revision+2 {
		t.Fatalf("delete replay changed revision: %d workspace=%s", revision, workspaceID)
	}
}

func TestArchivedWorkspaceDeletionRollsBackAuthorityAndContinuesIndependentSandboxes(t *testing.T) {
	storage := openArchiveConfigurationTestStore(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	badWorkspace, badSandbox, _ := seedArchivedWorkspaceDelete(t, storage, "aa-delete-failure", now)
	snapshotWorkspace, snapshotSandbox, _ := seedArchivedWorkspaceDelete(t, storage, "ab-delete-snapshot", now)
	seedReadyLocalSnapshot(t, storage, "archive-delete-retained", snapshotSandbox, snapshotWorkspace, now)
	_, goodSandbox, _ := seedArchivedWorkspaceDelete(t, storage, "zz-delete-success", now)
	if _, err := storage.pool.Exec(t.Context(), `CREATE FUNCTION secondbox.reject_archive_delete_operation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='delete-aa-delete-failure' AND NEW.state='succeeded' THEN RAISE EXCEPTION 'archive delete operation fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER archive_delete_operation BEFORE UPDATE ON secondbox.operations FOR EACH ROW EXECUTE FUNCTION secondbox.reject_archive_delete_operation()`); err != nil {
		t.Fatal(err)
	}
	err := storage.deleteArchivedWorkspaces(t.Context(), now)
	if err == nil || !strings.Contains(err.Error(), "archive delete operation fixture failure") || !strings.Contains(err.Error(), "Snapshot") {
		t.Fatalf("independent deletion errors missing: %v", err)
	}
	var authorityRetained, goodDeleted, snapshotRetained bool
	if err := storage.pool.QueryRow(t.Context(), `SELECT w.state='archived' AND w.archive_key='workspaces/aa-delete-failure' AND w.archive_descriptor_json IS NOT NULL AND s.state='stopped' AND NOT EXISTS(SELECT 1 FROM secondbox.archive_cleanup WHERE archive_key=w.archive_key) FROM secondbox.workspaces w JOIN secondbox.sandboxes s ON s.id=w.sandbox_id WHERE w.id=$1`, badWorkspace).Scan(&authorityRetained); err != nil {
		t.Fatal(err)
	}
	if err := storage.pool.QueryRow(t.Context(), `SELECT state='deleted' FROM secondbox.sandboxes WHERE id=$1`, goodSandbox).Scan(&goodDeleted); err != nil {
		t.Fatal(err)
	}
	if err := storage.pool.QueryRow(t.Context(), `SELECT state='archived' FROM secondbox.workspaces WHERE id=$1`, snapshotWorkspace).Scan(&snapshotRetained); err != nil {
		t.Fatal(err)
	}
	if !authorityRetained || !goodDeleted || !snapshotRetained {
		t.Fatalf("authority=%t healthy deletion=%t retained Snapshot guard=%t bad=%s", authorityRetained, goodDeleted, snapshotRetained, badSandbox)
	}
}
