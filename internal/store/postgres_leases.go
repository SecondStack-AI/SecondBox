package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func (store *PostgresControlPlaneStore) AcquireLease(
	ctx context.Context,
	input ports.LeaseInput,
) (contracts.Lease, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease acquire transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef +
		"\x1flease.acquire\x1f" + input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease acquire idempotency lock failed: %w", err)
	}
	replayedLease, replayed, err := lookupLeaseIdempotency(
		ctx, tx, input.TenantRef, input.SubjectRef,
		"lease.acquire", input.SandboxID,
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
	if err := lockCurrentSubjectGeneration(
		ctx, tx, input.TenantRef, input.SubjectRef,
		input.SandboxID, input.Generation, false,
	); err != nil {
		return contracts.Lease{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.leases SET state='expired',revision=revision+1,updated_at=$4
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND state='active' AND expires_at<=$4`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.Now.UTC(),
	); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox expired Lease update failed: %w", err)
	}
	lease := input.Lease
	lease.TenantRef, lease.SubjectRef = input.TenantRef, input.SubjectRef
	lease.SandboxID, lease.Generation = input.SandboxID, input.Generation
	lease.State = contracts.LeaseStateActive
	lease.ExpiresAt, lease.Revision = input.ExpiresAt.UTC(), 1
	lease.CreatedAt, lease.UpdatedAt = input.Now.UTC(), input.Now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.leases (
			id,tenant_ref,subject_ref,sandbox_id,generation,state,expires_at,revision,created_at,updated_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10
		)`,
		lease.ID, lease.TenantRef, lease.SubjectRef, lease.SandboxID, lease.Generation, lease.State, lease.ExpiresAt, lease.Revision, lease.CreatedAt, lease.UpdatedAt,
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
	tenantRef string,
	subjectRef string,
	sandboxID string,
	leaseID string,
) (contracts.Lease, error) {
	return scanLease(store.pool.QueryRow(ctx, `
		SELECT id,tenant_ref,subject_ref,sandbox_id,generation,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND id=$4`,
		tenantRef, subjectRef, sandboxID, leaseID,
	))
}

// GetLeaseByID reads one Lease without accepting a caller-supplied Sandbox scope.
func (store *PostgresControlPlaneStore) GetLeaseByID(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	leaseID string,
) (contracts.Lease, error) {
	return scanLease(store.pool.QueryRow(ctx, `
		SELECT id,tenant_ref,subject_ref,sandbox_id,generation,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, leaseID,
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
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef +
		"\x1flease.renew\x1f" + input.Lease.ID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease renew idempotency lock failed: %w", err)
	}
	replayedLease, replayed, err := lookupLeaseIdempotency(
		ctx, tx, input.TenantRef, input.SubjectRef,
		"lease.renew", input.Lease.ID,
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
	if err := lockCurrentSubjectGeneration(
		ctx, tx, input.TenantRef, input.SubjectRef,
		input.SandboxID, input.Generation, false,
	); err != nil {
		return contracts.Lease{}, err
	}
	lease, err := scanLease(tx.QueryRow(ctx, `
		SELECT id,tenant_ref,subject_ref,sandbox_id,generation,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND id=$4 FOR UPDATE`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.Lease.ID,
	))
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.Generation != input.Generation || lease.SubjectRef != input.SubjectRef ||
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
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef +
		"\x1flease.release\x1f" + input.Lease.ID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Lease{}, fmt.Errorf("SecondBox Lease release idempotency lock failed: %w", err)
	}
	replayedLease, replayed, err := lookupLeaseIdempotency(
		ctx, tx, input.TenantRef, input.SubjectRef,
		"lease.release", input.Lease.ID,
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
		SELECT id,tenant_ref,subject_ref,sandbox_id,generation,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND id=$4 FOR UPDATE`,
		input.TenantRef, input.SubjectRef, input.SandboxID, input.Lease.ID,
	))
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.Generation != input.Generation || lease.SubjectRef != input.SubjectRef {
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
func scanLease(row rowScanner) (contracts.Lease, error) {
	var lease contracts.Lease
	if err := row.Scan(
		&lease.ID, &lease.TenantRef, &lease.SubjectRef,
		&lease.SandboxID, &lease.Generation,
		&lease.State, &lease.ExpiresAt, &lease.Revision,
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
	tenantRef string,
	subjectRef string,
	operation string,
	targetID string,
	idempotencyKey string,
	requestHash string,
) (contracts.Lease, bool, error) {
	var priorHash, leaseID string
	err := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND operation=$3 AND target_id=$4 AND idempotency_key=$5`,
		tenantRef, subjectRef, operation, targetID, idempotencyKey,
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
		SELECT id,tenant_ref,subject_ref,sandbox_id,generation,state,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.leases
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, leaseID,
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
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,response_resource_id,
			created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		input.TenantRef, input.SubjectRef,
		operation, targetID, input.IdempotencyKey, input.RequestHash,
		leaseID, input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Lease idempotency insert failed: %w", err)
	}
	return nil
}

// ListGarbageObjectsDue marks expired unreachable objects, rechecks after grace, and claims deletion.
