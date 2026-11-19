package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
)

const workspaceRestoreTargetLossDelay = 30 * time.Second

func (store *PostgresControlPlaneStore) recoverLostWorkspaceRestores(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-workspaceRestoreTargetLossDelay)
	rows, err := store.pool.Query(ctx, `SELECT a.id,a.sandbox_id
 FROM secondbox.workspace_archives a LEFT JOIN secondbox.runners r ON r.id=a.target_runner_id
 WHERE a.state IN ('restore_queued','restore_queued_sent') AND a.target_runner_id<>''
 AND (r.id IS NULL OR r.state='offline' OR r.active_connection_id='' OR r.last_seen_at<=$1)
 AND GREATEST(a.created_at,COALESCE(r.last_seen_at,a.created_at))<=$1 ORDER BY a.updated_at,a.id LIMIT 32`, cutoff)
	if err != nil {
		return err
	}
	type candidate struct{ archive, sandbox string }
	var candidates []candidate
	for rows.Next() {
		var value candidate
		if err := rows.Scan(&value.archive, &value.sandbox); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		if err := store.recoverLostWorkspaceRestore(ctx, candidate.archive, candidate.sandbox, now, cutoff); err != nil {
			failures = append(failures, fmt.Errorf("SecondBox lost restore %s: %w", candidate.archive, err))
		}
	}
	return errors.Join(failures...)
}

func (store *PostgresControlPlaneStore) recoverLostWorkspaceRestore(ctx context.Context, archiveID, sandboxID string, now, cutoff time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return err
	}
	w := locked.Workspace
	if w.State != "restoring" || w.Mutation.Kind != "archive_restore" || w.Mutation.ID != archiveID || w.HomeRunnerID != "" || locked.CurrentInstanceID != "" {
		return nil
	}
	var operationID, commandID, target, operationState string
	var deadline *time.Time
	err = tx.QueryRow(ctx, `SELECT a.operation_id,a.command_id,a.target_runner_id,o.state,o.capacity_wait_deadline
 FROM secondbox.workspace_archives a JOIN secondbox.operations o ON o.id=a.operation_id
 LEFT JOIN secondbox.runners r ON r.id=a.target_runner_id
 WHERE a.id=$1 AND a.workspace_id=$2 AND a.state IN ('restore_queued','restore_queued_sent')
 AND (r.id IS NULL OR r.state='offline' OR r.active_connection_id='' OR r.last_seen_at<=$3)
 AND GREATEST(a.created_at,COALESCE(r.last_seen_at,a.created_at))<=$3 FOR UPDATE OF a`, archiveID, w.ID, cutoff).Scan(&operationID, &commandID, &target, &operationState, &deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if operationID != w.Mutation.OperationID || target == "" {
		return errors.New("SecondBox lost restore has inconsistent mutation authority")
	}
	expired := deadline != nil && !deadline.After(now)
	keepStart := !expired && locked.DesiredState == "running" && (operationState == "pending" || operationState == "running")
	if expired {
		if _, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='failed',error_code='capacity_wait_expired',error_message='workspace restore capacity wait expired before compute admission',retryable=true,completed_at=$2,updated_at=$2 WHERE id=$1 AND state IN ('pending','running')`, operationID, now); err != nil {
			return err
		}
	} else if !keepStart {
		if _, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='cancelled',error_code='start_cancelled',error_message='workspace start was cancelled before compute admission',retryable=false,completed_at=$2,updated_at=$2 WHERE id=$1 AND state IN ('pending','running')`, operationID, now); err != nil {
			return err
		}
	}
	// The object descriptor remains authoritative. The abandoned Runner becomes
	// a quarantined former home until its exact phase cleanup has a real receipt.
	if _, err := tx.Exec(ctx, `UPDATE secondbox.runner_commands SET state='expired',target_connection_id='',updated_at=$2 WHERE id=$1 AND state IN ('pending','delivering','delivered')`, commandID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET source_runner_id=$2,target_runner_id='',state='cleanup_queued',command_id=$3,error_code='restore_target_lost',error_message='restore target was lost before home commit',updated_at=$4 WHERE id=$1`, archiveID, target, archiveID+"-orphan-cleanup", now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state='archived',mutation_kind=CASE WHEN $3 THEN 'start' ELSE '' END,mutation_id=CASE WHEN $3 THEN $2 ELSE '' END,mutation_effect_id=CASE WHEN $3 THEN $2 ELSE '' END,mutation_operation_id=CASE WHEN $3 THEN $2 ELSE '' END,mutation_state=CASE WHEN $3 THEN 'queued' ELSE '' END,mutation_expected_generation=CASE WHEN $3 THEN generation ELSE NULL END,mutation_target_generation=CASE WHEN $3 THEN generation ELSE NULL END,updated_at=$4 WHERE id=$1`, w.ID, operationID, keepStart, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET state='stopped',desired_state=CASE WHEN desired_state='deleted' THEN 'deleted' WHEN $3 THEN 'running' ELSE 'stopped' END,reconcile_owner='',reconcile_claim_expires_at=NULL,next_reconcile_at=CASE WHEN desired_state='deleted' THEN $2::timestamptz ELSE NULL END,revision=revision+1,updated_at=$2 WHERE id=$1`, sandboxID, now, keepStart); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='succeeded',error_code='',error_message='',retryable=false,started_at=COALESCE(started_at,$2),completed_at=$2,updated_at=$2 WHERE sandbox_id=$1 AND kind IN ('stop','drain') AND state IN ('pending','running') AND EXISTS(SELECT 1 FROM secondbox.sandboxes WHERE id=$1 AND desired_state='stopped')`, sandboxID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
