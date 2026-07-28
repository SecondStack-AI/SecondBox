package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// GetSandboxLifecyclePolicy reads the immutable ProfileRevision pinned by the Sandbox.
func (store *PostgresControlPlaneStore) GetSandboxLifecyclePolicy(
	ctx context.Context,
	projectID string,
	sandboxID string,
) (contracts.LifecyclePolicy, contracts.CheckpointPolicy, error) {
	var specJSON []byte
	if err := store.pool.QueryRow(ctx, `
		SELECT revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.project_id=$1 AND sandbox.id=$2`,
		projectID, sandboxID,
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
	lockKey := input.Principal.ProjectID + "\x1flifecycle\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle idempotency lock failed: %w", err)
	}
	var priorHash, priorOperationID string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id FROM secondbox.idempotency_records
		WHERE project_id=$1 AND operation=$2 AND target_id=$3 AND idempotency_key=$4`,
		input.Principal.ProjectID, "sandbox."+input.Operation.Kind, input.SandboxID, input.IdempotencyKey,
	).Scan(&priorHash, &priorOperationID)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return contracts.Operation{}, ports.ErrIdempotencyConflict
		}
		operation, err := getOperationWithQuerier(ctx, tx, input.Principal.ProjectID, `id=$2`, priorOperationID)
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
		WHERE project_id=$1 AND id=$2 FOR UPDATE`,
		input.Principal.ProjectID, input.SandboxID,
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
		SET desired_state=$3,state=$4,next_reconcile_at=$5,lifecycle_failure_class='',
		    lifecycle_failure_message='',lifecycle_intent_kind=$6,
		    lifecycle_action='intent_received',
		    lifecycle_request_metadata_json=$7,lifecycle_termination_reason=$8,
		    revision=revision+1,updated_at=$5
		WHERE project_id=$1 AND id=$2`,
		input.Principal.ProjectID, input.SandboxID, input.DesiredState, nextState, input.Now.UTC(),
		kind, requestMetadataJSON, terminationReason,
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
	if err := insertOperation(ctx, tx, input.Principal.ProjectID, input.Operation); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			project_id,operation,target_id,idempotency_key,request_hash,response_resource_id,
			created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		input.Principal.ProjectID, "sandbox."+kind, input.SandboxID, input.IdempotencyKey,
		input.RequestHash, input.Operation.ID, input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle idempotency insert failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, input.Audit); err != nil {
		return contracts.Operation{}, err
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
func (store *PostgresControlPlaneStore) AcquireLease(
	ctx context.Context,
	input ports.LeaseInput,
) (contracts.Lease, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease acquire transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.ProjectID + "\x1flease.acquire\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease acquire idempotency lock failed: %w", err)
	}
	replayedLease, replayed, err := lookupLeaseIdempotency(
		ctx, tx, input.ProjectID, "lease.acquire", input.SandboxID,
		input.IdempotencyKey, input.RequestHash,
	)
	if err != nil {
		return contracts.Lease{}, err
	}
	if replayed {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Lease{}, fmt.Errorf("SecondBox Lease acquire replay commit failed: %w", err)
		}
		return replayedLease, nil
	}
	if err := lockCurrentGeneration(ctx, tx, input.ProjectID, input.SandboxID, input.Generation, false); err != nil {
		return contracts.Lease{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.leases SET state='expired',revision=revision+1,updated_at=$3
		WHERE project_id=$1 AND sandbox_id=$2 AND state='active' AND expires_at<=$3`,
		input.ProjectID, input.SandboxID, input.Now.UTC(),
	); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox expired Lease update failed: %w", err)
	}
	lease := input.Lease
	lease.ProjectID, lease.SandboxID, lease.Generation = input.ProjectID, input.SandboxID, input.Generation
	lease.ServiceAccountID, lease.State = input.ServiceAccountID, contracts.LeaseStateActive
	lease.ExpiresAt, lease.Revision = input.ExpiresAt.UTC(), 1
	lease.CreatedAt, lease.UpdatedAt = input.Now.UTC(), input.Now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.leases (
			id,project_id,sandbox_id,generation,service_account_id,state,expires_at,
			revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		lease.ID, lease.ProjectID, lease.SandboxID, lease.Generation, lease.ServiceAccountID,
		lease.State, lease.ExpiresAt, lease.Revision, lease.CreatedAt, lease.UpdatedAt,
	); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease insert failed: %w", err)
	}
	if err := insertLeaseIdempotency(
		ctx, tx, input, "lease.acquire", input.SandboxID, lease.ID,
	); err != nil {
		return contracts.Lease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease acquire commit failed: %w", err)
	}
	return lease, nil
}

// GetLease reads one Project-scoped Lease.
func (store *PostgresControlPlaneStore) GetLease(
	ctx context.Context,
	projectID string,
	sandboxID string,
	leaseID string,
) (contracts.Lease, error) {
	return scanLease(store.pool.QueryRow(ctx, `
		SELECT id,project_id,sandbox_id,generation,service_account_id,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases WHERE project_id=$1 AND sandbox_id=$2 AND id=$3`,
		projectID, sandboxID, leaseID,
	))
}

// GetLeaseByID reads one Lease without accepting a caller-supplied Sandbox scope.
func (store *PostgresControlPlaneStore) GetLeaseByID(
	ctx context.Context,
	projectID string,
	leaseID string,
) (contracts.Lease, error) {
	return scanLease(store.pool.QueryRow(ctx, `
		SELECT id,project_id,sandbox_id,generation,service_account_id,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases WHERE project_id=$1 AND id=$2`,
		projectID, leaseID,
	))
}

// RenewLease extends only active, unexpired current-generation authority.
func (store *PostgresControlPlaneStore) RenewLease(
	ctx context.Context,
	input ports.LeaseInput,
) (contracts.Lease, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease renew transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.ProjectID + "\x1flease.renew\x1f" + input.Lease.ID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease renew idempotency lock failed: %w", err)
	}
	replayedLease, replayed, err := lookupLeaseIdempotency(
		ctx, tx, input.ProjectID, "lease.renew", input.Lease.ID,
		input.IdempotencyKey, input.RequestHash,
	)
	if err != nil {
		return contracts.Lease{}, err
	}
	if replayed {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Lease{}, fmt.Errorf("SecondBox Lease renew replay commit failed: %w", err)
		}
		return replayedLease, nil
	}
	if err := lockCurrentGeneration(ctx, tx, input.ProjectID, input.SandboxID, input.Generation, false); err != nil {
		return contracts.Lease{}, err
	}
	lease, err := scanLease(tx.QueryRow(ctx, `
		SELECT id,project_id,sandbox_id,generation,service_account_id,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases WHERE project_id=$1 AND sandbox_id=$2 AND id=$3 FOR UPDATE`,
		input.ProjectID, input.SandboxID, input.Lease.ID,
	))
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.Generation != input.Generation || lease.ServiceAccountID != input.ServiceAccountID ||
		lease.State != contracts.LeaseStateActive || !input.Now.Before(lease.ExpiresAt) {
		return contracts.Lease{}, ports.ErrLeaseInactive
	}
	lease.ExpiresAt, lease.UpdatedAt, lease.Revision = input.ExpiresAt.UTC(), input.Now.UTC(), lease.Revision+1
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.leases SET expires_at=$2,revision=$3,updated_at=$4 WHERE id=$1`,
		lease.ID, lease.ExpiresAt, lease.Revision, lease.UpdatedAt,
	); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease renew update failed: %w", err)
	}
	if err := insertLeaseIdempotency(
		ctx, tx, input, "lease.renew", lease.ID, lease.ID,
	); err != nil {
		return contracts.Lease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease renew commit failed: %w", err)
	}
	return lease, nil
}

// ReleaseLease revokes activity authority without changing Sandbox desired state.
func (store *PostgresControlPlaneStore) ReleaseLease(
	ctx context.Context,
	input ports.LeaseInput,
) (contracts.Lease, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease release transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.ProjectID + "\x1flease.release\x1f" + input.Lease.ID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease release idempotency lock failed: %w", err)
	}
	replayedLease, replayed, err := lookupLeaseIdempotency(
		ctx, tx, input.ProjectID, "lease.release", input.Lease.ID,
		input.IdempotencyKey, input.RequestHash,
	)
	if err != nil {
		return contracts.Lease{}, err
	}
	if replayed {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Lease{}, fmt.Errorf("SecondBox Lease release replay commit failed: %w", err)
		}
		return replayedLease, nil
	}
	lease, err := scanLease(tx.QueryRow(ctx, `
		SELECT id,project_id,sandbox_id,generation,service_account_id,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases WHERE project_id=$1 AND sandbox_id=$2 AND id=$3 FOR UPDATE`,
		input.ProjectID, input.SandboxID, input.Lease.ID,
	))
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.Generation != input.Generation || lease.ServiceAccountID != input.ServiceAccountID {
		return contracts.Lease{}, ports.ErrGenerationFenced
	}
	if lease.State == contracts.LeaseStateActive {
		lease.State, lease.UpdatedAt, lease.Revision = contracts.LeaseStateReleased, input.Now.UTC(), lease.Revision+1
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.leases SET state=$2,revision=$3,updated_at=$4 WHERE id=$1`,
			lease.ID, lease.State, lease.Revision, lease.UpdatedAt,
		); err != nil {
			return contracts.Lease{}, fmt.Errorf("SecondBox Lease release update failed: %w", err)
		}
	}
	if err := insertLeaseIdempotency(
		ctx, tx, input, "lease.release", lease.ID, lease.ID,
	); err != nil {
		return contracts.Lease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease release commit failed: %w", err)
	}
	return lease, nil
}

