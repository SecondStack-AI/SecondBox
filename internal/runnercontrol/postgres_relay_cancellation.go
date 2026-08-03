package runnercontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// dataPlaneFrameSweepByteBudget bounds payload touched by one cleanup
// transaction independently from operator-owned session and frame limits.
const dataPlaneFrameSweepByteBudget int64 = 16 << 20

func (relay *PostgresFrameRelay) CancelDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	reason string,
	now time.Time,
) (bool, error) {
	return relay.cancelDataPlaneSession(ctx, tenantRef, subjectRef, sessionID, reason, now)
}

// SweepDataPlane requests cancellation for due work and removes expired relay payloads.
func (relay *PostgresFrameRelay) SweepDataPlane(
	ctx context.Context,
	now time.Time,
	limit int,
) (bool, error) {
	if limit < 1 || limit > 1000 {
		return false, errors.New("SecondBox data-plane sweep limit is invalid")
	}
	rows, err := relay.pool.Query(ctx, `
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
			if _, err := relay.ClosePortSession(ctx, PortTunnelClose{
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
			if _, err := relay.ExpireDataPlaneSession(
				ctx, session.tenantRef, session.subjectRef, session.id, now.UTC(),
			); err != nil {
				return false, err
			}
		} else {
			if _, err := relay.cancelDataPlaneSession(
				ctx, session.tenantRef, session.subjectRef,
				session.id, session.reason, now.UTC(),
			); err != nil {
				return false, err
			}
		}
	}
	framesChanged, err := relay.sweepDataPlaneFrames(
		ctx, now.UTC(), limit, dataPlaneFrameSweepByteBudget,
	)
	if err != nil {
		return false, err
	}
	sessionsChanged, err := relay.sweepDataPlaneSessions(ctx, now.UTC(), limit)
	if err != nil {
		return false, err
	}
	return len(due) > 0 || framesChanged || sessionsChanged, nil
}

func (relay *PostgresFrameRelay) sweepDataPlaneFrames(
	ctx context.Context,
	now time.Time,
	limit int,
	byteBudget int64,
) (bool, error) {
	if byteBudget < 1 {
		return false, errors.New("SecondBox data-plane frame sweep byte budget is invalid")
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox retained frame cleanup transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT id FROM secondbox.data_plane_sessions
		WHERE state IN ('completed','failed','cancelled','expired')
		  AND frames_retain_until<=$1 AND frame_cleanup_completed_at IS NULL
		ORDER BY frames_retain_until,id
		FOR UPDATE SKIP LOCKED
		LIMIT $2`, now, limit)
	if err != nil {
		return false, fmt.Errorf("SecondBox retained frame session lookup: %w", err)
	}
	candidateIDs := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, fmt.Errorf("SecondBox retained frame session scan: %w", err)
		}
		candidateIDs = append(candidateIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("SecondBox retained frame session rows: %w", err)
	}
	rows.Close()
	frameIDs := make([]string, 0, limit)
	if len(candidateIDs) > 0 {
		frameRows, err := tx.Query(ctx, `
			SELECT frame.id,frame.payload_bytes
			FROM secondbox.data_plane_frames AS frame
			JOIN secondbox.data_plane_sessions AS session ON session.id=frame.session_id
			WHERE frame.session_id=ANY($1)
			  AND (frame.state<>'claimed' OR frame.claim_expires_at<=$2)
			ORDER BY session.frames_retain_until,frame.session_id,
			         frame.direction,frame.sequence,frame.id
			FOR UPDATE OF frame
			LIMIT $3`, candidateIDs, now, limit)
		if err != nil {
			return false, fmt.Errorf("SecondBox retained relay frame lookup: %w", err)
		}
		var payloadBytes int64
		for frameRows.Next() {
			var id string
			var size int64
			if err := frameRows.Scan(&id, &size); err != nil {
				frameRows.Close()
				return false, fmt.Errorf("SecondBox retained relay frame scan: %w", err)
			}
			if len(frameIDs) > 0 && payloadBytes+size > byteBudget {
				break
			}
			frameIDs = append(frameIDs, id)
			payloadBytes += size
		}
		if err := frameRows.Err(); err != nil {
			frameRows.Close()
			return false, fmt.Errorf("SecondBox retained relay frame rows: %w", err)
		}
		frameRows.Close()
	}
	if len(frameIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM secondbox.data_plane_frames WHERE id=ANY($1)`, frameIDs,
		); err != nil {
			return false, fmt.Errorf("SecondBox retained relay frame cleanup: %w", err)
		}
	}
	var finalized int64
	if len(candidateIDs) > 0 {
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.data_plane_sessions AS session
			SET frame_cleanup_completed_at=$2
			WHERE session.id=ANY($1)
			  AND session.frame_cleanup_completed_at IS NULL
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.data_plane_frames AS frame
			    WHERE frame.session_id=session.id
			  )`, candidateIDs, now)
		if err != nil {
			return false, fmt.Errorf("SecondBox retained frame completion update: %w", err)
		}
		finalized = tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox retained frame cleanup commit: %w", err)
	}
	return len(frameIDs) > 0 || finalized > 0, nil
}

