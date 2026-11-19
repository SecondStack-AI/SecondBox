package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/imagepreparation"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
)

func (broker *PostgresEffectBroker) cancelObsoleteSandboxImagePreparations(ctx context.Context, now time.Time) (bool, error) {
	rows, err := broker.pool.Query(ctx, `SELECT sandbox.id FROM secondbox.sandboxes sandbox WHERE sandbox.desired_state IN ('stopped','deleted') AND sandbox.current_instance_id='' AND EXISTS(SELECT 1 FROM secondbox.lifecycle_effects effect WHERE effect.sandbox_id=sandbox.id AND effect.kind='prepare_image' AND effect.state NOT IN ('completed','failed')) ORDER BY sandbox.id LIMIT 32`)
	if err != nil {
		return false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	changed := false
	for _, id := range ids {
		updated, err := broker.cancelObsoleteSandboxImagePreparation(ctx, id, now)
		if err != nil {
			return false, fmt.Errorf("SecondBox obsolete Sandbox image preparation %s: %w", id, err)
		}
		changed = changed || updated
	}
	return changed, nil
}

func (broker *PostgresEffectBroker) cancelObsoleteSandboxImagePreparation(ctx context.Context, sandboxID string, now time.Time) (bool, error) {
	tx, err := broker.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return false, err
	}
	if (locked.DesiredState != "stopped" && locked.DesiredState != "deleted") || locked.CurrentInstanceID != "" || locked.Workspace.Mutation.Kind == "start" || locked.Workspace.Mutation.Kind == "archive_restore" {
		return false, nil
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secondbox.assignments WHERE sandbox_id=$1 AND state NOT IN ('fenced','released','failed_terminal'))`, sandboxID).Scan(&active); err != nil {
		return false, err
	}
	if active {
		return false, nil
	}
	changed, err := imagepreparation.CancelSandboxPreparations(ctx, tx, sandboxID, now)
	if err != nil {
		return false, err
	}
	return changed, tx.Commit(ctx)
}
