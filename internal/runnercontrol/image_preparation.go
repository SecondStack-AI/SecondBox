package runnercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
)

var preparedImageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func recordImagePreparationResult(ctx context.Context, tx pgx.Tx, runnerID string, result *runnerv1.PrepareImageResult, now time.Time) error {
	if result == nil || result.OperationId == "" || len(result.Manifest) > 1<<20 || len(result.Signature) > 8192 || len(result.Failure) > 512 {
		return errors.New("SecondBox image preparation evidence exceeds protocol bounds")
	}
	var commandID, state string
	var deadline time.Time
	err := tx.QueryRow(ctx, `SELECT command_id,state,effect_deadline FROM secondbox.lifecycle_effects WHERE id=$1 AND runner_id=$2 AND kind='prepare_image' FOR UPDATE`, result.OperationId, runnerID).Scan(&commandID, &state, &deadline)
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
		if !preparedImageDigestPattern.MatchString(result.ResolvedDigest) || len(result.Manifest) == 0 || len(result.Signature) == 0 {
			return errors.New("SecondBox image preparation signed evidence is incomplete")
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
	}
	return nil
}
