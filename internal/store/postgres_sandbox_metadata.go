package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// UpdateSandboxMetadata replaces application correlation metadata under the
// Sandbox revision fence without changing any lifecycle or runner authority.
func (store *PostgresControlPlaneStore) UpdateSandboxMetadata(
	ctx context.Context,
	input ports.UpdateSandboxMetadataInput,
) (contracts.Sandbox, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Sandbox{}, fmt.Errorf(
			"SecondBox Sandbox metadata transaction failed: %w",
			err,
		)
	}
	defer tx.Rollback(ctx)
	locked, err := lockSandboxWorkspace(
		ctx,
		tx,
		input.Principal.TenantRef,
		input.Principal.SubjectRef,
		input.SandboxID,
	)
	if err != nil {
		return contracts.Sandbox{}, err
	}
	if locked.Revision != input.ExpectedRevision {
		return contracts.Sandbox{}, ports.ErrRevisionConflict
	}
	if locked.SandboxState == contracts.SandboxStateDeleted ||
		locked.DesiredState == contracts.SandboxDesiredStateDeleted {
		return contracts.Sandbox{}, ports.ErrGenerationFenced
	}
	metadataJSON, err := json.Marshal(input.Metadata)
	if err != nil {
		return contracts.Sandbox{}, fmt.Errorf(
			"SecondBox Sandbox metadata encoding failed: %w",
			err,
		)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET metadata_json=$2,revision=revision+1,updated_at=$3
		WHERE id=$1`,
		input.SandboxID,
		metadataJSON,
		input.Now.UTC(),
	); err != nil {
		return contracts.Sandbox{}, fmt.Errorf(
			"SecondBox Sandbox metadata update failed: %w",
			err,
		)
	}
	sandbox, err := getSandboxWithQuerier(
		ctx,
		tx,
		input.Principal.TenantRef,
		input.Principal.SubjectRef,
		input.SandboxID,
	)
	if err != nil {
		return contracts.Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Sandbox{}, fmt.Errorf(
			"SecondBox Sandbox metadata commit failed: %w",
			err,
		)
	}
	return sandbox, nil
}
