package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/assetcatalog"
	"github.com/SecondStack-AI/SecondBox/internal/imagepreparation"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

const imagePreparationDeadline = 30 * time.Minute

// prepareStartImage reuses durable lifecycle effects and Runner command delivery.
// It returns before scheduling compute until image metadata is available.
func (broker *PostgresEffectBroker) prepareStartImage(ctx context.Context, claim ports.LifecycleReconcileClaim, plan startPlan, now time.Time) ([]*runnerv1.AssetReference, string, bool, error) {
	var state string
	var evidence []byte
	var deadline time.Time
	var home string
	if err := broker.pool.QueryRow(ctx, `SELECT home_runner_id FROM secondbox.workspaces WHERE id=$1`, plan.workspaceID).Scan(&home); err != nil {
		return nil, "", false, err
	}
	effectID := plan.operationID
	var restorePhase string
	if err := broker.pool.QueryRow(ctx, `SELECT COALESCE((SELECT id FROM secondbox.workspace_archives WHERE operation_id=$1 AND target_runner_id=$2 AND state='succeeded' ORDER BY updated_at DESC,id DESC LIMIT 1),'')`, plan.operationID, home).Scan(&restorePhase); err != nil {
		return nil, "", false, err
	}
	if restorePhase != "" {
		effectID = plan.operationID + "-image-" + restorePhase
	}
	err := broker.pool.QueryRow(ctx, `SELECT state,evidence_json,effect_deadline FROM secondbox.lifecycle_effects WHERE id=$1 AND runner_id=$2 AND kind='prepare_image'`, effectID, home).Scan(&state, &evidence, &deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		deadline = now.Add(imagePreparationDeadline)
		tx, err := broker.pool.Begin(ctx)
		if err != nil {
			return nil, "", false, err
		}
		defer tx.Rollback(ctx)
		locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, claim.SandboxID)
		if err != nil {
			return nil, "", false, err
		}
		validMutation := locked.Workspace.Mutation.Kind == "start" && locked.Workspace.Mutation.OperationID == plan.operationID
		if plan.mutationID == "" && locked.Workspace.Mutation.State == "" {
			validMutation = true
		}
		if locked.Revision != claim.Revision || locked.ReconcileOwner != claim.WorkerID || locked.Generation != plan.generation || locked.DesiredState != "running" || locked.CurrentInstanceID != "" || locked.Workspace.State != "ready" || !validMutation || locked.Workspace.HomeRunnerID != home {
			return nil, "", false, ports.ErrRevisionConflict
		}
		reference := plan.image.RequestedReference
		if restorePhase != "" {
			// A transferred start keeps any digest already resolved for that same
			// admitted intent, even if its public reference is a mutable tag.
			var sourceEvidence []byte
			err := tx.QueryRow(ctx, `SELECT evidence_json FROM secondbox.lifecycle_effects WHERE sandbox_id=$1 AND generation=$2 AND kind='prepare_image' AND state='completed' AND (id=$3 OR payload_json->>'startOperationId'=$3) ORDER BY created_at,id LIMIT 1`, claim.SandboxID, plan.generation, plan.operationID).Scan(&sourceEvidence)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, "", false, fmt.Errorf("SecondBox transferred start image evidence: %w", err)
			}
			if err == nil {
				var sourceResult runnerv1.PrepareImageResult
				if err := json.Unmarshal(sourceEvidence, &sourceResult); err != nil {
					return nil, "", false, fmt.Errorf("SecondBox transferred start image result: %w", err)
				}
				reference = contracts.ExecutionImageDigestReference(reference, sourceResult.ResolvedDigest)
			}
		}
		command := &runnerv1.PrepareImageCommand{OperationId: effectID, TenantRef: plan.tenantRef, Reference: reference, DeadlineUnixMs: uint64(deadline.UnixMilli())}
		payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_PrepareImage{PrepareImage: command}})
		if err != nil {
			return nil, "", false, err
		}
		commandID := "prepare-image-" + effectID
		binding, err := json.Marshal(imagepreparation.Payload{Role: "sandbox-start", StartOperationID: plan.operationID})
		if err != nil {
			return nil, "", false, err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO secondbox.lifecycle_effects (
			 id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			 command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			 claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,created_at,updated_at
			) SELECT $1,sandbox.id,sandbox.generation,'prepare_image','queued','','',workspace.home_runner_id,
			 $4,'','',0,0,$5,'',$6,'','',$8,'{}',$6,$6
			 FROM secondbox.sandboxes sandbox JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id
			 WHERE sandbox.id=$2 AND sandbox.reconcile_owner=$3 AND sandbox.generation=$7 AND workspace.home_runner_id=$9 AND (workspace.mutation_kind='start' OR workspace.mutation_state='')
			 ON CONFLICT (id) DO NOTHING`, effectID, claim.SandboxID, claim.WorkerID, commandID, deadline, now, plan.generation, binding, home)
		if err != nil {
			return nil, "", false, fmt.Errorf("SecondBox image preparation effect insert: %w", err)
		}
		if tag.RowsAffected() == 1 {
			_, err = tx.Exec(ctx, `INSERT INTO secondbox.runner_commands
			 (id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at,delivered_at)
			 SELECT command_id,runner_id,id,'prepare-image',$2,'pending','',0,$3,$3,NULL FROM secondbox.lifecycle_effects WHERE id=$1`, effectID, payload, now)
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
	assets, err := assetcatalog.ImportExecutionImageManifest(result.Manifest)
	if err != nil {
		return nil, "", false, err
	}
	if assets[0].Architecture != plan.spec.Architecture {
		return nil, "", false, errors.New("SecondBox selected image architecture does not satisfy the pinned Profile")
	}
	if _, err := broker.pool.Exec(ctx, `UPDATE secondbox.sandboxes SET execution_image_reference=$2,execution_image_digest=$3 WHERE id=$1 AND execution_image_digest='' AND generation=$4 AND reconcile_owner=$5`, claim.SandboxID, plan.image.RequestedReference, result.ResolvedDigest, plan.generation, claim.WorkerID); err != nil {
		return nil, "", false, err
	}
	return assets, contracts.ExecutionImageDigestReference(plan.image.RequestedReference, result.ResolvedDigest), true, nil
}
