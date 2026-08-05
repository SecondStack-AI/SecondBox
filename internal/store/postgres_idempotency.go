package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

func deleteExpiredIdempotencyRecord(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	operation string,
	targetID string,
	idempotencyKey string,
	expiresAt time.Time,
	now time.Time,
) (bool, error) {
	if expiresAt.After(now.UTC()) {
		return false, nil
	}
	_, err := tx.Exec(ctx, `
		DELETE FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
		tenantRef, subjectRef, operation, targetID, idempotencyKey,
	)
	return true, err
}
