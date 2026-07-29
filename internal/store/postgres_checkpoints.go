package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func (store *PostgresControlPlaneStore) StageCheckpoint(
	ctx context.Context,
	input ports.CheckpointPublicationInput,
) (contracts.WorkspaceCheckpoint, error) {
	checkpoint := input.Checkpoint
	compatibilityJSON, err := json.Marshal(checkpoint.Compatibility)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint compatibility encoding failed: %w", err)
	}
	if checkpoint.SourceGeneration != input.ExpectedWorkspaceGeneration {
		return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var tenantRef, subjectRef string
	if err := tx.QueryRow(ctx, `
		SELECT workspace.tenant_ref,workspace.subject_ref
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=workspace.sandbox_id
		WHERE workspace.id=$1 AND workspace.generation=$2 AND sandbox.state<>'deleted'`,
		checkpoint.WorkspaceID, input.ExpectedWorkspaceGeneration,
	).Scan(&tenantRef, &subjectRef); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
		}
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint authority lookup failed: %w", err)
	}
	checkpoint.ProjectID, checkpoint.TenantRef, checkpoint.SubjectRef =
		tenantRef, tenantRef, subjectRef
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		tenantRef+"\x1f"+subjectRef+"\x1fretained-byte-capacity",
	); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint subject capacity lock failed: %w", err)
	}
	var existingState, existingSHA256, existingStorageKey, existingWorkspaceID string
	var existingSizeBytes, existingGeneration int64
	existingErr := tx.QueryRow(ctx, `
		SELECT state,sha256,size_bytes,source_generation,storage_key,workspace_id
		FROM secondbox.workspace_checkpoints
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3`,
		checkpoint.ID, tenantRef, subjectRef,
	).Scan(
		&existingState, &existingSHA256, &existingSizeBytes, &existingGeneration,
		&existingStorageKey, &existingWorkspaceID,
	)
	if existingErr == nil {
		if existingSHA256 != checkpoint.SHA256 || existingSizeBytes != checkpoint.SizeBytes ||
			existingGeneration != checkpoint.SourceGeneration ||
			existingStorageKey != input.StorageKey ||
			existingWorkspaceID != checkpoint.WorkspaceID {
			return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
		}
		switch existingState {
		case contracts.ObjectStateStaging, contracts.ObjectStateVerified,
			contracts.ObjectStatePublished:
			checkpoint.State = existingState
			if err := tx.Commit(ctx); err != nil {
				return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging replay commit failed: %w", err)
			}
			return checkpoint, nil
		case contracts.ObjectStateQuotaFailed:
			return contracts.WorkspaceCheckpoint{}, ports.ErrQuotaExceeded
		default:
			return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
		}
	}
	if !errors.Is(existingErr, pgx.ErrNoRows) {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint replay lookup failed: %w", existingErr)
	}
	subjectQuota, err := readSubjectQuota(ctx, tx, tenantRef, subjectRef)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, err
	}
	subjectUsage, err := readSubjectQuotaUsage(ctx, tx, tenantRef, subjectRef)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, err
	}
	state := contracts.ObjectStateStaging
	quotaExceeded := subjectUsage.retainedBytes+checkpoint.SizeBytes > subjectQuota.MaxRetainedBytes
	if quotaExceeded {
		state = contracts.ObjectStateQuotaFailed
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO secondbox.workspace_checkpoints (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,source_generation,state,sha256,size_bytes,
			compatibility_json,storage_key,retain_until,created_at,published_at,
			garbage_collection_marked_at,garbage_collected_at
		)
		SELECT $1,workspace.tenant_ref,workspace.subject_ref,
		       workspace.sandbox_id,workspace.id,$2,$3,$4,$5,
		       $6,$7,$8,$9,NULL,NULL,NULL
		FROM secondbox.workspaces AS workspace
		WHERE workspace.id=$10 AND workspace.generation=$11`,
		checkpoint.ID, checkpoint.SourceGeneration, state, checkpoint.SHA256, checkpoint.SizeBytes,
		compatibilityJSON, input.StorageKey, checkpoint.RetainUntil.UTC(), checkpoint.CreatedAt.UTC(),
		checkpoint.WorkspaceID, input.ExpectedWorkspaceGeneration,
	)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging commit failed: %w", err)
	}
	if quotaExceeded {
		return contracts.WorkspaceCheckpoint{}, ports.ErrQuotaExceeded
	}
	checkpoint.State = contracts.ObjectStateStaging
	return checkpoint, nil
}

// VerifyCheckpoint records provider and content verification before reachability changes.
func (store *PostgresControlPlaneStore) VerifyCheckpoint(
	ctx context.Context,
	input ports.CheckpointPublicationInput,
	now time.Time,
) (contracts.WorkspaceCheckpoint, error) {
	checkpoint := input.Checkpoint
	compatibilityJSON, err := json.Marshal(checkpoint.Compatibility)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint verification compatibility encoding failed: %w", err)
	}
	var verifiedAt time.Time
	err = store.pool.QueryRow(ctx, `
		UPDATE secondbox.workspace_checkpoints
		SET state='verified',verified_at=$6
		WHERE id=$1 AND workspace_id=$2 AND source_generation=$3 AND state='staging'
		  AND sha256=$4 AND size_bytes=$5 AND storage_key=$7
		  AND compatibility_json=$8::jsonb
		RETURNING verified_at`,
		checkpoint.ID, checkpoint.WorkspaceID, checkpoint.SourceGeneration,
		checkpoint.SHA256, checkpoint.SizeBytes, now.UTC(), input.StorageKey, compatibilityJSON,
	).Scan(&verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var state string
		if err := store.pool.QueryRow(ctx, `
			SELECT state FROM secondbox.workspace_checkpoints
			WHERE id=$1 AND workspace_id=$2 AND source_generation=$3
			  AND sha256=$4 AND size_bytes=$5 AND storage_key=$6
			  AND compatibility_json=$7::jsonb`,
			checkpoint.ID, checkpoint.WorkspaceID, checkpoint.SourceGeneration,
			checkpoint.SHA256, checkpoint.SizeBytes, input.StorageKey, compatibilityJSON,
		).Scan(&state); err != nil ||
			(state != contracts.ObjectStateVerified && state != contracts.ObjectStatePublished) {
			return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
		}
		checkpoint.State = state
		return checkpoint, nil
	}
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint verification failed: %w", err)
	}
	checkpoint.State = contracts.ObjectStateVerified
	return checkpoint, nil
}

// PublishCheckpoint atomically makes verified bytes the current Workspace checkpoint.
func (store *PostgresControlPlaneStore) PublishCheckpoint(
	ctx context.Context,
	input ports.CheckpointPublicationInput,
	now time.Time,
) (contracts.WorkspaceCheckpoint, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publication transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var checkpoint contracts.WorkspaceCheckpoint
	var compatibilityJSON []byte
	var storageKey string
	err = tx.QueryRow(ctx, `
		SELECT id,tenant_ref,subject_ref,sandbox_id,workspace_id,source_generation,state,sha256,size_bytes,
		       compatibility_json,storage_key,retain_until,created_at,published_at,garbage_collected_at
		FROM secondbox.workspace_checkpoints
		WHERE id=$1 AND workspace_id=$2 FOR UPDATE`,
		input.Checkpoint.ID, input.Checkpoint.WorkspaceID,
	).Scan(
		&checkpoint.ID, &checkpoint.TenantRef, &checkpoint.SubjectRef,
		&checkpoint.SandboxID, &checkpoint.WorkspaceID,
		&checkpoint.SourceGeneration, &checkpoint.State, &checkpoint.SHA256, &checkpoint.SizeBytes,
		&compatibilityJSON, &storageKey, &checkpoint.RetainUntil, &checkpoint.CreatedAt,
		&checkpoint.PublishedAt, &checkpoint.GarbageCollectedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointNotFound
	}
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publication lookup failed: %w", err)
	}
	checkpoint.ProjectID = checkpoint.TenantRef
	if checkpoint.SHA256 != input.Checkpoint.SHA256 ||
		checkpoint.SizeBytes != input.Checkpoint.SizeBytes || storageKey != input.StorageKey {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_checkpoints
			SET state='integrity_failed' WHERE id=$1 AND state IN ('staging','verified')`,
			checkpoint.ID,
		); err != nil {
			return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint integrity failure update failed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint integrity failure commit failed: %w", err)
		}
		return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
	}
	if checkpoint.State == contracts.ObjectStatePublished {
		if err := json.Unmarshal(compatibilityJSON, &checkpoint.Compatibility); err != nil {
			return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox published checkpoint compatibility decoding failed: %w", err)
		}
		return checkpoint, tx.Commit(ctx)
	}
	if checkpoint.State != contracts.ObjectStateVerified {
		return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET current_checkpoint_id=$1,current_checkpoint_sha256=$2,
		    current_checkpoint_size_bytes=$3,retained_bytes=retained_bytes+$3,
		    garbage_collection_state='reachable',updated_at=$4
		WHERE id=$5 AND tenant_ref=$6 AND subject_ref=$7 AND generation=$8`,
		checkpoint.ID, checkpoint.SHA256, checkpoint.SizeBytes, now.UTC(),
		checkpoint.WorkspaceID, checkpoint.TenantRef, checkpoint.SubjectRef,
		input.ExpectedWorkspaceGeneration,
	)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox current checkpoint update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_checkpoints SET state='published',published_at=$2 WHERE id=$1`,
		checkpoint.ID, now.UTC(),
	); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publish update failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publication commit failed: %w", err)
	}
	if err := json.Unmarshal(compatibilityJSON, &checkpoint.Compatibility); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint compatibility decoding failed: %w", err)
	}
	checkpoint.State = contracts.ObjectStatePublished
	publishedAt := now.UTC()
	checkpoint.PublishedAt = &publishedAt
	return checkpoint, nil
}

// StageArtifact reserves quota and records unreachable metadata before immutable upload.
