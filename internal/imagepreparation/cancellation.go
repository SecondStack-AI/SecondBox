package imagepreparation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Call with the Sandbox/Workspace locked and compute admission revoked.
// Preparation only fills the immutable image cache; it never owns a Workspace writer.
func CancelSandboxPreparations(ctx context.Context, tx pgx.Tx, sandboxID string, now time.Time) (bool, error) {
	var count int
	err := tx.QueryRow(ctx, `WITH cancelled AS (
 UPDATE secondbox.lifecycle_effects SET state='failed',failure_class='start_cancelled',failure_message='Sandbox start was cancelled before compute admission',claim_owner='',claim_expires_at=$2,updated_at=$2
 WHERE sandbox_id=$1 AND kind='prepare_image' AND state NOT IN ('completed','failed') RETURNING command_id
 ), expired AS (
 UPDATE secondbox.runner_commands SET state='expired',target_connection_id='',updated_at=$2 WHERE id IN (SELECT command_id FROM cancelled) AND state IN ('pending','delivering','delivered') RETURNING id
 ) SELECT count(*) FROM cancelled`, sandboxID, now).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("SecondBox cancelled Sandbox image preparation cleanup: %w", err)
	}
	return count != 0, nil
}