// PingGuest records liveness while deliberately leaving useful activity unchanged.
func (store *PostgresControlPlaneStore) PingGuest(
	ctx context.Context,
	input ports.GenerationInput,
	liveness string,
) (contracts.Instance, error) {
	if liveness != contracts.GuestLivenessStarting && liveness != contracts.GuestLivenessReady &&
		liveness != contracts.GuestLivenessLost && liveness != contracts.GuestLivenessStopped {
		return contracts.Instance{}, errors.New("SecondBox guest liveness value is invalid")
	}
	var instance contracts.Instance
	err := store.pool.QueryRow(ctx, `
		UPDATE secondbox.instances AS instance
		SET guest_liveness=$4,guest_heartbeat_at=$5,updated_at=$5
		FROM secondbox.sandboxes AS sandbox
		WHERE sandbox.project_id=$1 AND sandbox.id=$2 AND sandbox.generation=$3
		  AND instance.id=sandbox.current_instance_id AND instance.generation=$3
		RETURNING instance.id,instance.sandbox_id,instance.generation,instance.state,
		          instance.guest_liveness,instance.termination_reason,instance.created_at,
		          instance.updated_at,instance.ready_at,instance.guest_heartbeat_at,instance.stopped_at`,
		input.ProjectID, input.SandboxID, input.Generation, liveness, input.Now.UTC(),
	).Scan(
		&instance.ID, &instance.SandboxID, &instance.Generation, &instance.State,
		&instance.GuestLiveness, &instance.TerminationReason, &instance.CreatedAt,
		&instance.UpdatedAt, &instance.ReadyAt, &instance.GuestHeartbeatAt, &instance.StoppedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Instance{}, ports.ErrGenerationFenced
	}
	if err != nil {
		return contracts.Instance{}, fmt.Errorf("SecondBox guest ping update failed: %w", err)
	}
	return instance, nil
}

// ReadSandboxInspection projects current persisted guest and useful-session evidence.
func (store *PostgresControlPlaneStore) ReadSandboxInspection(
	ctx context.Context,
	input ports.GenerationInput,
) (contracts.SandboxInspection, error) {
	var inspection contracts.SandboxInspection
	var guestLiveness string
	var guestHeartbeatAt sql.NullTime
	err := store.pool.QueryRow(ctx, `
		SELECT sandbox.id,sandbox.generation,COALESCE(instance.guest_liveness,''),
		       instance.guest_heartbeat_at,
		       (
		         SELECT count(*) FROM secondbox.activity_sessions AS session
		         WHERE session.sandbox_id=sandbox.id AND session.generation=sandbox.generation
		           AND session.state='active'
		       )
		FROM secondbox.sandboxes AS sandbox
		LEFT JOIN secondbox.instances AS instance ON instance.id=sandbox.current_instance_id
		WHERE sandbox.project_id=$1 AND sandbox.id=$2`,
		input.ProjectID, input.SandboxID,
	).Scan(
		&inspection.SandboxID, &inspection.Generation, &guestLiveness,
		&guestHeartbeatAt, &inspection.ActiveSessions,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.SandboxInspection{}, ports.ErrSandboxNotFound
	}
	if err != nil {
		return contracts.SandboxInspection{}, fmt.Errorf("SecondBox Sandbox inspection failed: %w", err)
	}
	if inspection.Generation != input.Generation {
		return contracts.SandboxInspection{}, ports.ErrGenerationFenced
	}
	if !guestHeartbeatAt.Valid {
		return contracts.SandboxInspection{}, ports.ErrLifecycleUnavailable
	}
	inspection.GuestHealthy = guestLiveness == contracts.GuestLivenessReady
	inspection.ObservedAt = guestHeartbeatAt.Time.UTC()
	return inspection, nil
}

// TouchActivity records explicit useful activity for the current generation.
func (store *PostgresControlPlaneStore) TouchActivity(
	ctx context.Context,
	input ports.ActivityInput,
) (time.Time, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("SecondBox touch transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.ProjectID + "\x1ftouch\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return time.Time{}, fmt.Errorf("SecondBox touch idempotency lock failed: %w", err)
	}
	var priorHash string
	var priorActivityAt time.Time
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,last_activity_at FROM secondbox.activity_touches
		WHERE project_id=$1 AND sandbox_id=$2 AND idempotency_key=$3`,
		input.ProjectID, input.SandboxID, input.IdempotencyKey,
	).Scan(&priorHash, &priorActivityAt)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return time.Time{}, ports.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return time.Time{}, fmt.Errorf("SecondBox touch replay commit failed: %w", err)
		}
		return priorActivityAt, nil
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("SecondBox touch idempotency lookup failed: %w", idempotencyErr)
	}
	if err := validateActivityAuthority(ctx, tx, input); err != nil {
		return time.Time{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$4,revision=revision+1,updated_at=$4
		WHERE project_id=$1 AND id=$2 AND generation=$3`,
		input.ProjectID, input.SandboxID, input.Generation, input.Now.UTC(),
	); err != nil {
		return time.Time{}, fmt.Errorf("SecondBox useful activity update failed: %w", err)
	}
	if input.Session.ID != "" {
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.activity_sessions SET last_activity_at=$5,updated_at=$5
			WHERE project_id=$1 AND sandbox_id=$2 AND generation=$3 AND id=$4 AND state='active'`,
			input.ProjectID, input.SandboxID, input.Generation, input.Session.ID, input.Now.UTC(),
		)
		if err != nil {
			return time.Time{}, fmt.Errorf("SecondBox activity session touch failed: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return time.Time{}, ports.ErrActivitySessionNotFound
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.activity_touches (
			project_id,sandbox_id,generation,service_account_id,lease_id,
			idempotency_key,request_hash,last_activity_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)`,
		input.ProjectID, input.SandboxID, input.Generation, input.ServiceAccountID,
		input.LeaseID, input.IdempotencyKey, input.RequestHash, input.Now.UTC(),
	); err != nil {
		return time.Time{}, fmt.Errorf("SecondBox touch idempotency insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, fmt.Errorf("SecondBox touch commit failed: %w", err)
	}
	return input.Now.UTC(), nil
}

