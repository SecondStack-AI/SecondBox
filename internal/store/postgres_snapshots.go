package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// CreateSnapshot admits one asynchronous stopped-Sandbox local clone.
func (store *PostgresControlPlaneStore) CreateSnapshot(
	ctx context.Context,
	input ports.SnapshotCreationInput,
) (contracts.Operation, error) {
	if input.Snapshot.RetainUntil == nil || input.EffectID == "" || input.CommandID == "" ||
		len(input.FencingToken) < 32 {
		return contracts.Operation{}, errors.New("SecondBox Snapshot local effect identity, retention, and fence are required")
	}
	metadataJSON, err := json.Marshal(input.Snapshot.Metadata)
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot metadata encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	replayed, found, err := lookupSnapshotMutationReplay(
		ctx, tx, input.Snapshot.TenantRef, input.Snapshot.SubjectRef,
		"snapshot.create", input.Snapshot.SandboxID, input.IdempotencyKey, input.RequestHash,
		input.Snapshot.CreatedAt,
	)
	if err != nil || found {
		return replayed, err
	}

	locked, err := lockSandboxWorkspace(
		ctx, tx, input.Snapshot.TenantRef, input.Snapshot.SubjectRef,
		input.Snapshot.SandboxID,
	)
	if err != nil {
		return contracts.Operation{}, err
	}
	revision := locked.Revision
	generation := locked.Generation
	workspaceID := locked.WorkspaceID
	sandboxState := locked.SandboxState
	var specJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT spec_json FROM secondbox.profile_revisions WHERE id=$1`,
		locked.ProfileRevisionID,
	).Scan(&specJSON); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot ProfileRevision lookup failed: %w", err)
	}
	if revision != input.ExpectedRevision {
		return contracts.Operation{}, ports.ErrRevisionConflict
	}
	if sandboxState != contracts.SandboxStateStopped {
		return contracts.Operation{}, ports.ErrSnapshotUnavailable
	}
	if locked.DesiredState == contracts.SandboxDesiredStateDeleted {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	homeRunnerID := locked.Workspace.HomeRunnerID
	workspaceState := locked.Workspace.State
	mutationState := locked.Workspace.Mutation.State
	workspaceGeneration := locked.Workspace.Generation
	capacity := locked.Workspace.LogicalCapacityBytes
	if workspaceState != "ready" || workspaceGeneration != generation {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	if mutationState != "" {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	if err := requireHomeRunnerReady(ctx, tx, homeRunnerID); err != nil {
		return contracts.Operation{}, err
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot ProfileRevision decoding failed: %w", err)
	}
	if !input.Snapshot.RetainUntil.Equal(input.Snapshot.CreatedAt.Add(
		time.Duration(spec.Retention.SnapshotRetentionSeconds) * time.Second,
	)) {
		return contracts.Operation{}, ports.ErrQuotaExceeded
	}
	var count int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM secondbox.snapshots
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND state IN ('creating','ready','deleting')`,
		input.Snapshot.TenantRef, input.Snapshot.SubjectRef, input.Snapshot.SandboxID,
	).Scan(&count); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot count lookup failed: %w", err)
	}
	tenantQuota, quota, err := lockTenantAndSubjectQuotaForAdmission(
		ctx, tx, input.Snapshot.TenantRef, input.Snapshot.SubjectRef, input.Snapshot.CreatedAt,
	)
	if err != nil {
		return contracts.Operation{}, err
	}
	usage, err := readSubjectQuotaUsage(ctx, tx, input.Snapshot.TenantRef, input.Snapshot.SubjectRef)
	if err != nil {
		return contracts.Operation{}, err
	}
	tenantUsage, err := readTenantQuotaUsage(ctx, tx, input.Snapshot.TenantRef, input.Snapshot.CreatedAt)
	if err != nil {
		return contracts.Operation{}, err
	}
	if count+1 > spec.Retention.SnapshotLimit || usage.snapshots+1 > quota.MaxSnapshots ||
		tenantDataPlaneQuotaWouldExceed(tenantQuota, tenantUsage, quotaUsage{snapshots: 1}) {
		return contracts.Operation{}, ports.ErrQuotaExceeded
	}

	snapshot := input.Snapshot
	snapshot.WorkspaceID = workspaceID
	snapshot.SourceGeneration = generation
	snapshot.SizeBytes = capacity
	snapshot.State = "creating"
	input.Operation.Snapshot = &snapshot
	input.Operation.SandboxID = snapshot.SandboxID
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at,retention_ended_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'{}',$9,$10,$11,$12,'creating',$13,$14,$14,NULL)`,
		snapshot.ID, snapshot.TenantRef, snapshot.SubjectRef, snapshot.SandboxID,
		workspaceID, homeRunnerID, input.Operation.ID, input.EffectID,
		generation, snapshot.Name, capacity, metadataJSON,
		snapshot.RetainUntil.UTC(), snapshot.CreatedAt.UTC(),
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot insert failed: %w", err)
	}
	if err := insertOperation(ctx, tx, snapshot.TenantRef, snapshot.SubjectRef, input.Operation); err != nil {
		return contracts.Operation{}, err
	}
	if err := setWorkspaceMutation(
		ctx, tx, workspaceID, "snapshot_create", input.EffectID, input.EffectID, input.Operation.ID,
		generation, generation, snapshot.CreatedAt,
	); err != nil {
		return contracts.Operation{}, err
	}
	command := localWorkspaceCommand(
		input.CommandID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
		input.Operation, input.EffectID, snapshot.SandboxID, workspaceID, snapshot.ID,
		generation, generation, capacity, homeRunnerID, input.FencingToken,
	)
	if err := queueLocalWorkspaceEffect(
		ctx, tx, input.EffectID, "local_snapshot_create", snapshot.SandboxID, generation,
		homeRunnerID, input.CommandID, snapshot.ID, input.FencingToken, command, snapshot.CreatedAt,
	); err != nil {
		return contracts.Operation{}, err
	}
	if err := recordSnapshotMutationIdempotency(
		ctx, tx, snapshot.TenantRef, snapshot.SubjectRef, "snapshot.create",
		snapshot.SandboxID, input.IdempotencyKey, input.RequestHash,
		input.Operation.ID, snapshot.CreatedAt, input.IdempotencyEnds,
	); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET revision=revision+1,updated_at=$2 WHERE id=$1`,
		snapshot.SandboxID, snapshot.CreatedAt,
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot Sandbox revision update failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot create commit failed: %w", err)
	}
	return input.Operation, nil
}

