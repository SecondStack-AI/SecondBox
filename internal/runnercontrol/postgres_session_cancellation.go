package runnercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/jackc/pgx/v5"
)

// CancelPublicDataPlaneSession atomically records a key-scoped response and requests cancellation.
func (relay *PostgresFrameRelay) CancelPublicDataPlaneSession(
	ctx context.Context,
	input PublicDataPlaneCancellation,
) (DataPlaneSession, bool, error) {
	if err := validatePublicDataPlaneCancellation(input); err != nil {
		return DataPlaneSession{}, false, err
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	idempotencyOperation := publicDataPlaneCancellationOperation(input)
	scope := input.TenantRef + "\x1f" + input.SubjectRef + "\x1f" +
		idempotencyOperation + "\x1f" +
		input.SessionID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, scope,
	); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation idempotency lock: %w", err)
	}
	replayedSession, found, err := lookupPublicDataPlaneCancellation(
		ctx, tx, input, idempotencyOperation,
	)
	if err != nil {
		return DataPlaneSession{}, false, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation replay commit: %w", err)
		}
		return replayedSession, true, nil
	}

	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		input.TenantRef, input.SubjectRef, input.SessionID,
	))
	if err != nil {
		return DataPlaneSession{}, false, err
	}
	if session.SandboxID != input.SandboxID ||
		session.Kind != input.SessionKind ||
		session.Operation != input.SessionOperation {
		return DataPlaneSession{}, false, ErrDataPlaneNotFound
	}
	if session.Generation != input.Generation {
		return DataPlaneSession{}, false, ports.ErrGenerationFenced
	}
	if session.State == "pending" || session.State == "running" {
		if err := relay.enqueueCancellation(
			ctx, tx, session,
			runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(),
			input.Reason, input.Now.UTC(),
		); err != nil {
			return DataPlaneSession{}, false, err
		}
	}
	session, err = scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		input.TenantRef, input.SubjectRef, input.SessionID,
	))
	if err != nil {
		return DataPlaneSession{}, false, err
	}
	responseJSON, err := json.Marshal(session)
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation response encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,response_resource_id,
			response_json,created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		input.TenantRef, input.SubjectRef,
		idempotencyOperation, input.SessionID,
		input.IdempotencyKey, input.RequestHash, session.ID, responseJSON,
		input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation idempotency insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation commit: %w", err)
	}
	return session, false, nil
}

func lookupPublicDataPlaneCancellation(
	ctx context.Context,
	tx pgx.Tx,
	input PublicDataPlaneCancellation,
	idempotencyOperation string,
) (DataPlaneSession, bool, error) {
	var requestHash string
	var responseJSON []byte
	var responseResourceID string
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id,response_json,expires_at
		FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
		input.TenantRef, input.SubjectRef,
		idempotencyOperation, input.SessionID, input.IdempotencyKey,
	).Scan(&requestHash, &responseResourceID, &responseJSON, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DataPlaneSession{}, false, nil
	}
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation idempotency lookup: %w", err)
	}
	if !expiresAt.After(input.Now.UTC()) {
		if _, err := tx.Exec(ctx, `
			DELETE FROM secondbox.idempotency_records
			WHERE tenant_ref=$1 AND subject_ref=$2
			  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
			input.TenantRef, input.SubjectRef,
			idempotencyOperation, input.SessionID, input.IdempotencyKey,
		); err != nil {
			return DataPlaneSession{}, false, fmt.Errorf("SecondBox expired public session cancellation cleanup: %w", err)
		}
		return DataPlaneSession{}, false, nil
	}
	if requestHash != input.RequestHash {
		return DataPlaneSession{}, false, ports.ErrIdempotencyConflict
	}
	if responseResourceID != input.SessionID || len(responseJSON) == 0 {
		return DataPlaneSession{}, false, errors.New("SecondBox public session cancellation response is invalid")
	}
	var session DataPlaneSession
	if err := json.Unmarshal(responseJSON, &session); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox public session cancellation response decoding: %w", err)
	}
	if session.ID != input.SessionID {
		return DataPlaneSession{}, false, errors.New("SecondBox public session cancellation response identity is invalid")
	}
	return session, true, nil
}

func validatePublicDataPlaneCancellation(input PublicDataPlaneCancellation) error {
	validOperation := input.SessionKind == "exec" &&
		input.SessionOperation == "exec-stream" ||
		input.SessionKind == "terminal" &&
			input.SessionOperation == "terminal"
	if input.TenantRef == "" || input.SubjectRef == "" ||
		input.SandboxID == "" || input.SessionID == "" ||
		input.IdempotencyKey == "" || input.RequestHash == "" || input.Reason == "" ||
		input.Generation < 1 || input.Now.IsZero() ||
		!input.IdempotencyEnds.After(input.Now) || !validOperation {
		return errors.New("SecondBox public session cancellation authority is incomplete")
	}
	return nil
}

func publicDataPlaneCancellationOperation(input PublicDataPlaneCancellation) string {
	if input.SessionKind == "exec" {
		return "exec-stream-cancel"
	}
	return "terminal-cancel"
}
