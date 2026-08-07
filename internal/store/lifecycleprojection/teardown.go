package lifecycleprojection

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Teardown orchestration milestones. Each names one durable control-plane
// transition on the drain, stop, and delete path, so the interval between two
// adjacent milestones is attributable to exactly one hop. The names are fixed
// and carry no identifiers.
const (
	StageTeardownDrainCommitted            = "teardown_drain_committed"
	StageTeardownFenceDispatched           = "teardown_fence_dispatched"
	StageTeardownFenceAcknowledged         = "teardown_fence_acknowledged"
	StageTeardownGenerationAdvanced        = "teardown_generation_advanced"
	StageTeardownStopCommitted             = "teardown_stop_committed"
	StageTeardownWorkspaceDeleteDispatched = "teardown_workspace_delete_dispatched"
	StageTeardownFinalized                 = "teardown_finalized"
)

// RecordTeardownStage attributes one teardown milestone to every in-flight
// drain, stop, or delete Operation of the Sandbox, inside the transaction that
// establishes the milestone's fact. A delete traverses stop semantics, so a
// stop Operation and a delete Operation observe the same shared hops.
//
// The write is intentionally keyed by Sandbox rather than by Operation: the
// transitions that establish these facts already hold the Sandbox row and do
// not all carry an Operation identity. Sandboxes with no in-flight lifecycle
// Operation record nothing, which is correct — an unattributed transition has
// no Operation wall clock to account for.
func RecordTeardownStage(
	ctx context.Context,
	tx pgx.Tx,
	sandboxID string,
	stage string,
	observedAt time.Time,
) error {
	if sandboxID == "" || stage == "" {
		return fmt.Errorf(
			"SecondBox teardown stage attribution requires a Sandbox and a stage, got %q and %q",
			sandboxID, stage,
		)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.operation_stage_timings (
			operation_id,sandbox_id,stage,observed_at
		)
		SELECT id,sandbox_id,$2,$3
		FROM secondbox.operations
		WHERE sandbox_id=$1 AND kind IN ('drain','stop','delete')
		  AND state IN ('pending','running')
		ON CONFLICT (operation_id,stage) DO NOTHING`,
		sandboxID, stage, observedAt.UTC(),
	); err != nil {
		return fmt.Errorf(
			"SecondBox teardown stage %q attribution failed: %w", stage, err,
		)
	}
	return nil
}
