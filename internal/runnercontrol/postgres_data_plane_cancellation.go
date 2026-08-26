package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (store *PostgresDataPlaneStore) CancelDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	reason string,
	now time.Time,
) (bool, error) {
	return store.cancelDataPlaneSession(ctx, tenantRef, subjectRef, sessionID, reason, now)
}

// SweepDataPlane requests cancellation for due work and removes expired sessions.
func (store *PostgresDataPlaneStore) SweepDataPlane(
	ctx context.Context,
	now time.Time,
	limit int,
) (bool, error) {
	if limit < 1 || limit > 1000 {
		return false, errors.New("SecondBox data-plane sweep limit is invalid")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT session.tenant_ref,session.subject_ref,session.id,
		       session.sandbox_id,session.generation,session.kind,
		       CASE
		         WHEN session.kind='terminal'
		              AND session.attachment_id=''
		              AND session.detach_expires_at IS NOT NULL
		              AND session.detach_expires_at<=$1
		           THEN 'Terminal detach interval expired'
		         WHEN session.lease_id<>''
		              AND NOT EXISTS (
		                SELECT 1 FROM secondbox.leases AS lease
		                WHERE lease.id=session.lease_id
		                  AND lease.tenant_ref=session.tenant_ref
		                  AND lease.subject_ref=session.subject_ref
		                  AND lease.sandbox_id=session.sandbox_id
		                  AND lease.generation=session.generation
		                  AND lease.state='active'
		                  AND lease.expires_at>$1
		              )
		           THEN 'operation Lease is inactive'
		         ELSE 'operation deadline exceeded'
		       END
		FROM secondbox.data_plane_sessions AS session
		WHERE session.state IN ('pending','running')
		  AND (
		    session.deadline_at<=$1
		    OR (
		      session.kind='terminal'
		      AND session.attachment_id=''
		      AND session.detach_expires_at IS NOT NULL
		      AND session.detach_expires_at<=$1
		    )
		    OR (
		      session.lease_id<>''
		      AND NOT EXISTS (
		        SELECT 1 FROM secondbox.leases AS lease
		        WHERE lease.id=session.lease_id
		          AND lease.tenant_ref=session.tenant_ref
		          AND lease.subject_ref=session.subject_ref
		          AND lease.sandbox_id=session.sandbox_id
		          AND lease.generation=session.generation
		          AND lease.state='active'
		          AND lease.expires_at>$1
		      )
		    )
		  )
		ORDER BY session.deadline_at,session.id
		LIMIT $2`,
		now.UTC(), limit,
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox due data-plane lookup: %w", err)
	}
	type dueSession struct {
		tenantRef  string
		subjectRef string
		id         string
		sandboxID  string
		generation int64
		kind       string
		reason     string
	}
	due := make([]dueSession, 0, limit)
	for rows.Next() {
		var session dueSession
		if err := rows.Scan(
			&session.tenantRef, &session.subjectRef, &session.id, &session.sandboxID,
			&session.generation, &session.kind, &session.reason,
		); err != nil {
			rows.Close()
			return false, fmt.Errorf("SecondBox due data-plane scan: %w", err)
		}
		due = append(due, session)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("SecondBox due data-plane rows: %w", err)
	}
	rows.Close()
	for _, session := range due {
		if session.kind == "port" {
			if _, err := store.ClosePortSession(ctx, PortTunnelClose{
				TenantRef: session.tenantRef, SubjectRef: session.subjectRef,
				SandboxID: session.sandboxID,
				SessionID: session.id, Generation: session.generation,
				Reason: session.reason, Now: now.UTC(),
			}); err != nil {
				return false, err
			}
			continue
		}
		if session.reason == "operation deadline exceeded" {
			if _, err := store.ExpireDataPlaneSession(
				ctx, session.tenantRef, session.subjectRef, session.id, now.UTC(),
			); err != nil {
				return false, err
			}
		} else {
			if _, err := store.cancelDataPlaneSession(
				ctx, session.tenantRef, session.subjectRef,
				session.id, session.reason, now.UTC(),
			); err != nil {
				return false, err
			}
		}
	}
	sessionsChanged, err := store.sweepDataPlaneSessions(ctx, now.UTC(), limit)
	if err != nil {
		return false, err
	}
	return len(due) > 0 || sessionsChanged, nil
}

func (store *PostgresDataPlaneStore) sweepDataPlaneSessions(
	ctx context.Context,
	now time.Time,
	limit int,
) (bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox retained session cleanup transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT session.id,session.tenant_ref,session.subject_ref
		FROM secondbox.data_plane_sessions AS session
		WHERE session.state IN ('completed','failed','cancelled','expired')
		  AND session.retain_until<=$1
		ORDER BY session.retain_until,session.id
		LIMIT $2`, now, limit)
	if err != nil {
		return false, fmt.Errorf("SecondBox retained session lookup: %w", err)
	}
	ids := make([]string, 0, limit)
	var scopes []rowlock.QuotaScope
	for rows.Next() {
		var id string
		var scope rowlock.QuotaScope
		if err := rows.Scan(&id, &scope.TenantRef, &scope.SubjectRef); err != nil {
			rows.Close()
			return false, fmt.Errorf("SecondBox retained session scan: %w", err)
		}
		ids = append(ids, id)
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("SecondBox retained session rows: %w", err)
	}
	rows.Close()
	if len(ids) > 0 {
		if err := rowlock.QuotaScopes(ctx, tx, scopes); err != nil {
			return false, fmt.Errorf("SecondBox retained session quota lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM secondbox.data_plane_sessions
			WHERE id=ANY($1) AND state IN ('completed','failed','cancelled','expired')
			  AND retain_until<=$2`, ids, now,
		); err != nil {
			return false, fmt.Errorf("SecondBox retained session cleanup: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox retained session cleanup commit: %w", err)
	}
	return len(ids) > 0, nil
}

// ExpireDataPlaneSession requests deadline cancellation without declaring guest work stopped.
func (store *PostgresDataPlaneStore) ExpireDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	now time.Time,
) (DataPlaneSession, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane expiry transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane expiry quota lock: %w", err)
	}
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if session.State == "completed" || session.State == "failed" ||
		session.State == "expired" || session.State == "cancelled" {
		return session, tx.Commit(ctx)
	}
	if session.State == "cancelling" {
		if err := tx.Commit(ctx); err != nil {
			return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane expiry replay commit: %w", err)
		}
		return session, nil
	}
	if err := store.enqueueCancellation(
		ctx, tx, session, dataPlaneDeadlineTerminal(session), "operation deadline exceeded", now.UTC(),
	); err != nil {
		return DataPlaneSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane expiry commit: %w", err)
	}
	session.State = "cancelling"
	session.TerminalKind = dataPlaneDeadlineTerminal(session)
	session.TerminalDetail = "operation deadline exceeded"
	session.ElapsedMilliseconds = max(0, now.UTC().Sub(session.CreatedAt).Milliseconds())
	session.UpdatedAt = now.UTC()
	return session, nil
}

// CancelSandboxSessions requests bounded termination of every active generation operation.
func (store *PostgresDataPlaneStore) CancelSandboxSessions(
	ctx context.Context,
	sandboxID string,
	generation int64,
	reason string,
	now time.Time,
) (int64, error) {
	if sandboxID == "" || generation < 1 || reason == "" {
		return 0, errors.New("SecondBox Sandbox session cancellation authority is incomplete")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT tenant_ref,subject_ref,id FROM secondbox.data_plane_sessions
		WHERE sandbox_id=$1 AND generation=$2 AND state IN ('pending','running')
		ORDER BY created_at,id`,
		sandboxID, generation,
	)
	if err != nil {
		return 0, fmt.Errorf("SecondBox Sandbox session cancellation lookup: %w", err)
	}
	type sessionIdentity struct {
		tenantRef  string
		subjectRef string
		sessionID  string
	}
	var sessions []sessionIdentity
	for rows.Next() {
		var identity sessionIdentity
		if err := rows.Scan(
			&identity.tenantRef, &identity.subjectRef, &identity.sessionID,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf("SecondBox Sandbox session cancellation scan: %w", err)
		}
		sessions = append(sessions, identity)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("SecondBox Sandbox session cancellation rows: %w", err)
	}
	rows.Close()
	var cancelled int64
	for _, identity := range sessions {
		changed, err := store.cancelDataPlaneSession(
			ctx, identity.tenantRef, identity.subjectRef,
			identity.sessionID, reason, now.UTC(),
		)
		if err != nil {
			return cancelled, err
		}
		if changed {
			cancelled++
		}
	}
	return cancelled, nil
}

func (store *PostgresDataPlaneStore) cancelDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	reason string,
	now time.Time,
) (bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox data-plane cancellation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return false, fmt.Errorf("SecondBox data-plane cancellation quota lock: %w", err)
	}
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return false, err
	}
	if session.State != "pending" && session.State != "running" {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("SecondBox data-plane cancellation replay commit: %w", err)
		}
		return false, nil
	}
	terminalKind := runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String()
	if session.Kind == "exec" || session.Kind == "terminal" {
		terminalKind = runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String()
	} else if session.Kind == "port" {
		terminalKind = runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED.String()
	}
	if err := store.enqueueCancellation(ctx, tx, session, terminalKind, reason, now.UTC()); err != nil {
		return false, err
	}
	if session.State == "pending" &&
		(session.Kind == "exec" || session.Kind == "terminal" || session.Kind == "file") {
		if err := store.completeUnstartedCancellation(
			ctx, tx, session, terminalKind, reason, now.UTC(),
		); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox data-plane cancellation commit: %w", err)
	}
	return true, nil
}

