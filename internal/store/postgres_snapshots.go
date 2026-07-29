package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// CreateSnapshot atomically retains the current published stopped-state checkpoint.
func (store *PostgresControlPlaneStore) CreateSnapshot(
	ctx context.Context,
	input ports.SnapshotCreationInput,
) (contracts.Snapshot, error) {
	snapshot := input.Snapshot
	metadataJSON, err := json.Marshal(snapshot.Metadata)
	if err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot metadata encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		snapshot.TenantRef+"\x1f"+snapshot.SubjectRef+"\x1fsnapshot-capacity",
	); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot Project capacity lock failed: %w", err)
	}

	var (
		sandboxRevision, checkpointGeneration, checkpointSize int64
		profileName, sandboxState, workspaceID, checkpointID  string
		checkpointSHA256, checkpointState                     string
		specJSON, compatibilityJSON                           []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT sandbox.revision,sandbox.profile_name,sandbox.state,
		       workspace.id,workspace.current_checkpoint_id,
		       checkpoint.source_generation,checkpoint.sha256,checkpoint.size_bytes,
		       checkpoint.state,checkpoint.compatibility_json,revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.workspace_checkpoints AS checkpoint
		  ON checkpoint.id=workspace.current_checkpoint_id
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.id=$1 AND sandbox.tenant_ref=$2 AND sandbox.subject_ref=$3
		  AND sandbox.state<>'deleted'
		FOR UPDATE OF sandbox,workspace,checkpoint`,
		snapshot.SandboxID, snapshot.TenantRef, snapshot.SubjectRef,
	).Scan(
		&sandboxRevision, &profileName, &sandboxState, &workspaceID, &checkpointID,
		&checkpointGeneration, &checkpointSHA256,
		&checkpointSize, &checkpointState, &compatibilityJSON, &specJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		existsErr := tx.QueryRow(ctx, `
			SELECT true FROM secondbox.sandboxes
			WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND state<>'deleted'`,
			snapshot.SandboxID, snapshot.TenantRef, snapshot.SubjectRef,
		).Scan(&exists)
		if errors.Is(existsErr, pgx.ErrNoRows) {
			return contracts.Snapshot{}, ports.ErrSandboxNotFound
		}
		if existsErr != nil {
			return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot Sandbox authority lookup failed: %w", existsErr)
		}
		return contracts.Snapshot{}, ports.ErrCheckpointNotFound
	}
	if err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot authority lookup failed: %w", err)
	}
	if sandboxRevision != input.ExpectedRevision {
		return contracts.Snapshot{}, ports.ErrRevisionConflict
	}
	if sandboxState != contracts.SandboxStateStopped {
		return contracts.Snapshot{}, ports.ErrSnapshotUnavailable
	}
	if checkpointState != contracts.ObjectStatePublished {
		return contracts.Snapshot{}, ports.ErrCheckpointNotFound
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot ProfileRevision decoding failed: %w", err)
	}
	if snapshot.RetainUntil != snapshot.CreatedAt.Add(
		time.Duration(spec.Checkpoint.RetentionSeconds)*time.Second,
	) {
		return contracts.Snapshot{}, ports.ErrQuotaExceeded
	}

	lockKey := snapshot.TenantRef + "\x1f" + snapshot.SubjectRef + "\x1fsnapshot.create\x1f" +
		snapshot.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot idempotency lock failed: %w", err)
	}
	var priorHash, priorSnapshotID string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id
		FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2 AND operation='snapshot.create'
		  AND target_id=$3 AND idempotency_key=$4`,
		snapshot.TenantRef, snapshot.SubjectRef, snapshot.SandboxID, input.IdempotencyKey,
	).Scan(&priorHash, &priorSnapshotID)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return contracts.Snapshot{}, ports.ErrIdempotencyConflict
		}
		replayed, err := scanSnapshot(tx.QueryRow(
			ctx, snapshotSelect+` WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3`,
			priorSnapshotID, snapshot.TenantRef, snapshot.SubjectRef,
		))
		if err != nil {
			return contracts.Snapshot{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot replay commit failed: %w", err)
		}
		return replayed, nil
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot idempotency lookup failed: %w", idempotencyErr)
	}

	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		snapshot.TenantRef+"\x1f"+snapshot.SubjectRef+"\x1fsnapshot-capacity",
	); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot Profile capacity lock failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.snapshots
		SET state='expired',retention_ended_at=retain_until
		WHERE tenant_ref=$1 AND subject_ref=$2 AND state='published' AND retain_until<=$3`,
		snapshot.TenantRef, snapshot.SubjectRef, snapshot.CreatedAt.UTC(),
	); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot expiry update failed: %w", err)
	}
	subjectQuota, err := readSubjectQuota(ctx, tx, snapshot.TenantRef, snapshot.SubjectRef)
	if err != nil {
		return contracts.Snapshot{}, err
	}
	subjectUsage, err := readSubjectQuotaUsage(ctx, tx, snapshot.TenantRef, snapshot.SubjectRef)
	if err != nil {
		return contracts.Snapshot{}, err
	}
	var sandboxSnapshotCount int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM secondbox.snapshots
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND state='published' AND retain_until>$4`,
		snapshot.TenantRef, snapshot.SubjectRef, snapshot.SandboxID, snapshot.CreatedAt.UTC(),
	).Scan(&sandboxSnapshotCount); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot policy usage lookup failed: %w", err)
	}
	if sandboxSnapshotCount+1 > spec.Checkpoint.SnapshotLimit ||
		subjectUsage.snapshots+1 > subjectQuota.MaxSnapshots {
		return contracts.Snapshot{}, ports.ErrQuotaExceeded
	}

	if err := json.Unmarshal(compatibilityJSON, &snapshot.Compatibility); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot compatibility decoding failed: %w", err)
	}
	snapshot.WorkspaceID = workspaceID
	snapshot.CheckpointID = checkpointID
	snapshot.SourceGeneration = checkpointGeneration
	snapshot.SHA256 = checkpointSHA256
	snapshot.SizeBytes = checkpointSize
	snapshot.State = contracts.ObjectStatePublished
	compatibilityJSON, err = json.Marshal(snapshot.Compatibility)
	if err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot compatibility encoding failed: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,checkpoint_id,source_generation,
			name,sha256,size_bytes,compatibility_json,metadata_json,state,
			retain_until,created_at,retention_ended_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'published',$13,$14,NULL)`,
		snapshot.ID, snapshot.TenantRef, snapshot.SubjectRef,
		snapshot.SandboxID, snapshot.WorkspaceID,
		snapshot.CheckpointID, snapshot.SourceGeneration, snapshot.Name, snapshot.SHA256,
		snapshot.SizeBytes, compatibilityJSON, metadataJSON,
		snapshot.RetainUntil.UTC(), snapshot.CreatedAt.UTC(),
	)
	if err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ($1,$2,'snapshot.create',$3,$4,$5,$6,$7,$8)`,
		snapshot.TenantRef, snapshot.SubjectRef,
		snapshot.SandboxID, input.IdempotencyKey,
		input.RequestHash, snapshot.ID, snapshot.CreatedAt.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot idempotency insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot commit failed: %w", err)
	}
	return snapshot, nil
}

