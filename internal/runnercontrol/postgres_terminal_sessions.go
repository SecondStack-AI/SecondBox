package runnercontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

type ExecClientFrame struct {
	Sequence int64
	Input    []byte
	EndInput bool
	Credit   int64
	Cancel   bool
}

// ExecServerFrame is one durable Runner frame projected onto the public WebSocket.
type ExecServerFrame struct {
	Sequence int64
	Output   *runnerv1.ExecOutput
	Terminal *runnerv1.ExecTerminal
}

// TerminalClientFrame is exactly one ordered public PTY input, resize, credit, or cancellation.
type TerminalClientFrame struct {
	Sequence      int64
	Input         []byte
	ResizeRows    uint32
	ResizeColumns uint32
	Credit        int64
	Cancel        bool
}

// TerminalServerFrame is one live PTY output or terminal acknowledgement.
type TerminalServerFrame struct {
	Sequence int64
	Output   []byte
	Terminal *runnerv1.ExecTerminal
}

// AcquireTerminalAttachment atomically grants the only active public attachment.
func (store *PostgresDataPlaneStore) AcquireTerminalAttachment(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	sessionID string,
	generation int64,
	attachmentID string,
	now time.Time,
) (DataPlaneSession, error) {
	if tenantRef == "" || subjectRef == "" || sandboxID == "" ||
		sessionID == "" || generation < 1 || attachmentID == "" {
		return DataPlaneSession{}, errors.New("SecondBox Terminal attachment authority is incomplete")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal attachment transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if err := hydrateDataPlaneTransport(ctx, tx, &session); err != nil {
		return DataPlaneSession{}, err
	}
	if session.Kind != "terminal" || session.Operation != "terminal" ||
		session.SandboxID != sandboxID || session.Generation != generation {
		return DataPlaneSession{}, ErrDataPlaneNotFound
	}
	if session.SubjectRef != subjectRef {
		return DataPlaneSession{}, ports.ErrAuthorizationDenied
	}
	if session.State != "pending" && session.State != "running" {
		return DataPlaneSession{}, ErrTerminalDetached
	}
	if session.AttachmentID != "" {
		return DataPlaneSession{}, ErrTerminalAttached
	}
	if session.DetachExpiresAt != nil && !now.UTC().Before(*session.DetachExpiresAt) {
		if err := store.enqueueCancellation(
			ctx, tx, session,
			runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(),
			"Terminal detach interval expired", now.UTC(),
		); err != nil {
			return DataPlaneSession{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal detach expiry commit: %w", err)
		}
		return DataPlaneSession{}, ErrTerminalDetached
	}
	var sandboxState, assignmentState, leaseState string
	var currentGeneration, leaseGeneration int64
	var currentInstance, assignmentID, runnerID, leaseAccount string
	var currentFence []byte
	var leaseExpiry time.Time
	err = tx.QueryRow(ctx, `
		SELECT sandbox.state,sandbox.generation,sandbox.current_instance_id,
		       assignment.id,assignment.runner_id,assignment.fencing_token,assignment.state,
		       lease.generation,lease.subject_ref,lease.state,lease.expires_at
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.assignments AS assignment
		  ON assignment.sandbox_id=sandbox.id
		  AND assignment.instance_id=sandbox.current_instance_id
		  AND assignment.generation=sandbox.generation
		JOIN secondbox.leases AS lease
		  ON lease.tenant_ref=sandbox.tenant_ref
		  AND lease.subject_ref=sandbox.subject_ref
		  AND lease.sandbox_id=sandbox.id
		  AND lease.id=$4
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2
		  AND sandbox.id=$3 AND sandbox.generation=$5
		FOR UPDATE OF sandbox,assignment,lease`,
		tenantRef, subjectRef, sandboxID, session.LeaseID, generation,
	).Scan(
		&sandboxState, &currentGeneration, &currentInstance,
		&assignmentID, &runnerID, &currentFence, &assignmentState,
		&leaseGeneration, &leaseAccount, &leaseState, &leaseExpiry,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DataPlaneSession{}, ports.ErrGenerationFenced
	}
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal attachment authority lookup: %w", err)
	}
	if sandboxState != contracts.SandboxStateReady ||
		currentGeneration != session.Generation ||
		currentInstance != session.InstanceID ||
		assignmentID != session.AssignmentID ||
		runnerID != session.RunnerID ||
		!bytes.Equal(currentFence, session.FencingToken) ||
		assignmentState != "ready" {
		return DataPlaneSession{}, ports.ErrGenerationFenced
	}
	if leaseGeneration != session.Generation ||
		leaseAccount != subjectRef ||
		leaseState != contracts.LeaseStateActive ||
		!now.UTC().Before(leaseExpiry) {
		return DataPlaneSession{}, ports.ErrLeaseInactive
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET attachment_id=$2,attached_at=$3,detached_at=NULL,detach_expires_at=NULL,updated_at=$3
		WHERE id=$1`,
		session.ID, attachmentID, now.UTC(),
	); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal attachment update: %w", err)
	}
	if err := touchDataPlaneActivity(ctx, tx, session, now.UTC()); err != nil {
		return DataPlaneSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal attachment commit: %w", err)
	}
	session.AttachmentID = attachmentID
	attachedAt := now.UTC()
	session.AttachedAt = &attachedAt
	session.DetachedAt = nil
	session.DetachExpiresAt = nil
	return session, nil
}

// DetachTerminalAttachment releases one active attachment or requests cancellation.
func (store *PostgresDataPlaneStore) DetachTerminalAttachment(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	attachmentID string,
	now time.Time,
) (bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox Terminal detach transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return false, err
	}
	if session.Kind != "terminal" || session.Operation != "terminal" {
		return false, ErrDataPlaneNotFound
	}
	if session.AttachmentID != attachmentID || attachmentID == "" {
		return false, ErrTerminalDetached
	}
	if session.State != "pending" && session.State != "running" {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.data_plane_sessions
			SET attachment_id='',updated_at=$2 WHERE id=$1`,
			session.ID, now.UTC(),
		); err != nil {
			return false, fmt.Errorf("SecondBox terminal attachment cleanup: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("SecondBox terminal attachment cleanup commit: %w", err)
		}
		return true, nil
	}
	if !session.Detachable || session.TerminalDetachSeconds == 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.data_plane_sessions
			SET attachment_id='',detached_at=$2,updated_at=$2 WHERE id=$1`,
			session.ID, now.UTC(),
		); err != nil {
			return false, fmt.Errorf("SecondBox non-detachable Terminal cleanup: %w", err)
		}
		session.AttachmentID = ""
		if err := store.enqueueCancellation(
			ctx, tx, session,
			runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(),
			"public Terminal client disconnected", now.UTC(),
		); err != nil {
			return false, err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.data_plane_sessions
			SET attachment_id='',detached_at=$2,
			    detach_expires_at=$2::timestamptz+($3::bigint * interval '1 second'),updated_at=$2
			WHERE id=$1`,
			session.ID, now.UTC(), session.TerminalDetachSeconds,
		); err != nil {
			return false, fmt.Errorf("SecondBox Terminal detach update: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox Terminal detach commit: %w", err)
	}
	return true, nil
}

