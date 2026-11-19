package runnercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/imagepreparation"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
)

var preparedImageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func recordImagePreparationResult(ctx context.Context, tx pgx.Tx, runnerID string, result *runnerv1.PrepareImageResult, now time.Time) error {
	if result == nil || result.OperationId == "" || len(result.Manifest) > 1<<20 || len(result.Failure) > 512 {
		return errors.New("SecondBox image preparation evidence exceeds protocol bounds")
	}
	var commandID, state string
	var sandboxID string
	var generation int64
	var deadline time.Time
	var bindingJSON []byte
	if err := tx.QueryRow(ctx, `SELECT sandbox_id,generation FROM secondbox.lifecycle_effects WHERE id=$1 AND runner_id=$2 AND kind='prepare_image'`, result.OperationId, runnerID).Scan(&sandboxID, &generation); err != nil {
		return fmt.Errorf("SecondBox image preparation Sandbox authority lookup: %w", err)
	}
	if sandboxID != "" {
		if _, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID); err != nil {
			return fmt.Errorf("SecondBox image preparation Sandbox authority lock: %w", err)
		}
	}
	err := tx.QueryRow(ctx, `SELECT command_id,state,effect_deadline,payload_json FROM secondbox.lifecycle_effects WHERE id=$1 AND runner_id=$2 AND kind='prepare_image' FOR UPDATE`, result.OperationId, runnerID).Scan(&commandID, &state, &deadline, &bindingJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("SecondBox image preparation result has no authorized command")
	}
	if err != nil {
		return fmt.Errorf("SecondBox image preparation authority lookup: %w", err)
	}
	if state == "completed" || state == "failed" {
		return nil
	}
	if !deadline.After(now) {
		result = &runnerv1.PrepareImageResult{OperationId: result.OperationId, Failure: "Image preparation deadline expired"}
	}
	terminal := false
	if result.Failure != "" {
		state, terminal = "failed", true
	} else if result.ResolvedDigest != "" {
		if !preparedImageDigestPattern.MatchString(result.ResolvedDigest) || len(result.Manifest) == 0 {
			return errors.New("SecondBox image preparation evidence is incomplete")
		}
		state, terminal = "completed", true
	}
	evidence, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.lifecycle_effects SET state=$2,evidence_json=$3,updated_at=$4 WHERE id=$1`, result.OperationId, state, evidence, now); err != nil {
		return err
	}
	if terminal {
		if _, err := tx.Exec(ctx, `UPDATE secondbox.runner_commands SET state='acknowledged',updated_at=$2 WHERE id=$1`, commandID, now); err != nil {
			return err
		}
		// Release an old start claim as well as scheduling its successor. A
		// preparation can finish before that claim commits its bounded wait;
		// the old owner must not overwrite this terminal-evidence wakeup.
		if sandboxID != "" {
			startOperationID, err := imagepreparation.SandboxStartOperationID(bindingJSON, result.OperationId)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes s SET next_reconcile_at=$4,reconcile_owner='',reconcile_claim_expires_at=NULL WHERE s.id=$1 AND s.generation=$2 AND s.desired_state='running' AND s.current_instance_id='' AND EXISTS(SELECT 1 FROM secondbox.workspaces w WHERE w.id=s.workspace_id AND w.home_runner_id=$5 AND w.mutation_kind='start' AND w.mutation_operation_id=$3)`, sandboxID, generation, startOperationID, now, runnerID); err != nil {
				return fmt.Errorf("SecondBox image preparation lifecycle wake: %w", err)
			}
		}
	}
	return nil
}

// pinReadySandboxExecutionImage records the Sandbox execution-image pin from the
// Instance that became ready. The pinned reference stays as the client selected
// it, so only a newly resolved digest replaces it: a start that inherited the pin
// resolves the same digest and keeps the selected tag, and a start that carries no
// selected image never reaches this write.
func pinReadySandboxExecutionImage(ctx context.Context, tx pgx.Tx, sandboxID, operationID, resolvedDigest string) error {
	if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes AS sandbox
	 SET execution_image_reference=CASE WHEN sandbox.execution_image_digest=$3
	  THEN sandbox.execution_image_reference
	  ELSE COALESCE((SELECT NULLIF(operation.request_metadata_json->>'executionImageReference','')
	   FROM secondbox.operations AS operation WHERE operation.id=$2),sandbox.execution_image_reference) END,
	  execution_image_digest=$3
	 WHERE sandbox.id=$1`, sandboxID, operationID, resolvedDigest); err != nil {
		return fmt.Errorf("SecondBox ready Sandbox image pin update failed: %w", err)
	}
	return nil
}
