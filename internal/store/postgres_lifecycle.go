package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/lifecycleprojection"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GetSandboxLifecyclePolicy reads the immutable ProfileRevision pinned by the Sandbox.
func (store *PostgresControlPlaneStore) GetSandboxLifecyclePolicy(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
) (contracts.LifecyclePolicy, contracts.RetentionPolicy, error) {
	var specJSON []byte
	if err := store.pool.QueryRow(ctx, `
		SELECT revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$3`,
		tenantRef, subjectRef, sandboxID,
	).Scan(&specJSON); err != nil {
		return contracts.LifecyclePolicy{}, contracts.RetentionPolicy{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return contracts.LifecyclePolicy{}, contracts.RetentionPolicy{},
			fmt.Errorf("SecondBox pinned lifecycle policy decoding failed: %w", err)
	}
	return spec.Lifecycle, spec.Retention, nil
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
	if err := rowlock.TenantAndSubjectQuota(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef,
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle quota lock failed: %w", err)
	}
	lockKey := input.Principal.TenantRef + "\x1f" + input.Principal.SubjectRef +
		"\x1flifecycle\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle idempotency lock failed: %w", err)
	}
	var priorHash, priorOperationID string
	var expiresAt time.Time
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id,expires_at FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
		input.Principal.TenantRef, input.Principal.SubjectRef,
		"sandbox."+input.Operation.Kind, input.SandboxID, input.IdempotencyKey,
	).Scan(&priorHash, &priorOperationID, &expiresAt)
	if idempotencyErr == nil {
		expired, err := deleteExpiredIdempotencyRecord(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef,
			"sandbox."+input.Operation.Kind, input.SandboxID, input.IdempotencyKey,
			expiresAt, input.Now,
		)
		if err != nil {
			return contracts.Operation{}, fmt.Errorf("SecondBox expired lifecycle idempotency cleanup failed: %w", err)
		}
		if expired {
			idempotencyErr = pgx.ErrNoRows
		} else {
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
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle idempotency lookup failed: %w", idempotencyErr)
	}
	locked, err := lockSandboxWorkspace(
		ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID,
	)
	if err != nil {
		return contracts.Operation{}, err
	}
	observed := locked.SandboxState
	desired := locked.DesiredState
	generation := locked.Generation
	revision := locked.Revision
	if revision != input.ExpectedRevision {
		return contracts.Operation{}, ports.ErrRevisionConflict
	}
	if observed == contracts.SandboxStateDeleted || desired == contracts.SandboxDesiredStateDeleted {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	kind := input.Operation.Kind
	deleteWaitingForCreate := kind == "delete" &&
		(locked.Workspace.Mutation.Kind == "create" ||
			locked.Workspace.Mutation.Kind == "clone") &&
		locked.Workspace.Mutation.State != ""
	if locked.Workspace.Mutation.State != "" && !deleteWaitingForCreate {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	if locked.Workspace.Generation != generation {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	if locked.Workspace.State != "ready" &&
		!(kind == "delete" &&
			(locked.Workspace.State == "creating" || locked.Workspace.State == "failed")) {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	var tenantQuota contracts.TenantQuota
	var subjectQuota contracts.QuotaLimits
	if kind == "start" {
		tenantQuota, subjectQuota, err = lockTenantAndSubjectQuotaForAdmission(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.Now,
		)
		if err != nil {
			return contracts.Operation{}, err
		}
	}
	if kind == "start" {
		if observed != contracts.SandboxStateStopped && observed != contracts.SandboxStateFailed {
			return contracts.Operation{}, ports.ErrWorkspaceMutation
		}
		var specJSON []byte
		if err := tx.QueryRow(ctx, `SELECT spec_json FROM secondbox.profile_revisions WHERE id=$1`,
			locked.ProfileRevisionID).Scan(&specJSON); err != nil {
			return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle quota Profile lookup failed: %w", err)
		}
		var spec contracts.ProfileRevisionSpec
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle quota Profile decoding failed: %w", err)
		}
		subjectUsage, err := readSubjectQuotaUsage(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef,
		)
		if err != nil {
			return contracts.Operation{}, err
		}
		tenantUsage, err := readTenantQuotaUsage(ctx, tx, input.Principal.TenantRef, input.Now)
		if err != nil {
			return contracts.Operation{}, err
		}
		delta := quotaUsage{activeInstances: 1, cpuMillis: spec.Resources.CPUMillis, memoryBytes: spec.Resources.MemoryBytes}
		if subjectUsage.activeInstances+1 > subjectQuota.MaxActiveInstances ||
			subjectUsage.cpuMillis+delta.cpuMillis > subjectQuota.MaxCPUMillis ||
			subjectUsage.memoryBytes+delta.memoryBytes > subjectQuota.MaxMemoryBytes ||
			tenantDataPlaneQuotaWouldExceed(tenantQuota, tenantUsage, delta) {
			return contracts.Operation{}, ports.ErrQuotaExceeded
		}
		if err := requireHomeRunnerReady(ctx, tx, locked.Workspace.HomeRunnerID); err != nil {
			return contracts.Operation{}, err
		}
		if err := setWorkspaceMutation(
			ctx, tx, locked.WorkspaceID, "start", input.Operation.ID,
			input.Operation.ID, input.Operation.ID, generation, generation, input.Now,
		); err != nil {
			return contracts.Operation{}, err
		}
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
	} else if kind == "stop" || kind == "delete" {
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
	wakeTrigger ports.LifecycleWakeTrigger,
) (ports.LifecycleReconcileClaim, bool, error) {
	claims, err := store.ClaimLifecycleBatch(ctx, workerID, now, claimDuration, 1, wakeTrigger)
	if err != nil {
		return ports.LifecycleReconcileClaim{}, false, err
	}
	if len(claims) == 0 {
		return ports.LifecycleReconcileClaim{}, false, nil
	}
	return claims[0], true, nil
}

// ClaimLifecycleBatch atomically claims a bounded ordered cohort. The caller
// still processes effects sequentially, so batching removes claim round trips
// without introducing concurrent serializable scheduler transactions.
func (store *PostgresControlPlaneStore) ClaimLifecycleBatch(
	ctx context.Context,
	workerID string,
	now time.Time,
	claimDuration time.Duration,
	batchSize int,
	wakeTrigger ports.LifecycleWakeTrigger,
) ([]ports.LifecycleReconcileClaim, error) {
	if workerID == "" || claimDuration <= 0 || batchSize <= 0 {
		return nil, errors.New("SecondBox lifecycle claim worker, duration, and batch size are required")
	}
	pickupStage, err := lifecyclePickupStage(wakeTrigger)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	if err := expireLifecycleLeases(ctx, store.pool, now, batchSize); err != nil {
		return nil, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	// The durable schedule is the single authority over which Sandboxes hold
	// reconciliation work. A Sandbox the reconciler left at rest carries no
	// next_reconcile_at at all, so it is absent from this scan and from
	// sandboxes_lifecycle_reconcile_idx until an external event schedules it;
	// the reconciler's own rest matrix is not restated here. The two remaining
	// predicates are terminal conditions no decision produces: a deleted
	// Sandbox has no further transition, and a Sandbox holding a lifecycle
	// failure class waits for an operator rather than for this worker.
	rows, err := tx.Query(ctx, `
		SELECT sandbox.id,sandbox.tenant_ref,sandbox.subject_ref
		FROM secondbox.sandboxes AS sandbox
		WHERE sandbox.state<>'deleted' AND sandbox.next_reconcile_at<=$1
		  AND NOT (
		    sandbox.state='failed' AND sandbox.lifecycle_failure_class<>''
		  )
		  AND (
		    sandbox.reconcile_claim_expires_at IS NULL
		    OR sandbox.reconcile_claim_expires_at<=$1
		    OR sandbox.reconcile_owner=$2
		  )
		ORDER BY sandbox.next_reconcile_at,sandbox.id
		LIMIT $3`, now, workerID, batchSize)
	if err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim candidate lookup failed: %w", err)
	}
	candidateSandboxIDs := make([]string, 0, batchSize)
	var quotaScopes []rowlock.QuotaScope
	for rows.Next() {
		var sandboxID string
		var quotaScope rowlock.QuotaScope
		if err := rows.Scan(&sandboxID, &quotaScope.TenantRef, &quotaScope.SubjectRef); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox lifecycle claim candidate scan failed: %w", err)
		}
		candidateSandboxIDs = append(candidateSandboxIDs, sandboxID)
		quotaScopes = append(quotaScopes, quotaScope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("SecondBox lifecycle claim candidate iteration failed: %w", err)
	}
	rows.Close()
	if len(candidateSandboxIDs) == 0 {
		return nil, nil
	}
	if err := rowlock.QuotaScopes(ctx, tx, quotaScopes); err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim quota lock failed: %w", err)
	}
	rows, err = tx.Query(ctx, `
		SELECT sandbox.id
		FROM secondbox.sandboxes AS sandbox
		WHERE sandbox.id=ANY($1::text[])
		  AND sandbox.state<>'deleted' AND sandbox.next_reconcile_at<=$2
		  AND NOT (sandbox.state='failed' AND sandbox.lifecycle_failure_class<>'')
		  AND (
		    sandbox.reconcile_claim_expires_at IS NULL
		    OR sandbox.reconcile_claim_expires_at<=$2
		    OR sandbox.reconcile_owner=$3
		  )
		ORDER BY sandbox.next_reconcile_at,sandbox.id
		FOR UPDATE OF sandbox SKIP LOCKED`, candidateSandboxIDs, now, workerID)
	if err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim candidate lock failed: %w", err)
	}
	claimedSandboxIDs := make([]string, 0, len(candidateSandboxIDs))
	for rows.Next() {
		var sandboxID string
		if err := rows.Scan(&sandboxID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox lifecycle claim candidate lock scan failed: %w", err)
		}
		claimedSandboxIDs = append(claimedSandboxIDs, sandboxID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("SecondBox lifecycle claim candidate lock rows failed: %w", err)
	}
	rows.Close()
	if len(claimedSandboxIDs) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `
		WITH due AS (
		  SELECT id
		  FROM secondbox.leases
		  WHERE sandbox_id=ANY($2::text[]) AND state='active' AND expires_at<=$1
		  ORDER BY expires_at,id
		  FOR UPDATE SKIP LOCKED
		)
		UPDATE secondbox.leases AS lease
		SET state='expired',revision=lease.revision+1,updated_at=$1
		FROM due
		WHERE lease.id=due.id`, now, claimedSandboxIDs); err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claimed Lease cleanup failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions AS session
		SET state='closed',closed_at=$1,updated_at=$1
		FROM secondbox.leases AS lease
		WHERE session.sandbox_id=ANY($2::text[])
		  AND session.state='active' AND session.lease_id=lease.id
		  AND (lease.state<>'active' OR lease.expires_at<=$1)`, now, claimedSandboxIDs); err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claimed Lease session cleanup failed: %w", err)
	}
	rows, err = tx.Query(ctx, `
		SELECT sandbox.id,sandbox.state,sandbox.desired_state,sandbox.revision,
		       revision.spec_json,sandbox.lifecycle_intent_kind,
		       COALESCE(sandbox.lifecycle_termination_reason,''),
		       instance.ready_at,sandbox.last_activity_at,sandbox.drain_started_at,
		       instance.id IS NOT NULL,
		       COALESCE(instance.guest_liveness,''),
		       COALESCE(instance.termination_reason,''),
		       COALESCE(stop_effect.state,''),
		       (
		         SELECT count(*)
		         FROM secondbox.activity_sessions AS session
		         JOIN secondbox.leases AS lease ON lease.id=session.lease_id
		         WHERE session.sandbox_id=sandbox.id AND session.generation=sandbox.generation
		           AND session.tenant_ref=sandbox.tenant_ref
		           AND session.subject_ref=sandbox.subject_ref
		           AND session.state='active'
		           AND lease.state='active' AND lease.expires_at>$1
		       )
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		LEFT JOIN secondbox.instances AS instance ON instance.id=sandbox.current_instance_id
		LEFT JOIN LATERAL (
		  SELECT effect.state
		  FROM secondbox.lifecycle_effects AS effect
		  WHERE effect.sandbox_id=sandbox.id AND effect.generation=sandbox.generation
		    AND effect.kind='stop'
		  ORDER BY effect.created_at DESC,effect.id DESC LIMIT 1
		) AS stop_effect ON true
		WHERE sandbox.id=ANY($2::text[])
		ORDER BY sandbox.next_reconcile_at,sandbox.id
		`, now, claimedSandboxIDs)
	if err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim lookup failed: %w", err)
	}
	claims := make([]ports.LifecycleReconcileClaim, 0, batchSize)
	for rows.Next() {
		var (
			claim                                   ports.LifecycleReconcileClaim
			specJSON                                []byte
			intentKind                              sql.NullString
			readyAt, lastActivityAt, drainStartedAt sql.NullTime
		)
		if err := rows.Scan(
			&claim.SandboxID, &claim.ObservedState, &claim.DesiredState, &claim.Revision,
			&specJSON, &intentKind, &claim.IntentTerminationReason,
			&readyAt, &lastActivityAt, &drainStartedAt,
			&claim.HasInstance,
			&claim.GuestLiveness, &claim.InstanceTerminationReason,
			&claim.StopEffectState, &claim.ActiveSessions,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox lifecycle claim scan failed: %w", err)
		}
		var spec contracts.ProfileRevisionSpec
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SecondBox lifecycle claim policy decoding failed: %w", err)
		}
		claim.WorkerID = workerID
		claim.IntentKind = intentKind.String
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
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("SecondBox lifecycle claim rows failed: %w", err)
	}
	rows.Close()
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET reconcile_owner=$1,reconcile_claim_expires_at=$2
		WHERE id=ANY($3::text[])`,
		workerID, now.Add(claimDuration), claimedSandboxIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim update failed: %w", err)
	}
	if tag.RowsAffected() != int64(len(claims)) {
		return nil, ports.ErrRevisionConflict
	}
	// The wake trigger is recorded once per Operation, on the first claim of its
	// Sandbox, in the transaction that establishes the claim. A pickup stage
	// that names the poll deadline rather than the commit notification is the
	// evidence that a transition left its Sandbox due in the future while its
	// successor decision was already available.
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.operation_stage_timings (
			operation_id,sandbox_id,stage,observed_at
		)
		SELECT id,sandbox_id,$1,$2
		FROM secondbox.operations
		WHERE sandbox_id=ANY($3::text[]) AND state IN ('pending','running')
		ON CONFLICT (operation_id,stage) DO NOTHING`,
		pickupStage, now, claimedSandboxIDs,
	); err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim pickup attribution failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("SecondBox lifecycle claim commit failed: %w", err)
	}
	return claims, nil
}

// lifecyclePickupStage maps a wake trigger onto its fixed stage name. An
// unrecognized trigger is a programming error and stops the claim rather than
// silently attributing the pickup to the wrong cause.
func lifecyclePickupStage(trigger ports.LifecycleWakeTrigger) (string, error) {
	switch trigger {
	case ports.LifecycleWakeTriggerNotify:
		return StageLifecyclePickupNotify, nil
	case ports.LifecycleWakeTriggerDeadline:
		return StageLifecyclePickupDeadline, nil
	case ports.LifecycleWakeTriggerImmediate:
		return StageLifecyclePickupImmediate, nil
	default:
		return "", fmt.Errorf("SecondBox lifecycle claim wake trigger %q is unrecognized", trigger)
	}
}

func expireLifecycleLeases(
	ctx context.Context,
	pool *pgxpool.Pool,
	now time.Time,
	limit int,
) error {
	if _, err := pool.Exec(ctx, `
		WITH due AS (
		  SELECT id
		  FROM secondbox.leases
		  WHERE state='active' AND expires_at<=$1
		  ORDER BY expires_at,id
		  FOR UPDATE SKIP LOCKED
		  LIMIT $2
		)
		UPDATE secondbox.leases AS lease
		SET state='expired',revision=lease.revision+1,updated_at=$1
		FROM due
		WHERE lease.id=due.id`, now, limit); err != nil {
		return fmt.Errorf("SecondBox lifecycle expired Lease sweep failed: %w", err)
	}
	return nil
}

// ApplyLifecycleAction commits one claimed transition only while owner and
// revision remain current.
//
// A zero nextReconcileAt parks the Sandbox: the commit clears its durable
// deadline, which removes it from the claim scan until an external event
// schedules it again.
//
// `revision` is the Sandbox's public ETag, and a `wait` changes no field a
// caller can observe, so a wait holds it — and updated_at with it — exactly
// where they were. Without that, a caller that reads a Sandbox and sends
// If-Match on what it read races a reconciliation it cannot see and loses the
// precondition to a transition that changed nothing.
//
// The revision still fences the claim. Every action that changes durable state
// advances it, so a claim token that has committed such an action can never
// commit a second one, and a wait that holds the revision still requires
// reconcile_owner to name this claim's worker — which the commit clears.
func (store *PostgresControlPlaneStore) ApplyLifecycleAction(
	ctx context.Context,
	claim ports.LifecycleReconcileClaim,
	action string,
	terminationReason string,
	now time.Time,
	nextReconcileAt time.Time,
) error {
	nextState := claim.ObservedState
	switch action {
	case "wait":
	case "finish_stop":
		nextState = contracts.SandboxStateStopped
	case "start_instance":
		nextState = contracts.SandboxStateStarting
	case "mark_ready":
		nextState = contracts.SandboxStateReady
	case "drain":
		nextState = contracts.SandboxStateDraining
	case "stop_instance":
		nextState = contracts.SandboxStateStopping
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
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, claim.SandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.ErrRevisionConflict
	}
	if err != nil {
		return fmt.Errorf("SecondBox lifecycle action Sandbox/Workspace lock failed: %w", err)
	}
	if locked.Revision != claim.Revision || locked.ReconcileOwner != claim.WorkerID {
		return ports.ErrRevisionConflict
	}
	if action == "delete" && locked.Workspace.Mutation.State != "" {
		return ports.ErrWorkspaceMutation
	}
	var finishStopWorkspaceID string
	var finishStopGeneration int64
	var finishStopTerminationReason string
	if action == "finish_stop" {
		finishStopWorkspaceID = locked.WorkspaceID
		finishStopGeneration = locked.Generation
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(lifecycle_termination_reason,'')
			FROM secondbox.sandboxes WHERE id=$1`,
			claim.SandboxID,
		).Scan(&finishStopTerminationReason); err != nil {
			return fmt.Errorf("SecondBox finish-stop termination reason lookup failed: %w", err)
		}
		var effectState string
		err = tx.QueryRow(ctx, `
			SELECT state FROM secondbox.lifecycle_effects WHERE id=$1 FOR UPDATE`,
			locked.Workspace.Mutation.EffectID,
		).Scan(&effectState)
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.ErrRevisionConflict
		}
		if err != nil {
			return fmt.Errorf("SecondBox finish-stop generation lookup failed: %w", err)
		}
		if locked.Workspace.Mutation.Kind != "stop" ||
			locked.Workspace.Mutation.ID == "" ||
			locked.Workspace.Mutation.State != "runner_succeeded" ||
			effectState != "runner_succeeded" ||
			locked.Workspace.Mutation.ExpectedGeneration != finishStopGeneration ||
			locked.Workspace.Mutation.TargetGeneration != finishStopGeneration+1 {
			return ports.ErrWorkspaceMutation
		}
		if terminationReason == "" {
			terminationReason = finishStopTerminationReason
		}
	}
	var scheduledAt *time.Time
	if !nextReconcileAt.IsZero() {
		scheduled := nextReconcileAt.UTC()
		scheduledAt = &scheduled
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state=$1,lifecycle_action=CASE WHEN $2='wait' THEN lifecycle_action ELSE $2 END,
		    desired_state=CASE
		      WHEN $2='drain' AND $3 IN ('idle_timeout','maximum_duration') THEN 'stopped'
		      ELSE desired_state
		    END,
		    lifecycle_termination_reason=CASE WHEN $3='' THEN lifecycle_termination_reason ELSE $3 END,
		    lifecycle_failure_class=CASE WHEN $2='fail' THEN 'internal' ELSE lifecycle_failure_class END,
		    lifecycle_failure_message=CASE WHEN $2='fail' THEN 'unrecognized lifecycle state' ELSE lifecycle_failure_message END,
		    next_reconcile_at=CASE WHEN $2='fail' THEN NULL::timestamptz ELSE $4::timestamptz END,
		    reconcile_owner='',reconcile_claim_expires_at=NULL,
		    drain_started_at=CASE
		      WHEN $2='drain' THEN COALESCE(drain_started_at,$5)
		      WHEN $1<>'draining' THEN NULL
		      ELSE drain_started_at
		    END,
		    current_instance_id=CASE WHEN $2='finish_stop' THEN '' ELSE current_instance_id END,
		    generation=CASE WHEN $2='finish_stop' THEN generation+1 ELSE generation END,
		    last_activity_at=CASE WHEN $2='mark_ready' THEN COALESCE(last_activity_at,$5) ELSE last_activity_at END,
		    revision=CASE WHEN $2='wait' THEN revision ELSE revision+1 END,
		    updated_at=CASE WHEN $2='wait' THEN updated_at ELSE $5 END
		WHERE id=$6 AND reconcile_owner=$7 AND revision=$8`,
		nextState, action, terminationReason, scheduledAt,
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
			SET generation=$2,mutation_kind='',mutation_id='',mutation_effect_id='',
			    mutation_operation_id='',mutation_expected_generation=NULL,
			    mutation_target_generation=NULL,mutation_state='',updated_at=$3
			WHERE id=$1 AND generation=$4 AND mutation_kind='stop'
			  AND mutation_state='runner_succeeded'`,
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
	case "drain":
		if err := lifecycleprojection.RecordTeardownStage(
			ctx, tx, claim.SandboxID,
			lifecycleprojection.StageTeardownDrainCommitted, now,
		); err != nil {
			return err
		}
	case "mark_ready":
		if err := lifecycleprojection.ProjectReadyOperations(
			ctx, tx, claim.SandboxID, now,
		); err != nil {
			return fmt.Errorf("SecondBox lifecycle ready projection failed: %w", err)
		}
	case "finish_stop":
		// The stop milestone is recorded before the Operations complete, so a
		// stop Operation still observes the transition that satisfied it.
		if err := lifecycleprojection.RecordTeardownStage(
			ctx, tx, claim.SandboxID,
			lifecycleprojection.StageTeardownStopCommitted, now,
		); err != nil {
			return err
		}
		if err := completeOperations(
			[]string{"drain", "stop"}, contracts.OperationStateSucceeded, "", "",
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
	case "fail":
		if err := completeOperations(
			[]string{"create", "start", "drain", "stop", "delete"},
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
