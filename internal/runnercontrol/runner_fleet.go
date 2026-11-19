package runnercontrol

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRunnerRetired = errors.New("SecondBox Runner identity has been retired")

// Retirement is a permanent fence even while a previously issued certificate
// remains within its validity period.
func validateRunnerFleetRegistration(ctx context.Context, tx pgx.Tx, runnerID string) error {
	var state string
	err := tx.QueryRow(ctx, `SELECT fleet_state FROM secondbox.runners WHERE id=$1 FOR UPDATE`, runnerID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "retired" {
		return ErrRunnerRetired
	}
	return nil
}

func acknowledgeFleetDrain(ctx context.Context, tx pgx.Tx, runnerID string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE secondbox.runner_commands SET state='acknowledged',updated_at=$2
 WHERE id=$1 AND kind='drain' AND state IN ('pending','delivering','delivered')`, "fleet-drain-"+runnerID, now)
	return err
}
