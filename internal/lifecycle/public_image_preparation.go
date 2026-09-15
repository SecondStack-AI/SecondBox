package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/imagepreparation"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// ReconcileImagePreparations advances existing Operations without allocating a Sandbox.
func (broker *PostgresEffectBroker) ReconcileImagePreparations(ctx context.Context, now time.Time) (bool, error) {
	tx, err := broker.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT operation.id,operation.tenant_ref,operation.request_metadata_json,operation.created_at
	 FROM secondbox.operations operation WHERE operation.kind='prepare_image' AND operation.state IN ('pending','running')
	 AND (operation.created_at<=$1
	 OR (operation.state='pending' AND EXISTS (SELECT 1 FROM secondbox.lifecycle_effects effect WHERE effect.id=operation.id||'-resolve' AND effect.state IN ('completed','failed')))
	 OR (operation.state='running' AND (NOT EXISTS (SELECT 1 FROM secondbox.lifecycle_effects effect WHERE effect.assignment_id=operation.id AND effect.kind='prepare_image' AND effect.state='queued')
	 OR EXISTS (SELECT 1 FROM secondbox.lifecycle_effects effect WHERE effect.assignment_id=operation.id AND effect.kind='prepare_image' AND effect.state='failed'))))
	 ORDER BY operation.created_at,operation.id LIMIT 16 FOR UPDATE OF operation SKIP LOCKED`, now.Add(-imagepreparation.Deadline))
	if err != nil {
		return false, err
	}
	type pending struct {
		id, tenant string
		metadata   map[string]string
		created    time.Time
	}
	var operations []pending
	for rows.Next() {
		var operation pending
		var encoded []byte
		if err := rows.Scan(&operation.id, &operation.tenant, &encoded, &operation.created); err != nil {
			rows.Close()
			return false, err
		}
		if err := json.Unmarshal(encoded, &operation.metadata); err != nil {
			rows.Close()
			return false, err
		}
		operations = append(operations, operation)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	changed := false
	for _, operation := range operations {
		updated, err := broker.reconcileImagePreparation(ctx, tx, operation.id, operation.tenant, operation.metadata["executionImageReference"], operation.created.Add(imagepreparation.Deadline), now)
		if err != nil {
			return false, err
		}
		changed = changed || updated
	}
	return changed, tx.Commit(ctx)
}

func (broker *PostgresEffectBroker) reconcileImagePreparation(ctx context.Context, tx pgx.Tx, id, tenant, reference string, deadline, now time.Time) (bool, error) {
	fail := func(message string) (bool, error) {
		_, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='failed',error_code='image_preparation_failed',error_message=$2,completed_at=$3,updated_at=$3 WHERE id=$1`, id, message, now)
		return true, err
	}
	if !deadline.After(now) {
		return fail("Image preparation deadline expired")
	}
	var state string
	var encoded, payload []byte
	if err := tx.QueryRow(ctx, `SELECT state,evidence_json,payload_json FROM secondbox.lifecycle_effects WHERE id=$1`, id+"-resolve").Scan(&state, &encoded, &payload); err != nil {
		return false, err
	}
	if state == "failed" {
		return fail("Image resolution or verification failed")
	}
	if state != "completed" {
		return false, nil
	}
	var resolved runnerv1.PrepareImageResult
	if err := json.Unmarshal(encoded, &resolved); err != nil {
		return false, err
	}
	assets, err := broker.config.ExecutionImageAuthority.ImportManifest(resolved.Manifest, resolved.Signature)
	if err != nil {
		return fail("Image publisher signature or manifest is invalid")
	}
	var plan imagepreparation.Payload
	if err := json.Unmarshal(payload, &plan); err != nil {
		return false, err
	}
	var targets []imagepreparation.Target
	for _, target := range plan.Targets {
		if slices.Contains(target.Architectures, assets[0].Architecture) {
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		return fail("Image architecture has no authorized preparation target")
	}
	var queued int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM secondbox.lifecycle_effects WHERE assignment_id=$1 AND kind='prepare_image' AND payload_json->>'role'='prepare'`, id).Scan(&queued); err != nil {
		return false, err
	}
	if queued == 0 {
		for index, target := range targets {
			if err := imagepreparation.Queue(ctx, tx, fmt.Sprintf("%s-prepare-%d", id, index), id, tenant, contracts.ExecutionImageDigestReference(reference, resolved.ResolvedDigest), target.RunnerID, imagepreparation.Payload{Role: "prepare"}, now, deadline); err != nil {
				return false, err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='running',started_at=COALESCE(started_at,$2),updated_at=$2 WHERE id=$1`, id, now)
		return true, err
	}
	rows, err := tx.Query(ctx, `SELECT state,evidence_json FROM secondbox.lifecycle_effects WHERE assignment_id=$1 AND kind='prepare_image' AND payload_json->>'role'='prepare' ORDER BY id`, id)
	if err != nil {
		return false, err
	}
	complete := 0
	failed := false
	for rows.Next() {
		var evidence []byte
		if err := rows.Scan(&state, &evidence); err != nil {
			rows.Close()
			return false, err
		}
		if state == "failed" {
			failed = true
		}
		if state != "completed" {
			continue
		}
		var result runnerv1.PrepareImageResult
		if err := json.Unmarshal(evidence, &result); err != nil {
			rows.Close()
			return false, err
		}
		if result.ResolvedDigest != resolved.ResolvedDigest || !bytes.Equal(result.Manifest, resolved.Manifest) || !bytes.Equal(result.Signature, resolved.Signature) {
			failed = true
		}
		complete++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if failed {
		return fail("Image preparation failed on an authorized target")
	}
	if complete != len(targets) {
		return false, nil
	}
	_, err = tx.Exec(ctx, `UPDATE secondbox.operations SET state='succeeded',completed_at=$2,updated_at=$2 WHERE id=$1`, id, now)
	return true, err
}