// ListSnapshots returns retained Snapshot metadata in deterministic newest-first order.
func (store *PostgresControlPlaneStore) ListSnapshots(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	limit int,
	cursor string,
	now time.Time,
) (contracts.SnapshotPage, error) {
	var cursorCreatedAt time.Time
	var cursorID string
	if cursor != "" {
		if err := store.pool.QueryRow(ctx, `
			SELECT created_at,id FROM secondbox.snapshots
			WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND sandbox_id=$4
			  AND state='published' AND retain_until>$5`,
			cursor, tenantRef, subjectRef, sandboxID, now.UTC(),
		).Scan(&cursorCreatedAt, &cursorID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return contracts.SnapshotPage{}, errors.New("SecondBox Snapshot page cursor is invalid")
			}
			return contracts.SnapshotPage{}, fmt.Errorf("SecondBox Snapshot cursor lookup failed: %w", err)
		}
	}
	var exists bool
	if err := store.pool.QueryRow(ctx, `
		SELECT true FROM secondbox.sandboxes
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND state<>'deleted'`,
		sandboxID, tenantRef, subjectRef,
	).Scan(&exists); err != nil {
		return contracts.SnapshotPage{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	rows, err := store.pool.Query(ctx, snapshotSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND state='published' AND retain_until>$4
		  AND ($5='' OR (created_at,id)<($6,$5))
		ORDER BY created_at DESC,id DESC
		LIMIT $7`,
		tenantRef, subjectRef, sandboxID, now.UTC(), cursorID, cursorCreatedAt, limit+1,
	)
	if err != nil {
		return contracts.SnapshotPage{}, fmt.Errorf("SecondBox Snapshot list failed: %w", err)
	}
	defer rows.Close()
	snapshots := make([]contracts.Snapshot, 0, limit+1)
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return contracts.SnapshotPage{}, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return contracts.SnapshotPage{}, fmt.Errorf("SecondBox Snapshot list iteration failed: %w", err)
	}
	page := contracts.SnapshotPage{Items: snapshots}
	if len(page.Items) > limit {
		nextCursor := page.Items[limit-1].ID
		page.NextCursor = &nextCursor
		page.Items = page.Items[:limit]
	}
	return page, nil
}

// GetSnapshot returns retained Snapshot metadata inside one Project.
func (store *PostgresControlPlaneStore) GetSnapshot(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	snapshotID string,
	now time.Time,
) (contracts.Snapshot, error) {
	snapshot, err := scanSnapshot(store.pool.QueryRow(ctx, snapshotSelect+`
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		  AND state='published' AND retain_until>$4`,
		snapshotID, tenantRef, subjectRef, now.UTC(),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Snapshot{}, ports.ErrSnapshotNotFound
	}
	return snapshot, err
}

// EndSnapshotRetention removes one checkpoint reachability root and preserves terminal evidence.
func (store *PostgresControlPlaneStore) EndSnapshotRetention(
	ctx context.Context,
	input ports.SnapshotRetentionInput,
) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Snapshot retention transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef + "\x1fsnapshot.delete\x1f" +
		input.SnapshotID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return fmt.Errorf("SecondBox Snapshot retention idempotency lock failed: %w", err)
	}
	var priorHash string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2 AND operation='snapshot.delete'
		  AND target_id=$3 AND idempotency_key=$4`,
		input.TenantRef, input.SubjectRef, input.SnapshotID, input.IdempotencyKey,
	).Scan(&priorHash)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return ports.ErrIdempotencyConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return fmt.Errorf("SecondBox Snapshot retention idempotency lookup failed: %w", idempotencyErr)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.snapshots
		SET state='retention_ended',retain_until=$4,retention_ended_at=$4
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		  AND state='published' AND retain_until>$4`,
		input.SnapshotID, input.TenantRef, input.SubjectRef, input.Now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox Snapshot retention update failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrSnapshotNotFound
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ($1,$2,'snapshot.delete',$3,$4,$5,$3,$6,$7)`,
		input.TenantRef, input.SubjectRef,
		input.SnapshotID, input.IdempotencyKey, input.RequestHash,
		input.Now.UTC(), input.IdempotencyEnds.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Snapshot retention idempotency insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox Snapshot retention commit failed: %w", err)
	}
	return nil
}