// DeleteSnapshot admits one asynchronous local Snapshot deletion.
func (store *PostgresControlPlaneStore) DeleteSnapshot(
	ctx context.Context,
	input ports.SnapshotDeletionInput,
) (contracts.Operation, error) {
	return store.deleteSnapshot(ctx, input, true)
}

func (store *PostgresControlPlaneStore) deleteSnapshot(
	ctx context.Context,
	input ports.SnapshotDeletionInput,
	recordIdempotency bool,
) (contracts.Operation, error) {
	if input.EffectID == "" || input.CommandID == "" || len(input.FencingToken) < 32 {
		return contracts.Operation{}, errors.New("SecondBox Snapshot delete effect identity and fence are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot delete transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if recordIdempotency {
		replayed, found, err := lookupSnapshotMutationReplay(
			ctx, tx, input.TenantRef, input.SubjectRef, "snapshot.delete",
			input.SnapshotID, input.IdempotencyKey, input.RequestHash, input.Now,
		)
		if err != nil || found {
			return replayed, err
		}
	}
	var sandboxID, workspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT sandbox_id,workspace_id FROM secondbox.snapshots
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3`,
		input.SnapshotID, input.TenantRef, input.SubjectRef,
	).Scan(&sandboxID, &workspaceID); err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrSnapshotNotFound)
	}
	locked, err := lockSandboxWorkspace(
		ctx, tx, input.TenantRef, input.SubjectRef, sandboxID,
	)
	if err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrSnapshotNotFound)
	}
	if locked.WorkspaceID != workspaceID {
		return contracts.Operation{}, ports.ErrSnapshotNotFound
	}
	generation := locked.Generation
	homeRunnerID := locked.Workspace.HomeRunnerID
	mutationState := locked.Workspace.Mutation.State
	workspaceGeneration := locked.Workspace.Generation
	capacity := locked.Workspace.LogicalCapacityBytes
	if mutationState != "" {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	snapshot, _, err := lockSnapshotAfterWorkspace(
		ctx, tx, input.TenantRef, input.SubjectRef, locked, input.SnapshotID,
	)
	if err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrSnapshotNotFound)
	}
	if snapshot.State != "ready" {
		return contracts.Operation{}, ports.ErrSnapshotUnavailable
	}
	var restoreReference, cloneReference bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM secondbox.workspace_restores
			WHERE snapshot_id=$1 AND workspace_id=$2
			  AND state NOT IN ('finalized','failed')
		)`,
		input.SnapshotID, workspaceID,
	).Scan(&restoreReference); err != nil {
		return contracts.Operation{}, fmt.Errorf(
			"SecondBox Snapshot restore reference lookup failed: %w",
			err,
		)
	}
	if restoreReference {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM secondbox.lifecycle_effects
			WHERE storage_object_id=$1 AND kind='local_workspace_clone'
			  AND state NOT IN ('succeeded','runner_failed','cancelled')
		)`,
		input.SnapshotID,
	).Scan(&cloneReference); err != nil {
		return contracts.Operation{}, fmt.Errorf(
			"SecondBox Snapshot clone reference lookup failed: %w",
			err,
		)
	}
	if cloneReference {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	snapshot.State = "deleting"
	input.Operation.SandboxID = sandboxID
	input.Operation.Snapshot = &snapshot
	if err := insertOperation(ctx, tx, input.TenantRef, input.SubjectRef, input.Operation); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.snapshots
		SET state='deleting',operation_id=$2,effect_id=$3,updated_at=$4
		WHERE id=$1 AND state='ready'`,
		input.SnapshotID, input.Operation.ID, input.EffectID, input.Now.UTC(),
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot delete state update failed: %w", err)
	}
	if err := setWorkspaceMutation(
		ctx, tx, workspaceID, "snapshot_delete", input.EffectID, input.EffectID, input.Operation.ID,
		workspaceGeneration, workspaceGeneration, input.Now,
	); err != nil {
		return contracts.Operation{}, err
	}
	command := localWorkspaceCommand(
		input.CommandID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE,
		input.Operation, input.EffectID, sandboxID, workspaceID, input.SnapshotID,
		workspaceGeneration, workspaceGeneration, capacity, homeRunnerID, input.FencingToken,
	)
	if err := queueLocalWorkspaceEffect(
		ctx, tx, input.EffectID, "local_snapshot_delete", sandboxID, generation,
		homeRunnerID, input.CommandID, input.SnapshotID, input.FencingToken, command, input.Now,
	); err != nil {
		return contracts.Operation{}, err
	}
	if recordIdempotency {
		if err := recordSnapshotMutationIdempotency(
			ctx, tx, input.TenantRef, input.SubjectRef, "snapshot.delete",
			input.SnapshotID, input.IdempotencyKey, input.RequestHash,
			input.Operation.ID, input.Now, input.IdempotencyEnds,
		); err != nil {
			return contracts.Operation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot delete commit failed: %w", err)
	}
	return input.Operation, nil
}

