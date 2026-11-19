package runnercontrol

import (
	"context"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
)

func archiveStartContinuation(ctx context.Context, tx pgx.Tx, locked *rowlock.SandboxWorkspace, operationID string, now time.Time) (bool, bool, error) {
	var kind, state string
	var deadline *time.Time
	if err := tx.QueryRow(ctx, `SELECT kind,state,capacity_wait_deadline FROM secondbox.operations WHERE id=$1`, operationID).Scan(&kind, &state, &deadline); err != nil {
		return false, false, err
	}
	start := kind == "start" || kind == "create"
	if start && (state == "pending" || state == "running") && locked.CurrentInstanceID == "" && deadline != nil && !deadline.After(now) {
		if _, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='failed',error_code='capacity_wait_expired',error_message='workspace transfer capacity wait expired before compute admission',retryable=true,completed_at=$2,updated_at=$2 WHERE id=$1 AND state IN ('pending','running')`, operationID, now); err != nil {
			return false, false, err
		}
		state = "failed"
		if locked.DesiredState == "running" {
			if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET desired_state='stopped',next_reconcile_at=NULL,reconcile_owner='',reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$2 WHERE id=$1`, locked.SandboxID, now); err != nil {
				return false, false, err
			}
			locked.DesiredState = "stopped"
		}
	}
	return start, start && (state == "pending" || state == "running") && locked.DesiredState == "running", nil
}
