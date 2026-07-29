package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// GetSandboxLifecyclePolicy reads the immutable ProfileRevision pinned by the Sandbox.
func (store *PostgresControlPlaneStore) GetSandboxLifecyclePolicy(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
) (contracts.LifecyclePolicy, contracts.CheckpointPolicy, error) {
	var specJSON []byte
	if err := store.pool.QueryRow(ctx, `
		SELECT revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$3`,
		tenantRef, subjectRef, sandboxID,
	).Scan(&specJSON); err != nil {
		return contracts.LifecyclePolicy{}, contracts.CheckpointPolicy{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return contracts.LifecyclePolicy{}, contracts.CheckpointPolicy{},
			fmt.Errorf("SecondBox pinned lifecycle policy decoding failed: %w", err)
	}
	return spec.Lifecycle, spec.Checkpoint, nil
}

// SetSandboxDesiredState records intent without claiming runner-side completion.
func (store *PostgresControlPlaneStore) SetSandboxDesiredState(
	ctx context.Context,
	input ports.LifecycleIntentInput,
) (contracts.Operation, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle intent transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.Principal.TenantRef + "\x1f" + input.Principal.SubjectRef +
		"\x1flifecycle\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle idempotency lock failed: %w", err)
	}
	var priorHash, priorOperationID string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
		input.Principal.TenantRef, input.Principal.SubjectRef,
		"sandbox."+input.Operation.Kind, input.SandboxID, input.IdempotencyKey,
	).Scan(&priorHash, &priorOperationID)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return contracts.Operation{}, ports.ErrIdempotencyConflict
		}
		operation, err := getOperationWithQuerier(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, `id=$3`, priorOperationID,
		)
		if err != nil {
			return contracts.Operation{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle replay commit failed: %w", err)
		}
		return operation, nil
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle idempotency lookup failed: %w", idempotencyErr)
	}
	var observed, desired string
	var generation, revision int64
	if err := tx.QueryRow(ctx, `
		SELECT state,desired_state,generation,revision FROM secondbox.sandboxes
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID,
	).Scan(&observed, &desired, &generation, &revision); err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if revision != input.ExpectedRevision {
		return contracts.Operation{}, ports.ErrRevisionConflict
	}
	if observed == contracts.SandboxStateDeleted || desired == contracts.SandboxDesiredStateDeleted {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	kind := input.Operation.Kind
	if kind == "checkpoint" && observed == contracts.SandboxStateStopped {
		return contracts.Operation{}, ports.ErrLifecycleUnavailable
	}
	databaseSatisfied := observed == contracts.SandboxStateStopped && (kind == "drain" || kind == "stop")
	nextState := observed
	switch input.DesiredState {
	case contracts.SandboxDesiredStateRunning,
		contracts.SandboxDesiredStateStopped,
		contracts.SandboxDesiredStateDeleted:
	default:
		return contracts.Operation{}, errors.New("SecondBox lifecycle desired state is invalid")
	}
	requestMetadataJSON, err := json.Marshal(input.Operation.RequestMetadata)
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle request metadata encoding failed: %w", err)
	}
	terminationReason := ""
	if kind == "drain" {
		terminationReason = contracts.TerminationReasonRequestedDrain
	} else if kind == "stop" || kind == "checkpoint" || kind == "delete" {
		terminationReason = contracts.TerminationReasonRequestedStop
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET desired_state=$4,state=$5,next_reconcile_at=$6,lifecycle_failure_class='',
		    lifecycle_failure_message='',lifecycle_intent_kind=$7,
		    lifecycle_action='intent_received',
		    lifecycle_request_metadata_json=$8,lifecycle_termination_reason=$9,
		    revision=revision+1,updated_at=$6
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID,
		input.DesiredState, nextState, input.Now.UTC(), kind, requestMetadataJSON, terminationReason,
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle intent update failed: %w", err)
	}
	input.Operation.SandboxID = input.SandboxID
	if databaseSatisfied {
		input.Operation.State = contracts.OperationStateSucceeded
		completedAt := input.Now.UTC()
		input.Operation.StartedAt = &completedAt
		input.Operation.CompletedAt = &completedAt
	} else {
		input.Operation.State = contracts.OperationStatePending
	}
	if err := insertOperation(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.Operation,
	); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,response_resource_id,
			created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		input.Principal.TenantRef, input.Principal.SubjectRef,
		"sandbox."+kind, input.SandboxID, input.IdempotencyKey,
		input.RequestHash, input.Operation.ID, input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle idempotency insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle intent commit failed: %w", err)
	}
	return input.Operation, nil
}

// ClaimLifecycle claims one due desired-state record with durable revision fencing.
func (store *PostgresControlPlaneStore) ClaimLifecycle(
	ctx context.Context,
	workerID string,
	now time.Time,
	claimDuration time.Duration,
) (ports.LifecycleReconcileClaim, bool, error) {
	if workerID == "" || claimDuration <= 0 {
		return ports.LifecycleReconcileClaim{}, false,
			errors.New("SecondBox lifecycle claim worker and duration are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ports.LifecycleReconcileClaim{}, false, fmt.Errorf("SecondBox lifecycle claim transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.leases
		SET state='expired',revision=revision+1,updated_at=$1
		WHERE state='active' AND expires_at<=$1`,
		now.UTC(),
	); err != nil {
		return ports.LifecycleReconcileClaim{}, false,
			fmt.Errorf("SecondBox lifecycle expired Lease cleanup failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions AS session
		SET state='closed',closed_at=$1,updated_at=$1
		FROM secondbox.leases AS lease
		WHERE session.state='active' AND session.lease_id=lease.id
		  AND lease.state<>'active'`,
		now.UTC(),
	); err != nil {
		return ports.LifecycleReconcileClaim{}, false,
			fmt.Errorf("SecondBox lifecycle inactive Lease session cleanup failed: %w", err)
	}
	var (
		claim                                   ports.LifecycleReconcileClaim
		specJSON                                []byte
		intentKind                              sql.NullString
		readyAt, lastActivityAt, drainStartedAt sql.NullTime
	)
	err = tx.QueryRow(ctx, `
		SELECT sandbox.id,sandbox.state,sandbox.desired_state,sandbox.revision,
		       revision.spec_json,sandbox.lifecycle_intent_kind,
		       COALESCE(sandbox.lifecycle_termination_reason,''),
		       instance.ready_at,sandbox.last_activity_at,sandbox.drain_started_at,
		       instance.id IS NOT NULL,
		       COALESCE(instance.guest_liveness,''),
		       COALESCE(instance.termination_reason,''),
		       COALESCE(materialization.state,''),
		       COALESCE(
		         checkpoint.state,
		         CASE WHEN checkpoint_effect.state='runner_failed' THEN 'integrity_failed' ELSE '' END
		       ),
		       COALESCE(stop_effect.state,''),
		       (
		         SELECT count(*) FROM secondbox.activity_sessions AS session
		         WHERE session.sandbox_id=sandbox.id AND session.generation=sandbox.generation
		           AND session.tenant_ref=sandbox.tenant_ref
		           AND session.subject_ref=sandbox.subject_ref
		           AND session.state='active'
		       )
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		LEFT JOIN secondbox.instances AS instance ON instance.id=sandbox.current_instance_id
		LEFT JOIN secondbox.workspace_materializations AS materialization
		  ON materialization.workspace_id=sandbox.workspace_id
		  AND materialization.generation=sandbox.generation
		  AND materialization.state IN ('preparing','ready')
		LEFT JOIN LATERAL (
		  SELECT effect.checkpoint_id,effect.state
		  FROM secondbox.lifecycle_effects AS effect
		  WHERE effect.sandbox_id=sandbox.id AND effect.generation=sandbox.generation
		    AND effect.kind='checkpoint'
		  ORDER BY effect.created_at DESC,effect.id DESC LIMIT 1
		) AS checkpoint_effect ON true
		LEFT JOIN secondbox.workspace_checkpoints AS checkpoint
		  ON checkpoint.id=checkpoint_effect.checkpoint_id
		LEFT JOIN LATERAL (
		  SELECT effect.state
		  FROM secondbox.lifecycle_effects AS effect
		  WHERE effect.sandbox_id=sandbox.id AND effect.generation=sandbox.generation
		    AND effect.kind='stop'
		  ORDER BY effect.created_at DESC,effect.id DESC LIMIT 1
		) AS stop_effect ON true
		WHERE sandbox.state<>'deleted' AND sandbox.next_reconcile_at<=$1
		  AND NOT (
		    sandbox.state IN ('stopped','failed')
		    AND sandbox.desired_state='stopped'
		  )
		  AND (
		    sandbox.reconcile_claim_expires_at IS NULL
		    OR sandbox.reconcile_claim_expires_at<=$1
		    OR sandbox.reconcile_owner=$2
		  )
		ORDER BY sandbox.next_reconcile_at,sandbox.id
		FOR UPDATE OF sandbox SKIP LOCKED
		LIMIT 1`,
		now.UTC(), workerID,
	).Scan(
		&claim.SandboxID, &claim.ObservedState, &claim.DesiredState, &claim.Revision,
		&specJSON, &intentKind, &claim.IntentTerminationReason,
		&readyAt, &lastActivityAt, &drainStartedAt,
		&claim.HasInstance,
		&claim.GuestLiveness, &claim.InstanceTerminationReason,
		&claim.MaterializationState, &claim.CheckpointState,
		&claim.StopEffectState, &claim.ActiveSessions,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.LifecycleReconcileClaim{}, false, nil
	}
	if err != nil {
		return ports.LifecycleReconcileClaim{}, false, fmt.Errorf("SecondBox lifecycle claim lookup failed: %w", err)
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return ports.LifecycleReconcileClaim{}, false, fmt.Errorf("SecondBox lifecycle claim policy decoding failed: %w", err)
	}
	claim.WorkerID = workerID
	claim.IntentKind = intentKind.String
	claim.CheckpointOnStop = spec.Checkpoint.OnStop
	claim.ForceCheckpoint = intentKind.String == "checkpoint"
	claim.DrainGraceSeconds = spec.Lifecycle.DrainGraceSeconds
	claim.IdleSeconds = spec.Lifecycle.IdleSeconds
	claim.MaximumDurationSeconds = spec.Lifecycle.MaximumDurationSeconds
	if readyAt.Valid {
		claim.ReadyAt = &readyAt.Time
	}
	if lastActivityAt.Valid {
		claim.LastActivityAt = &lastActivityAt.Time
	}
	if drainStartedAt.Valid {
		claim.DrainStartedAt = &drainStartedAt.Time
	}
	claim.Revision++
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET reconcile_owner=$2,reconcile_claim_expires_at=$3,revision=$4,updated_at=$1
		WHERE id=$5 AND revision=$6`,
		now.UTC(), workerID, now.UTC().Add(claimDuration), claim.Revision,
		claim.SandboxID, claim.Revision-1,
	)
	if err != nil {
		return ports.LifecycleReconcileClaim{}, false, fmt.Errorf("SecondBox lifecycle claim update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.LifecycleReconcileClaim{}, false, ports.ErrRevisionConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return ports.LifecycleReconcileClaim{}, false, fmt.Errorf("SecondBox lifecycle claim commit failed: %w", err)
	}
	return claim, true, nil
}

// ApplyLifecycleAction commits one claimed transition only while owner and revision remain current.
func (store *PostgresControlPlaneStore) ApplyLifecycleAction(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	action string,
	terminationReason string,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	nextState := claim.ObservedState
	deletedAt := (*time.Time)(nil)
	switch action {
	case "wait":
	case "finish_create_stopped", "finish_stop":
		nextState = contracts.SandboxStateStopped
	case "materialize", "start_instance":
		nextState = contracts.SandboxStateStarting
	case "mark_ready":
		nextState = contracts.SandboxStateReady
	case "drain":
		nextState = contracts.SandboxStateDraining
	case "checkpoint":
		nextState = contracts.SandboxStateCheckpointing
	case "stop_instance":
		nextState = contracts.SandboxStateStopping
	case "delete":
		nextState = contracts.SandboxStateDeleting
	case "finish_delete":
		nextState = contracts.SandboxStateDeleted
		value := now.UTC()
		deletedAt = &value
	case "fail":
		nextState = contracts.SandboxStateFailed
	default:
		return errors.New("SecondBox lifecycle reconciliation action is invalid")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle action transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var finishStopWorkspaceID string
	var finishStopGeneration int64
	var finishStopTerminationReason string
	if action == "finish_stop" {
		err := tx.QueryRow(ctx, `
			SELECT sandbox.workspace_id,sandbox.generation,
			       COALESCE(sandbox.lifecycle_termination_reason,'')
			FROM secondbox.sandboxes AS sandbox
			WHERE sandbox.id=$1 AND sandbox.reconcile_owner=$2 AND sandbox.revision=$3
			FOR UPDATE OF sandbox`,
			claim.SandboxID, claim.WorkerID, claim.Revision,
		).Scan(
			&finishStopWorkspaceID, &finishStopGeneration,
			&finishStopTerminationReason,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.ErrRevisionConflict
		}
		if err != nil {
			return fmt.Errorf("SecondBox finish-stop generation lookup failed: %w", err)
		}
		var activeMaterializations int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*)
			FROM secondbox.workspace_materializations
			WHERE workspace_id=$1 AND generation=$2
			  AND state IN ('preparing','ready')`,
			finishStopWorkspaceID, finishStopGeneration,
		).Scan(&activeMaterializations); err != nil {
			return fmt.Errorf("SecondBox finish-stop materialization lookup failed: %w", err)
		}
		if activeMaterializations != 0 {
			return ports.ErrMaterializationConflict
		}
		if terminationReason == "" {
			terminationReason = finishStopTerminationReason
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state=$1,lifecycle_action=CASE WHEN $2='wait' THEN lifecycle_action ELSE $2 END,
		    lifecycle_termination_reason=CASE WHEN $3='' THEN lifecycle_termination_reason ELSE $3 END,
		    lifecycle_failure_class=CASE WHEN $2='fail' THEN 'internal' ELSE lifecycle_failure_class END,
		    lifecycle_failure_message=CASE WHEN $2='fail' THEN 'unrecognized lifecycle state' ELSE lifecycle_failure_message END,
		    next_reconcile_at=CASE WHEN $2='fail' THEN NULL::timestamptz ELSE $4::timestamptz END,
		    reconcile_owner='',reconcile_claim_expires_at=NULL,
		    drain_started_at=CASE
		      WHEN $2='drain' THEN COALESCE(drain_started_at,$6)
		      WHEN $1<>'draining' THEN NULL
		      ELSE drain_started_at
		    END,
		    current_instance_id=CASE WHEN $2 IN ('finish_stop','finish_delete') THEN '' ELSE current_instance_id END,
		    generation=CASE WHEN $2='finish_stop' THEN generation+1 ELSE generation END,
		    last_activity_at=CASE WHEN $2='mark_ready' THEN COALESCE(last_activity_at,$6) ELSE last_activity_at END,
		    deleted_at=COALESCE($5,deleted_at),revision=revision+1,updated_at=$6
		WHERE id=$7 AND reconcile_owner=$8 AND revision=$9`,
		nextState, action, terminationReason, nextReconcileAt.UTC(), deletedAt,
		now.UTC(), claim.SandboxID, claim.WorkerID, claim.Revision,
	)
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle action update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrRevisionConflict
	}
	if action == "finish_stop" {
		nextGeneration := finishStopGeneration + 1
		workspaceTag, err := tx.Exec(ctx, `
			UPDATE secondbox.workspaces
			SET generation=$2,updated_at=$3
			WHERE id=$1 AND generation=$4`,
			finishStopWorkspaceID, nextGeneration, now.UTC(), finishStopGeneration,
		)
		if err != nil {
			return fmt.Errorf("SecondBox finish-stop Workspace generation advance failed: %w", err)
		}
		if workspaceTag.RowsAffected() != 1 {
			return ports.ErrGenerationFenced
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.leases
			SET state='fenced',revision=revision+1,updated_at=$3
			WHERE sandbox_id=$1 AND generation=$2 AND state='active'`,
			claim.SandboxID, finishStopGeneration, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox finish-stop Lease fence failed: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.activity_sessions
			SET state='closed',closed_at=$3,updated_at=$3
			WHERE sandbox_id=$1 AND generation=$2 AND state='active'`,
			claim.SandboxID, finishStopGeneration, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox finish-stop activity session fence failed: %w", err)
		}
	}
	if terminationReason != "" {
		instanceGeneration := int64(0)
		if action == "finish_stop" {
			instanceGeneration = finishStopGeneration
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.instances
			SET termination_reason=CASE WHEN termination_reason='' THEN $2 ELSE termination_reason END,
			    updated_at=$3
			WHERE sandbox_id=$1
			  AND generation=CASE
			    WHEN $4=0 THEN (SELECT generation FROM secondbox.sandboxes WHERE id=$1)
			    ELSE $4
			  END`,
			claim.SandboxID, terminationReason, now.UTC(), instanceGeneration,
		); err != nil {
			return fmt.Errorf("SecondBox stable Instance termination reason update failed: %w", err)
		}
	}
	completeOperations := func(
		kinds []string,
		state string,
		errorCode string,
		errorMessage string,
	) error {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.operations
			SET state=$3,error_code=$4,error_message=$5,retryable=false,
			    started_at=COALESCE(started_at,$6),completed_at=$6,updated_at=$6
			WHERE sandbox_id=$1 AND kind=ANY($2::text[])
			  AND state IN ('pending','running')`,
			claim.SandboxID, kinds, state, errorCode, errorMessage, now.UTC(),
		); err != nil {
			return fmt.Errorf("SecondBox lifecycle Operation completion failed: %w", err)
		}
		return nil
	}
	switch action {
	case "finish_create_stopped":
		if err := completeOperations(
			[]string{"create"}, contracts.OperationStateSucceeded, "", "",
		); err != nil {
			return err
		}
	case "mark_ready":
		if err := completeOperations(
			[]string{"create", "start"}, contracts.OperationStateSucceeded, "", "",
		); err != nil {
			return err
		}
	case "finish_stop":
		if err := completeOperations(
			[]string{"drain", "stop", "checkpoint"}, contracts.OperationStateSucceeded, "", "",
		); err != nil {
			return err
		}
		if claim.DesiredState != contracts.SandboxDesiredStateRunning {
			if err := completeOperations(
				[]string{"create", "start"}, contracts.OperationStateFailed,
				"state_conflict", "Sandbox stopped before the requested running state was reached",
			); err != nil {
				return err
			}
		}
	case "finish_delete":
		if err := completeOperations(
			[]string{"delete"}, contracts.OperationStateSucceeded, "", "",
		); err != nil {
			return err
		}
		if err := completeOperations(
			[]string{"create", "start", "drain", "stop", "checkpoint"},
			contracts.OperationStateFailed, "state_conflict",
			"Sandbox deletion superseded the lifecycle operation",
		); err != nil {
			return err
		}
	case "fail":
		if err := completeOperations(
			[]string{"create", "start", "drain", "stop", "checkpoint", "delete"},
			contracts.OperationStateFailed, "internal_error", "Lifecycle reconciliation failed",
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox lifecycle action commit failed: %w", err)
	}
	return nil
}

// AcquireLease creates bounded authority for the current generation.
