package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

func (store *PostgresControlPlaneStore) ListGarbageObjectsDue(
	ctx context.Context,
	now time.Time,
	grace time.Duration,
	limit int,
) ([]ports.GarbageObject, error) {
	if grace <= 0 || limit < 1 {
		return nil, errors.New("SecondBox garbage collection grace and limit must be positive")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("SecondBox garbage collection transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.artifacts
		SET state='garbage_pending',garbage_collection_marked_at=$1
		WHERE state IN ('published','staging','integrity_failed') AND retain_until<=$1`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox Artifact garbage marking failed: %w", err)
	}
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT 'artifact'::text AS kind,artifact.id,artifact.storage_key,artifact.sha256,artifact.size_bytes
			FROM secondbox.artifacts AS artifact
			WHERE artifact.state IN ('garbage_pending','garbage_deleting')
			  AND artifact.garbage_collection_marked_at<=$1
			ORDER BY id
			LIMIT $2
		)
		SELECT kind,id,storage_key,sha256,size_bytes FROM candidates`,
		now.UTC().Add(-grace), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox garbage candidates query failed: %w", err)
	}
	candidates := make([]ports.GarbageObject, 0)
	for rows.Next() {
		var candidate ports.GarbageObject
		if err := rows.Scan(
			&candidate.Kind, &candidate.ID, &candidate.StorageKey,
			&candidate.SHA256, &candidate.SizeBytes,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox garbage candidate scan failed: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox garbage candidate iteration failed: %w", err)
	}
	for _, candidate := range candidates {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.artifacts
			SET state='garbage_deleting' WHERE id=$1 AND state IN ('garbage_pending','garbage_deleting')`,
			candidate.ID,
		); err != nil {
			return nil, fmt.Errorf("SecondBox garbage deletion claim failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("SecondBox garbage deletion claim commit failed: %w", err)
	}
	return candidates, nil
}

// CompleteGarbageObject records terminal deletion evidence for a claimed object.
func (store *PostgresControlPlaneStore) CompleteGarbageObject(
	ctx context.Context,
	object ports.GarbageObject,
	now time.Time,
) error {
	switch object.Kind {
	case "artifact":
		tag, err := store.pool.Exec(ctx, `
			UPDATE secondbox.artifacts SET state='deleted',garbage_collected_at=$2
			WHERE id=$1 AND state='garbage_deleting'`, object.ID, now.UTC())
		if err != nil {
			return fmt.Errorf("SecondBox garbage completion failed: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("SecondBox garbage object deletion claim is no longer current")
		}
		return nil
	default:
		return errors.New("SecondBox garbage object kind is invalid")
	}
}
