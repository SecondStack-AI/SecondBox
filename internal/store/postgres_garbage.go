package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/jackc/pgx/v5"
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
		UPDATE secondbox.snapshots
		SET state='expired',retention_ended_at=retain_until
		WHERE state='published' AND retain_until<=$1`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox Snapshot expiry marking failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_checkpoints
		SET state='garbage_pending',garbage_collection_marked_at=$1
		WHERE state IN ('staging','verified','integrity_failed','quota_failed')
		  AND retain_until<=$1`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox interrupted checkpoint garbage marking failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_checkpoints AS checkpoint
		SET state='garbage_pending',garbage_collection_marked_at=$1
		WHERE checkpoint.state='published' AND checkpoint.retain_until<=$1
		  AND NOT EXISTS (
		    SELECT 1 FROM secondbox.workspaces AS workspace
		    WHERE workspace.current_checkpoint_id=checkpoint.id
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM secondbox.workspace_materializations AS materialization
		    WHERE materialization.source_checkpoint_id=checkpoint.id
		      AND materialization.state IN ('preparing','ready')
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM secondbox.snapshots AS snapshot
		    WHERE snapshot.checkpoint_id=checkpoint.id
		      AND snapshot.state='published' AND snapshot.retain_until>$1
		  )`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox checkpoint garbage marking failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.artifacts
		SET state='garbage_pending',garbage_collection_marked_at=$1
		WHERE state IN ('published','staging','integrity_failed') AND retain_until<=$1`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox Artifact garbage marking failed: %w", err)
	}
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT 'checkpoint'::text AS kind,checkpoint.id,checkpoint.storage_key,
			       checkpoint.sha256,checkpoint.size_bytes
			FROM secondbox.workspace_checkpoints AS checkpoint
			WHERE checkpoint.state IN ('garbage_pending','garbage_deleting')
			  AND checkpoint.garbage_collection_marked_at<=$1
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspaces AS workspace
			    WHERE workspace.current_checkpoint_id=checkpoint.id
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspace_materializations AS materialization
			    WHERE materialization.source_checkpoint_id=checkpoint.id
			      AND materialization.state IN ('preparing','ready')
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.snapshots AS snapshot
			    WHERE snapshot.checkpoint_id=checkpoint.id
			      AND snapshot.state='published' AND snapshot.retain_until>$2
			  )
			UNION ALL
			SELECT 'artifact'::text,artifact.id,artifact.storage_key,artifact.sha256,artifact.size_bytes
			FROM secondbox.artifacts AS artifact
			WHERE artifact.state IN ('garbage_pending','garbage_deleting')
			  AND artifact.garbage_collection_marked_at<=$1
			ORDER BY id
			LIMIT $3
		)
		SELECT kind,id,storage_key,sha256,size_bytes FROM candidates`,
		now.UTC().Add(-grace), now.UTC(), limit,
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
		table := "workspace_checkpoints"
		if candidate.Kind == "artifact" {
			table = "artifacts"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.`+table+`
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
	case "checkpoint":
		tx, err := store.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("SecondBox checkpoint garbage completion transaction failed: %w", err)
		}
		defer tx.Rollback(ctx)
		var workspaceID string
		var sizeBytes int64
		var wasRetained bool
		if err := tx.QueryRow(ctx, `
			SELECT checkpoint.workspace_id,checkpoint.size_bytes,
			       checkpoint.published_at IS NOT NULL
			FROM secondbox.workspace_checkpoints AS checkpoint
			WHERE checkpoint.id=$1 AND checkpoint.state='garbage_deleting'
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspaces AS workspace
			    WHERE workspace.current_checkpoint_id=checkpoint.id
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspace_materializations AS materialization
			    WHERE materialization.source_checkpoint_id=checkpoint.id
			      AND materialization.state IN ('preparing','ready')
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.snapshots AS snapshot
			    WHERE snapshot.checkpoint_id=checkpoint.id
			      AND snapshot.state='published' AND snapshot.retain_until>$2
			  )
			FOR UPDATE OF checkpoint`,
			object.ID, now.UTC(),
		).Scan(&workspaceID, &sizeBytes, &wasRetained); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("SecondBox checkpoint deletion claim is no longer current")
			}
			return fmt.Errorf("SecondBox checkpoint deletion claim lookup failed: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_checkpoints AS checkpoint
			SET state='deleted',garbage_collected_at=$2
			WHERE checkpoint.id=$1`, object.ID, now.UTC()); err != nil {
			return fmt.Errorf("SecondBox checkpoint garbage completion failed: %w", err)
		}
		if wasRetained {
			tag, err := tx.Exec(ctx, `
				UPDATE secondbox.workspaces
				SET retained_bytes=retained_bytes-$2,updated_at=$3
				WHERE id=$1 AND retained_bytes>=$2`,
				workspaceID, sizeBytes, now.UTC(),
			)
			if err != nil {
				return fmt.Errorf("SecondBox checkpoint retained-byte release failed: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return errors.New("SecondBox checkpoint retained-byte evidence is inconsistent")
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("SecondBox checkpoint garbage completion commit failed: %w", err)
		}
		return nil
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
