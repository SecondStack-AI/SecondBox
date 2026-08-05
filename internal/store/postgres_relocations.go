package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// RelocateSandbox admits one stopped Workspace transfer under its mutation slot.
func (store *PostgresControlPlaneStore) RelocateSandbox(
	ctx context.Context,
	input ports.WorkspaceRelocationInput,
) (contracts.Operation, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.Principal.TenantRef + "\x1f" + input.Principal.SubjectRef +
		"\x1frelocate\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation idempotency lock failed: %w", err)
	}
	var priorHash, priorOperationID string
	var expiresAt time.Time
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id,expires_at FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2 AND operation='sandbox.relocate'
		  AND target_id=$3 AND idempotency_key=$4`,
		input.Principal.TenantRef, input.Principal.SubjectRef,
		input.SandboxID, input.IdempotencyKey,
	).Scan(&priorHash, &priorOperationID, &expiresAt)
	if idempotencyErr == nil {
		expired, err := deleteExpiredIdempotencyRecord(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef,
			"sandbox.relocate", input.SandboxID, input.IdempotencyKey, expiresAt, input.Now,
		)
		if err != nil {
			return contracts.Operation{}, fmt.Errorf("SecondBox expired Workspace relocation idempotency cleanup failed: %w", err)
		}
		if expired {
			idempotencyErr = pgx.ErrNoRows
		} else {
			if priorHash != input.RequestHash {
				return contracts.Operation{}, ports.ErrIdempotencyConflict
			}
			operation, err := getOperationWithQuerier(
				ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, `id=$3`, priorOperationID,
			)
			if err != nil {
				return contracts.Operation{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation replay commit failed: %w", err)
			}
			return operation, nil
		}
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation idempotency lookup failed: %w", idempotencyErr)
	}
	locked, err := rowlock.SandboxWorkspaceForSubject(
		ctx,
		tx,
		input.Principal.TenantRef,
		input.Principal.SubjectRef,
		input.SandboxID,
	)
	if err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if locked.Revision != input.ExpectedRevision {
		return contracts.Operation{}, ports.ErrRevisionConflict
	}
	if locked.SandboxState != contracts.SandboxStateStopped ||
		locked.DesiredState != contracts.SandboxDesiredStateStopped ||
		locked.CurrentInstanceID != "" {
		return contracts.Operation{}, ports.ErrSandboxNotStopped
	}
	workspace := locked.Workspace
	if workspace.State != "ready" || workspace.Generation != locked.Generation {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	if workspace.Mutation.State != "" {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	var snapshotsPresent bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM secondbox.snapshots
			WHERE sandbox_id=$1 AND workspace_id=$2 AND state<>'deleted'
		)`,
		locked.SandboxID, locked.WorkspaceID,
	).Scan(&snapshotsPresent); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation Snapshot lookup failed: %w", err)
	}
	if snapshotsPresent {
		return contracts.Operation{}, ports.ErrRelocationSnapshotsPresent
	}
	var sourceConnectionID string
	var sourceCapabilitiesJSON, sourceVersionsJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT active_connection_id,capabilities_json,protocol_versions_json
		FROM secondbox.runners WHERE id=$1 FOR SHARE`,
		workspace.HomeRunnerID,
	).Scan(&sourceConnectionID, &sourceCapabilitiesJSON, &sourceVersionsJSON); err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrHomeRunnerUnavailable)
	}
	if sourceConnectionID == "" {
		return contracts.Operation{}, ports.ErrHomeRunnerUnavailable
	}
	var sourceCapabilities, sourceVersions []string
	if err := json.Unmarshal(sourceCapabilitiesJSON, &sourceCapabilities); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation source capabilities decoding failed: %w", err)
	}
	if err := json.Unmarshal(sourceVersionsJSON, &sourceVersions); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation source protocol decoding failed: %w", err)
	}
	if !contains(sourceCapabilities, "local-workspace") ||
		!contains(sourceCapabilities, "workspace-relocation") ||
		!contains(sourceVersions, "2") {
		return contracts.Operation{}, ports.ErrHomeRunnerUnavailable
	}
	var specJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT spec_json FROM secondbox.profile_revisions WHERE id=$1`,
		locked.ProfileRevisionID,
	).Scan(&specJSON); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation ProfileRevision lookup failed: %w", err)
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation ProfileRevision decoding failed: %w", err)
	}
	if input.RunnerPool != "" && input.RunnerPool != spec.Pool {
		return contracts.Operation{}, ports.ErrRelocationTargetUnavailable
	}
	targetRunnerID, err := selectWorkspaceRelocationTarget(
		ctx,
		tx,
		spec,
		workspace.HomeRunnerID,
		input.TargetRunnerID,
	)
	if err != nil {
		return contracts.Operation{}, err
	}
	input.Operation.SandboxID = input.SandboxID
	input.Operation.State = contracts.OperationStatePending
	input.Operation.RequestMetadata = map[string]string{"targetRunnerId": targetRunnerID}
	if err := setWorkspaceMutation(
		ctx,
		tx,
		workspace.ID,
		"relocate",
		input.RelocationID,
		input.ExportCommandID,
		input.Operation.ID,
		workspace.Generation,
		workspace.Generation,
		input.Now,
	); err != nil {
		return contracts.Operation{}, err
	}
	command := localWorkspaceCommand(
		input.ExportCommandID,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT,
		input.Operation,
		input.ExportCommandID,
		locked.SandboxID,
		workspace.ID,
		"",
		workspace.Generation,
		workspace.Generation,
		workspace.LogicalCapacityBytes,
		workspace.HomeRunnerID,
		input.FencingToken,
	)
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{LocalWorkspace: command},
	})
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation export command encoding failed: %w", err)
	}
	now := input.Now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.workspace_relocations (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,operation_id,
			source_runner_id,target_runner_id,generation,logical_capacity_bytes,state,
			export_command_id,cleanup_command_id,fencing_token,checksum,
			failure_code,failure_message,retry_count,created_at,updated_at,completed_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'queued',$11,'',$12,'','','',0,$13,$13,NULL
		)`,
		input.RelocationID, input.Principal.TenantRef, input.Principal.SubjectRef,
		locked.SandboxID, workspace.ID, input.Operation.ID,
		workspace.HomeRunnerID, targetRunnerID, workspace.Generation,
		workspace.LogicalCapacityBytes, input.ExportCommandID,
		input.FencingToken, now,
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5,NULL)`,
		input.ExportCommandID, workspace.HomeRunnerID, input.RelocationID, payload, now,
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation export command insert failed: %w", err)
	}
	if err := insertOperation(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.Operation,
	); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ($1,$2,'sandbox.relocate',$3,$4,$5,$6,$7,$8)`,
		input.Principal.TenantRef, input.Principal.SubjectRef,
		input.SandboxID, input.IdempotencyKey, input.RequestHash,
		input.Operation.ID, now, input.IdempotencyEnds.UTC(),
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation idempotency insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET revision=revision+1,updated_at=$2 WHERE id=$1`,
		locked.SandboxID, now,
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation Sandbox revision update failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Workspace relocation commit failed: %w", err)
	}
	return input.Operation, nil
}

func selectWorkspaceRelocationTarget(
	ctx context.Context,
	tx pgx.Tx,
	spec contracts.ProfileRevisionSpec,
	sourceRunnerID string,
	exactRunnerID string,
) (string, error) {
	return selectRunnerForPlacement(ctx, tx, spec, runnerPlacementOptions{
		exactRunnerID:            exactRunnerID,
		excludedRunnerID:         sourceRunnerID,
		requireWorkspaceTransfer: true,
		unavailable:              ports.ErrRelocationTargetUnavailable,
		errorPrefix:              "SecondBox Workspace relocation target",
	})
}