func (store *PostgresDataPlaneStore) completeUnstartedCancellation(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	terminalKind string,
	detail string,
	now time.Time,
) error {
	identity := dataPlaneProjection{}
	if session.Kind == "exec" || session.Kind == "terminal" {
		value, ok := runnerv1.ExecTerminalKind_value[terminalKind]
		if !ok {
			return errors.New("SecondBox unstarted Exec cancellation terminal kind is invalid")
		}
		identity.execTerm = &runnerv1.ExecTerminal{
			Kind:       runnerv1.ExecTerminalKind(value),
			SafeDetail: detail,
		}
	} else if session.Kind == "file" {
		value, ok := runnerv1.FileTerminalKind_value[terminalKind]
		if !ok {
			return errors.New("SecondBox unstarted File cancellation terminal kind is invalid")
		}
		identity.fileTerm = &runnerv1.FileTerminal{
			Kind:       runnerv1.FileTerminalKind(value),
			SafeDetail: detail,
		}
	} else {
		return errors.New("SecondBox unstarted data-plane cancellation kind is invalid")
	}
	return applyDataPlaneProjection(ctx, tx, session, identity, 0, store.retention, now)
}

func (store *PostgresDataPlaneStore) enqueueCancellation(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	terminalKind string,
	detail string,
	now time.Time,
) error {
	kind := runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC
	switch session.Kind {
	case "exec":
	case "file":
		kind = runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE
	case "terminal":
		kind = runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY
	case "port":
		kind = runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT
	default:
		return errors.New("SecondBox data-plane cancellation kind is invalid")
	}
	message := &runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_DataPlaneCancel{DataPlaneCancel: &runnerv1.DataPlaneCancelCommand{
			Fence: &runnerv1.AssignmentFence{
				AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
				InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
				FencingToken: append([]byte(nil), session.FencingToken...),
			},
			OperationId: session.ID, StreamId: session.StreamID, Kind: kind, Reason: detail,
		}},
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return fmt.Errorf("SecondBox data-plane cancellation command encoding: %w", err)
	}
	commandID := session.ID + "_cancel"
	if session.Kind == "port" {
		commandID = session.ID + "_port_cancel"
	}
	if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'data-plane-cancel',$4,'pending','',0,$5,$5,NULL)
			ON CONFLICT (id) DO NOTHING`,
		commandID, session.RunnerID, session.AssignmentID, payload, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox data-plane cancellation command insert: %w", err)
	}
	elapsedMilliseconds := max(0, now.UTC().Sub(session.CreatedAt).Milliseconds())
	limitBytes := int64(0)
	if terminalKind == runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED.String() {
		limitBytes = session.MaximumResponseBytes
	}
	if _, err := tx.Exec(ctx, `
			UPDATE secondbox.data_plane_sessions
			SET state='cancelling',terminal_kind=$2,terminal_detail=$3,
			    elapsed_milliseconds=$4,limit_bytes=$5,updated_at=$6
			WHERE id=$1`,
		session.ID, terminalKind, detail, elapsedMilliseconds, limitBytes, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox data-plane cancellation update: %w", err)
	}
	return nil
}
