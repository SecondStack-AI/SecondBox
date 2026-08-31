package runnercontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

type workspaceRelocationAuthority struct {
	id, operationID, sandboxID, workspaceID      string
	sourceRunnerID, targetRunnerID, state        string
	exportCommandID, cleanupCommandID, requestID string
	failureCode, failureMessage                  string
	generation, capacity, retryCount             int64
	fencingToken                                 []byte
	egressContext                                *string
}

func recordLocalWorkspaceRelocationResult(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	runnerID string,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) error {
	authority, err := lockWorkspaceRelocation(ctx, tx, result.OperationId)
	if err != nil {
		return err
	}
	if authority.sandboxID != result.SandboxId ||
		authority.workspaceID != result.WorkspaceId ||
		authority.sourceRunnerID != runnerID ||
		locked.Workspace.Mutation.Kind != "relocate" ||
		locked.Workspace.Mutation.ID != authority.id ||
		locked.Workspace.Mutation.OperationID != authority.operationID ||
		!equalRelocationContext(locked.EgressContext, authority.egressContext) {
		return errors.New("SecondBox Workspace relocation result conflicts with durable authority")
	}
	succeeded := result.Terminal ==
		runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
	if succeeded && (authority.generation != int64(result.Generation) ||
		authority.capacity != int64(result.LogicalCapacityBytes)) {
		return errors.New("SecondBox Workspace relocation receipt has inconsistent generation or capacity")
	}
	if succeeded && result.ReceiptRecordedAtUnixMs == 0 {
		return errors.New("SecondBox Workspace relocation success lacks a durable receipt")
	}
	switch result.Kind {
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT:
		if result.EffectId != authority.exportCommandID {
			return errors.New("SecondBox Workspace relocation export result has the wrong command authority")
		}
		if authority.state == "source_sealed" && succeeded {
			return nil
		}
		if authority.state != "queued" || locked.Workspace.HomeRunnerID != authority.sourceRunnerID {
			return errors.New("SecondBox Workspace relocation export result is reordered")
		}
		if err := acknowledgeRunnerCommand(ctx, tx, authority.exportCommandID, now); err != nil {
			return err
		}
		if succeeded {
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.workspace_relocations
				SET state='source_sealed',updated_at=$2 WHERE id=$1`, authority.id, now.UTC(),
			); err != nil {
				return fmt.Errorf("SecondBox Workspace relocation source seal: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.workspaces SET mutation_state='source_sealed',updated_at=$2
				WHERE id=$1 AND mutation_id=$3`, authority.workspaceID, now.UTC(), authority.id,
			); err != nil {
				return fmt.Errorf("SecondBox Workspace relocation source seal mutation: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.operations
				SET state='running',started_at=COALESCE(started_at,$2),updated_at=$2
				WHERE id=$1 AND state='pending'`, authority.operationID, now.UTC(),
			); err != nil {
				return fmt.Errorf("SecondBox Workspace relocation Operation start: %w", err)
			}
			return nil
		}
		return beginWorkspaceRelocationAbort(
			ctx,
			tx,
			authority,
			localWorkspaceOperationErrorCode(result.Terminal),
			result.SafeDetail,
			now,
		)
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE:
		if result.EffectId != authority.cleanupCommandID || authority.state != "deleting_source" ||
			locked.Workspace.HomeRunnerID != authority.targetRunnerID {
			return errors.New("SecondBox Workspace relocation source deletion result is reordered")
		}
		if err := acknowledgeRunnerCommand(ctx, tx, authority.cleanupCommandID, now); err != nil {
			return err
		}
		if !succeeded {
			return retryWorkspaceRelocationCleanup(ctx, tx, authority, result.Kind, now)
		}
		if err := releaseWorkspaceRelocationMutation(ctx, tx, authority.workspaceID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_relocations
			SET state='succeeded',completed_at=$2,updated_at=$2 WHERE id=$1`, authority.id, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox Workspace relocation completion: %w", err)
		}
		return finishRelocationOperation(ctx, tx, authority.operationID, "succeeded", "", "", now)
	case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE:
		if result.EffectId != authority.cleanupCommandID || authority.state != "aborting" ||
			locked.Workspace.HomeRunnerID != authority.sourceRunnerID {
			return errors.New("SecondBox Workspace relocation abort result is reordered")
		}
		if err := acknowledgeRunnerCommand(ctx, tx, authority.cleanupCommandID, now); err != nil {
			return err
		}
		if !succeeded {
			return retryWorkspaceRelocationCleanup(ctx, tx, authority, result.Kind, now)
		}
		if err := releaseWorkspaceRelocationMutation(ctx, tx, authority.workspaceID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_relocations
			SET state='failed',completed_at=$2,updated_at=$2 WHERE id=$1`, authority.id, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox Workspace relocation abort completion: %w", err)
		}
		return finishRelocationOperation(
			ctx, tx, authority.operationID, "failed", authority.failureCode, authority.failureMessage, now,
		)
	default:
		return errors.New("SecondBox Workspace relocation result kind is unsupported")
	}
}

func retryWorkspaceRelocationCleanup(
	ctx context.Context,
	tx pgx.Tx,
	authority workspaceRelocationAuthority,
	kind runnerv1.LocalWorkspaceCommandKind,
	now time.Time,
) error {
	nextRetry := authority.retryCount + 1
	commandID := fmt.Sprintf("%s-retry-%d", authority.cleanupCommandID, nextRetry)
	payload, err := workspaceRelocationCommandPayload(authority, commandID, authority.sourceRunnerID, kind)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_relocations
		SET cleanup_command_id=$2,retry_count=$3,updated_at=$4 WHERE id=$1`,
		authority.id, commandID, nextRetry, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation cleanup retry: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces SET mutation_effect_id=$2,updated_at=$3 WHERE id=$1`,
		authority.workspaceID, commandID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation cleanup retry mutation: %w", err)
	}
	return insertWorkspaceRelocationCommand(
		ctx, tx, commandID, authority.sourceRunnerID, authority.id, payload, now,
	)
}

func releaseWorkspaceRelocationMutation(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',
		    mutation_expected_generation=NULL,mutation_target_generation=NULL,
		    mutation_state='',updated_at=$2 WHERE id=$1`, workspaceID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation mutation release: %w", err)
	}
	return nil
}

func finishRelocationOperation(
	ctx context.Context,
	tx pgx.Tx,
	operationID string,
	state string,
	errorCode string,
	errorMessage string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operations
		SET state=$2,error_code=$3,error_message=$4,retryable=false,
		    started_at=COALESCE(started_at,$5),completed_at=$5,updated_at=$5
		WHERE id=$1 AND state IN ('pending','running')`,
		operationID, state, errorCode, errorMessage, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation Operation completion: %w", err)
	}
	return nil
}

func queueWorkspaceRelocationRestarts(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	connectionID string,
	now time.Time,
) error {
	rows, err := tx.Query(ctx, `
		SELECT relocation.id,relocation.operation_id,relocation.sandbox_id,
		       relocation.workspace_id,relocation.source_runner_id,
		       relocation.target_runner_id,relocation.state,
		       relocation.export_command_id,relocation.cleanup_command_id,
		       operation.request_id,relocation.failure_code,relocation.failure_message,
		       relocation.generation,relocation.logical_capacity_bytes,
		       relocation.retry_count,relocation.fencing_token,relocation.egress_context
		FROM secondbox.workspace_relocations AS relocation
		JOIN secondbox.operations AS operation ON operation.id=relocation.operation_id
		WHERE (relocation.source_runner_id=$1 OR relocation.target_runner_id=$1)
		  AND relocation.state='source_sealed'
		ORDER BY relocation.id`, runnerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox Workspace relocation restart lookup: %w", err)
	}
	defer rows.Close()
	var authorities []workspaceRelocationAuthority
	for rows.Next() {
		var authority workspaceRelocationAuthority
		if err := rows.Scan(
			&authority.id, &authority.operationID, &authority.sandboxID,
			&authority.workspaceID, &authority.sourceRunnerID, &authority.targetRunnerID,
			&authority.state, &authority.exportCommandID, &authority.cleanupCommandID,
			&authority.requestID, &authority.failureCode, &authority.failureMessage,
			&authority.generation, &authority.capacity, &authority.retryCount,
			&authority.fencingToken, &authority.egressContext,
		); err != nil {
			return fmt.Errorf("SecondBox Workspace relocation restart scan: %w", err)
		}
		authorities = append(authorities, authority)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation restart iteration: %w", err)
	}
	rows.Close()
	for _, authority := range authorities {
		commandID := authority.id + "-export-restart-" + connectionID
		payload, err := workspaceRelocationCommandPayload(
			authority,
			commandID,
			runnerID,
			runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT,
		)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_relocations
			SET export_command_id=$2,retry_count=retry_count+1,updated_at=$3 WHERE id=$1`,
			authority.id, commandID, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox Workspace relocation restart transition: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces SET mutation_effect_id=$2,updated_at=$3
			WHERE id=$1 AND mutation_kind='relocate' AND mutation_id=$4`,
			authority.workspaceID, commandID, now.UTC(), authority.id,
		); err != nil {
			return fmt.Errorf("SecondBox Workspace relocation restart mutation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.runner_commands
			SET state='expired',target_connection_id='',updated_at=$3
			WHERE runner_id=$1 AND assignment_id=$2 AND kind='local-workspace'
			  AND state IN ('pending','delivering','delivered')`,
			authority.sourceRunnerID, authority.id, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox Workspace relocation prior restart expiry: %w", err)
		}
		if err := insertWorkspaceRelocationCommand(
			ctx, tx, commandID, authority.sourceRunnerID, authority.id, payload, now,
		); err != nil {
			return err
		}
	}
	return nil
}

