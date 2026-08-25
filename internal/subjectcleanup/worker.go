// Package subjectcleanup coordinates restart-safe Subject expiry and teardown.
package subjectcleanup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StageCancelWork            = "cancel_work"
	StageDeleteSnapshots       = "delete_snapshots"
	StageStopDeleteSandboxes   = "stop_delete_sandboxes"
	StageReleaseResources      = "release_resources"
	StageRequestWorkspace      = "request_workspace_removal"
	StageAwaitAcknowledgements = "await_acknowledgements"
	StageSucceeded             = "succeeded"
	StageFailed                = "failed"
)

// SessionCanceller uses the acknowledged Runner cancellation path for active work.
type SessionCanceller interface {
	CancelSandboxSessions(context.Context, string, int64, string, time.Time) (int64, error)
}

// Worker advances at most one persisted Subject cleanup stage per pass.
type Worker struct {
	pool          *pgxpool.Pool
	sessions      SessionCanceller
	workerID      string
	claimDuration time.Duration
	pollInterval  time.Duration
}

type claim struct {
	operationID string
	tenantRef   string
	subjectRef  string
	stage       string
}

// NewWorker connects the cleanup coordinator to PostgreSQL authority.
func NewWorker(
	ctx context.Context,
	databaseURL string,
	sessions SessionCanceller,
	workerID string,
	claimDuration time.Duration,
	pollInterval time.Duration,
) (*Worker, error) {
	if databaseURL == "" || sessions == nil || workerID == "" || claimDuration <= 0 || pollInterval <= 0 {
		return nil, errors.New("SecondBox Subject cleanup worker requires database, session cancellation, identity, and retry bounds")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Subject cleanup PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox Subject cleanup PostgreSQL readiness: %w", err)
	}
	return &Worker{
		pool: pool, sessions: sessions, workerID: workerID,
		claimDuration: claimDuration, pollInterval: pollInterval,
	}, nil
}

func (worker *Worker) Close() {
	worker.pool.Close()
}

// PollInterval is the bounded restart and notification fallback cadence.
func (worker *Worker) PollInterval() time.Duration {
	return worker.pollInterval
}

// RunOnce expires durable management records and advances one cleanup stage.
func (worker *Worker) RunOnce(ctx context.Context, now time.Time) (bool, error) {
	now = now.UTC()
	if err := worker.reconcileExpiry(ctx, now); err != nil {
		return false, err
	}
	item, found, err := worker.claimNext(ctx, now)
	if err != nil || !found {
		return found, err
	}
	switch item.stage {
	case StageCancelWork:
		return true, worker.cancelWork(ctx, item, now)
	case StageDeleteSnapshots:
		return true, worker.deleteSnapshots(ctx, item, now)
	case StageStopDeleteSandboxes:
		return true, worker.stopDeleteSandboxes(ctx, item, now)
	case StageReleaseResources:
		return true, worker.releaseResources(ctx, item, now)
	case StageRequestWorkspace:
		return true, worker.requestWorkspaceRemoval(ctx, item, now)
	case StageAwaitAcknowledgements:
		return true, worker.awaitAcknowledgements(ctx, item, now)
	default:
		return true, worker.fail(ctx, item, "cleanup_state_conflict", "Subject cleanup has an invalid durable stage", now)
	}
}