// OpenActivitySession admits one useful generation-bound session.
func (store *PostgresControlPlaneStore) OpenActivitySession(
	ctx context.Context,
	input ports.ActivityInput,
) (contracts.ActivitySession, error) {
	if !validActivityKind(input.Session.Kind) {
		return contracts.ActivitySession{}, errors.New("SecondBox activity session kind is invalid")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ActivitySession{}, fmt.Errorf("SecondBox activity session transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := validateActivityAuthority(ctx, tx, input); err != nil {
		return contracts.ActivitySession{}, err
	}
	session := input.Session
	session.ProjectID, session.SandboxID, session.Generation = input.ProjectID, input.SandboxID, input.Generation
	session.State, session.LeaseID = contracts.ActivitySessionStateActive, input.LeaseID
	session.CreatedAt, session.LastActivityAt = input.Now.UTC(), input.Now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.activity_sessions (
			id,project_id,sandbox_id,generation,kind,state,lease_id,last_activity_at,
			created_at,updated_at,closed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,NULL)`,
		session.ID, session.ProjectID, session.SandboxID, session.Generation, session.Kind,
		session.State, session.LeaseID, session.LastActivityAt, session.CreatedAt,
	); err != nil {
		return contracts.ActivitySession{}, fmt.Errorf("SecondBox activity session insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$4,revision=revision+1,updated_at=$4
		WHERE project_id=$1 AND id=$2 AND generation=$3`,
		input.ProjectID, input.SandboxID, input.Generation, input.Now.UTC(),
	); err != nil {
		return contracts.ActivitySession{}, fmt.Errorf("SecondBox session activity update failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ActivitySession{}, fmt.Errorf("SecondBox activity session commit failed: %w", err)
	}
	return session, nil
}

// CloseActivitySession removes idle suppression but does not alter desired state.
func (store *PostgresControlPlaneStore) CloseActivitySession(
	ctx context.Context,
	input ports.ActivityInput,
) (contracts.ActivitySession, error) {
	var session contracts.ActivitySession
	err := store.pool.QueryRow(ctx, `
		UPDATE secondbox.activity_sessions
		SET state='closed',closed_at=$5,updated_at=$5
		WHERE project_id=$1 AND sandbox_id=$2 AND generation=$3 AND id=$4 AND state='active'
		RETURNING id,project_id,sandbox_id,generation,kind,state,lease_id,last_activity_at,created_at,closed_at`,
		input.ProjectID, input.SandboxID, input.Generation, input.Session.ID, input.Now.UTC(),
	).Scan(
		&session.ID, &session.ProjectID, &session.SandboxID, &session.Generation,
		&session.Kind, &session.State, &session.LeaseID, &session.LastActivityAt,
		&session.CreatedAt, &session.ClosedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.ActivitySession{}, ports.ErrActivitySessionNotFound
	}
	if err != nil {
		return contracts.ActivitySession{}, fmt.Errorf("SecondBox activity session close failed: %w", err)
	}
	return session, nil
}

// AcquireMaterialization creates exclusive generation-bound runner-local writer evidence.
func (store *PostgresControlPlaneStore) AcquireMaterialization(
	ctx context.Context,
	input ports.MaterializationInput,
) (contracts.WorkspaceMaterialization, error) {
	materialization := input.Materialization
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var workspaceGeneration int64
	var sandboxID, sandboxState, currentCheckpointID string
	if err := tx.QueryRow(ctx, `
		SELECT workspace.generation,workspace.sandbox_id,sandbox.state,workspace.current_checkpoint_id
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=workspace.sandbox_id
		WHERE workspace.id=$1
		FOR UPDATE OF workspace,sandbox`,
		materialization.WorkspaceID,
	).Scan(&workspaceGeneration, &sandboxID, &sandboxState, &currentCheckpointID); err != nil {
		return contracts.WorkspaceMaterialization{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if workspaceGeneration != input.ExpectedWorkspaceGeneration ||
		materialization.Generation != input.ExpectedWorkspaceGeneration {
		return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
	}
	if materialization.SourceCheckpointID != "" {
		if sandboxState != contracts.SandboxStateStopped ||
			currentCheckpointID != materialization.SourceCheckpointID {
			return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
		}
		var checkpointState string
		if err := tx.QueryRow(ctx, `
			SELECT state FROM secondbox.workspace_checkpoints
			WHERE id=$1 AND workspace_id=$2 FOR UPDATE`,
			materialization.SourceCheckpointID, materialization.WorkspaceID,
		).Scan(&checkpointState); err != nil {
			return contracts.WorkspaceMaterialization{}, mapNotFound(err, ports.ErrCheckpointNotFound)
		}
		if checkpointState != contracts.ObjectStatePublished {
			return contracts.WorkspaceMaterialization{}, ports.ErrCheckpointIntegrity
		}
	}
	releaseProofJSON, err := json.Marshal(map[string]string{})
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization proof encoding failed: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO secondbox.workspace_materializations (
			id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
			source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at,released_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,1,$10,$10,NULL)`,
		materialization.ID, materialization.WorkspaceID, sandboxID, materialization.AssignmentID,
		materialization.RunnerID, materialization.Generation, materialization.SourceCheckpointID,
		contracts.MaterializationStatePreparing, releaseProofJSON, materialization.CreatedAt.UTC(),
	)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return contracts.WorkspaceMaterialization{}, ports.ErrMaterializationConflict
		}
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization commit failed: %w", err)
	}
	materialization.State, materialization.Revision = contracts.MaterializationStatePreparing, 1
	materialization.UpdatedAt = materialization.CreatedAt
	return materialization, nil
}

// ConfirmMaterialization records that the assigned Runner verified its generation-bound active image.
func (store *PostgresControlPlaneStore) ConfirmMaterialization(
	ctx context.Context,
	input ports.MaterializationInput,
	now time.Time,
) (contracts.WorkspaceMaterialization, error) {
	var materialization contracts.WorkspaceMaterialization
	var releaseProofJSON []byte
	err := store.pool.QueryRow(ctx, `
		UPDATE secondbox.workspace_materializations
		SET state='ready',revision=revision+1,updated_at=$6
		WHERE id=$1 AND workspace_id=$2 AND assignment_id=$3 AND runner_id=$4
		  AND generation=$5 AND state='preparing'
		  AND EXISTS (
		    SELECT 1 FROM secondbox.workspaces AS workspace
		    WHERE workspace.id=$2 AND workspace.generation=$5
		  )
		RETURNING id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
		          source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at`,
		input.Materialization.ID, input.Materialization.WorkspaceID,
		input.Materialization.AssignmentID, input.Materialization.RunnerID,
		input.ExpectedWorkspaceGeneration, now.UTC(),
	).Scan(
		&materialization.ID, &materialization.WorkspaceID, &materialization.SandboxID,
		&materialization.AssignmentID, &materialization.RunnerID, &materialization.Generation,
		&materialization.SourceCheckpointID, &materialization.State, &releaseProofJSON,
		&materialization.Revision, &materialization.CreatedAt, &materialization.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
	}
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization confirmation failed: %w", err)
	}
	if err := json.Unmarshal(releaseProofJSON, &materialization.ReleaseProof); err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization proof decoding failed: %w", err)
	}
	return materialization, nil
}

// ReleaseMaterialization records proof that runner-local writer authority ended.
func (store *PostgresControlPlaneStore) ReleaseMaterialization(
	ctx context.Context,
	input ports.MaterializationInput,
	releaseProof map[string]string,
	now time.Time,
) (contracts.WorkspaceMaterialization, error) {
	proofJSON, err := json.Marshal(releaseProof)
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization release proof encoding failed: %w", err)
	}
	var materialization contracts.WorkspaceMaterialization
	var storedProofJSON []byte
	err = store.pool.QueryRow(ctx, `
		UPDATE secondbox.workspace_materializations
		SET state='released',release_proof_json=$4,revision=revision+1,updated_at=$5,released_at=$5
		WHERE id=$1 AND workspace_id=$2 AND generation=$3 AND state IN ('preparing','ready')
		RETURNING id,workspace_id,sandbox_id,assignment_id,runner_id,generation,
		          source_checkpoint_id,state,release_proof_json,revision,created_at,updated_at`,
		input.Materialization.ID, input.Materialization.WorkspaceID,
		input.ExpectedWorkspaceGeneration, proofJSON, now.UTC(),
	).Scan(
		&materialization.ID, &materialization.WorkspaceID, &materialization.SandboxID,
		&materialization.AssignmentID, &materialization.RunnerID, &materialization.Generation,
		&materialization.SourceCheckpointID, &materialization.State, &storedProofJSON,
		&materialization.Revision, &materialization.CreatedAt, &materialization.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceMaterialization{}, ports.ErrGenerationFenced
	}
	if err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization release failed: %w", err)
	}
	if err := json.Unmarshal(storedProofJSON, &materialization.ReleaseProof); err != nil {
		return contracts.WorkspaceMaterialization{}, fmt.Errorf("SecondBox materialization release proof decoding failed: %w", err)
	}
	return materialization, nil
}

// StageCheckpoint reserves retained-byte quota before recording unreachable upload metadata.
func (store *PostgresControlPlaneStore) StageCheckpoint(
	ctx context.Context,
	input ports.CheckpointPublicationInput,
) (contracts.WorkspaceCheckpoint, error) {
	checkpoint := input.Checkpoint
	compatibilityJSON, err := json.Marshal(checkpoint.Compatibility)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint compatibility encoding failed: %w", err)
	}
	if checkpoint.SourceGeneration != input.ExpectedWorkspaceGeneration {
		return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var projectID, profileName string
	if err := tx.QueryRow(ctx, `
		SELECT workspace.project_id,sandbox.profile_name
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=workspace.sandbox_id
		WHERE workspace.id=$1 AND workspace.generation=$2 AND sandbox.state<>'deleted'`,
		checkpoint.WorkspaceID, input.ExpectedWorkspaceGeneration,
	).Scan(&projectID, &profileName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
		}
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint authority lookup failed: %w", err)
	}
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		projectID+"\x1fretained-byte-capacity",
	); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint Project capacity lock failed: %w", err)
	}
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		profileName+"\x1fretained-byte-capacity",
	); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint Profile capacity lock failed: %w", err)
	}
	var existingState, existingSHA256, existingStorageKey, existingWorkspaceID string
	var existingSizeBytes, existingGeneration int64
	existingErr := tx.QueryRow(ctx, `
		SELECT state,sha256,size_bytes,source_generation,storage_key,workspace_id
		FROM secondbox.workspace_checkpoints WHERE id=$1`,
		checkpoint.ID,
	).Scan(
		&existingState, &existingSHA256, &existingSizeBytes, &existingGeneration,
		&existingStorageKey, &existingWorkspaceID,
	)
	if existingErr == nil {
		if existingSHA256 != checkpoint.SHA256 || existingSizeBytes != checkpoint.SizeBytes ||
			existingGeneration != checkpoint.SourceGeneration ||
			existingStorageKey != input.StorageKey ||
			existingWorkspaceID != checkpoint.WorkspaceID {
			return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
		}
		switch existingState {
		case contracts.ObjectStateStaging, contracts.ObjectStateVerified,
			contracts.ObjectStatePublished:
			checkpoint.State = existingState
			if err := tx.Commit(ctx); err != nil {
				return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging replay commit failed: %w", err)
			}
			return checkpoint, nil
		case contracts.ObjectStateQuotaFailed:
			return contracts.WorkspaceCheckpoint{}, ports.ErrQuotaExceeded
		default:
			return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
		}
	}
	if !errors.Is(existingErr, pgx.ErrNoRows) {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint replay lookup failed: %w", existingErr)
	}
	projectQuota, err := readQuota(ctx, tx, "project_quotas", "project_id", projectID)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, err
	}
	projectUsage, err := readQuotaUsage(ctx, tx, "sandbox.project_id=$1", projectID)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, err
	}
	profileQuota, err := readQuota(ctx, tx, "profile_quotas", "profile_name", profileName)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, err
	}
	profileUsage, err := readQuotaUsage(ctx, tx, "sandbox.profile_name=$1", profileName)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, err
	}
	state := contracts.ObjectStateStaging
	quotaExceeded := projectUsage.retainedBytes+checkpoint.SizeBytes > projectQuota.MaxRetainedBytes ||
		profileUsage.retainedBytes+checkpoint.SizeBytes > profileQuota.MaxRetainedBytes
	if quotaExceeded {
		state = contracts.ObjectStateQuotaFailed
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO secondbox.workspace_checkpoints (
			id,project_id,sandbox_id,workspace_id,source_generation,state,sha256,size_bytes,
			compatibility_json,storage_key,retain_until,created_at,published_at,
			garbage_collection_marked_at,garbage_collected_at
		)
		SELECT $1,workspace.project_id,workspace.sandbox_id,workspace.id,$2,$3,$4,$5,
		       $6,$7,$8,$9,NULL,NULL,NULL
		FROM secondbox.workspaces AS workspace
		WHERE workspace.id=$10 AND workspace.generation=$11`,
		checkpoint.ID, checkpoint.SourceGeneration, state, checkpoint.SHA256, checkpoint.SizeBytes,
		compatibilityJSON, input.StorageKey, checkpoint.RetainUntil.UTC(), checkpoint.CreatedAt.UTC(),
		checkpoint.WorkspaceID, input.ExpectedWorkspaceGeneration,
	)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint staging commit failed: %w", err)
	}
	if quotaExceeded {
		return contracts.WorkspaceCheckpoint{}, ports.ErrQuotaExceeded
	}
	checkpoint.State = contracts.ObjectStateStaging
	return checkpoint, nil
}

// VerifyCheckpoint records provider and content verification before reachability changes.
func (store *PostgresControlPlaneStore) VerifyCheckpoint(
	ctx context.Context,
	input ports.CheckpointPublicationInput,
	now time.Time,
) (contracts.WorkspaceCheckpoint, error) {
	checkpoint := input.Checkpoint
	compatibilityJSON, err := json.Marshal(checkpoint.Compatibility)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint verification compatibility encoding failed: %w", err)
	}
	var verifiedAt time.Time
	err = store.pool.QueryRow(ctx, `
		UPDATE secondbox.workspace_checkpoints
		SET state='verified',verified_at=$6
		WHERE id=$1 AND workspace_id=$2 AND source_generation=$3 AND state='staging'
		  AND sha256=$4 AND size_bytes=$5 AND storage_key=$7
		  AND compatibility_json=$8::jsonb
		RETURNING verified_at`,
		checkpoint.ID, checkpoint.WorkspaceID, checkpoint.SourceGeneration,
		checkpoint.SHA256, checkpoint.SizeBytes, now.UTC(), input.StorageKey, compatibilityJSON,
	).Scan(&verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var state string
		if err := store.pool.QueryRow(ctx, `
			SELECT state FROM secondbox.workspace_checkpoints
			WHERE id=$1 AND workspace_id=$2 AND source_generation=$3
			  AND sha256=$4 AND size_bytes=$5 AND storage_key=$6
			  AND compatibility_json=$7::jsonb`,
			checkpoint.ID, checkpoint.WorkspaceID, checkpoint.SourceGeneration,
			checkpoint.SHA256, checkpoint.SizeBytes, input.StorageKey, compatibilityJSON,
		).Scan(&state); err != nil ||
			(state != contracts.ObjectStateVerified && state != contracts.ObjectStatePublished) {
			return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
		}
		checkpoint.State = state
		return checkpoint, nil
	}
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint verification failed: %w", err)
	}
	checkpoint.State = contracts.ObjectStateVerified
	return checkpoint, nil
}