const snapshotSelect = `
	SELECT id,tenant_ref,subject_ref,sandbox_id,workspace_id,checkpoint_id,source_generation,
	       name,sha256,size_bytes,state,metadata_json,compatibility_json,
	       retain_until,created_at,retention_ended_at
	FROM secondbox.snapshots`

type snapshotScanner interface {
	Scan(...any) error
}

func scanSnapshot(row snapshotScanner) (contracts.Snapshot, error) {
	var snapshot contracts.Snapshot
	var metadataJSON, compatibilityJSON []byte
	if err := row.Scan(
		&snapshot.ID, &snapshot.TenantRef, &snapshot.SubjectRef,
		&snapshot.SandboxID, &snapshot.WorkspaceID,
		&snapshot.CheckpointID, &snapshot.SourceGeneration, &snapshot.Name,
		&snapshot.SHA256, &snapshot.SizeBytes, &snapshot.State, &metadataJSON,
		&compatibilityJSON, &snapshot.RetainUntil, &snapshot.CreatedAt,
		&snapshot.RetentionEndedAt,
	); err != nil {
		return contracts.Snapshot{}, err
	}
	if err := json.Unmarshal(metadataJSON, &snapshot.Metadata); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot metadata decoding failed: %w", err)
	}
	if err := json.Unmarshal(compatibilityJSON, &snapshot.Compatibility); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot compatibility decoding failed: %w", err)
	}
	snapshot.ProjectID = snapshot.TenantRef
	return snapshot, nil
}