func (relay *PostgresFrameRelay) sweepDataPlaneSessions(
	ctx context.Context,
	now time.Time,
	limit int,
) (bool, error) {
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox retained session cleanup transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT session.id FROM secondbox.data_plane_sessions AS session
		WHERE session.state IN ('completed','failed','cancelled','expired')
		  AND session.retain_until<=$1
		  AND session.frame_cleanup_completed_at IS NOT NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM secondbox.data_plane_frames AS frame
		    WHERE frame.session_id=session.id
		  )
		ORDER BY session.retain_until,session.id
		FOR UPDATE OF session SKIP LOCKED
		LIMIT $2`, now, limit)
	if err != nil {
		return false, fmt.Errorf("SecondBox retained session lookup: %w", err)
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, fmt.Errorf("SecondBox retained session scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("SecondBox retained session rows: %w", err)
	}
	rows.Close()
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM secondbox.data_plane_sessions WHERE id=ANY($1)`, ids,
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
func (relay *PostgresFrameRelay) ExpireDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	now time.Time,
) (DataPlaneSession, error) {
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane expiry transaction: %w", err)
	}
	defer tx.Rollback(ctx)
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
	if err := relay.enqueueCancellation(
		ctx, tx, session, relayDeadlineTerminal(session), "operation deadline exceeded", now.UTC(),
	); err != nil {
		return DataPlaneSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane expiry commit: %w", err)
	}
	session.State = "cancelling"
	session.TerminalKind = relayDeadlineTerminal(session)
	session.TerminalDetail = "operation deadline exceeded"
	session.ElapsedMilliseconds = max(0, now.UTC().Sub(session.CreatedAt).Milliseconds())
	session.UpdatedAt = now.UTC()
	return session, nil
}

// CancelSandboxSessions requests bounded termination of every active generation operation.
func (relay *PostgresFrameRelay) CancelSandboxSessions(
	ctx context.Context,
	sandboxID string,
	generation int64,
	reason string,
	now time.Time,
) (int64, error) {
	if sandboxID == "" || generation < 1 || reason == "" {
		return 0, errors.New("SecondBox Sandbox session cancellation authority is incomplete")
	}
	rows, err := relay.pool.Query(ctx, `
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
		changed, err := relay.cancelDataPlaneSession(
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

func (relay *PostgresFrameRelay) cancelDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	reason string,
	now time.Time,
) (bool, error) {
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox data-plane cancellation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
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
	}
	if err := relay.enqueueCancellation(ctx, tx, session, terminalKind, reason, now.UTC()); err != nil {
		return false, err
	}
	if session.State == "pending" && (session.Kind == "exec" || session.Kind == "file") {
		if err := relay.completeUnstartedCancellation(
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

func (relay *PostgresFrameRelay) completeUnstartedCancellation(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	terminalKind string,
	detail string,
	now time.Time,
) error {
	identity := inboundIdentity{sequence: 1}
	if session.Kind == "exec" {
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
	return applyInboundPayload(ctx, tx, session, identity, 0, "", relay.retention, now)
}

func (relay *PostgresFrameRelay) enqueueCancellation(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	terminalKind string,
	detail string,
	now time.Time,
) error {
	if session.Kind == "exec" || session.Kind == "file" {
		kind := runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC
		if session.Kind == "file" {
			kind = runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE
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
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'data-plane-cancel',$4,'pending','',0,$5,$5,NULL)
			ON CONFLICT (id) DO NOTHING`,
			session.ID+"_cancel", session.RunnerID, session.AssignmentID, payload, now.UTC(),
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
	sequence := session.NextOutboundSequence
	message := cancellationMessage(session, uint64(sequence), detail)
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return fmt.Errorf("SecondBox cancellation frame encoding: %w", err)
	}
	hash := sha256.Sum256(payload)
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at
		) VALUES ($1,$2,'outbound',$3,$4,$5,$6,-100,'pending','',NULL,0,$7,$7,NULL)`,
		fmt.Sprintf("%s_cancel_%d", session.ID, sequence), session.ID, sequence,
		hex.EncodeToString(hash[:]), payload, len(payload), now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox cancellation frame insert: %w", err)
	}
	elapsedMilliseconds := max(0, now.UTC().Sub(session.CreatedAt).Milliseconds())
	limitBytes := int64(0)
	if terminalKind == runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED.String() {
		limitBytes = session.MaximumResponseBytes
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET state='cancelling',terminal_kind=$2,terminal_detail=$3,
		    elapsed_milliseconds=$4,limit_bytes=$5,
		    next_outbound_sequence=next_outbound_sequence+1,
		    frames_retain_until=retain_until,frame_cleanup_completed_at=NULL,
		    updated_at=$6
		WHERE id=$1`,
		session.ID, terminalKind, detail, elapsedMilliseconds, limitBytes, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox data-plane cancellation update: %w", err)
	}
	return nil
}