// PublishCheckpoint atomically makes verified bytes the current Workspace checkpoint.
func (store *PostgresControlPlaneStore) PublishCheckpoint(
	ctx context.Context,
	input ports.CheckpointPublicationInput,
	now time.Time,
) (contracts.WorkspaceCheckpoint, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publication transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var checkpoint contracts.WorkspaceCheckpoint
	var compatibilityJSON []byte
	var storageKey string
	err = tx.QueryRow(ctx, `
		SELECT id,project_id,sandbox_id,workspace_id,source_generation,state,sha256,size_bytes,
		       compatibility_json,storage_key,retain_until,created_at,published_at,garbage_collected_at
		FROM secondbox.workspace_checkpoints WHERE id=$1 FOR UPDATE`,
		input.Checkpoint.ID,
	).Scan(
		&checkpoint.ID, &checkpoint.ProjectID, &checkpoint.SandboxID, &checkpoint.WorkspaceID,
		&checkpoint.SourceGeneration, &checkpoint.State, &checkpoint.SHA256, &checkpoint.SizeBytes,
		&compatibilityJSON, &storageKey, &checkpoint.RetainUntil, &checkpoint.CreatedAt,
		&checkpoint.PublishedAt, &checkpoint.GarbageCollectedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointNotFound
	}
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publication lookup failed: %w", err)
	}
	if checkpoint.SHA256 != input.Checkpoint.SHA256 ||
		checkpoint.SizeBytes != input.Checkpoint.SizeBytes || storageKey != input.StorageKey {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_checkpoints
			SET state='integrity_failed' WHERE id=$1 AND state IN ('staging','verified')`,
			checkpoint.ID,
		); err != nil {
			return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint integrity failure update failed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint integrity failure commit failed: %w", err)
		}
		return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
	}
	if checkpoint.State == contracts.ObjectStatePublished {
		if err := json.Unmarshal(compatibilityJSON, &checkpoint.Compatibility); err != nil {
			return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox published checkpoint compatibility decoding failed: %w", err)
		}
		return checkpoint, tx.Commit(ctx)
	}
	if checkpoint.State != contracts.ObjectStateVerified {
		return contracts.WorkspaceCheckpoint{}, ports.ErrCheckpointIntegrity
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET current_checkpoint_id=$1,current_checkpoint_sha256=$2,
		    current_checkpoint_size_bytes=$3,retained_bytes=retained_bytes+$3,
		    garbage_collection_state='reachable',updated_at=$4
		WHERE id=$5 AND generation=$6`,
		checkpoint.ID, checkpoint.SHA256, checkpoint.SizeBytes, now.UTC(),
		checkpoint.WorkspaceID, input.ExpectedWorkspaceGeneration,
	)
	if err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox current checkpoint update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.WorkspaceCheckpoint{}, ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_checkpoints SET state='published',published_at=$2 WHERE id=$1`,
		checkpoint.ID, now.UTC(),
	); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publish update failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint publication commit failed: %w", err)
	}
	if err := json.Unmarshal(compatibilityJSON, &checkpoint.Compatibility); err != nil {
		return contracts.WorkspaceCheckpoint{}, fmt.Errorf("SecondBox checkpoint compatibility decoding failed: %w", err)
	}
	checkpoint.State = contracts.ObjectStatePublished
	publishedAt := now.UTC()
	checkpoint.PublishedAt = &publishedAt
	return checkpoint, nil
}

// StageArtifact reserves quota and records unreachable metadata before immutable upload.
func (store *PostgresControlPlaneStore) StageArtifact(
	ctx context.Context,
	input ports.ArtifactPublicationInput,
) (contracts.Artifact, error) {
	artifact := input.Artifact
	metadataJSON, err := json.Marshal(artifact.Metadata)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact metadata encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact staging transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		artifact.ProjectID+"\x1fretained-byte-capacity",
	); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact Project retained-byte capacity lock failed: %w", err)
	}
	var sandboxGeneration int64
	var profileName string
	var specJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT sandbox.generation,sandbox.profile_name,revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.id=$1 AND sandbox.project_id=$2 AND sandbox.state<>'deleted'
		FOR UPDATE OF sandbox`,
		artifact.SandboxID, artifact.ProjectID,
	).Scan(&sandboxGeneration, &profileName, &specJSON); err != nil {
		return contracts.Artifact{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if sandboxGeneration != input.ExpectedGeneration ||
		artifact.SourceGeneration != input.ExpectedGeneration {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		profileName+"\x1fretained-byte-capacity",
	); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact Profile retained-byte capacity lock failed: %w", err)
	}
	if input.LeaseID != "" {
		var leaseGeneration int64
		var leaseAccount, leaseState string
		var leaseExpiry time.Time
		if err := tx.QueryRow(ctx, `
			SELECT generation,service_account_id,state,expires_at
			FROM secondbox.leases
			WHERE project_id=$1 AND sandbox_id=$2 AND id=$3 FOR UPDATE`,
			artifact.ProjectID, artifact.SandboxID, input.LeaseID,
		).Scan(&leaseGeneration, &leaseAccount, &leaseState, &leaseExpiry); err != nil {
			return contracts.Artifact{}, mapNotFound(err, ports.ErrLeaseNotFound)
		}
		if leaseGeneration != input.ExpectedGeneration ||
			leaseAccount != input.ServiceAccountID ||
			leaseState != contracts.LeaseStateActive ||
			!artifact.CreatedAt.Before(leaseExpiry) {
			return contracts.Artifact{}, ports.ErrLeaseInactive
		}
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact ProfileRevision decoding failed: %w", err)
	}
	if artifact.SizeBytes > spec.Execution.MaximumTransferBytes ||
		!artifact.RetainUntil.Equal(artifact.CreatedAt.Add(
			time.Duration(spec.Checkpoint.ArtifactRetentionSeconds)*time.Second,
		)) {
		return contracts.Artifact{}, ports.ErrQuotaExceeded
	}
	if input.IdempotencyKey != "" {
		lockKey := artifact.ProjectID + "\x1fartifact.upload\x1f" +
			artifact.SandboxID + "\x1f" + input.IdempotencyKey
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact idempotency lock failed: %w", err)
		}
		var priorHash, artifactID string
		idempotencyErr := tx.QueryRow(ctx, `
			SELECT request_hash,response_resource_id
			FROM secondbox.idempotency_records
			WHERE project_id=$1 AND operation='artifact.upload' AND target_id=$2 AND idempotency_key=$3`,
			artifact.ProjectID, artifact.SandboxID, input.IdempotencyKey,
		).Scan(&priorHash, &artifactID)
		if idempotencyErr == nil {
			if priorHash != input.RequestHash {
				return contracts.Artifact{}, ports.ErrIdempotencyConflict
			}
			replayed, _, err := scanArtifact(tx.QueryRow(ctx, `
				SELECT id,project_id,sandbox_id,source_generation,name,media_type,size_bytes,
				       sha256,state,metadata_json,retain_until,created_at,published_at,
				       garbage_collected_at,storage_key
				FROM secondbox.artifacts WHERE id=$1 AND project_id=$2`,
				artifactID, artifact.ProjectID,
			))
			if err != nil {
				return contracts.Artifact{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact replay commit failed: %w", err)
			}
			return replayed, nil
		}
		if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact idempotency lookup failed: %w", idempotencyErr)
		}
	}
	projectQuota, err := readQuota(ctx, tx, "project_quotas", "project_id", artifact.ProjectID)
	if err != nil {
		return contracts.Artifact{}, err
	}
	projectUsage, err := readQuotaUsage(ctx, tx, "sandbox.project_id=$1", artifact.ProjectID)
	if err != nil {
		return contracts.Artifact{}, err
	}
	profileQuota, err := readQuota(ctx, tx, "profile_quotas", "profile_name", profileName)
	if err != nil {
		return contracts.Artifact{}, err
	}
	profileUsage, err := readQuotaUsage(ctx, tx, "sandbox.profile_name=$1", profileName)
	if err != nil {
		return contracts.Artifact{}, err
	}
	if projectUsage.artifacts+1 > projectQuota.MaxArtifacts ||
		projectUsage.retainedBytes+artifact.SizeBytes > projectQuota.MaxRetainedBytes ||
		profileUsage.artifacts+1 > profileQuota.MaxArtifacts ||
		profileUsage.retainedBytes+artifact.SizeBytes > profileQuota.MaxRetainedBytes {
		return contracts.Artifact{}, ports.ErrQuotaExceeded
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO secondbox.artifacts (
			id,project_id,sandbox_id,source_generation,name,media_type,size_bytes,sha256,
			storage_key,state,metadata_json,retain_until,created_at,published_at,
			garbage_collection_marked_at,garbage_collected_at
		)
		SELECT $1,sandbox.project_id,sandbox.id,$2,$3,$4,$5,$6,$7,'staging',$8,$9,$10,NULL,NULL,NULL
		FROM secondbox.sandboxes AS sandbox
		WHERE sandbox.id=$11 AND sandbox.project_id=$12 AND sandbox.generation=$13`,
		artifact.ID, artifact.SourceGeneration, artifact.Name, artifact.MediaType,
		artifact.SizeBytes, artifact.SHA256, input.StorageKey, metadataJSON,
		artifact.RetainUntil.UTC(), artifact.CreatedAt.UTC(), artifact.SandboxID,
		artifact.ProjectID, input.ExpectedGeneration,
	)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact staging failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	if input.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.idempotency_records (
				project_id,operation,target_id,idempotency_key,request_hash,
				response_resource_id,created_at,expires_at
			) VALUES ($1,'artifact.upload',$2,$3,$4,$5,$6,$7)`,
			artifact.ProjectID, artifact.SandboxID, input.IdempotencyKey,
			input.RequestHash, artifact.ID, artifact.CreatedAt.UTC(), input.IdempotencyEnds.UTC(),
		); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact idempotency insert failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact staging commit failed: %w", err)
	}
	artifact.State = contracts.ObjectStateStaging
	return artifact, nil
}