// RecordTerminalClientFrame advances the compact ordered Terminal projection
// without retaining the input, resize, credit, or cancellation payload.
func (store *PostgresDataPlaneStore) RecordTerminalClientFrame(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	attachmentID string,
	frame TerminalClientFrame,
	now time.Time,
) (bool, error) {
	if tenantRef == "" || subjectRef == "" || sessionID == "" ||
		attachmentID == "" || frame.Sequence < 0 || now.IsZero() {
		return false, errors.New("SecondBox live Terminal frame identity is incomplete")
	}
	kinds := 0
	if frame.Input != nil {
		kinds++
	}
	if frame.ResizeRows != 0 || frame.ResizeColumns != 0 {
		kinds++
	}
	if frame.Credit != 0 {
		kinds++
	}
	if frame.Cancel {
		kinds++
	}
	if kinds != 1 || frame.Credit < 0 ||
		((frame.ResizeRows == 0) != (frame.ResizeColumns == 0)) ||
		(frame.Input != nil && len(frame.Input) == 0) ||
		frame.ResizeRows > 1000 || frame.ResizeColumns > 1000 {
		return false, errors.New("SecondBox live Terminal frame requires exactly one valid payload")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox live Terminal frame transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return false, err
	}
	if session.Kind != "terminal" || session.Operation != "terminal" {
		return false, ErrDataPlaneNotFound
	}
	if session.AttachmentID != attachmentID {
		return false, ErrTerminalDetached
	}
	if session.State != "pending" && session.State != "running" {
		return false, ErrDataPlaneSequence
	}
	if frame.Sequence != session.NextClientSequence {
		return false, ErrDataPlaneSequence
	}
	if session.RequestStreamBytes+int64(len(frame.Input)) > session.MaximumRequestBytes {
		return false, ErrDataPlaneSessionLimit
	}
	if frame.Credit > 0 &&
		session.ResponseCreditBytes-sessionInboundPayloadBytes(session)+frame.Credit >
			session.StreamWindowBytes {
		return false, ErrDataPlaneFrameLimit
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET request_stream_bytes=request_stream_bytes+$2,
		    outbound_bytes=outbound_bytes+$3,
		    response_credit_bytes=response_credit_bytes+$4,
		    next_outbound_sequence=next_outbound_sequence+1,
		    state=CASE WHEN $5 THEN 'cancelling' ELSE state END,
		    terminal_kind=CASE WHEN $5 THEN $6 ELSE terminal_kind END,
		    terminal_detail=CASE WHEN $5 THEN 'public Terminal client cancellation' ELSE terminal_detail END,
		    updated_at=$7
		WHERE id=$1`,
		session.ID, len(frame.Input), len(frame.Input), frame.Credit, frame.Cancel,
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(), now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox live Terminal session update: %w", err)
	}
	if err := touchDataPlaneActivity(ctx, tx, session, now.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox live Terminal frame commit: %w", err)
	}
	return true, nil
}

// RecordTerminalServerFrame advances Terminal output and terminal evidence
// without retaining a replayable payload in PostgreSQL.
func (store *PostgresDataPlaneStore) RecordTerminalServerFrame(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	frame TerminalServerFrame,
	now time.Time,
) (DataPlaneSession, error) {
	if tenantRef == "" || subjectRef == "" || sessionID == "" ||
		frame.Sequence < 0 || now.IsZero() || (frame.Output == nil) == (frame.Terminal == nil) {
		return DataPlaneSession{}, errors.New("SecondBox live Terminal response is incomplete")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox live Terminal response transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if session.Kind != "terminal" || session.Operation != "terminal" {
		return DataPlaneSession{}, ErrDataPlaneNotFound
	}
	runnerSequence := frame.Sequence + 1
	var nextInbound int64
	if err := tx.QueryRow(ctx, `
		SELECT next_inbound_sequence FROM secondbox.data_plane_sessions WHERE id=$1`,
		session.ID,
	).Scan(&nextInbound); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox live Terminal response sequence lookup: %w", err)
	}
	if runnerSequence < nextInbound {
		if err := tx.Commit(ctx); err != nil {
			return DataPlaneSession{}, fmt.Errorf("SecondBox live Terminal replay commit: %w", err)
		}
		return session, nil
	}
	if runnerSequence != nextInbound ||
		sessionInboundPayloadBytes(session)+int64(len(frame.Output)) > session.MaximumResponseBytes ||
		sessionInboundPayloadBytes(session)+int64(len(frame.Output)) > session.ResponseCreditBytes {
		return DataPlaneSession{}, ErrDataPlaneSequence
	}
	identity := dataPlaneProjection{
		ptyOutput: &runnerv1.ExecOutput{
			Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
			Data:    bytes.Clone(frame.Output),
		},
		ptyTerm: frame.Terminal,
	}
	if err := applyDataPlaneProjection(
		ctx, tx, session, identity, int64(len(frame.Output)), store.retention, now.UTC(),
	); err != nil {
		return DataPlaneSession{}, err
	}
	updated, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE id=$1`, session.ID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox live Terminal response commit: %w", err)
	}
	return updated, nil
}

func sessionInboundPayloadBytes(session DataPlaneSession) int64 {
	return session.InboundBytes
}
