package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (store *PostgresControlPlaneStore) AcquireMaterialization(
	ctx context.Context,
	input ports.MaterializationInput,
) (contracts.WorkspaceMaterialization, error) {
	materialization := input.Materialization
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var workspaceGeneration int64
	var sandboxID, sandboxState, currentCheckpointID string
	if err := tx.QueryRow(ctx, `
		SELECT workspace.generation,workspace.sandbox_id,sandbox.state,workspace.current_checkpoint_id
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=workspace.sandbox_id
		WHERE workspace.id=$1
		FOR UPDATE OF workspace,sandbox`,
		materialization.WorkspaceID,
	).Scan(&workspaceGeneration, &sandboxID, &sandboxState, &currentCheckpointID); err != nil {
		return contracts.WorkspaceMaterialization{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if workspaceGeneration != input.ExpectedWorkspaceGeneration ||
		materialization.Generation != input.ExpectedWorkspaceGeneration {
		return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
	}
	if materialization.SourceCheckpointID != "" {
		if sandboxState != contracts.SandboxStateStopped ||
			currentCheckpointID != materialization.SourceCheckpointID {
			return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
		}
		var checkpointState string
		if err := tx.QueryRow(ctx, `
			SELECT state FROM secondbox.workspace_checkpoints
			WHERE id=$1 AND workspace_id=$2 FOR UPDATE`,
			materialization.SourceCheckpointID, materialization.WorkspaceID,
		).Scan(&checkpointState); err != nil {
			return contracts.WorkspaceMaterialization{}, mapNotFound(err, ports.ErrCheckpointNotFound)
		}
		if checkpointState != contracts.ObjectStatePublished {
			return contracts.WorkspaceMaterialization{}, ports.ErrCheckpointIntegrity
		}
	}
	releaseProofJSON, err := json.Marshal(map[string]string{})
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization proof encoding failed: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO secondbox.workspace_materializations (
			id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
			source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at,released_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,1,$10,$10,NULL)`,
		materialization.ID, materialization.WorkspaceID, sandboxID, materialization.AssignmentID,
		materialization.RunnerID, materialization.Generation, materialization.SourceCheckpointID,
		contracts.MaterializationStatePreparing, releaseProofJSON, materialization.CreatedAt.UTC(),
	)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return contracts.WorkspaceMaterialization{}, ports.ErrMaterializationConflict
		}
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization commit failed: %w", err)
	}
	materialization.State, materialization.Revision = contracts.MaterializationStatePreparing, 1
	materialization.UpdatedAt = materialization.CreatedAt
	return materialization, nil
}

// ConfirmMaterialization records that the assigned Runner verified its generation-bound active image.
func (store *PostgresControlPlaneStore) ConfirmMaterialization(
	ctx context.Context,
	input ports.MaterializationInput,
	now time.Time,
) (contracts.WorkspaceMaterialization, error) {
	var materialization contracts.WorkspaceMaterialization
	var releaseProofJSON []byte
	err := store.pool.QueryRow(ctx, `
		UPDATE secondbox.workspace_materializations
		SET state='ready',revision=revision+1,updated_at=$6
		WHERE id=$1 AND workspace_id=$2 AND assignment_id=$3 AND runner_id=$4
		  AND generation=$5 AND state='preparing'
		  AND EXISTS (
		    SELECT 1 FROM secondbox.workspaces AS workspace
		    WHERE workspace.id=$2 AND workspace.generation=$5
		  )
		RETURNING id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
		          source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at`,
		input.Materialization.ID, input.Materialization.WorkspaceID,
		input.Materialization.AssignmentID, input.Materialization.RunnerID,
		input.ExpectedWorkspaceGeneration, now.UTC(),
	).Scan(
		&materialization.ID, &materialization.WorkspaceID, &materialization.SandboxID,
		&materialization.AssignmentID, &materialization.RunnerID, &materialization.Generation,
		&materialization.SourceCheckpointID, &materialization.State, &releaseProofJSON,
		&materialization.Revision, &materialization.CreatedAt, &materialization.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
	}
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization confirmation failed: %w", err)
	}
	if err := json.Unmarshal(releaseProofJSON, &materialization.ReleaseProof); err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization proof decoding failed: %w", err)
	}
	return materialization, nil
}

// ReleaseMaterialization records proof that runner-local writer authority ended.
func (store *PostgresControlPlaneStore) ReleaseMaterialization(
	ctx context.Context,
	input ports.MaterializationInput,
	releaseProof map[string]string,
	now time.Time,
) (contracts.WorkspaceMaterialization, error) {
	proofJSON, err := json.Marshal(releaseProof)
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization release proof encoding failed: %w", err)
	}
	var materialization contracts.WorkspaceMaterialization
	var storedProofJSON []byte
	err = store.pool.QueryRow(ctx, `
		UPDATE secondbox.workspace_materializations
		SET state='released',release_proof_json=$4,revision=revision+1,updated_at=$5,released_at=$5
		WHERE id=$1 AND workspace_id=$2 AND generation=$3 AND state IN ('preparing','ready')
		RETURNING id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
		          source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at`,
		input.Materialization.ID, input.Materialization.WorkspaceID,
		input.ExpectedWorkspaceGeneration, proofJSON, now.UTC(),
	).Scan(
		&materialization.ID, &materialization.WorkspaceID, &materialization.SandboxID,
		&materialization.AssignmentID, &materialization.RunnerID, &materialization.Generation,
		&materialization.SourceCheckpointID, &materialization.State, &storedProofJSON,
		&materialization.Revision, &materialization.CreatedAt, &materialization.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
	}
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization release failed: %w", err)
	}
	if err := json.Unmarshal(storedProofJSON, &materialization.ReleaseProof); err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization release proof decoding failed: %w", err)
	}
	return materialization, nil
}

// StageCheckpoint reserves retained-byte quota before recording unreachable upload metadata.