// PublishArtifact atomically exposes metadata only after provider hash and size verification.
func (store *PostgresControlPlaneStore) PublishArtifact(
	ctx context.Context,
	input ports.ArtifactPublicationInput,
	now time.Time,
) (contracts.Artifact, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact publication transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var storedState, storedSHA256, storedStorageKey string
	var storedSizeBytes, storedGeneration int64
	if err := tx.QueryRow(ctx, `
		SELECT state,sha256,size_bytes,storage_key,source_generation
		FROM secondbox.artifacts
		WHERE id=$1 AND project_id=$2 AND sandbox_id=$3
		FOR UPDATE`,
		input.Artifact.ID, input.Artifact.ProjectID, input.Artifact.SandboxID,
	).Scan(
		&storedState, &storedSHA256, &storedSizeBytes, &storedStorageKey, &storedGeneration,
	); err != nil {
		return contracts.Artifact{}, mapNotFound(err, ports.ErrArtifactNotFound)
	}
	if storedGeneration != input.ExpectedGeneration {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	var currentGeneration int64
	if err := tx.QueryRow(ctx, `
		SELECT generation FROM secondbox.sandboxes
		WHERE id=$1 AND project_id=$2 FOR UPDATE`,
		input.Artifact.SandboxID, input.Artifact.ProjectID,
	).Scan(&currentGeneration); err != nil {
		return contracts.Artifact{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if currentGeneration != input.ExpectedGeneration {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	if storedState != contracts.ObjectStateStaging {
		return contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	if storedSHA256 != input.Artifact.SHA256 || storedSizeBytes != input.Artifact.SizeBytes ||
		storedStorageKey != input.StorageKey {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.artifacts SET state='integrity_failed'
			WHERE id=$1 AND state='staging'`,
			input.Artifact.ID,
		); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact integrity failure update failed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact integrity failure commit failed: %w", err)
		}
		return contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	var artifact contracts.Artifact
	var metadataJSON []byte
	err = tx.QueryRow(ctx, `
		UPDATE secondbox.artifacts
		SET state='published',published_at=$5
		WHERE id=$1 AND project_id=$2 AND sandbox_id=$3 AND source_generation=$4
		  AND state='staging' AND sha256=$6 AND size_bytes=$7 AND storage_key=$8
		RETURNING id,project_id,sandbox_id,source_generation,name,media_type,size_bytes,sha256,
		          state,metadata_json,retain_until,created_at,published_at,garbage_collected_at`,
		input.Artifact.ID, input.Artifact.ProjectID, input.Artifact.SandboxID,
		input.ExpectedGeneration, now.UTC(), input.Artifact.SHA256,
		input.Artifact.SizeBytes, input.StorageKey,
	).Scan(
		&artifact.ID, &artifact.ProjectID, &artifact.SandboxID, &artifact.SourceGeneration,
		&artifact.Name, &artifact.MediaType, &artifact.SizeBytes, &artifact.SHA256,
		&artifact.State, &metadataJSON, &artifact.RetainUntil, &artifact.CreatedAt,
		&artifact.PublishedAt, &artifact.GarbageCollectedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact publication failed: %w", err)
	}
	if err := json.Unmarshal(metadataJSON, &artifact.Metadata); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact metadata decoding failed: %w", err)
	}
	if input.Audit.ID != "" {
		if err := insertAuditEvent(ctx, tx, input.Audit); err != nil {
			return contracts.Artifact{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact publication commit failed: %w", err)
	}
	return artifact, nil
}

// ListArtifacts returns retained, published Artifact metadata in deterministic newest-first order.
func (store *PostgresControlPlaneStore) ListArtifacts(
	ctx context.Context,
	projectID string,
	sandboxID string,
	limit int,
	cursor string,
	now time.Time,
) (contracts.ArtifactPage, error) {
	var cursorCreatedAt time.Time
	var cursorID string
	if cursor != "" {
		if err := store.pool.QueryRow(ctx, `
			SELECT created_at,id FROM secondbox.artifacts
			WHERE id=$1 AND project_id=$2 AND sandbox_id=$3
			  AND state='published' AND retain_until>$4`,
			cursor, projectID, sandboxID, now.UTC(),
		).Scan(&cursorCreatedAt, &cursorID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return contracts.ArtifactPage{}, errors.New("SecondBox Artifact page cursor is invalid")
			}
			return contracts.ArtifactPage{}, fmt.Errorf("SecondBox Artifact cursor lookup failed: %w", err)
		}
	}
	var sandboxExists bool
	if err := store.pool.QueryRow(ctx, `
		SELECT true FROM secondbox.sandboxes
		WHERE id=$1 AND project_id=$2 AND state<>'deleted'`,
		sandboxID, projectID,
	).Scan(&sandboxExists); err != nil {
		return contracts.ArtifactPage{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	rows, err := store.pool.Query(ctx, `
		SELECT id,project_id,sandbox_id,source_generation,name,media_type,size_bytes,
		       sha256,state,metadata_json,retain_until,created_at,published_at,
		       garbage_collected_at,storage_key
		FROM secondbox.artifacts
		WHERE project_id=$1 AND sandbox_id=$2 AND state='published' AND retain_until>$3
		  AND ($4='' OR (created_at,id)<($5,$4))
		ORDER BY created_at DESC,id DESC
		LIMIT $6`,
		projectID, sandboxID, now.UTC(), cursorID, cursorCreatedAt, limit+1,
	)
	if err != nil {
		return contracts.ArtifactPage{}, fmt.Errorf("SecondBox Artifact list failed: %w", err)
	}
	defer rows.Close()
	artifacts := make([]contracts.Artifact, 0, limit+1)
	for rows.Next() {
		artifact, _, err := scanArtifact(rows)
		if err != nil {
			return contracts.ArtifactPage{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return contracts.ArtifactPage{}, fmt.Errorf("SecondBox Artifact list iteration failed: %w", err)
	}
	page := contracts.ArtifactPage{Items: artifacts}
	if len(page.Items) > limit {
		cursor := page.Items[limit-1].ID
		page.NextCursor = &cursor
		page.Items = page.Items[:limit]
	}
	return page, nil
}

// GetArtifactObject resolves retained public metadata and its private immutable key.
func (store *PostgresControlPlaneStore) GetArtifactObject(
	ctx context.Context,
	projectID string,
	artifactID string,
	now time.Time,
) (ports.ArtifactObject, error) {
	artifact, storageKey, err := scanArtifact(store.pool.QueryRow(ctx, `
		SELECT id,project_id,sandbox_id,source_generation,name,media_type,size_bytes,
		       sha256,state,metadata_json,retain_until,created_at,published_at,
		       garbage_collected_at,storage_key
		FROM secondbox.artifacts
		WHERE id=$1 AND project_id=$2 AND state='published' AND retain_until>$3`,
		artifactID, projectID, now.UTC(),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.ArtifactObject{}, ports.ErrArtifactNotFound
	}
	if err != nil {
		return ports.ArtifactObject{}, err
	}
	return ports.ArtifactObject{Artifact: artifact, StorageKey: storageKey}, nil
}

// EndArtifactRetention hides public metadata and leaves provider deletion to two-phase garbage collection.
func (store *PostgresControlPlaneStore) EndArtifactRetention(
	ctx context.Context,
	input ports.ArtifactRetentionInput,
) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Artifact retention transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.ProjectID + "\x1fartifact.delete\x1f" +
		input.ArtifactID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return fmt.Errorf("SecondBox Artifact retention idempotency lock failed: %w", err)
	}
	var priorHash string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash FROM secondbox.idempotency_records
		WHERE project_id=$1 AND operation='artifact.delete'
		  AND target_id=$2 AND idempotency_key=$3`,
		input.ProjectID, input.ArtifactID, input.IdempotencyKey,
	).Scan(&priorHash)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return ports.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("SecondBox Artifact retention replay commit failed: %w", err)
		}
		return nil
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return fmt.Errorf("SecondBox Artifact retention idempotency lookup failed: %w", idempotencyErr)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.artifacts
		SET state='garbage_pending',retain_until=$3,garbage_collection_marked_at=$3
		WHERE id=$1 AND project_id=$2 AND state='published' AND retain_until>$3`,
		input.ArtifactID, input.ProjectID, input.Now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox Artifact retention update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrArtifactNotFound
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			project_id,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ($1,'artifact.delete',$2,$3,$4,$2,$5,$6)`,
		input.ProjectID, input.ArtifactID, input.IdempotencyKey, input.RequestHash,
		input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Artifact retention idempotency insert failed: %w", err)
	}
	if input.Audit.ID != "" {
		if err := insertAuditEvent(ctx, tx, input.Audit); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox Artifact retention commit failed: %w", err)
	}
	return nil
}

type artifactRow interface {
	Scan(...any) error
}

func scanArtifact(row artifactRow) (contracts.Artifact, string, error) {
	var artifact contracts.Artifact
	var metadataJSON []byte
	var storageKey string
	if err := row.Scan(
		&artifact.ID, &artifact.ProjectID, &artifact.SandboxID, &artifact.SourceGeneration,
		&artifact.Name, &artifact.MediaType, &artifact.SizeBytes, &artifact.SHA256,
		&artifact.State, &metadataJSON, &artifact.RetainUntil, &artifact.CreatedAt,
		&artifact.PublishedAt, &artifact.GarbageCollectedAt, &storageKey,
	); err != nil {
		return contracts.Artifact{}, "", err
	}
	if err := json.Unmarshal(metadataJSON, &artifact.Metadata); err != nil {
		return contracts.Artifact{}, "", fmt.Errorf("SecondBox Artifact metadata decoding failed: %w", err)
	}
	return artifact, storageKey, nil
}

func lockCurrentGeneration(
	ctx context.Context,
	tx pgx.Tx,
	projectID string,
	sandboxID string,
	generation int64,
	allowDraining bool,
) error {
	var currentGeneration int64
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT generation,state FROM secondbox.sandboxes
		WHERE project_id=$1 AND id=$2 FOR UPDATE`,
		projectID, sandboxID,
	).Scan(&currentGeneration, &state); err != nil {
		return mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if currentGeneration != generation {
		return ports.ErrGenerationFenced
	}
	if state != contracts.SandboxStateStarting && state != contracts.SandboxStateReady &&
		(!allowDraining || state != contracts.SandboxStateDraining) {
		return ports.ErrLifecycleUnavailable
	}
	return nil
}

func validateActivityAuthority(ctx context.Context, tx pgx.Tx, input ports.ActivityInput) error {
	if err := lockCurrentGeneration(ctx, tx, input.ProjectID, input.SandboxID, input.Generation, true); err != nil {
		return err
	}
	if input.LeaseID == "" {
		return nil
	}
	var generation int64
	var serviceAccountID, state string
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT generation,service_account_id,state,expires_at FROM secondbox.leases
		WHERE project_id=$1 AND sandbox_id=$2 AND id=$3 FOR UPDATE`,
		input.ProjectID, input.SandboxID, input.LeaseID,
	).Scan(&generation, &serviceAccountID, &state, &expiresAt); err != nil {
		return mapNotFound(err, ports.ErrLeaseNotFound)
	}
	if generation != input.Generation || serviceAccountID != input.ServiceAccountID ||
		state != contracts.LeaseStateActive || !input.Now.Before(expiresAt) {
		return ports.ErrLeaseInactive
	}
	return nil
}

func validActivityKind(kind string) bool {
	switch kind {
	case contracts.ActivitySessionKindExec, contracts.ActivitySessionKindFile,
		contracts.ActivitySessionKindPTY, contracts.ActivitySessionKindPort:
		return true
	default:
		return false
	}
}

func scanLease(row rowScanner) (contracts.Lease, error) {
	var lease contracts.Lease
	if err := row.Scan(
		&lease.ID, &lease.ProjectID, &lease.SandboxID, &lease.Generation,
		&lease.ServiceAccountID, &lease.State, &lease.ExpiresAt, &lease.Revision,
		&lease.CreatedAt, &lease.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.Lease{}, ports.ErrLeaseNotFound
		}
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease lookup failed: %w", err)
	}
	return lease, nil
}

func lookupLeaseIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	projectID string,
	operation string,
	targetID string,
	idempotencyKey string,
	requestHash string,
) (contracts.Lease, bool, error) {
	var priorHash, leaseID string
	err := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id FROM secondbox.idempotency_records
		WHERE project_id=$1 AND operation=$2 AND target_id=$3 AND idempotency_key=$4`,
		projectID, operation, targetID, idempotencyKey,
	).Scan(&priorHash, &leaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Lease{}, false, nil
	}
	if err != nil {
		return contracts.Lease{}, false, fmt.Errorf("SecondBox Lease idempotency lookup failed: %w", err)
	}
	if priorHash != requestHash {
		return contracts.Lease{}, false, ports.ErrIdempotencyConflict
	}
	lease, err := scanLease(tx.QueryRow(ctx, `
		SELECT id,project_id,sandbox_id,generation,service_account_id,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases WHERE project_id=$1 AND id=$2`,
		projectID, leaseID,
	))
	if err != nil {
		return contracts.Lease{}, false, err
	}
	return lease, true, nil
}

func insertLeaseIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	input ports.LeaseInput,
	operation string,
	targetID string,
	leaseID string,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			project_id,operation,target_id,idempotency_key,request_hash,response_resource_id,
			created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		input.ProjectID, operation, targetID, input.IdempotencyKey, input.RequestHash,
		leaseID, input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Lease idempotency insert failed: %w", err)
	}
	return nil
}

// ListGarbageObjectsDue marks expired unreachable objects, rechecks after grace, and claims deletion.
func (store *PostgresControlPlaneStore) ListGarbageObjectsDue(
	ctx context.Context,
	now time.Time,
	grace time.Duration,
	limit int,
) ([]ports.GarbageObject, error) {
	if grace <= 0 || limit < 1 {
		return nil, errors.New("SecondBox garbage collection grace and limit must be positive")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("SecondBox garbage collection transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.snapshots
		SET state='expired',retention_ended_at=retain_until
		WHERE state='published' AND retain_until<=$1`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox Snapshot expiry marking failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_checkpoints
		SET state='garbage_pending',garbage_collection_marked_at=$1
		WHERE state IN ('staging','verified','integrity_failed','quota_failed')
		  AND retain_until<=$1`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox interrupted checkpoint garbage marking failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspace_checkpoints AS checkpoint
		SET state='garbage_pending',garbage_collection_marked_at=$1
		WHERE checkpoint.state='published' AND checkpoint.retain_until<=$1
		  AND NOT EXISTS (
		    SELECT 1 FROM secondbox.workspaces AS workspace
		    WHERE workspace.current_checkpoint_id=checkpoint.id
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM secondbox.workspace_materializations AS materialization
		    WHERE materialization.source_checkpoint_id=checkpoint.id
		      AND materialization.state IN ('preparing','ready')
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM secondbox.snapshots AS snapshot
		    WHERE snapshot.checkpoint_id=checkpoint.id
		      AND snapshot.state='published' AND snapshot.retain_until>$1
		  )`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox checkpoint garbage marking failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.artifacts
		SET state='garbage_pending',garbage_collection_marked_at=$1
		WHERE state IN ('published','staging','integrity_failed') AND retain_until<=$1`, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("SecondBox Artifact garbage marking failed: %w", err)
	}
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT 'checkpoint'::text AS kind,checkpoint.id,checkpoint.storage_key,
			       checkpoint.sha256,checkpoint.size_bytes
			FROM secondbox.workspace_checkpoints AS checkpoint
			WHERE checkpoint.state IN ('garbage_pending','garbage_deleting')
			  AND checkpoint.garbage_collection_marked_at<=$1
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspaces AS workspace
			    WHERE workspace.current_checkpoint_id=checkpoint.id
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspace_materializations AS materialization
			    WHERE materialization.source_checkpoint_id=checkpoint.id
			      AND materialization.state IN ('preparing','ready')
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.snapshots AS snapshot
			    WHERE snapshot.checkpoint_id=checkpoint.id
			      AND snapshot.state='published' AND snapshot.retain_until>$2
			  )
			UNION ALL
			SELECT 'artifact'::text,artifact.id,artifact.storage_key,artifact.sha256,artifact.size_bytes
			FROM secondbox.artifacts AS artifact
			WHERE artifact.state IN ('garbage_pending','garbage_deleting')
			  AND artifact.garbage_collection_marked_at<=$1
			ORDER BY id
			LIMIT $3
		)
		SELECT kind,id,storage_key,sha256,size_bytes FROM candidates`,
		now.UTC().Add(-grace), now.UTC(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox garbage candidates query failed: %w", err)
	}
	candidates := make([]ports.GarbageObject, 0)
	for rows.Next() {
		var candidate ports.GarbageObject
		if err := rows.Scan(
			&candidate.Kind, &candidate.ID, &candidate.StorageKey,
			&candidate.SHA256, &candidate.SizeBytes,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox garbage candidate scan failed: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox garbage candidate iteration failed: %w", err)
	}
	for _, candidate := range candidates {
		table := "workspace_checkpoints"
		if candidate.Kind == "artifact" {
			table = "artifacts"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.`+table+`
			SET state='garbage_deleting' WHERE id=$1 AND state IN ('garbage_pending','garbage_deleting')`,
			candidate.ID,
		); err != nil {
			return nil, fmt.Errorf("SecondBox garbage deletion claim failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("SecondBox garbage deletion claim commit failed: %w", err)
	}
	return candidates, nil
}

// CompleteGarbageObject records terminal deletion evidence for a claimed object.
func (store *PostgresControlPlaneStore) CompleteGarbageObject(
	ctx context.Context,
	object ports.GarbageObject,
	now time.Time,
) error {
	switch object.Kind {
	case "checkpoint":
		tx, err := store.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("SecondBox checkpoint garbage completion transaction failed: %w", err)
		}
		defer tx.Rollback(ctx)
		var workspaceID string
		var sizeBytes int64
		var wasRetained bool
		if err := tx.QueryRow(ctx, `
			SELECT checkpoint.workspace_id,checkpoint.size_bytes,
			       checkpoint.published_at IS NOT NULL
			FROM secondbox.workspace_checkpoints AS checkpoint
			WHERE checkpoint.id=$1 AND checkpoint.state='garbage_deleting'
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspaces AS workspace
			    WHERE workspace.current_checkpoint_id=checkpoint.id
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.workspace_materializations AS materialization
			    WHERE materialization.source_checkpoint_id=checkpoint.id
			      AND materialization.state IN ('preparing','ready')
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.snapshots AS snapshot
			    WHERE snapshot.checkpoint_id=checkpoint.id
			      AND snapshot.state='published' AND snapshot.retain_until>$2
			  )
			FOR UPDATE OF checkpoint`,
			object.ID, now.UTC(),
		).Scan(&workspaceID, &sizeBytes, &wasRetained); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("SecondBox checkpoint deletion claim is no longer current")
			}
			return fmt.Errorf("SecondBox checkpoint deletion claim lookup failed: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.workspace_checkpoints AS checkpoint
			SET state='deleted',garbage_collected_at=$2
			WHERE checkpoint.id=$1`, object.ID, now.UTC()); err != nil {
			return fmt.Errorf("SecondBox checkpoint garbage completion failed: %w", err)
		}
		if wasRetained {
			tag, err := tx.Exec(ctx, `
				UPDATE secondbox.workspaces
				SET retained_bytes=retained_bytes-$2,updated_at=$3
				WHERE id=$1 AND retained_bytes>=$2`,
				workspaceID, sizeBytes, now.UTC(),
			)
			if err != nil {
				return fmt.Errorf("SecondBox checkpoint retained-byte release failed: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return errors.New("SecondBox checkpoint retained-byte evidence is inconsistent")
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("SecondBox checkpoint garbage completion commit failed: %w", err)
		}
		return nil
	case "artifact":
		tag, err := store.pool.Exec(ctx, `
			UPDATE secondbox.artifacts SET state='deleted',garbage_collected_at=$2
			WHERE id=$1 AND state='garbage_deleting'`, object.ID, now.UTC())
		if err != nil {
			return fmt.Errorf("SecondBox garbage completion failed: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("SecondBox garbage object deletion claim is no longer current")
		}
		return nil
	default:
		return errors.New("SecondBox garbage object kind is invalid")
	}
}
