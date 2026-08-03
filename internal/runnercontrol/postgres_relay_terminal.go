package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
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

// TerminalServerFrame is one retained PTY output or terminal acknowledgement.
type TerminalServerFrame struct {
	Sequence int64
	Output   []byte
	Terminal *runnerv1.ExecTerminal
}

// AcquireTerminalAttachment atomically grants the only active public attachment.
func (relay *PostgresFrameRelay) AcquireTerminalAttachment(
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
	tx, err := relay.pool.Begin(ctx)
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
		if err := relay.enqueueCancellation(
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
func (relay *PostgresFrameRelay) DetachTerminalAttachment(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	attachmentID string,
	now time.Time,
) (bool, error) {
	tx, err := relay.pool.Begin(ctx)
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
			SET attachment_id='',frames_retain_until=LEAST(frames_retain_until,$2),
			    updated_at=$2 WHERE id=$1`,
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
		if err := relay.enqueueCancellation(
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

// AppendTerminalClientFrame durably appends one exactly ordered attached PTY control.
func (relay *PostgresFrameRelay) AppendTerminalClientFrame(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	attachmentID string,
	frame TerminalClientFrame,
	now time.Time,
) (bool, error) {
	if tenantRef == "" || subjectRef == "" ||
		sessionID == "" || attachmentID == "" || frame.Sequence < 0 {
		return false, errors.New("SecondBox public Terminal frame identity is incomplete")
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
		((frame.ResizeRows == 0) != (frame.ResizeColumns == 0)) {
		return false, errors.New("SecondBox public Terminal frame requires exactly one valid payload")
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox public Terminal frame transaction: %w", err)
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
		return false, ErrRelaySequence
	}
	if frame.Input != nil && len(frame.Input) == 0 {
		return false, errors.New("SecondBox public Terminal input is empty")
	}
	if frame.ResizeRows > 1000 || frame.ResizeColumns > 1000 {
		return false, errors.New("SecondBox public Terminal resize is invalid")
	}
	if frame.Input != nil &&
		session.RequestStreamBytes+int64(len(frame.Input)) > session.MaximumRequestBytes {
		return false, ErrRelaySessionLimit
	}
	if frame.Credit > 0 {
		emitted := int64(len(session.Stdout) + len(session.Stderr))
		if session.ResponseCreditBytes-emitted+frame.Credit > session.StreamWindowBytes {
			return false, ErrRelayFrameLimit
		}
	}
	runnerSequence := frame.Sequence + 2
	fence := &runnerv1.AssignmentFence{
		AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
		InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
		FencingToken: bytes.Clone(session.FencingToken),
	}
	var message *runnerv1.ControlPlaneToRunner
	if frame.Cancel {
		message = &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: session.ID, StreamId: session.StreamID,
				Sequence: uint64(runnerSequence), Correlation: dataPlaneCorrelation(session),
				Payload: &runnerv1.ExecFrame_Cancel{Cancel: &runnerv1.ExecCancel{
					Reason: "public Terminal client cancellation",
				}},
			}},
		}
	} else {
		pty := &runnerv1.PtyFrame{
			Fence: fence, OperationId: session.ID, StreamId: session.StreamID,
			Sequence: uint64(runnerSequence), Correlation: dataPlaneCorrelation(session),
		}
		switch {
		case frame.Input != nil:
			pty.Payload = &runnerv1.PtyFrame_Input{Input: &runnerv1.PtyInput{Data: bytes.Clone(frame.Input)}}
		case frame.Credit > 0:
			pty.Payload = &runnerv1.PtyFrame_Credit{Credit: &runnerv1.StreamCredit{ByteCount: uint64(frame.Credit)}}
		default:
			pty.Payload = &runnerv1.PtyFrame_Resize{Resize: &runnerv1.PtyResize{
				Rows: frame.ResizeRows, Columns: frame.ResizeColumns,
			}}
		}
		message = &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Pty{Pty: pty},
		}
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return false, fmt.Errorf("SecondBox public Terminal frame encoding: %w", err)
	}
	if int64(len(payload)) > relay.maximumFrameBytes {
		return false, ErrRelayFrameLimit
	}
	hash := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(hash[:])
	var priorHash string
	err = tx.QueryRow(ctx, `
		SELECT payload_hash FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='outbound' AND sequence=$2`,
		session.ID, runnerSequence,
	).Scan(&priorHash)
	if err == nil {
		if priorHash != payloadHash {
			return false, ErrRelaySequence
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("SecondBox public Terminal duplicate commit: %w", err)
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("SecondBox public Terminal sequence lookup: %w", err)
	}
	if runnerSequence != session.NextOutboundSequence {
		return false, ErrRelaySequence
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at
		) VALUES ($1,$2,'outbound',$3,$4,$5,$6,0,'pending','',NULL,0,$7,$7,NULL)`,
		fmt.Sprintf("%s_terminal_%d", session.ID, frame.Sequence), session.ID,
		runnerSequence, payloadHash, payload, len(payload), now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox public Terminal frame insert: %w", err)
	}
	inputBytes := int64(len(frame.Input))
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET request_stream_bytes=request_stream_bytes+$2,
		    outbound_bytes=outbound_bytes+$3,
		    response_credit_bytes=response_credit_bytes+$4,
		    next_outbound_sequence=next_outbound_sequence+1,
		    frames_retain_until=retain_until,frame_cleanup_completed_at=NULL,
		    state=CASE WHEN $5 THEN 'cancelling' ELSE state END,
		    terminal_kind=CASE WHEN $5 THEN $6 ELSE terminal_kind END,
		    terminal_detail=CASE WHEN $5 THEN 'public Terminal client cancellation' ELSE terminal_detail END,
		    updated_at=$7
		WHERE id=$1`,
		session.ID, inputBytes, len(payload), frame.Credit, frame.Cancel,
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(), now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox public Terminal session update: %w", err)
	}
	if err := touchDataPlaneActivity(ctx, tx, session, now.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox public Terminal frame commit: %w", err)
	}
	return true, nil
}

// ListTerminalServerFrames returns retained PTY output and terminal frames.
func (relay *PostgresFrameRelay) ListTerminalServerFrames(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	afterSequence int64,
	limit int,
) ([]TerminalServerFrame, error) {
	if tenantRef == "" || subjectRef == "" ||
		sessionID == "" || afterSequence < -1 || limit < 1 || limit > 256 {
		return nil, errors.New("SecondBox public Terminal frame query is invalid")
	}
	var kind, operation string
	if err := relay.pool.QueryRow(ctx, `
		SELECT kind,operation FROM secondbox.data_plane_sessions
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, sessionID,
	).Scan(&kind, &operation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDataPlaneNotFound
		}
		return nil, fmt.Errorf("SecondBox public Terminal session lookup: %w", err)
	}
	if kind != "terminal" || operation != "terminal" {
		return nil, ErrDataPlaneNotFound
	}
	rows, err := relay.pool.Query(ctx, `
		SELECT sequence,payload FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='inbound' AND sequence>$2
		ORDER BY sequence
		LIMIT $3`, sessionID, afterSequence+1, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox public Terminal frame lookup: %w", err)
	}
	defer rows.Close()
	result := make([]TerminalServerFrame, 0, limit)
	for rows.Next() {
		var runnerSequence int64
		var payload []byte
		if err := rows.Scan(&runnerSequence, &payload); err != nil {
			return nil, fmt.Errorf("SecondBox public Terminal frame scan: %w", err)
		}
		var message runnerv1.RunnerToControlPlane
		if err := proto.Unmarshal(payload, &message); err != nil {
			return nil, fmt.Errorf("SecondBox public Terminal frame decoding: %w", err)
		}
		pty := message.GetPty()
		if pty == nil || (pty.GetOutput() == nil && pty.GetTerminal() == nil) {
			return nil, errors.New("SecondBox public Terminal retained frame is invalid")
		}
		var output []byte
		if pty.GetOutput() != nil {
			output = bytes.Clone(pty.GetOutput().Data)
		}
		result = append(result, TerminalServerFrame{
			Sequence: runnerSequence - 1, Output: output, Terminal: pty.GetTerminal(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox public Terminal frame rows: %w", err)
	}
	return result, nil
}
