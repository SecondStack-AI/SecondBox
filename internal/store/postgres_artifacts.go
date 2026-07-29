package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func (store *PostgresControlPlaneStore) StageArtifact(
	ctx context.Context,
	input ports.ArtifactPublicationInput,
) (contracts.Artifact, error) {
	artifact := input.Artifact
	metadataJSON, err := json.Marshal(artifact.Metadata)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact metadata encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact staging transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		artifact.TenantRef+"\x1f"+artifact.SubjectRef+"\x1fretained-byte-capacity",
	); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact Project retained-byte capacity lock failed: %w", err)
	}
	var sandboxGeneration int64
	var specJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT sandbox.generation,revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.id=$1 AND sandbox.tenant_ref=$2 AND sandbox.subject_ref=$3
		  AND sandbox.state<>'deleted'
		FOR UPDATE OF sandbox`,
		artifact.SandboxID, artifact.TenantRef, artifact.SubjectRef,
	).Scan(&sandboxGeneration, &specJSON); err != nil {
		return contracts.Artifact{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if sandboxGeneration != input.ExpectedGeneration ||
		artifact.SourceGeneration != input.ExpectedGeneration {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	if input.LeaseID != "" {
		var leaseGeneration int64
		var leaseAccount, leaseState string
		var leaseExpiry time.Time
		if err := tx.QueryRow(ctx, `
			SELECT generation,subject_ref,state,expires_at
			FROM secondbox.leases
			WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND id=$4 FOR UPDATE`,
			artifact.TenantRef, artifact.SubjectRef, artifact.SandboxID, input.LeaseID,
		).Scan(&leaseGeneration, &leaseAccount, &leaseState, &leaseExpiry); err != nil {
			return contracts.Artifact{}, mapNotFound(err, ports.ErrLeaseNotFound)
		}
		if leaseGeneration != input.ExpectedGeneration ||
			leaseAccount != artifact.SubjectRef ||
			leaseState != contracts.LeaseStateActive ||
			!artifact.CreatedAt.Before(leaseExpiry) {
			return contracts.Artifact{}, ports.ErrLeaseInactive
		}
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact ProfileRevision decoding failed: %w", err)
	}
	if artifact.SizeBytes > spec.Execution.MaximumTransferBytes ||
		!artifact.RetainUntil.Equal(artifact.CreatedAt.Add(
			time.Duration(spec.Checkpoint.ArtifactRetentionSeconds)*time.Second,
		)) {
		return contracts.Artifact{}, ports.ErrQuotaExceeded
	}
	if input.IdempotencyKey != "" {
		lockKey := artifact.TenantRef + "\x1f" + artifact.SubjectRef + "\x1fartifact.upload\x1f" +
			artifact.SandboxID + "\x1f" + input.IdempotencyKey
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact idempotency lock failed: %w", err)
		}
		var priorHash, artifactID string
		idempotencyErr := tx.QueryRow(ctx, `
			SELECT request_hash,response_resource_id
			FROM secondbox.idempotency_records
			WHERE tenant_ref=$1 AND subject_ref=$2
			  AND operation='artifact.upload' AND target_id=$3 AND idempotency_key=$4`,
			artifact.TenantRef, artifact.SubjectRef, artifact.SandboxID, input.IdempotencyKey,
		).Scan(&priorHash, &artifactID)
		if idempotencyErr == nil {
			if priorHash != input.RequestHash {
				return contracts.Artifact{}, ports.ErrIdempotencyConflict
			}
			replayed, _, err := scanArtifact(tx.QueryRow(ctx, `
				SELECT id,tenant_ref,subject_ref,sandbox_id,source_generation,name,media_type,size_bytes,
				       sha256,state,metadata_json,retain_until,created_at,published_at,
				       garbage_collected_at,storage_key
				FROM secondbox.artifacts
				WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3`,
				artifactID, artifact.TenantRef, artifact.SubjectRef,
			))
			if err != nil {
				return contracts.Artifact{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact replay commit failed: %w", err)
			}
			return replayed, nil
		}
		if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact idempotency lookup failed: %w", idempotencyErr)
		}
	}
	subjectQuota, err := readSubjectQuota(ctx, tx, artifact.TenantRef, artifact.SubjectRef)
	if err != nil {
		return contracts.Artifact{}, err
	}
	subjectUsage, err := readSubjectQuotaUsage(ctx, tx, artifact.TenantRef, artifact.SubjectRef)
	if err != nil {
		return contracts.Artifact{}, err
	}
	if subjectUsage.artifacts+1 > subjectQuota.MaxArtifacts ||
		subjectUsage.retainedBytes+artifact.SizeBytes > subjectQuota.MaxRetainedBytes {
		return contracts.Artifact{}, ports.ErrQuotaExceeded
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO secondbox.artifacts (
			id,tenant_ref,subject_ref,sandbox_id,source_generation,name,media_type,size_bytes,sha256,
			storage_key,state,metadata_json,retain_until,created_at,published_at,
			garbage_collection_marked_at,garbage_collected_at
		)
		SELECT $1,sandbox.tenant_ref,sandbox.subject_ref,sandbox.id,
		       $2,$3,$4,$5,$6,$7,'staging',$8,$9,$10,NULL,NULL,NULL
		FROM secondbox.sandboxes AS sandbox
		WHERE sandbox.id=$11 AND sandbox.tenant_ref=$12 AND sandbox.subject_ref=$13
		  AND sandbox.generation=$14`,
		artifact.ID, artifact.SourceGeneration, artifact.Name, artifact.MediaType,
		artifact.SizeBytes, artifact.SHA256, input.StorageKey, metadataJSON,
		artifact.RetainUntil.UTC(), artifact.CreatedAt.UTC(), artifact.SandboxID,
		artifact.TenantRef, artifact.SubjectRef, input.ExpectedGeneration,
	)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact staging failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	if input.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.idempotency_records (
				tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
				response_resource_id,created_at,expires_at
			) VALUES ($1,$2,'artifact.upload',$3,$4,$5,$6,$7,$8)`,
			artifact.TenantRef, artifact.SubjectRef,
			artifact.SandboxID, input.IdempotencyKey,
			input.RequestHash, artifact.ID, artifact.CreatedAt.UTC(), input.IdempotencyEnds.UTC(),
		); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact idempotency insert failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact staging commit failed: %w", err)
	}
	artifact.State = contracts.ObjectStateStaging
	return artifact, nil
}

// PublishArtifact atomically exposes metadata only after provider hash and size verification.
func (store *PostgresControlPlaneStore) PublishArtifact(
	ctx context.Context,
	input ports.ArtifactPublicationInput,
	now time.Time,
) (contracts.Artifact, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact publication transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var storedState, storedSHA256, storedStorageKey string
	var storedSizeBytes, storedGeneration int64
	if err := tx.QueryRow(ctx, `
		SELECT state,sha256,size_bytes,storage_key,source_generation
		FROM secondbox.artifacts
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND sandbox_id=$4
		FOR UPDATE`,
		input.Artifact.ID, input.Artifact.TenantRef, input.Artifact.SubjectRef,
		input.Artifact.SandboxID,
	).Scan(
		&storedState, &storedSHA256, &storedSizeBytes, &storedStorageKey, &storedGeneration,
	); err != nil {
		return contracts.Artifact{}, mapNotFound(err, ports.ErrArtifactNotFound)
	}
	if storedGeneration != input.ExpectedGeneration {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	var currentGeneration int64
	if err := tx.QueryRow(ctx, `
		SELECT generation FROM secondbox.sandboxes
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 FOR UPDATE`,
		input.Artifact.SandboxID, input.Artifact.TenantRef, input.Artifact.SubjectRef,
	).Scan(&currentGeneration); err != nil {
		return contracts.Artifact{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if currentGeneration != input.ExpectedGeneration {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	if storedState != contracts.ObjectStateStaging {
		return contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	if storedSHA256 != input.Artifact.SHA256 || storedSizeBytes != input.Artifact.SizeBytes ||
		storedStorageKey != input.StorageKey {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.artifacts SET state='integrity_failed'
			WHERE id=$1 AND state='staging'`,
			input.Artifact.ID,
		); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact integrity failure update failed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact integrity failure commit failed: %w", err)
		}
		return contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	var artifact contracts.Artifact
	var metadataJSON []byte
	err = tx.QueryRow(ctx, `
		UPDATE secondbox.artifacts
		SET state='published',published_at=$6
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		  AND sandbox_id=$4 AND source_generation=$5
		  AND state='staging' AND sha256=$7 AND size_bytes=$8 AND storage_key=$9
		RETURNING id,tenant_ref,subject_ref,sandbox_id,source_generation,name,media_type,size_bytes,sha256,
		          state,metadata_json,retain_until,created_at,published_at,garbage_collected_at`,
		input.Artifact.ID, input.Artifact.TenantRef, input.Artifact.SubjectRef,
		input.Artifact.SandboxID, input.ExpectedGeneration, now.UTC(), input.Artifact.SHA256,
		input.Artifact.SizeBytes, input.StorageKey,
	).Scan(
		&artifact.ID, &artifact.TenantRef, &artifact.SubjectRef,
		&artifact.SandboxID, &artifact.SourceGeneration,
		&artifact.Name, &artifact.MediaType, &artifact.SizeBytes, &artifact.SHA256,
		&artifact.State, &metadataJSON, &artifact.RetainUntil, &artifact.CreatedAt,
		&artifact.PublishedAt, &artifact.GarbageCollectedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact publication failed: %w", err)
	}
	if err := json.Unmarshal(metadataJSON, &artifact.Metadata); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact metadata decoding failed: %w", err)
	}
	artifact.ProjectID = artifact.TenantRef
	if err := tx.Commit(ctx); err != nil {
		return contracts.Artifact{}, fmt.Errorf("SecondBox Artifact publication commit failed: %w", err)
	}
	return artifact, nil
}

