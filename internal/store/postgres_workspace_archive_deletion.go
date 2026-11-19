package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store/lifecycleprojection"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
)

func (store *PostgresControlPlaneStore) deleteArchivedWorkspaces(ctx context.Context, now time.Time) error {
	rows, err := store.pool.Query(ctx, `SELECT sandbox.id FROM secondbox.workspaces workspace JOIN secondbox.sandboxes sandbox ON sandbox.workspace_id=workspace.id WHERE workspace.state='archived' AND sandbox.desired_state='deleted' AND workspace.mutation_state='' ORDER BY sandbox.id`)
	if err != nil {
		return err
	}
	var sandboxIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		sandboxIDs = append(sandboxIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range sandboxIDs {
		if err := store.deleteArchivedWorkspace(ctx, id, now.UTC()); err != nil {
			failures = append(failures, fmt.Errorf("SecondBox archived Workspace deletion %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (store *PostgresControlPlaneStore) deleteArchivedWorkspace(ctx context.Context, sandboxID string, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	workspace := locked.Workspace
	if workspace.State != "archived" || locked.DesiredState != "deleted" || workspace.Mutation.State != "" {
		return nil
	}
	if workspace.HomeRunnerID != "" || locked.CurrentInstanceID != "" || workspace.Generation != locked.Generation {
		return errors.New("SecondBox archived Workspace deletion conflicts with resident authority")
	}
	var retainedSnapshot, activeAssignment, pendingRestore bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secondbox.snapshots WHERE workspace_id=$1 AND state<>'deleted'),EXISTS(SELECT 1 FROM secondbox.assignments WHERE sandbox_id=$2 AND state NOT IN ('fenced','released','failed_terminal')),EXISTS(SELECT 1 FROM secondbox.workspace_restores WHERE workspace_id=$1 AND state NOT IN ('finalized','failed'))`, workspace.ID, sandboxID).Scan(&retainedSnapshot, &activeAssignment, &pendingRestore); err != nil {
		return err
	}
	if retainedSnapshot {
		return errors.New("SecondBox archived Workspace deletion has a retained Snapshot")
	}
	if activeAssignment || pendingRestore {
		return errors.New("SecondBox archived Workspace deletion has unsettled compute or restore authority")
	}
	var key string
	var descriptor []byte
	if err := tx.QueryRow(ctx, `SELECT archive_key,archive_descriptor_json FROM secondbox.workspaces WHERE id=$1`, workspace.ID).Scan(&key, &descriptor); err != nil {
		return err
	}
	if key == "" || len(descriptor) == 0 {
		return errors.New("SecondBox archived Workspace deletion lacks its authoritative object descriptor")
	}
	var operationID string
	if err := tx.QueryRow(ctx, `SELECT id FROM secondbox.operations WHERE sandbox_id=$1 AND kind='delete' AND state IN ('pending','running') ORDER BY created_at DESC,id DESC LIMIT 1`, sandboxID).Scan(&operationID); err != nil {
		return err
	}
	// Retain the blob descriptor through the deleting barrier. Garbage collection
	// becomes authorized only by the transaction that commits public deletion.
	if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET state='deleting',lifecycle_action='delete',reconcile_owner='',reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$2 WHERE id=$1`, sandboxID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state='deleting',mutation_kind='workspace_delete',mutation_id=$2,mutation_effect_id=$2,mutation_operation_id=$2,mutation_expected_generation=generation,mutation_target_generation=generation,mutation_state='deleting',updated_at=$3 WHERE id=$1`, workspace.ID, operationID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.archive_cleanup(archive_key,created_at) VALUES($1,$2) ON CONFLICT(archive_key) DO NOTHING`, key, now); err != nil {
		return err
	}
	if err := lifecycleprojection.RecordTeardownStage(ctx, tx, sandboxID, lifecycleprojection.StageTeardownWorkspaceDeleteDispatched, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state='deleted',archive_key='',archive_descriptor_json=NULL,mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',mutation_expected_generation=NULL,mutation_target_generation=NULL,mutation_state='',updated_at=$2 WHERE id=$1`, workspace.ID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET state='deleted',current_instance_id='',deleted_at=$2,next_reconcile_at=NULL,reconcile_owner='',reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$2 WHERE id=$1 AND state='deleting' AND desired_state='deleted'`, sandboxID, now); err != nil {
		return err
	}
	if err := lifecycleprojection.RecordTeardownStage(ctx, tx, sandboxID, lifecycleprojection.StageTeardownFinalized, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='succeeded',error_code='',error_message='',retryable=false,started_at=COALESCE(started_at,$2),completed_at=$2,updated_at=$2 WHERE sandbox_id=$1 AND kind='delete' AND state IN ('pending','running')`, sandboxID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='failed',error_code='state_conflict',error_message='Sandbox deletion superseded the lifecycle operation',retryable=false,started_at=COALESCE(started_at,$2),completed_at=$2,updated_at=$2 WHERE sandbox_id=$1 AND kind IN ('create','start','drain','stop') AND state IN ('pending','running')`, sandboxID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