// QueueExpiredSnapshotDelete admits at most one due retention deletion through
// the same asynchronous local effect path as an explicit API deletion.
func (store *PostgresControlPlaneStore) QueueExpiredSnapshotDelete(
	ctx context.Context,
	input ports.SnapshotRetentionInput,
) (bool, error) {
	if input.OperationID == "" ||
		input.EffectID == "" ||
		input.CommandID == "" ||
		input.RequestID == "" ||
		len(input.FencingToken) < 32 {
		return false, errors.New("SecondBox Snapshot retention identities and fence are required")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT id,tenant_ref,subject_ref
		FROM secondbox.snapshots
		WHERE state='ready' AND retain_until<=$1
		ORDER BY retain_until,id
		LIMIT 32`,
		input.Now.UTC(),
	)
	if err != nil {
		return false, fmt.Errorf("SecondBox expired Snapshot selection failed: %w", err)
	}
	type candidate struct {
		id, tenantRef, subjectRef string
	}
	candidates := make([]candidate, 0, 32)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.tenantRef, &item.subjectRef); err != nil {
			rows.Close()
			return false, fmt.Errorf("SecondBox expired Snapshot scan failed: %w", err)
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("SecondBox expired Snapshot iteration failed: %w", err)
	}
	for _, item := range candidates {
		_, err := store.deleteSnapshot(ctx, ports.SnapshotDeletionInput{
			TenantRef: item.tenantRef, SubjectRef: item.subjectRef, SnapshotID: item.id,
			Operation: contracts.Operation{
				ID: input.OperationID, Kind: "snapshot_delete",
				State: contracts.OperationStatePending, RequestID: input.RequestID,
				CreatedAt: input.Now.UTC(), UpdatedAt: input.Now.UTC(),
			},
			EffectID: input.EffectID, CommandID: input.CommandID,
			FencingToken: input.FencingToken, Now: input.Now.UTC(),
		}, false)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, ports.ErrSnapshotNotFound),
			errors.Is(err, ports.ErrSnapshotUnavailable),
			errors.Is(err, ports.ErrWorkspaceMutation):
			continue
		default:
			return false, err
		}
	}
	return false, nil
}

// RestoreSnapshot admits the prepare phase of one stopped-Sandbox in-place restore.
func (store *PostgresControlPlaneStore) RestoreSnapshot(
	ctx context.Context,
	input ports.SnapshotRestoreInput,
) (contracts.Operation, error) {
	if input.RestoreID == "" || input.PrepareEffectID == "" || input.SwapEffectID == "" ||
		input.FinalizeEffectID == "" || input.AbortEffectID == "" ||
		input.PrepareCommandID == "" || input.SwapCommandID == "" ||
		input.FinalizeCommandID == "" || input.AbortCommandID == "" ||
		len(input.FencingToken) < 32 {
		return contracts.Operation{}, errors.New("SecondBox Snapshot restore identities and fence are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot restore transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	replayed, found, err := lookupSnapshotMutationReplay(
		ctx, tx, input.TenantRef, input.SubjectRef, "snapshot.restore",
		input.SandboxID, input.IdempotencyKey, input.RequestHash, input.Now,
	)
	if err != nil || found {
		return replayed, err
	}
	locked, err := lockSandboxWorkspace(
		ctx, tx, input.TenantRef, input.SubjectRef, input.SandboxID,
	)
	if err != nil {
		return contracts.Operation{}, err
	}
	state := locked.SandboxState
	workspaceID := locked.WorkspaceID
	generation := locked.Generation
	revision := locked.Revision
	if revision != input.ExpectedRevision {
		return contracts.Operation{}, ports.ErrRevisionConflict
	}
	if state != contracts.SandboxStateStopped {
		return contracts.Operation{}, ports.ErrSnapshotUnavailable
	}
	if locked.CurrentInstanceID != "" || locked.ReconcileOwner != "" {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	if locked.DesiredState == contracts.SandboxDesiredStateDeleted {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	homeRunnerID := locked.Workspace.HomeRunnerID
	workspaceState := locked.Workspace.State
	mutationState := locked.Workspace.Mutation.State
	workspaceGeneration := locked.Workspace.Generation
	capacity := locked.Workspace.LogicalCapacityBytes
	if mutationState != "" {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	if workspaceState != "ready" || workspaceGeneration != generation {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	snapshot, _, err := lockSnapshotAfterWorkspace(
		ctx, tx, input.TenantRef, input.SubjectRef, locked, input.SnapshotID,
	)
	if err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrSnapshotNotFound)
	}
	if snapshot.SandboxID != input.SandboxID {
		return contracts.Operation{}, ports.ErrSnapshotNotFound
	}
	if snapshot.State != "ready" {
		return contracts.Operation{}, ports.ErrSnapshotUnavailable
	}
	if err := requireHomeRunnerReady(ctx, tx, homeRunnerID); err != nil {
		return contracts.Operation{}, err
	}
	targetGeneration := generation + 1
	input.Operation.SandboxID = input.SandboxID
	input.Operation.Snapshot = &snapshot
	if err := insertOperation(ctx, tx, input.TenantRef, input.SubjectRef, input.Operation); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.workspace_restores (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,snapshot_id,home_runner_id,
			operation_id,prepare_effect_id,swap_effect_id,finalize_effect_id,
			abort_effect_id,prepare_command_id,swap_command_id,finalize_command_id,abort_command_id,
			expected_generation,target_generation,state,prepare_receipt_json,swap_receipt_json,
			finalize_receipt_json,abort_receipt_json,failure_class,failure_message,created_at,updated_at,
			database_committed_at,finalized_at,failed_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
			$17,$18,'requested','{}','{}','{}','{}','','',$19,$19,NULL,NULL,NULL
		)`,
		input.RestoreID, input.TenantRef, input.SubjectRef, input.SandboxID, workspaceID,
		input.SnapshotID, homeRunnerID, input.Operation.ID, input.PrepareEffectID,
		input.SwapEffectID, input.FinalizeEffectID, input.AbortEffectID,
		input.PrepareCommandID, input.SwapCommandID, input.FinalizeCommandID, input.AbortCommandID,
		generation, targetGeneration, input.Now.UTC(),
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot restore insert failed: %w", err)
	}
	if err := setWorkspaceMutation(
		ctx, tx, workspaceID, "snapshot_restore", input.RestoreID,
		input.PrepareEffectID, input.Operation.ID,
		generation, targetGeneration, input.Now,
	); err != nil {
		return contracts.Operation{}, err
	}
	command := localWorkspaceCommand(
		input.PrepareCommandID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		input.Operation, input.PrepareEffectID, input.SandboxID, workspaceID, input.SnapshotID,
		generation, targetGeneration, capacity, homeRunnerID, input.FencingToken,
	)
	if err := queueLocalWorkspaceEffect(
		ctx, tx, input.PrepareEffectID, "local_snapshot_restore_prepare", input.SandboxID,
		generation, homeRunnerID, input.PrepareCommandID, input.SnapshotID, input.FencingToken,
		command, input.Now,
	); err != nil {
		return contracts.Operation{}, err
	}
	if err := recordSnapshotMutationIdempotency(
		ctx, tx, input.TenantRef, input.SubjectRef, "snapshot.restore",
		input.SandboxID, input.IdempotencyKey, input.RequestHash,
		input.Operation.ID, input.Now, input.IdempotencyEnds,
	); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET revision=revision+1,updated_at=$2 WHERE id=$1`,
		input.SandboxID, input.Now.UTC(),
	); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot restore Sandbox revision update failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Snapshot restore commit failed: %w", err)
	}
	return input.Operation, nil
}

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
			  AND state<>'deleted'`,
			cursor, tenantRef, subjectRef, sandboxID,
		).Scan(&cursorCreatedAt, &cursorID); err != nil {
			return contracts.SnapshotPage{}, errors.New("SecondBox Snapshot page cursor is invalid")
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
		  AND state<>'deleted' AND retain_until>$4
		  AND ($5='' OR (created_at,id)<($6,$5))
		ORDER BY created_at DESC,id DESC LIMIT $7`,
		tenantRef, subjectRef, sandboxID, now.UTC(), cursorID, cursorCreatedAt, limit+1,
	)
	if err != nil {
		return contracts.SnapshotPage{}, fmt.Errorf("SecondBox Snapshot list failed: %w", err)
	}
	defer rows.Close()
	items := make([]contracts.Snapshot, 0, limit+1)
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return contracts.SnapshotPage{}, err
		}
		items = append(items, snapshot)
	}
	if err := rows.Err(); err != nil {
		return contracts.SnapshotPage{}, fmt.Errorf("SecondBox Snapshot list iteration failed: %w", err)
	}
	page := contracts.SnapshotPage{Items: items}
	if len(page.Items) > limit {
		next := page.Items[limit-1].ID
		page.NextCursor = &next
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func (store *PostgresControlPlaneStore) GetSnapshot(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	snapshotID string,
	now time.Time,
) (contracts.Snapshot, error) {
	snapshot, err := scanSnapshot(store.pool.QueryRow(
		ctx, snapshotSelect+`
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		  AND state<>'deleted' AND retain_until>$4`,
		snapshotID, tenantRef, subjectRef, now.UTC(),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Snapshot{}, ports.ErrSnapshotNotFound
	}
	return snapshot, err
}

func lookupSnapshotMutationReplay(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	kind string,
	targetID string,
	idempotencyKey string,
	requestHash string,
	now time.Time,
) (contracts.Operation, bool, error) {
	lockKey := tenantRef + "\x1f" + subjectRef + "\x1f" + kind + "\x1f" + targetID + "\x1f" + idempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Operation{}, false, fmt.Errorf("SecondBox Snapshot idempotency lock failed: %w", err)
	}
	var priorHash, operationID string
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id,expires_at FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2 AND operation=$3
		  AND target_id=$4 AND idempotency_key=$5`,
		tenantRef, subjectRef, kind, targetID, idempotencyKey,
	).Scan(&priorHash, &operationID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Operation{}, false, nil
	}
	if err != nil {
		return contracts.Operation{}, false, fmt.Errorf("SecondBox Snapshot idempotency lookup failed: %w", err)
	}
	expired, err := deleteExpiredIdempotencyRecord(
		ctx, tx, tenantRef, subjectRef, kind, targetID, idempotencyKey, expiresAt, now,
	)
	if err != nil {
		return contracts.Operation{}, false, fmt.Errorf("SecondBox expired Snapshot idempotency cleanup failed: %w", err)
	}
	if expired {
		return contracts.Operation{}, false, nil
	}
	if priorHash != requestHash {
		return contracts.Operation{}, false, ports.ErrIdempotencyConflict
	}
	operation, err := getOperationWithQuerier(ctx, tx, tenantRef, subjectRef, `id=$3`, operationID)
	if err != nil {
		return contracts.Operation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Operation{}, false, fmt.Errorf("SecondBox Snapshot replay commit failed: %w", err)
	}
	return operation, true, nil
}

func recordSnapshotMutationIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	kind string,
	targetID string,
	idempotencyKey string,
	requestHash string,
	operationID string,
	now time.Time,
	expiresAt time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		tenantRef, subjectRef, kind, targetID, idempotencyKey,
		requestHash, operationID, now.UTC(), expiresAt.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Snapshot idempotency insert failed: %w", err)
	}
	return nil
}

func requireHomeRunnerReady(ctx context.Context, tx pgx.Tx, runnerID string) error {
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM secondbox.runners WHERE id=$1 FOR SHARE`,
		runnerID,
	).Scan(&state); err != nil || state != "ready" {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("SecondBox Snapshot home Runner lookup failed: %w", err)
		}
		return ports.ErrHomeRunnerUnavailable
	}
	return nil
}

func setWorkspaceMutation(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	kind string,
	mutationID string,
	effectID string,
	operationID string,
	expectedGeneration int64,
	targetGeneration int64,
	now time.Time,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_kind=$2,mutation_id=$3,mutation_effect_id=$4,
		    mutation_operation_id=$5,mutation_expected_generation=$6,
		    mutation_target_generation=$7,mutation_state='queued',updated_at=$8
		WHERE id=$1 AND mutation_state=''`,
		workspaceID, kind, mutationID, effectID, operationID,
		expectedGeneration, targetGeneration, now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox Workspace mutation admission failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrWorkspaceMutation
	}
	return nil
}

