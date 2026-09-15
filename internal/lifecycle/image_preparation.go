package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

const imagePreparationDeadline = 30 * time.Minute

// prepareStartImage reuses durable lifecycle effects and Runner command delivery.
// It returns before scheduling compute until signed metadata is available.
func (broker *PostgresEffectBroker) prepareStartImage(ctx context.Context, claim ports.LifecycleReconcileClaim, plan startPlan, now time.Time) ([]*runnerv1.AssetReference, string, bool, error) {
	var state string
	var evidence []byte
	var deadline time.Time
	err := broker.pool.QueryRow(ctx, `SELECT state,evidence_json,effect_deadline FROM secondbox.lifecycle_effects WHERE id=$1 AND kind='prepare_image'`, plan.operationID).Scan(&state, &evidence, &deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		deadline = now.Add(imagePreparationDeadline)
		if plan.attributed != nil && plan.attributed.ExpiresAt.Before(deadline) {
			deadline = plan.attributed.ExpiresAt
		}
		command := &runnerv1.PrepareImageCommand{OperationId: plan.operationID, TenantRef: plan.tenantRef, Reference: plan.image.RequestedReference, DeadlineUnixMs: uint64(deadline.UnixMilli())}
		payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_PrepareImage{PrepareImage: command}})
		if err != nil {
			return nil, "", false, err
		}
		tx, err := broker.pool.Begin(ctx)
		if err != nil {
			return nil, "", false, err
		}
		defer tx.Rollback(ctx)
		commandID := "prepare-image-" + plan.operationID
		tag, err := tx.Exec(ctx, `
			INSERT INTO secondbox.lifecycle_effects (
			 id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			 command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			 claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,created_at,updated_at
			) SELECT $1,sandbox.id,sandbox.generation,'prepare_image','queued','','',workspace.home_runner_id,
			 $4,'','',0,0,$5,'',$6,'','','{}','{}',$6,$6
			 FROM secondbox.sandboxes sandbox JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id
			 WHERE sandbox.id=$2 AND sandbox.reconcile_owner=$3 AND sandbox.generation=$7
			 ON CONFLICT (id) DO NOTHING`, plan.operationID, claim.SandboxID, claim.WorkerID, commandID, deadline, now, plan.generation)
		if err != nil {
			return nil, "", false, fmt.Errorf("SecondBox image preparation effect insert: %w", err)
		}
		if tag.RowsAffected() == 1 {
			_, err = tx.Exec(ctx, `INSERT INTO secondbox.runner_commands
			 (id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at,delivered_at)
			 SELECT command_id,runner_id,id,'prepare-image',$2,'pending','',0,$3,$3,NULL FROM secondbox.lifecycle_effects WHERE id=$1`, plan.operationID, payload, now)
			if err != nil {
				return nil, "", false, fmt.Errorf("SecondBox image preparation command insert: %w", err)
			}
		}
		return nil, "", false, tx.Commit(ctx)
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("SecondBox image preparation lookup: %w", err)
	}
	if !deadline.After(now) {
		return nil, "", false, errors.New("SecondBox image preparation deadline expired")
	}
	if state == "failed" {
		return nil, "", false, errors.New("SecondBox image preparation failed")
	}
	if state != "completed" {
		return nil, "", false, nil
	}
	var result runnerv1.PrepareImageResult
	if err := json.Unmarshal(evidence, &result); err != nil {
		return nil, "", false, err
	}
	assets, err := broker.config.ExecutionImageAuthority.ImportManifest(result.Manifest, result.Signature)
	if err != nil {
		return nil, "", false, err
	}
	if assets[0].Architecture != plan.spec.Architecture {
		return nil, "", false, errors.New("SecondBox selected image architecture does not satisfy the pinned Profile")
	}
	return assets, contracts.ExecutionImageDigestReference(plan.image.RequestedReference, result.ResolvedDigest), true, nil
}
