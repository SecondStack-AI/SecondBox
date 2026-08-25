package runnercontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
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

// TerminalCheckpoint is one absolute, payload-free Terminal accounting projection.
type TerminalCheckpoint struct {
	AttachmentID        string
	NextClientSequence  int64
	RequestBytes        int64
	ResponseCredit      int64
	InboundBytes        int64
	NextInboundSequence int64
	RecoveryAllowance   int64
	Cancel              bool
	Terminal            *runnerv1.ExecTerminal
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
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal attachment quota lock: %w", err)
	}
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
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return false, fmt.Errorf("SecondBox Terminal detach quota lock: %w", err)
	}
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

// CheckpointTerminal persists one compact accounting projection without retaining
// any input, resize, credit, output, or cancellation payload.
func (store *PostgresDataPlaneStore) CheckpointTerminal(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	checkpoint TerminalCheckpoint,
	now time.Time,
) (DataPlaneSession, error) {
	if tenantRef == "" || subjectRef == "" || sessionID == "" ||
		(checkpoint.AttachmentID == "" && checkpoint.Terminal == nil) || now.IsZero() {
		return DataPlaneSession{}, errors.New("SecondBox Terminal checkpoint identity is incomplete")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal checkpoint transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal checkpoint quota lock: %w", err)
	}
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	updated, err := store.applyTerminalCheckpoint(ctx, tx, session, checkpoint, now.UTC())
	if err != nil {
		return DataPlaneSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal checkpoint commit: %w", err)
	}
	return updated, nil
}

func (store *PostgresDataPlaneStore) applyTerminalCheckpoint(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	checkpoint TerminalCheckpoint,
	now time.Time,
) (DataPlaneSession, error) {
	if session.Kind != "terminal" || session.Operation != "terminal" {
		return DataPlaneSession{}, ErrDataPlaneNotFound
	}
	if checkpoint.AttachmentID != session.AttachmentID ||
		(checkpoint.AttachmentID == "" && checkpoint.Terminal == nil) {
		return DataPlaneSession{}, ErrTerminalDetached
	}
	if session.State != "pending" && session.State != "running" && session.State != "cancelling" {
		if checkpoint.Terminal != nil && session.CompletedAt != nil {
			return session, nil
		}
		return DataPlaneSession{}, ErrDataPlaneSequence
	}
	if checkpoint.NextClientSequence < session.NextClientSequence ||
		checkpoint.RequestBytes < session.RequestStreamBytes ||
		checkpoint.ResponseCredit < session.ResponseCreditBytes ||
		checkpoint.InboundBytes < session.InboundBytes ||
		checkpoint.NextInboundSequence < session.NextInboundSequence {
		return DataPlaneSession{}, ErrDataPlaneSequence
	}
	if checkpoint.RequestBytes > session.MaximumRequestBytes ||
		checkpoint.InboundBytes > session.MaximumResponseBytes ||
		checkpoint.ResponseCredit < checkpoint.InboundBytes ||
		(checkpoint.RecoveryAllowance != 0 && checkpoint.RecoveryAllowance != session.StreamWindowBytes) ||
		checkpoint.ResponseCredit-checkpoint.InboundBytes > session.StreamWindowBytes+checkpoint.RecoveryAllowance ||
		checkpoint.NextClientSequence > int64(^uint64(0)>>1)-2 ||
		checkpoint.NextInboundSequence < 1 {
		return DataPlaneSession{}, ErrDataPlaneSessionLimit
	}
	nextInbound := checkpoint.NextInboundSequence
	if checkpoint.Terminal != nil {
		if nextInbound <= session.NextInboundSequence {
			return DataPlaneSession{}, ErrDataPlaneSequence
		}
		nextInbound--
	}
	// A periodic checkpoint may lag until the configured poll interval, while
	// detach, close, and terminal outcome flush synchronously. After a crash the
	// client supplies its last acknowledged output sequence for Runner replay,
	// the Runner supplies its authoritative next input sequence and enforces its
	// own credit, and the control plane grants at most one stream window while
	// reconstructing the gap. Sequence and credit staleness are therefore both
	// bounded without putting PostgreSQL on the per-frame payload path.
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET request_stream_bytes=$2,outbound_bytes=$2,response_credit_bytes=$3,
		    inbound_bytes=$4,next_outbound_sequence=$5,next_inbound_sequence=$6,
		    state=CASE WHEN $7 THEN 'cancelling' ELSE state END,
		    terminal_kind=CASE WHEN $7 THEN $8 ELSE terminal_kind END,
		    terminal_detail=CASE WHEN $7 THEN 'public Terminal client cancellation' ELSE terminal_detail END,
		    updated_at=$9
		WHERE id=$1`,
		session.ID, checkpoint.RequestBytes, checkpoint.ResponseCredit,
		checkpoint.InboundBytes, checkpoint.NextClientSequence+2, nextInbound,
		checkpoint.Cancel,
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(), now.UTC(),
	); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox Terminal checkpoint update: %w", err)
	}
	if checkpoint.Terminal != nil {
		session.RequestStreamBytes = checkpoint.RequestBytes
		session.ResponseCreditBytes = checkpoint.ResponseCredit
		session.InboundBytes = checkpoint.InboundBytes
		session.NextInboundSequence = nextInbound
		if checkpoint.Cancel {
			session.State = "cancelling"
			session.TerminalKind = runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String()
			session.TerminalDetail = "public Terminal client cancellation"
		}
		if err := applyDataPlaneProjection(
			ctx, tx, session, dataPlaneProjection{ptyTerm: checkpoint.Terminal},
			0, store.retention, now.UTC(),
		); err != nil {
			return DataPlaneSession{}, err
		}
	} else if err := touchDataPlaneActivity(ctx, tx, session, now.UTC()); err != nil {
		return DataPlaneSession{}, err
	}
	updated, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+` WHERE id=$1`, session.ID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	return updated, nil
}