func (worker *Worker) claimNext(ctx context.Context, now time.Time) (claim, bool, error) {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return claim{}, false, fmt.Errorf("SecondBox Subject cleanup claim transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var item claim
	err = tx.QueryRow(ctx, `
		SELECT operation_id,tenant_ref,subject_ref,stage
		FROM secondbox.subject_cleanup_operations
		WHERE stage NOT IN ('succeeded','failed')
		  AND next_reconcile_at<=$1 AND reconcile_claim_expires_at<=$1
		ORDER BY next_reconcile_at,operation_id
		FOR UPDATE SKIP LOCKED LIMIT 1`, now,
	).Scan(&item.operationID, &item.tenantRef, &item.subjectRef, &item.stage)
	if errors.Is(err, pgx.ErrNoRows) {
		return claim{}, false, nil
	}
	if err != nil {
		return claim{}, false, fmt.Errorf("SecondBox Subject cleanup claim lookup: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.subject_cleanup_operations
		SET reconcile_owner=$2,reconcile_claim_expires_at=$3,updated_at=$1
		WHERE operation_id=$4`,
		now, worker.workerID, now.Add(worker.claimDuration), item.operationID,
	); err != nil {
		return claim{}, false, fmt.Errorf("SecondBox Subject cleanup claim update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operations
		SET state='running',started_at=COALESCE(started_at,$2),updated_at=$2
		WHERE id=$1 AND state='pending'`, item.operationID, now,
	); err != nil {
		return claim{}, false, fmt.Errorf("SecondBox Subject cleanup Operation start: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.subjects SET cleanup_state='running',updated_at=$3
		WHERE tenant_ref=$1 AND ref=$2 AND cleanup_state='pending'`,
		item.tenantRef, item.subjectRef, now,
	); err != nil {
		return claim{}, false, fmt.Errorf("SecondBox Subject cleanup state start: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return claim{}, false, fmt.Errorf("SecondBox Subject cleanup claim commit: %w", err)
	}
	return item, true, nil
}

func (worker *Worker) cancelWork(ctx context.Context, item claim, now time.Time) error {
	rows, err := worker.pool.Query(ctx, `
		SELECT id,generation FROM secondbox.sandboxes
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state<>'deleted'
		ORDER BY created_at,id`, item.tenantRef, item.subjectRef)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Sandbox cancellation lookup: %w", err)
	}
	type sandboxGeneration struct {
		id         string
		generation int64
	}
	var sandboxes []sandboxGeneration
	for rows.Next() {
		var sandbox sandboxGeneration
		if err := rows.Scan(&sandbox.id, &sandbox.generation); err != nil {
			rows.Close()
			return fmt.Errorf("SecondBox Subject cleanup Sandbox cancellation scan: %w", err)
		}
		sandboxes = append(sandboxes, sandbox)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("SecondBox Subject cleanup Sandbox cancellation rows: %w", err)
	}
	rows.Close()
	for _, sandbox := range sandboxes {
		if _, err := worker.sessions.CancelSandboxSessions(
			ctx, sandbox.id, sandbox.generation, "Subject cleanup requested", now,
		); err != nil {
			return fmt.Errorf("SecondBox Subject cleanup session cancellation: %w", err)
		}
	}
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup cancellation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, item.tenantRef, item.subjectRef); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup cancellation quota lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operations
		SET state='cancelled',error_code='state_conflict',
			error_message='Subject cleanup cancelled the operation',retryable=false,
			completed_at=$4,updated_at=$4
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id<>$3
		  AND kind<>'delete' AND state IN ('pending','running')`,
		item.tenantRef, item.subjectRef, item.operationID, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Operation cancellation: %w", err)
	}
	if err := worker.advance(ctx, tx, item, StageDeleteSnapshots, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (worker *Worker) deleteSnapshots(ctx context.Context, item claim, now time.Time) error {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Snapshot transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, item.tenantRef, item.subjectRef); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Snapshot quota lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.snapshots SET retain_until=LEAST(retain_until,$3),updated_at=$3
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state='ready'`,
		item.tenantRef, item.subjectRef, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Snapshot expiry: %w", err)
	}
	var remaining, failed int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE state='failed') FROM secondbox.snapshots
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state<>'deleted'`,
		item.tenantRef, item.subjectRef,
	).Scan(&remaining, &failed); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Snapshot wait: %w", err)
	}
	if failed != 0 {
		if err := tx.Rollback(ctx); err != nil {
			return fmt.Errorf("SecondBox Subject cleanup Snapshot failure rollback: %w", err)
		}
		return worker.fail(ctx, item, "workspace_cleanup_failed", "Runner-owned Snapshot removal failed", now)
	}
	if remaining != 0 {
		return worker.deferClaim(ctx, tx, item, now)
	}
	if err := worker.advance(ctx, tx, item, StageStopDeleteSandboxes, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (worker *Worker) stopDeleteSandboxes(ctx context.Context, item claim, now time.Time) error {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Sandbox deletion transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, item.tenantRef, item.subjectRef); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Sandbox quota lock: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id FROM secondbox.sandboxes
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state<>'deleted'
		ORDER BY created_at,id FOR UPDATE`, item.tenantRef, item.subjectRef)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Sandbox deletion lookup: %w", err)
	}
	var sandboxIDs []string
	for rows.Next() {
		var sandboxID string
		if err := rows.Scan(&sandboxID); err != nil {
			rows.Close()
			return fmt.Errorf("SecondBox Subject cleanup Sandbox deletion scan: %w", err)
		}
		sandboxIDs = append(sandboxIDs, sandboxID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("SecondBox Subject cleanup Sandbox deletion rows: %w", err)
	}
	rows.Close()
	for _, sandboxID := range sandboxIDs {
		operationID := stableID("op", item.operationID, sandboxID, "delete")
		requestID := stableID("req", item.operationID, sandboxID, "delete")
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.operations (
				id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
				request_metadata_json,error_code,error_message,retryable,created_at,updated_at
			) VALUES ($1,$2,$3,$4,'','delete','pending',$5,'{}','','',false,$6,$6)
			ON CONFLICT (id) DO NOTHING`,
			operationID, item.tenantRef, item.subjectRef, sandboxID, requestID, now,
		); err != nil {
			return fmt.Errorf("SecondBox Subject cleanup delete Operation insert: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET desired_state='deleted',next_reconcile_at=$2,
				lifecycle_intent_kind='delete',lifecycle_action='intent_received',
				lifecycle_termination_reason='requested_stop',revision=revision+1,updated_at=$2
			WHERE id=$1 AND state<>'deleted'`, sandboxID, now,
		); err != nil {
			return fmt.Errorf("SecondBox Subject cleanup Sandbox delete intent: %w", err)
		}
	}
	if err := worker.advance(ctx, tx, item, StageReleaseResources, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (worker *Worker) releaseResources(ctx context.Context, item claim, now time.Time) error {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup resource release transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, item.tenantRef, item.subjectRef); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup resource quota lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.leases SET state='released',revision=revision+1,updated_at=$3
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state='active'`,
		item.tenantRef, item.subjectRef, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Lease release: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions SET state='closed',closed_at=$3,updated_at=$3
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state='active'`,
		item.tenantRef, item.subjectRef, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup activity release: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions SET state='closed',closed_at=COALESCE(closed_at,$3),updated_at=$3
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state IN ('open','closing')`,
		item.tenantRef, item.subjectRef, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Port release: %w", err)
	}
	if err := worker.advance(ctx, tx, item, StageRequestWorkspace, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (worker *Worker) requestWorkspaceRemoval(ctx context.Context, item claim, now time.Time) error {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Workspace request transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, item.tenantRef, item.subjectRef); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Workspace quota lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET next_reconcile_at=$3,updated_at=$3
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state<>'deleted' AND desired_state='deleted'`,
		item.tenantRef, item.subjectRef, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Workspace removal wake: %w", err)
	}
	if err := worker.advance(ctx, tx, item, StageAwaitAcknowledgements, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (worker *Worker) awaitAcknowledgements(ctx context.Context, item claim, now time.Time) error {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup acknowledgement transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var remaining, failed int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE sandbox.state<>'deleted'),
		       count(*) FILTER (WHERE
		         (workspace.state='failed' AND workspace.mutation_kind='workspace_delete') OR
		         EXISTS (
		           SELECT 1 FROM secondbox.operations AS deletion
		           JOIN secondbox.operations AS cleanup ON cleanup.id=$3
		           WHERE deletion.sandbox_id=sandbox.id AND deletion.kind='delete'
		             AND deletion.state='failed' AND deletion.created_at>=cleanup.created_at
		         ))
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2`,
		item.tenantRef, item.subjectRef, item.operationID,
	).Scan(&remaining, &failed); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup acknowledgement lookup: %w", err)
	}
	if failed != 0 {
		if err := tx.Rollback(ctx); err != nil {
			return fmt.Errorf("SecondBox Subject cleanup failure rollback: %w", err)
		}
		return worker.fail(ctx, item, "workspace_cleanup_failed", "Runner-owned Workspace removal failed", now)
	}
	if remaining != 0 {
		return worker.deferClaim(ctx, tx, item, now)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operations
		SET state='succeeded',completed_at=$2,updated_at=$2
		WHERE id=$1 AND state IN ('pending','running')`, item.operationID, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Operation completion: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.subjects
		SET cleanup_state='succeeded',revision=revision+1,updated_at=$3
		WHERE tenant_ref=$1 AND ref=$2`, item.tenantRef, item.subjectRef, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup Subject completion: %w", err)
	}
	if err := worker.advance(ctx, tx, item, StageSucceeded, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (worker *Worker) advance(ctx context.Context, tx pgx.Tx, item claim, stage string, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.subject_cleanup_operations
		SET stage=$3,reconcile_owner='',reconcile_claim_expires_at=$4,
			next_reconcile_at=$4,updated_at=$4
		WHERE operation_id=$1 AND stage=$2 AND reconcile_owner=$5`,
		item.operationID, item.stage, stage, now, worker.workerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup stage transition: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("SecondBox Subject cleanup claim is no longer current")
	}
	return nil
}

func (worker *Worker) deferClaim(ctx context.Context, tx pgx.Tx, item claim, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.subject_cleanup_operations
		SET reconcile_owner='',reconcile_claim_expires_at=$2,next_reconcile_at=$3,
			retry_count=retry_count+1,updated_at=$2
		WHERE operation_id=$1 AND stage=$4 AND reconcile_owner=$5`,
		item.operationID, now, now.Add(worker.pollInterval), item.stage, worker.workerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup retry deferral: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("SecondBox Subject cleanup claim is no longer current")
	}
	return tx.Commit(ctx)
}

func (worker *Worker) fail(ctx context.Context, item claim, code string, message string, now time.Time) error {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup failure transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operations
		SET state='failed',error_code=$2,error_message=$3,retryable=false,
			completed_at=$4,updated_at=$4 WHERE id=$1 AND state IN ('pending','running')`,
		item.operationID, code, message, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup terminal Operation failure: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.subjects SET cleanup_state='failed',revision=revision+1,updated_at=$3
		WHERE tenant_ref=$1 AND ref=$2`, item.tenantRef, item.subjectRef, now,
	); err != nil {
		return fmt.Errorf("SecondBox Subject cleanup terminal Subject failure: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.subject_cleanup_operations
		SET stage='failed',reconcile_owner='',reconcile_claim_expires_at=$2,
			next_reconcile_at=$2,updated_at=$2
		WHERE operation_id=$1 AND stage=$3 AND reconcile_owner=$4`,
		item.operationID, now, item.stage, worker.workerID,
	)
	if err != nil {
		return fmt.Errorf("SecondBox Subject cleanup terminal stage failure: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("SecondBox Subject cleanup claim is no longer current")
	}
	return tx.Commit(ctx)
}

func (worker *Worker) reconcileExpiry(ctx context.Context, now time.Time) error {
	tx, err := worker.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox management expiry transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT tenant_ref,subject_ref FROM secondbox.application_authorities
		WHERE state='active' AND expires_at IS NOT NULL AND expires_at<=$1
		UNION
		SELECT tenant_ref,ref FROM secondbox.subjects
		WHERE state='active' AND expires_at IS NOT NULL AND expires_at<=$1
		ORDER BY 1,2`, now)
	if err != nil {
		return fmt.Errorf("SecondBox management expiry quota candidate lookup: %w", err)
	}
	var quotaScopes []rowlock.QuotaScope
	for rows.Next() {
		var scope rowlock.QuotaScope
		if err := rows.Scan(&scope.TenantRef, &scope.SubjectRef); err != nil {
			rows.Close()
			return fmt.Errorf("SecondBox management expiry quota candidate scan: %w", err)
		}
		quotaScopes = append(quotaScopes, scope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("SecondBox management expiry quota candidate rows: %w", err)
	}
	rows.Close()
	if err := rowlock.QuotaScopes(ctx, tx, quotaScopes); err != nil {
		return fmt.Errorf("SecondBox management expiry quota lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH expired AS (
		  UPDATE secondbox.tenant_controller_authorities
		  SET state='expired',revision=revision+1,updated_at=$1
		  WHERE state='active' AND expires_at IS NOT NULL AND expires_at<=$1
		  RETURNING id,tenant_ref
		)
		INSERT INTO secondbox.audit_events (
		  id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,
		  resource_id,outcome,request_id,details_json,created_at
		)
		SELECT 'audit_tca_expired_' || id,tenant_ref,'secondbox','system','expiry-reconciler',
		  'tenant_controller_authority.expired','tenant_controller_authority',id,
		  'accepted','request-expiry-reconciler','{}',$1 FROM expired
		ON CONFLICT (id) DO NOTHING`, now); err != nil {
		return fmt.Errorf("SecondBox TenantControllerAuthority expiry reconciliation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH expired AS (
		  UPDATE secondbox.application_authorities
		  SET state='expired',revision=revision+1,updated_at=$1
		  WHERE state='active' AND expires_at IS NOT NULL AND expires_at<=$1
		  RETURNING id,tenant_ref,subject_ref
		)
		INSERT INTO secondbox.audit_events (
		  id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,
		  resource_id,outcome,request_id,details_json,created_at
		)
		SELECT 'audit_apa_expired_' || id,tenant_ref,subject_ref,'system','expiry-reconciler',
		  'application_authority.expired','application_authority',id,
		  'accepted','request-expiry-reconciler','{}',$1 FROM expired
		ON CONFLICT (id) DO NOTHING`, now); err != nil {
		return fmt.Errorf("SecondBox ApplicationAuthority expiry reconciliation: %w", err)
	}
	rows, err = tx.Query(ctx, `
		SELECT tenant_ref,ref FROM secondbox.subjects
		WHERE state='active' AND expires_at IS NOT NULL AND expires_at<=$1
		ORDER BY expires_at,tenant_ref,ref FOR UPDATE`, now)
	if err != nil {
		return fmt.Errorf("SecondBox expired Subject selection: %w", err)
	}
	type expiredSubject struct{ tenantRef, subjectRef string }
	var subjects []expiredSubject
	for rows.Next() {
		var subject expiredSubject
		if err := rows.Scan(&subject.tenantRef, &subject.subjectRef); err != nil {
			rows.Close()
			return fmt.Errorf("SecondBox expired Subject scan: %w", err)
		}
		subjects = append(subjects, subject)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("SecondBox expired Subject rows: %w", err)
	}
	rows.Close()
	for _, subject := range subjects {
		operationID := stableID("op", "subject-expiry", subject.tenantRef, subject.subjectRef)
		requestID := stableID("req", "subject-expiry", subject.tenantRef, subject.subjectRef)
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.application_authorities
			SET state='revoked',revision=revision+1,updated_at=$3
			WHERE tenant_ref=$1 AND subject_ref=$2 AND state='active'`,
			subject.tenantRef, subject.subjectRef, now,
		); err != nil {
			return fmt.Errorf("SecondBox expired Subject authority revocation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.operations (
				id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
				request_metadata_json,error_code,error_message,retryable,created_at,updated_at
			) VALUES ($1,$2,$3,'','','subject_cleanup','pending',$4,'{}','','',false,$5,$5)
			ON CONFLICT (id) DO NOTHING`, operationID, subject.tenantRef, subject.subjectRef, requestID, now,
		); err != nil {
			return fmt.Errorf("SecondBox expired Subject cleanup Operation insert: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.subject_cleanup_operations (
				operation_id,tenant_ref,subject_ref,stage,reconcile_owner,
				reconcile_claim_expires_at,next_reconcile_at,retry_count,retry_limit,
				created_at,updated_at
			) VALUES ($1,$2,$3,'cancel_work','',$4,$4,0,20,$4,$4)
			ON CONFLICT (tenant_ref,subject_ref) DO NOTHING`,
			operationID, subject.tenantRef, subject.subjectRef, now,
		); err != nil {
			return fmt.Errorf("SecondBox expired Subject cleanup coordinator insert: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.subjects
			SET state='expired',cleanup_state='pending',cleanup_operation_id=$3,
				revision=revision+1,updated_at=$4
			WHERE tenant_ref=$1 AND ref=$2 AND state='active'`,
			subject.tenantRef, subject.subjectRef, operationID, now,
		); err != nil {
			return fmt.Errorf("SecondBox expired Subject closure: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.audit_events (
				id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,
				resource_id,outcome,request_id,details_json,created_at
			) VALUES ($1,$2,$3,'system','expiry-reconciler','subject.expired','subject',$3,
				'accepted',$4,'{}',$5)
			ON CONFLICT (id) DO NOTHING`,
			stableID("aud", "subject-expiry", subject.tenantRef, subject.subjectRef),
			subject.tenantRef, subject.subjectRef, requestID, now,
		); err != nil {
			return fmt.Errorf("SecondBox expired Subject audit insert: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox management expiry commit: %w", err)
	}
	return nil
}

func stableID(prefix string, parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return prefix + "_" + hex.EncodeToString(hash.Sum(nil)[:16])
}
