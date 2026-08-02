package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

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
		WITH updated_instance AS (
		  UPDATE secondbox.instances AS instance
		  SET guest_liveness=$5,guest_heartbeat_at=$6,updated_at=$6
		  FROM secondbox.sandboxes AS sandbox
		  WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2
		    AND sandbox.id=$3 AND sandbox.generation=$4
		    AND instance.id=sandbox.current_instance_id AND instance.generation=$4
		  RETURNING instance.id,instance.sandbox_id,instance.generation,instance.state,
		            instance.guest_liveness,instance.termination_reason,instance.created_at,
		            instance.updated_at,instance.ready_at,instance.guest_heartbeat_at,instance.stopped_at
		), lifecycle_wakeup AS (
		  UPDATE secondbox.sandboxes AS sandbox
		  SET next_reconcile_at=$6,reconcile_owner='',reconcile_claim_expires_at=NULL,
		      revision=revision+1,updated_at=$6
		  FROM updated_instance AS instance
		  WHERE $5 IN ('lost','stopped')
		    AND sandbox.id=instance.sandbox_id
		    AND sandbox.generation=instance.generation
		    AND sandbox.current_instance_id=instance.id
		)
		SELECT id,sandbox_id,generation,state,guest_liveness,termination_reason,created_at,
		       updated_at,ready_at,guest_heartbeat_at,stopped_at
		FROM updated_instance`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.Generation,
		liveness, input.Now.UTC(),
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
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$3`,
		input.TenantRef, input.SubjectRef, input.SandboxID,
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
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef +
		"\x1ftouch\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return time.Time{}, fmt.Errorf("SecondBox touch idempotency lock failed: %w", err)
	}
	var priorHash string
	var priorActivityAt time.Time
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,last_activity_at FROM secondbox.activity_touches
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND idempotency_key=$4`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.IdempotencyKey,
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
		UPDATE secondbox.sandboxes SET last_activity_at=$5,revision=revision+1,updated_at=$5
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 AND generation=$4`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.Generation, input.Now.UTC(),
	); err != nil {
		return time.Time{}, fmt.Errorf("SecondBox useful activity update failed: %w", err)
	}
	if input.Session.ID != "" {
		tag, err := tx.Exec(ctx, `
			UPDATE secondbox.activity_sessions SET last_activity_at=$6,updated_at=$6
			WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
			  AND generation=$4 AND id=$5 AND state='active'`,
			input.TenantRef, input.SubjectRef, input.SandboxID,
			input.Generation, input.Session.ID, input.Now.UTC(),
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
			tenant_ref,subject_ref,sandbox_id,generation,lease_id,idempotency_key,request_hash,last_activity_at,created_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$8
		)`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.Generation, input.LeaseID, input.IdempotencyKey, input.RequestHash, input.Now.UTC(),
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
	session.TenantRef, session.SandboxID, session.Generation = input.TenantRef, input.SandboxID, input.Generation
	session.TenantRef, session.SubjectRef = input.TenantRef, input.SubjectRef
	session.State, session.LeaseID = contracts.ActivitySessionStateActive, input.LeaseID
	session.CreatedAt, session.LastActivityAt = input.Now.UTC(), input.Now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,last_activity_at,
			created_at,updated_at,closed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,NULL)`,
		session.ID, session.TenantRef, session.SubjectRef,
		session.SandboxID, session.Generation, session.Kind,
		session.State, session.LeaseID, session.LastActivityAt, session.CreatedAt,
	); err != nil {
		return contracts.ActivitySession{}, fmt.Errorf("SecondBox activity session insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$5,revision=revision+1,updated_at=$5
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 AND generation=$4`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.Generation, input.Now.UTC(),
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
		SET state='closed',closed_at=$6,updated_at=$6
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND generation=$4 AND id=$5 AND state='active'
		RETURNING id,tenant_ref,subject_ref,sandbox_id,generation,
		          kind,state,lease_id,last_activity_at,created_at,closed_at`,
		input.TenantRef, input.SubjectRef, input.SandboxID,
		input.Generation, input.Session.ID, input.Now.UTC(),
	).Scan(
		&session.ID, &session.TenantRef, &session.SubjectRef,
		&session.SandboxID, &session.Generation,
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

func lockCurrentSubjectGeneration(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	generation int64,
	allowDraining bool,
) error {
	var currentGeneration int64
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT generation,state FROM secondbox.sandboxes
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sandboxID,
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
	if err := lockCurrentSubjectGeneration(
		ctx, tx, input.TenantRef, input.SubjectRef,
		input.SandboxID, input.Generation, true,
	); err != nil {
		return err
	}
	if input.LeaseID == "" {
		return nil
	}
	var generation int64
	var leaseSubjectRef, state string
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT generation,subject_ref,state,expires_at FROM secondbox.leases
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND id=$4 FOR UPDATE`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.LeaseID,
	).Scan(&generation, &leaseSubjectRef, &state, &expiresAt); err != nil {
		return mapNotFound(err, ports.ErrLeaseNotFound)
	}
	if generation != input.Generation || leaseSubjectRef != input.SubjectRef ||
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