// ListArtifacts returns retained, published Artifact metadata in deterministic newest-first order.
func (store *PostgresControlPlaneStore) ListArtifacts(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	limit int,
	cursor string,
	now time.Time,
) (contracts.ArtifactPage, error) {
	var cursorCreatedAt time.Time
	var cursorID string
	if cursor != "" {
		if err := store.pool.QueryRow(ctx, `
			SELECT created_at,id FROM secondbox.artifacts
			WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND sandbox_id=$4
			  AND state='published' AND retain_until>$5`,
			cursor, tenantRef, subjectRef, sandboxID, now.UTC(),
		).Scan(&cursorCreatedAt, &cursorID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return contracts.ArtifactPage{}, errors.New("SecondBox Artifact page cursor is invalid")
			}
			return contracts.ArtifactPage{}, fmt.Errorf("SecondBox Artifact cursor lookup failed: %w", err)
		}
	}
	var sandboxExists bool
	if err := store.pool.QueryRow(ctx, `
		SELECT true FROM secondbox.sandboxes
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND state<>'deleted'`,
		sandboxID, tenantRef, subjectRef,
	).Scan(&sandboxExists); err != nil {
		return contracts.ArtifactPage{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	rows, err := store.pool.Query(ctx, `
		SELECT id,tenant_ref,subject_ref,sandbox_id,source_generation,name,media_type,size_bytes,
		       sha256,state,metadata_json,retain_until,created_at,published_at,
		       garbage_collected_at,storage_key
		FROM secondbox.artifacts
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND state='published' AND retain_until>$4
		  AND ($5='' OR (created_at,id)<($6,$5))
		ORDER BY created_at DESC,id DESC
		LIMIT $7`,
		tenantRef, subjectRef, sandboxID, now.UTC(), cursorID, cursorCreatedAt, limit+1,
	)
	if err != nil {
		return contracts.ArtifactPage{}, fmt.Errorf("SecondBox Artifact list failed: %w", err)
	}
	defer rows.Close()
	artifacts := make([]contracts.Artifact, 0, limit+1)
	for rows.Next() {
		artifact, _, err := scanArtifact(rows)
		if err != nil {
			return contracts.ArtifactPage{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return contracts.ArtifactPage{}, fmt.Errorf("SecondBox Artifact list iteration failed: %w", err)
	}
	page := contracts.ArtifactPage{Items: artifacts}
	if len(page.Items) > limit {
		cursor := page.Items[limit-1].ID
		page.NextCursor = &cursor
		page.Items = page.Items[:limit]
	}
	return page, nil
}

// GetArtifactObject resolves retained public metadata and its private immutable key.
func (store *PostgresControlPlaneStore) GetArtifactObject(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	artifactID string,
	now time.Time,
) (ports.ArtifactObject, error) {
	artifact, storageKey, err := scanArtifact(store.pool.QueryRow(ctx, `
		SELECT id,tenant_ref,subject_ref,sandbox_id,source_generation,name,media_type,size_bytes,
		       sha256,state,metadata_json,retain_until,created_at,published_at,
		       garbage_collected_at,storage_key
		FROM secondbox.artifacts
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		  AND state='published' AND retain_until>$4`,
		artifactID, tenantRef, subjectRef, now.UTC(),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.ArtifactObject{}, ports.ErrArtifactNotFound
	}
	if err != nil {
		return ports.ArtifactObject{}, err
	}
	return ports.ArtifactObject{Artifact: artifact, StorageKey: storageKey}, nil
}

// EndArtifactRetention hides public metadata and leaves provider deletion to two-phase garbage collection.
func (store *PostgresControlPlaneStore) EndArtifactRetention(
	ctx context.Context,
	input ports.ArtifactRetentionInput,
) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Artifact retention transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef + "\x1fartifact.delete\x1f" +
		input.ArtifactID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return fmt.Errorf("SecondBox Artifact retention idempotency lock failed: %w", err)
	}
	var priorHash string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2 AND operation='artifact.delete'
		  AND target_id=$3 AND idempotency_key=$4`,
		input.TenantRef, input.SubjectRef, input.ArtifactID, input.IdempotencyKey,
	).Scan(&priorHash)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return ports.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("SecondBox Artifact retention replay commit failed: %w", err)
		}
		return nil
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return fmt.Errorf("SecondBox Artifact retention idempotency lookup failed: %w", idempotencyErr)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.artifacts
		SET state='garbage_pending',retain_until=$4,garbage_collection_marked_at=$4
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		  AND state='published' AND retain_until>$4`,
		input.ArtifactID, input.TenantRef, input.SubjectRef, input.Now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox Artifact retention update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrArtifactNotFound
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ($1,$2,'artifact.delete',$3,$4,$5,$3,$6,$7)`,
		input.TenantRef, input.SubjectRef,
		input.ArtifactID, input.IdempotencyKey, input.RequestHash,
		input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Artifact retention idempotency insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox Artifact retention commit failed: %w", err)
	}
	return nil
}

type artifactRow interface {
	Scan(...any) error
}

func scanArtifact(row artifactRow) (contracts.Artifact, string, error) {
	var artifact contracts.Artifact
	var metadataJSON []byte
	var storageKey string
	if err := row.Scan(
		&artifact.ID, &artifact.TenantRef, &artifact.SubjectRef,
		&artifact.SandboxID, &artifact.SourceGeneration,
		&artifact.Name, &artifact.MediaType, &artifact.SizeBytes, &artifact.SHA256,
		&artifact.State, &metadataJSON, &artifact.RetainUntil, &artifact.CreatedAt,
		&artifact.PublishedAt, &artifact.GarbageCollectedAt, &storageKey,
	); err != nil {
		return contracts.Artifact{}, "", err
	}
	if err := json.Unmarshal(metadataJSON, &artifact.Metadata); err != nil {
		return contracts.Artifact{}, "", fmt.Errorf("SecondBox Artifact metadata decoding failed: %w", err)
	}
	artifact.ProjectID = artifact.TenantRef
	return artifact, storageKey, nil
}
