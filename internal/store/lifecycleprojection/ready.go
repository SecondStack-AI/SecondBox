// Package lifecycleprojection owns PostgreSQL projections shared by durable
// lifecycle transitions and the runner evidence transactions that establish
// those transitions' prerequisites.
package lifecycleprojection

import (
	"context"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// ProjectReadyOperations completes pending create or start Operations in the
// same transaction that projects their Sandbox ready.
func ProjectReadyOperations(
	ctx context.Context,
	tx pgx.Tx,
	sandboxID string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		WITH inserted_stage AS (
			INSERT INTO secondbox.operation_stage_timings (
				operation_id,sandbox_id,stage,observed_at
			)
			SELECT id,sandbox_id,'ready_projected',$2
			FROM secondbox.operations
			WHERE sandbox_id=$1 AND kind IN ('create','start')
			  AND state IN ('pending','running')
			ON CONFLICT (operation_id,stage) DO NOTHING
			RETURNING 1
		)
		UPDATE secondbox.operations
		SET state=$3,error_code='',error_message='',retryable=false,
		    started_at=COALESCE(started_at,$2),completed_at=$2,updated_at=$2
		WHERE sandbox_id=$1 AND kind IN ('create','start')
		  AND state IN ('pending','running')`,
		sandboxID,
		now.UTC(),
		contracts.OperationStateSucceeded,
	); err != nil {
		return fmt.Errorf("SecondBox ready Operation projection failed: %w", err)
	}
	return nil
}
