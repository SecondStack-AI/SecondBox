package runnercontrol

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
)

func recordWorkspaceArchiveResult(ctx context.Context, tx pgx.Tx, locked rowlock.SandboxWorkspace, runnerID string, result *runnerv1.LocalWorkspaceResult, now time.Time) error {
	var id, source, target, state, command, key string
	var generation, capacity int64
	var descriptorJSON []byte
	if err := tx.QueryRow(ctx, `SELECT a.id,a.source_runner_id,a.target_runner_id,a.state,a.command_id,a.generation,a.logical_capacity_bytes,a.archive_key,a.descriptor_json FROM secondbox.workspace_archives a JOIN secondbox.runner_commands c ON c.assignment_id=a.id WHERE a.operation_id=$1 AND c.id=$2 AND c.runner_id=$3 FOR UPDATE OF a`, result.OperationId, result.EffectId, runnerID).Scan(&id, &source, &target, &state, &command, &generation, &capacity, &key, &descriptorJSON); err != nil {
		return err
	}
	if result.SandboxId != locked.SandboxID || result.WorkspaceId != locked.WorkspaceID {
		return errors.New("SecondBox workspace archive receipt authority mismatch")
	}
	if result.EffectId != command || state == "succeeded" || state == "failed" {
		var replay bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secondbox.runner_commands WHERE id=$1 AND runner_id=$2 AND assignment_id=$3 AND state='acknowledged')`, result.EffectId, runnerID, id).Scan(&replay); err != nil {
			return err
		}
		if replay {
			return nil
		}
		return errors.New("SecondBox workspace archive receipt effect mismatch")
	}
	expectedKind := runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT
	switch state {
	case "restore_queued_sent":
		expectedKind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT
	case "cleanup_queued_sent":
		expectedKind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE
	case "abort_queued_sent":
		expectedKind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE
	}
	if result.Kind != expectedKind {
		return errors.New("SecondBox workspace archive receipt command kind mismatch")
	}
	expectedRunner := source
	if state == "restore_queued_sent" {
		expectedRunner = target
	}
	if runnerID != expectedRunner {
		return errors.New("SecondBox workspace archive receipt came from another Runner")
	}
	var startOperation, continueStart bool
	if state != "cleanup_queued_sent" {
		var err error
		startOperation, continueStart, err = archiveStartContinuation(ctx, tx, &locked, result.OperationId, now)
		if err != nil {
			return err
		}
	}
	if err := acknowledgeRunnerCommand(ctx, tx, command, now); err != nil {
		return err
	}
	success := result.Terminal == runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
	if success && (result.Generation != uint64(generation) || result.LogicalCapacityBytes != uint64(capacity) || result.ReceiptRecordedAtUnixMs == 0) {
		return errors.New("SecondBox workspace archive receipt has invalid generation or size")
	}
	switch state {
	case "queued_sent":
		if locked.Workspace.HomeRunnerID != source || locked.Workspace.Mutation.Kind != "archive" || locked.Workspace.Mutation.ID != id {
			return errors.New("SecondBox workspace archive upload lost its mutation")
		}
		if !success {
			if _, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET state='abort_queued',command_id=$2,error_code=$3,error_message=$4,updated_at=$5 WHERE id=$1`, id, id+"-abort", localWorkspaceOperationErrorCode(result.Terminal), result.SafeDetail, now); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO secondbox.archive_cleanup(archive_key,created_at) VALUES($1,$2) ON CONFLICT DO NOTHING`, key, now)
			return err
		}
		if !validWorkspaceArchiveDescriptor(result.ArchiveDescriptor) {
			return errors.New("SecondBox workspace archive upload receipt lacks verified archive evidence")
		}
		descriptor, err := json.Marshal(result.ArchiveDescriptor)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state='archived',home_runner_id='',archive_key=$2,archive_descriptor_json=$3,archived_at=$4,mutation_kind=CASE WHEN $5 THEN 'start' ELSE '' END,mutation_id=CASE WHEN $5 THEN $6 ELSE '' END,mutation_effect_id=CASE WHEN $5 THEN $6 ELSE '' END,mutation_operation_id=CASE WHEN $5 THEN $6 ELSE '' END,mutation_state=CASE WHEN $5 THEN 'queued' ELSE '' END,mutation_expected_generation=CASE WHEN $5 THEN generation ELSE NULL END,mutation_target_generation=CASE WHEN $5 THEN generation ELSE NULL END,updated_at=$4 WHERE id=$1`, locked.WorkspaceID, key, descriptor, now, continueStart, result.OperationId); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET state='cleanup_queued',command_id=$2,descriptor_json=$3,updated_at=$4 WHERE id=$1`, id, id+"-cleanup", descriptor, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET revision=revision+1,reconcile_owner='',reconcile_claim_expires_at=NULL,next_reconcile_at=CASE WHEN desired_state='deleted' THEN $2::timestamptz ELSE NULL END,updated_at=$2 WHERE id=$1`, locked.SandboxID, now); err != nil {
			return err
		}
		if startOperation {
			if continueStart {
				return nil
			}
			if err := completeArchiveRestoreStop(ctx, tx, locked.SandboxID, now); err != nil {
				return err
			}
			return finishPendingOperation(ctx, tx, result.OperationId, "cancelled", "start_cancelled", "workspace start was cancelled before compute admission", now)
		}
		return finishPendingOperation(ctx, tx, result.OperationId, "succeeded", "", "", now)
	case "restore_queued_sent":
		if locked.Workspace.Mutation.Kind != "archive_restore" || locked.Workspace.Mutation.ID != id || locked.Workspace.HomeRunnerID != "" {
			return errors.New("SecondBox workspace archive restore lost its mutation")
		}
		if !success {
			if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state='archived',mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',mutation_state='',mutation_expected_generation=NULL,mutation_target_generation=NULL,updated_at=$2 WHERE id=$1`, locked.WorkspaceID, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET state='stopped',desired_state=CASE WHEN desired_state='deleted' THEN 'deleted' ELSE 'stopped' END,revision=revision+1,reconcile_owner='',reconcile_claim_expires_at=NULL,next_reconcile_at=CASE WHEN desired_state='deleted' THEN $2::timestamptz ELSE NULL END,updated_at=$2 WHERE id=$1`, locked.SandboxID, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET state='failed',updated_at=$2 WHERE id=$1`, id, now); err != nil {
				return err
			}
			if err := completeArchiveRestoreStop(ctx, tx, locked.SandboxID, now); err != nil {
				return err
			}
			return finishPendingOperation(ctx, tx, result.OperationId, "failed", localWorkspaceOperationErrorCode(result.Terminal), result.SafeDetail, now)
		}
		var expected runnerv1.WorkspaceArchiveDescriptor
		if err := json.Unmarshal(descriptorJSON, &expected); err != nil {
			return err
		}
		if !validWorkspaceArchiveDescriptor(result.ArchiveDescriptor) || result.ArchiveDescriptor.ArchiveBytes != expected.ArchiveBytes || result.ArchiveDescriptor.ArchiveChecksum != expected.ArchiveChecksum || result.ArchiveDescriptor.ImageChecksum != expected.ImageChecksum {
			return errors.New("SecondBox workspace archive import receipt checksum mismatch")
		}
		keepStart := continueStart
		if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state='ready',home_runner_id=$2,archive_key='',archive_descriptor_json=NULL,archived_at=NULL,mutation_kind=CASE WHEN $3 THEN 'start' ELSE '' END,mutation_id=CASE WHEN $3 THEN $4 ELSE '' END,mutation_effect_id=CASE WHEN $3 THEN $4 ELSE '' END,mutation_operation_id=CASE WHEN $3 THEN $4 ELSE '' END,mutation_state=CASE WHEN $3 THEN 'queued' ELSE '' END,mutation_expected_generation=CASE WHEN $3 THEN generation ELSE NULL END,mutation_target_generation=CASE WHEN $3 THEN generation ELSE NULL END,updated_at=$5 WHERE id=$1`, locked.WorkspaceID, target, keepStart, result.OperationId, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET state='succeeded',updated_at=$2 WHERE id=$1`, id, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO secondbox.archive_cleanup(archive_key,created_at) VALUES($1,$2) ON CONFLICT DO NOTHING`, key, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET state='stopped',revision=revision+1,reconcile_owner='',reconcile_claim_expires_at=NULL,next_reconcile_at=CASE WHEN desired_state IN ('running','deleted') THEN $2::timestamptz ELSE NULL END,updated_at=$2 WHERE id=$1`, locked.SandboxID, now); err != nil {
			return err
		}
		if !keepStart {
			if err := completeArchiveRestoreStop(ctx, tx, locked.SandboxID, now); err != nil {
				return err
			}
			return finishPendingOperation(ctx, tx, result.OperationId, "cancelled", "start_cancelled", "workspace start was cancelled before compute admission", now)
		}
		return nil
	case "cleanup_queued_sent", "abort_queued_sent":
		if !success {
			phase := "cleanup_queued"
			if state == "abort_queued_sent" {
				phase = "abort_queued"
			}
			_, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET state=$2,command_id=$3,updated_at=$4 WHERE id=$1`, id, phase, fmt.Sprintf("%s-retry-%d", id, now.UnixNano()), now)
			return err
		}
		terminal := "succeeded"
		if state == "abort_queued_sent" {
			terminal = "failed"
			if locked.Workspace.Mutation.Kind != "archive" || locked.Workspace.Mutation.ID != id || locked.Workspace.HomeRunnerID != source {
				return errors.New("SecondBox archive abort lost source authority")
			}
			if err := releaseWorkspaceRelocationMutation(ctx, tx, locked.WorkspaceID, now); err != nil {
				return err
			}
			var code, message string
			if err := tx.QueryRow(ctx, `SELECT error_code,error_message FROM secondbox.workspace_archives WHERE id=$1`, id).Scan(&code, &message); err != nil {
				return err
			}
			if err := finishPendingOperation(ctx, tx, result.OperationId, "failed", code, message, now); err != nil {
				return err
			}
			if startOperation {
				if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET desired_state=CASE WHEN desired_state='deleted' THEN 'deleted' ELSE 'stopped' END,state='stopped',next_reconcile_at=CASE WHEN desired_state='deleted' THEN $2::timestamptz ELSE NULL END,reconcile_owner='',reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$2 WHERE id=$1`, locked.SandboxID, now); err != nil {
					return err
				}
				if err := completeArchiveRestoreStop(ctx, tx, locked.SandboxID, now); err != nil {
					return err
				}
			}
		}
		_, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET state=$2,updated_at=$3 WHERE id=$1`, id, terminal, now)
		return err
	default:
		return errors.New("SecondBox workspace archive receipt is reordered")
	}
}

func completeArchiveRestoreStop(ctx context.Context, tx pgx.Tx, sandboxID string, now time.Time) error {
	// A stop admitted while import held the mutation slot has no compute left
	// to stop. Its Operation must finish before the Sandbox parks at rest.
	_, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='succeeded',error_code='',error_message='',retryable=false,started_at=COALESCE(started_at,$2),completed_at=$2,updated_at=$2 WHERE sandbox_id=$1 AND kind IN ('stop','drain') AND state IN ('pending','running') AND EXISTS(SELECT 1 FROM secondbox.sandboxes WHERE id=$1 AND state='stopped' AND desired_state='stopped' AND current_instance_id='')`, sandboxID, now)
	return err
}

func validWorkspaceArchiveDescriptor(descriptor *runnerv1.WorkspaceArchiveDescriptor) bool {
	if descriptor == nil || descriptor.ArchiveBytes == 0 || descriptor.ArchiveBytes > 1<<50 {
		return false
	}
	for _, digest := range []string{descriptor.ArchiveChecksum, descriptor.ImageChecksum} {
		if len(digest) != 71 || digest[:7] != "sha256:" {
			return false
		}
		if _, err := hex.DecodeString(digest[7:]); err != nil {
			return false
		}
	}
	return true
}