func localWorkspaceCommand(
	commandID string,
	kind runnerv1.LocalWorkspaceCommandKind,
	operation contracts.Operation,
	effectID string,
	sandboxID string,
	workspaceID string,
	snapshotID string,
	expectedGeneration int64,
	nextGeneration int64,
	capacity int64,
	homeRunnerID string,
	fencingToken []byte,
) *runnerv1.LocalWorkspaceCommand {
	return &runnerv1.LocalWorkspaceCommand{
		MessageId: commandID, CommandVersion: 1, Kind: kind,
		OperationId: operation.ID, EffectId: effectID, SandboxId: sandboxID,
		WorkspaceId: workspaceID, SnapshotId: snapshotID,
		ExpectedGeneration: uint64(expectedGeneration), NextGeneration: uint64(nextGeneration),
		LogicalCapacityBytes: uint64(capacity), FencingToken: append([]byte(nil), fencingToken...),
		Correlation: &runnerv1.Correlation{
			RequestId: operation.RequestID, OperationId: operation.ID,
			SandboxId: sandboxID, SandboxGeneration: uint64(expectedGeneration),
			RunnerId: homeRunnerID,
		},
	}
}

func queueLocalWorkspaceEffect(
	ctx context.Context,
	tx pgx.Tx,
	effectID string,
	effectKind string,
	sandboxID string,
	generation int64,
	runnerID string,
	commandID string,
	snapshotID string,
	fencingToken []byte,
	command *runnerv1.LocalWorkspaceCommand,
	now time.Time,
) error {
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{LocalWorkspace: command},
	})
	if err != nil {
		return fmt.Errorf("SecondBox local Workspace command encoding failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			$1,$2,$3,$4,'queued','','',$5,$6,$7,$8,0,8,$9,'',$10,'','','{}','{}',$10,$10
		)`,
		effectID, sandboxID, generation, effectKind, runnerID, commandID,
		snapshotID, fencingToken, now.UTC().Add(10*time.Minute), now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox local Workspace effect insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5,NULL)`,
		commandID, runnerID, effectID, payload, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox local Workspace command insert failed: %w", err)
	}
	return nil
}

const snapshotSelect = `
	SELECT id,tenant_ref,subject_ref,sandbox_id,workspace_id,source_generation,
	       name,size_bytes,state,metadata_json,retain_until,created_at,retention_ended_at
	FROM secondbox.snapshots`

type snapshotScanner interface {
	Scan(...any) error
}

func scanSnapshot(row snapshotScanner) (contracts.Snapshot, error) {
	var snapshot contracts.Snapshot
	var metadataJSON []byte
	var retainUntil time.Time
	if err := row.Scan(
		&snapshot.ID, &snapshot.TenantRef, &snapshot.SubjectRef,
		&snapshot.SandboxID, &snapshot.WorkspaceID, &snapshot.SourceGeneration,
		&snapshot.Name, &snapshot.SizeBytes, &snapshot.State, &metadataJSON,
		&retainUntil, &snapshot.CreatedAt, &snapshot.RetentionEndedAt,
	); err != nil {
		return contracts.Snapshot{}, err
	}
	snapshot.RetainUntil = &retainUntil
	if err := json.Unmarshal(metadataJSON, &snapshot.Metadata); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("SecondBox Snapshot metadata decoding failed: %w", err)
	}
	return snapshot, nil
}
