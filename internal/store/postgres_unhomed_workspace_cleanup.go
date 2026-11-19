package store

import (
	"context"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
)

// A never-placed create has no Runner-side storage or execution to acknowledge.
func finishUnhomedWorkspaceCreate(ctx context.Context, tx pgx.Tx, locked rowlock.SandboxWorkspace, state, code, message string, now time.Time) error {
	workspace := locked.Workspace
	if workspace.HomeRunnerID != "" || workspace.Generation != 1 || locked.CurrentInstanceID != "" || len(workspace.LocalReceipt) != 0 || (workspace.State != "creating" && workspace.State != "failed") || (state != "failed" && state != "deleted") {
		return ports.ErrWorkspaceMutation
	}
	var neverPlaced bool
	if err := tx.QueryRow(ctx, `SELECT archive_key='' AND NOT EXISTS(SELECT 1 FROM secondbox.assignments WHERE sandbox_id=$2) AND NOT EXISTS(SELECT 1 FROM secondbox.lifecycle_effects WHERE sandbox_id=$2 AND runner_id<>'') AND NOT EXISTS(SELECT 1 FROM secondbox.snapshots WHERE workspace_id=$1 AND state<>'deleted') FROM secondbox.workspaces WHERE id=$1`, workspace.ID, locked.SandboxID).Scan(&neverPlaced); err != nil {
		return err
	}
	if !neverPlaced {
		return ports.ErrWorkspaceMutation
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.runner_commands SET state='expired',target_connection_id='',updated_at=$2 WHERE runner_id='' AND assignment_id IN (SELECT id FROM secondbox.lifecycle_effects WHERE sandbox_id=$1) AND state IN ('pending','delivering','delivered')`, locked.SandboxID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.lifecycle_effects SET state='failed',failure_class=$2,failure_message=$3,claim_owner='',claim_expires_at=$4,updated_at=$4 WHERE sandbox_id=$1 AND runner_id='' AND state NOT IN ('succeeded','failed','runner_failed')`, locked.SandboxID, code, message, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state=$2,mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',mutation_state='',mutation_expected_generation=NULL,mutation_target_generation=NULL,updated_at=$3 WHERE id=$1`, workspace.ID, state, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET state=$2,desired_state=CASE WHEN $2='deleted' THEN 'deleted' ELSE 'stopped' END,deleted_at=CASE WHEN $2='deleted' THEN $3::timestamptz ELSE NULL END,next_reconcile_at=NULL,reconcile_owner='',reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$3 WHERE id=$1`, locked.SandboxID, state, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state=CASE WHEN kind IN ('delete','stop') THEN 'succeeded' WHEN $2='capacity_wait_expired' THEN 'failed' ELSE 'cancelled' END,retryable=kind NOT IN ('delete','stop') AND $2='capacity_wait_expired',error_code=CASE WHEN kind IN ('delete','stop') THEN '' ELSE $2 END,error_message=CASE WHEN kind IN ('delete','stop') THEN '' ELSE $3 END,completed_at=$4,updated_at=$4 WHERE sandbox_id=$1 AND state IN ('pending','running')`, locked.SandboxID, code, message, now)
	return err
}
