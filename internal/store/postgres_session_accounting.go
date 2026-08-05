package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SweepSessionAccounting removes at most limit expired accounting records.
func (store *PostgresControlPlaneStore) SweepSessionAccounting(
	ctx context.Context,
	now time.Time,
	activityRetention time.Duration,
	limit int,
) (int64, error) {
	if now.IsZero() || activityRetention <= 0 || limit <= 0 {
		return 0, errors.New("SecondBox session-accounting sweep configuration is invalid")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("SecondBox session-accounting sweep transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var idempotencyDeleted int64
	if err := tx.QueryRow(ctx, `
		WITH candidates AS (
			SELECT ctid FROM secondbox.idempotency_records
			WHERE expires_at <= $1 ORDER BY expires_at,ctid
			LIMIT $2 FOR UPDATE SKIP LOCKED
		), deleted AS (
			DELETE FROM secondbox.idempotency_records AS records
			USING candidates WHERE records.ctid=candidates.ctid RETURNING 1
		)
		SELECT count(*) FROM deleted`, now.UTC(), limit).Scan(&idempotencyDeleted); err != nil {
		return 0, fmt.Errorf("SecondBox expired idempotency sweep failed: %w", err)
	}
	remaining := int64(limit) - idempotencyDeleted
	var activityDeleted int64
	if remaining > 0 {
		if err := tx.QueryRow(ctx, `
			WITH candidates AS (
				SELECT ctid FROM secondbox.activity_touches
				WHERE created_at <= $1 ORDER BY created_at,ctid
				LIMIT $2 FOR UPDATE SKIP LOCKED
			), deleted AS (
				DELETE FROM secondbox.activity_touches AS touches
				USING candidates WHERE touches.ctid=candidates.ctid RETURNING 1
			)
			SELECT count(*) FROM deleted`, now.UTC().Add(-activityRetention), remaining).Scan(&activityDeleted); err != nil {
			return 0, fmt.Errorf("SecondBox aged activity-touch sweep failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("SecondBox session-accounting sweep commit failed: %w", err)
	}
	return idempotencyDeleted + activityDeleted, nil
}