func (store *PostgresStateStore) RouteWorkspaceTransfer(
	ctx context.Context,
	runnerID string,
	frame *runnerv1.WorkspaceTransferFrame,
	now time.Time,
) (string, error) {
	if frame == nil {
		return "", errors.New("SecondBox Workspace transfer frame is absent")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("SecondBox Workspace transfer transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, frame.SandboxId)
	if err != nil {
		return "", fmt.Errorf("SecondBox Workspace transfer Sandbox/Workspace lock: %w", err)
	}
	authority, err := lockWorkspaceRelocation(ctx, tx, frame.OperationId)
	if err != nil {
		return "", err
	}
	if authority.sandboxID != frame.SandboxId ||
		authority.workspaceID != frame.WorkspaceId ||
		authority.generation != int64(frame.Generation) ||
		locked.WorkspaceID != authority.workspaceID ||
		locked.Generation != authority.generation ||
		locked.Workspace.Generation != authority.generation ||
		locked.SandboxState != "stopped" ||
		locked.DesiredState != "stopped" ||
		locked.CurrentInstanceID != "" ||
		locked.Workspace.Mutation.Kind != "relocate" ||
		locked.Workspace.Mutation.ID != authority.id ||
		locked.Workspace.Mutation.OperationID != authority.operationID ||
		!equalRelocationContext(locked.EgressContext, authority.egressContext) {
		return "", errors.New("SecondBox Workspace transfer conflicts with durable authority")
	}
	peerRunnerID := ""
	switch runnerID {
	case authority.sourceRunnerID:
		if authority.state != "source_sealed" || locked.Workspace.HomeRunnerID != authority.sourceRunnerID {
			return "", errors.New("SecondBox Workspace transfer source is not sealed authority")
		}
		if frame.GetCredit() != nil {
			return "", errors.New("SecondBox Workspace transfer source sent target credit")
		}
		if open := frame.GetOpen(); open != nil &&
			(open.LogicalCapacityBytes != uint64(authority.capacity) ||
				!bytes.Equal(open.FencingToken, authority.fencingToken)) {
			return "", errors.New("SecondBox Workspace transfer open conflicts with durable authority")
		}
		peerRunnerID = authority.targetRunnerID
		if frame.GetResult() != nil || frame.GetCancel() != nil {
			if err := beginWorkspaceRelocationAbort(
				ctx, tx, authority, "workspace_relocation_failed", workspaceTransferFailureDetail(frame), now,
			); err != nil {
				return "", err
			}
		}
	case authority.targetRunnerID:
		if authority.state != "source_sealed" || locked.Workspace.HomeRunnerID != authority.sourceRunnerID {
			return "", errors.New("SecondBox Workspace transfer target lacks source authority")
		}
		if frame.GetOpen() != nil || frame.GetChunk() != nil || frame.GetCommit() != nil {
			return "", errors.New("SecondBox Workspace transfer target sent source payload")
		}
		peerRunnerID = authority.sourceRunnerID
		if result := frame.GetResult(); result != nil {
			if result.Terminal == runnerv1.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_SUCCEEDED {
				if result.SizeBytes != uint64(authority.capacity) ||
					!strings.HasPrefix(result.Sha256, "sha256:") {
					return "", errors.New("SecondBox Workspace transfer target receipt is inconsistent")
				}
				if err := commitWorkspaceRelocation(ctx, tx, locked, authority, result.Sha256, now); err != nil {
					return "", err
				}
			} else if err := beginWorkspaceRelocationAbort(
				ctx, tx, authority, "workspace_relocation_failed", workspaceTransferFailureDetail(frame), now,
			); err != nil {
				return "", err
			}
		} else if frame.GetCancel() != nil {
			if err := beginWorkspaceRelocationAbort(
				ctx, tx, authority, "workspace_relocation_failed", workspaceTransferFailureDetail(frame), now,
			); err != nil {
				return "", err
			}
		}
	default:
		return "", errors.New("SecondBox Workspace transfer came from an unrelated Runner")
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("SecondBox Workspace transfer commit: %w", err)
	}
	return peerRunnerID, nil
}

func (store *PostgresStateStore) FailWorkspaceTransfer(
	ctx context.Context,
	operationID string,
	failureCode string,
	failureMessage string,
	now time.Time,
) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Workspace transfer failure transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var sandboxID string
	if err := tx.QueryRow(ctx, `
		SELECT sandbox_id FROM secondbox.workspace_relocations WHERE operation_id=$1`,
		operationID,
	).Scan(&sandboxID); err != nil {
		return fmt.Errorf("SecondBox Workspace transfer failure lookup: %w", err)
	}
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return fmt.Errorf("SecondBox Workspace transfer failure Sandbox/Workspace lock: %w", err)
	}
	authority, err := lockWorkspaceRelocation(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if authority.state == "deleting_source" || authority.state == "succeeded" ||
		authority.state == "failed" {
		return tx.Commit(ctx)
	}
	if locked.Workspace.HomeRunnerID != authority.sourceRunnerID {
		return errors.New("SecondBox Workspace transfer failure cannot restore source authority")
	}
	if err := beginWorkspaceRelocationAbort(
		ctx, tx, authority, failureCode, failureMessage, now,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox Workspace transfer failure commit: %w", err)
	}
	return nil
}

func lockWorkspaceRelocation(
	ctx context.Context,
	tx pgx.Tx,
	operationID string,
) (workspaceRelocationAuthority, error) {
	var authority workspaceRelocationAuthority
	err := tx.QueryRow(ctx, `
		SELECT relocation.id,relocation.operation_id,relocation.sandbox_id,
		       relocation.workspace_id,relocation.source_runner_id,
		       relocation.target_runner_id,relocation.state,
		       relocation.export_command_id,relocation.cleanup_command_id,
		       operation.request_id,relocation.failure_code,relocation.failure_message,
		       relocation.generation,relocation.logical_capacity_bytes,
		       relocation.retry_count,relocation.fencing_token,relocation.egress_context
		FROM secondbox.workspace_relocations AS relocation
		JOIN secondbox.operations AS operation ON operation.id=relocation.operation_id
		WHERE relocation.operation_id=$1
		FOR UPDATE OF relocation`, operationID,
	).Scan(
		&authority.id, &authority.operationID, &authority.sandboxID,
		&authority.workspaceID, &authority.sourceRunnerID, &authority.targetRunnerID,
		&authority.state, &authority.exportCommandID, &authority.cleanupCommandID,
		&authority.requestID, &authority.failureCode, &authority.failureMessage,
		&authority.generation, &authority.capacity, &authority.retryCount,
		&authority.fencingToken, &authority.egressContext,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspaceRelocationAuthority{}, errors.New("SecondBox Workspace transfer has no durable relocation authority")
	}
	if err != nil {
		return workspaceRelocationAuthority{}, fmt.Errorf("SecondBox Workspace transfer authority lookup: %w", err)
	}
	if authority.egressContext != nil {
		if err := contracts.ValidateEgressContextName(*authority.egressContext); err != nil {
			return workspaceRelocationAuthority{}, fmt.Errorf("SecondBox persisted Workspace relocation egress context is invalid: %w", err)
		}
	}
	return authority, nil
}

func equalRelocationContext(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func commitWorkspaceRelocation(
	ctx context.Context,
	tx pgx.Tx,
	locked rowlock.SandboxWorkspace,
	authority workspaceRelocationAuthority,
	checksum string,
	now time.Time,
) error {
	commandID := authority.id + "-delete-source"
	payload, err := workspaceRelocationCommandPayload(
		authority,
		commandID,
		authority.sourceRunnerID,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE,
	)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET home_runner_id=$2,local_receipt_json=$3,mutation_effect_id=$4,
		    mutation_state='deleting_source',updated_at=$5
		WHERE id=$1 AND home_runner_id=$6 AND mutation_kind='relocate' AND mutation_id=$7`,
		authority.workspaceID,
		authority.targetRunnerID,
		[]byte(fmt.Sprintf(`{"relocationId":%q,"checksum":%q}`, authority.id, checksum)),
		commandID,
		now.UTC(),
		authority.sourceRunnerID,
		authority.id,
	)
	if err != nil {
		return fmt.Errorf("SecondBox Workspace relocation home commit: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("SecondBox Workspace relocation home authority changed before commit")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET revision=revision+1,updated_at=$2 WHERE id=$1`,
		locked.SandboxID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation Sandbox revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_relocations
		SET state='deleting_source',cleanup_command_id=$2,checksum=$3,updated_at=$4
		WHERE id=$1`, authority.id, commandID, checksum, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation target completion: %w", err)
	}
	return insertWorkspaceRelocationCommand(
		ctx, tx, commandID, authority.sourceRunnerID, authority.id, payload, now,
	)
}

func beginWorkspaceRelocationAbort(
	ctx context.Context,
	tx pgx.Tx,
	authority workspaceRelocationAuthority,
	failureCode string,
	failureMessage string,
	now time.Time,
) error {
	if authority.state == "aborting" || authority.state == "failed" {
		return nil
	}
	if authority.state == "deleting_source" || authority.state == "succeeded" {
		return errors.New("SecondBox Workspace relocation cannot abort after the home commit")
	}
	commandID := authority.id + "-abort-source"
	mutationState := "aborting"
	if authority.state == "source_sealed" {
		mutationState = "aborting_source_sealed"
	}
	payload, err := workspaceRelocationCommandPayload(
		authority,
		commandID,
		authority.sourceRunnerID,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE,
	)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_relocations
		SET state='aborting',cleanup_command_id=$2,failure_code=$3,
		    failure_message=$4,updated_at=$5 WHERE id=$1`,
		authority.id, commandID, failureCode, failureMessage, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation abort transition: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_effect_id=$2,mutation_state=$3,updated_at=$4
		WHERE id=$1 AND mutation_kind='relocate' AND mutation_id=$5`,
		authority.workspaceID, commandID, mutationState, now.UTC(), authority.id,
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation abort mutation: %w", err)
	}
	return insertWorkspaceRelocationCommand(
		ctx, tx, commandID, authority.sourceRunnerID, authority.id, payload, now,
	)
}

func workspaceRelocationCommandPayload(
	authority workspaceRelocationAuthority,
	commandID string,
	runnerID string,
	kind runnerv1.LocalWorkspaceCommandKind,
) ([]byte, error) {
	command := &runnerv1.LocalWorkspaceCommand{
		MessageId: commandID, CommandVersion: 1, Kind: kind,
		OperationId: authority.operationID, EffectId: commandID,
		SandboxId: authority.sandboxID, WorkspaceId: authority.workspaceID,
		ExpectedGeneration:   uint64(authority.generation),
		NextGeneration:       uint64(authority.generation),
		LogicalCapacityBytes: uint64(authority.capacity),
		FencingToken:         append([]byte(nil), authority.fencingToken...),
		Correlation: &runnerv1.Correlation{
			RequestId: authority.requestID, OperationId: authority.operationID,
			SandboxId: authority.sandboxID, SandboxGeneration: uint64(authority.generation),
			RunnerId: runnerID,
		},
	}
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{LocalWorkspace: command},
	})
	if err != nil {
		return nil, fmt.Errorf("SecondBox Workspace relocation command encoding: %w", err)
	}
	return payload, nil
}

func insertWorkspaceRelocationCommand(
	ctx context.Context,
	tx pgx.Tx,
	commandID string,
	runnerID string,
	relocationID string,
	payload []byte,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5,NULL)
		ON CONFLICT (id) DO NOTHING`,
		commandID, runnerID, relocationID, payload, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Workspace relocation command insert: %w", err)
	}
	return nil
}

func workspaceTransferFailureDetail(frame *runnerv1.WorkspaceTransferFrame) string {
	if result := frame.GetResult(); result != nil && strings.TrimSpace(result.SafeDetail) != "" {
		return result.SafeDetail
	}
	if cancel := frame.GetCancel(); cancel != nil && strings.TrimSpace(cancel.SafeDetail) != "" {
		return cancel.SafeDetail
	}
	return "Workspace relocation transfer failed"
}
