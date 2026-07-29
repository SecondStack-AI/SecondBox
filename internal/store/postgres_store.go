// Package store implements PostgreSQL-backed SecondBox control-plane authority.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// PostgresControlPlaneStore persists standalone SecondBox authority.
type PostgresControlPlaneStore struct {
	pool *pgxpool.Pool
}

// NewPostgresControlPlaneStore connects to the required PostgreSQL authority.
func NewPostgresControlPlaneStore(ctx context.Context, databaseURL string) (*PostgresControlPlaneStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox PostgreSQL pool creation failed: %w", err)
	}
	controlPlaneStore := &PostgresControlPlaneStore{pool: pool}
	if err := controlPlaneStore.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return controlPlaneStore, nil
}

// Close releases all PostgreSQL connections.
func (store *PostgresControlPlaneStore) Close() {
	store.pool.Close()
}

// Ping proves the PostgreSQL authority is reachable.
func (store *PostgresControlPlaneStore) Ping(ctx context.Context) error {
	if err := store.pool.Ping(ctx); err != nil {
		return fmt.Errorf("SecondBox PostgreSQL readiness failed: %w", err)
	}
	return nil
}

func (store *PostgresControlPlaneStore) CreateProfile(
	ctx context.Context,
	profile contracts.Profile,
	idempotency ports.AdminIdempotencyInput,
) (contracts.Profile, ports.AdminIdempotencyResult, error) {
	specJSON, err := json.Marshal(profile.CurrentRevision.Spec)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ProfileRevision spec encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Profile
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.profiles (name,state,current_revision_id,revision,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		profile.Name, profile.State, profile.CurrentRevision.ID, profile.Revision, profile.CreatedAt, profile.UpdatedAt,
	); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.profile_revisions (id,profile_name,revision_number,spec_json,created_at)
		VALUES ($1,$2,$3,$4,$5)`,
		profile.CurrentRevision.ID, profile.Name, profile.CurrentRevision.Number, specJSON, profile.CurrentRevision.CreatedAt,
	); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ProfileRevision insert failed: %w", err)
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, profile)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile create commit failed: %w", err)
	}
	return profile, idempotencyResult, nil
}

// EnsureBuiltInProfile materializes or advances one code-owned immutable Profile revision.
func (store *PostgresControlPlaneStore) EnsureBuiltInProfile(
	ctx context.Context,
	desired contracts.Profile,
) (contracts.Profile, error) {
	specJSON, err := json.Marshal(desired.CurrentRevision.Spec)
	if err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox built-in ProfileRevision spec encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"secondbox-built-in-profile:"+desired.Name,
	); err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile lock failed: %w", err)
	}
	current, err := scanProfile(tx.QueryRow(
		ctx, profileSelect+` WHERE profile.name=$1 FOR UPDATE OF profile`, desired.Name,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.profiles (
				name,state,current_revision_id,revision,created_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6)`,
			desired.Name, desired.State, desired.CurrentRevision.ID,
			desired.Revision, desired.CreatedAt, desired.UpdatedAt,
		); err != nil {
			return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile insert failed: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.profile_revisions (
				id,profile_name,revision_number,spec_json,created_at
			) VALUES ($1,$2,$3,$4,$5)`,
			desired.CurrentRevision.ID, desired.Name, desired.CurrentRevision.Number,
			specJSON, desired.CurrentRevision.CreatedAt,
		); err != nil {
			return contracts.Profile{}, fmt.Errorf("SecondBox built-in ProfileRevision insert failed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile commit failed: %w", err)
		}
		return desired, nil
	}
	if err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile lookup failed: %w", err)
	}
	if current.State != contracts.ProfileStateEnabled {
		return contracts.Profile{}, errors.New("SecondBox built-in Profile durable state is disabled")
	}
	switch {
	case current.CurrentRevision.Number > desired.CurrentRevision.Number:
		if err := tx.Commit(ctx); err != nil {
			return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile forward-version commit failed: %w", err)
		}
		return current, nil
	case current.CurrentRevision.Number == desired.CurrentRevision.Number:
		if current.CurrentRevision.ID != desired.CurrentRevision.ID ||
			!reflect.DeepEqual(current.CurrentRevision.Spec, desired.CurrentRevision.Spec) {
			return contracts.Profile{}, errors.New("SecondBox built-in Profile revision drift detected")
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile verification commit failed: %w", err)
		}
		return current, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ($1,$2,$3,$4,$5)`,
		desired.CurrentRevision.ID, desired.Name, desired.CurrentRevision.Number,
		specJSON, desired.CurrentRevision.CreatedAt,
	); err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox built-in ProfileRevision append failed: %w", err)
	}
	current.CurrentRevision = desired.CurrentRevision
	current.Revision++
	current.UpdatedAt = desired.UpdatedAt.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.profiles
		SET current_revision_id=$2,revision=$3,updated_at=$4
		WHERE name=$1`,
		current.Name, current.CurrentRevision.ID, current.Revision, current.UpdatedAt,
	); err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile head update failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox built-in Profile revision commit failed: %w", err)
	}
	return current, nil
}

func (store *PostgresControlPlaneStore) ReviseProfile(
	ctx context.Context,
	name string,
	revision contracts.ProfileRevision,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.Profile, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile revise transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Profile
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	profile, err := scanProfile(tx.QueryRow(ctx, profileSelect+` WHERE profile.name=$1 FOR UPDATE OF profile`, name))
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrProfileNotFound)
	}
	if expectedRevision > 0 && expectedRevision != profile.Revision {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	revision.Number = profile.CurrentRevision.Number + 1
	specJSON, err := json.Marshal(revision.Spec)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ProfileRevision spec encoding failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.profile_revisions (id,profile_name,revision_number,spec_json,created_at)
		VALUES ($1,$2,$3,$4,$5)`, revision.ID, name, revision.Number, specJSON, revision.CreatedAt,
	); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ProfileRevision append failed: %w", err)
	}
	profile.CurrentRevision = revision
	profile.Revision++
	profile.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.profiles SET current_revision_id=$2,revision=$3,updated_at=$4 WHERE name=$1`,
		name, revision.ID, profile.Revision, profile.UpdatedAt,
	); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile head update failed: %w", err)
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, profile)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile revise commit failed: %w", err)
	}
	return profile, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) DisableProfile(
	ctx context.Context,
	name string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.Profile, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile disable transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Profile
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	profile, err := scanProfile(tx.QueryRow(ctx, profileSelect+` WHERE profile.name=$1 FOR UPDATE OF profile`, name))
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrProfileNotFound)
	}
	if expectedRevision > 0 && expectedRevision != profile.Revision {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if profile.State != contracts.ProfileStateDisabled {
		profile.State = contracts.ProfileStateDisabled
		profile.Revision++
		profile.UpdatedAt = now.UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.profiles SET state=$2,revision=$3,updated_at=$4 WHERE name=$1`,
			name, profile.State, profile.Revision, profile.UpdatedAt,
		); err != nil {
			return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile disable failed: %w", err)
		}
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, profile)
	if err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile disable commit failed: %w", err)
	}
	return profile, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) GetProfile(ctx context.Context, name string) (contracts.Profile, error) {
	profile, err := scanProfile(store.pool.QueryRow(ctx, profileSelect+` WHERE profile.name=$1`, name))
	return profile, mapNotFound(err, ports.ErrProfileNotFound)
}

func (store *PostgresControlPlaneStore) ListProfiles(
	ctx context.Context,
	limit int,
	cursor string,
) (contracts.ProfilePage, error) {
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		profileListCursorResource,
		"",
		cursor,
		`SELECT created_at FROM secondbox.profiles WHERE name=$1`,
	)
	if err != nil {
		return contracts.ProfilePage{}, err
	}
	rows, err := store.pool.Query(ctx, profileSelect+`
		WHERE NOT $1 OR (profile.created_at,profile.name) > ($2,$3)
		ORDER BY profile.created_at,profile.name
		LIMIT $4`,
		boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.ProfilePage{}, fmt.Errorf("SecondBox Profile list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.ProfilePage{Items: make([]contracts.Profile, 0)}
	for rows.Next() {
		profile, scanErr := scanProfile(rows)
		if scanErr != nil {
			return contracts.ProfilePage{}, fmt.Errorf("SecondBox Profile list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, profile)
	}
	if err := rows.Err(); err != nil {
		return contracts.ProfilePage{}, fmt.Errorf("SecondBox Profile list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(
			profileListCursorResource, "", page.Items[limit-1].Name,
		)
		if err != nil {
			return contracts.ProfilePage{}, err
		}
	}
	return page, nil
}

func (store *PostgresControlPlaneStore) RegisterRunnerPool(
	ctx context.Context,
	pool contracts.RunnerPool,
) error {
	architecturesJSON, err := json.Marshal(pool.Architectures)
	if err != nil {
		return fmt.Errorf("SecondBox runner-pool architectures encoding failed: %w", err)
	}
	capabilitiesJSON, err := json.Marshal(pool.Capabilities)
	if err != nil {
		return fmt.Errorf("SecondBox runner-pool capabilities encoding failed: %w", err)
	}
	capacityPolicyJSON, err := json.Marshal(pool.CapacityPolicy)
	if err != nil {
		return fmt.Errorf("SecondBox runner-pool capacity policy encoding failed: %w", err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO secondbox.runner_pools (
			name,state,architectures_json,capabilities_json,capacity_policy_json,
			ready_runner_count,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (name) DO UPDATE SET
			state=EXCLUDED.state,architectures_json=EXCLUDED.architectures_json,
			capabilities_json=EXCLUDED.capabilities_json,
			capacity_policy_json=EXCLUDED.capacity_policy_json,
			ready_runner_count=EXCLUDED.ready_runner_count,
			revision=secondbox.runner_pools.revision+1,updated_at=EXCLUDED.updated_at`,
		pool.Name, pool.State, architecturesJSON, capabilitiesJSON, capacityPolicyJSON,
		pool.ReadyRunnerCount, pool.Revision, pool.CreatedAt, pool.UpdatedAt,
	); err != nil {
		return fmt.Errorf("SecondBox RunnerPool registration failed: %w", err)
	}
	return nil
}

func (store *PostgresControlPlaneStore) CreateSandbox(
	ctx context.Context,
	input ports.CreateSandboxInput,
) (contracts.Sandbox, contracts.Operation, bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.Principal.TenantRef + "\x1f" + input.Principal.SubjectRef +
		"\x1fcreate-sandbox\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox idempotency lock failed: %w", err)
	}
	var priorHash, priorSandboxID string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id
		FROM secondbox.idempotency_records
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND operation='sandbox.create' AND target_id='' AND idempotency_key=$3`,
		input.Principal.TenantRef, input.Principal.SubjectRef, input.IdempotencyKey,
	).Scan(&priorHash, &priorSandboxID)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrIdempotencyConflict
		}
		sandbox, err := getSandboxWithQuerier(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, priorSandboxID,
		)
		if err != nil {
			return contracts.Sandbox{}, contracts.Operation{}, false, err
		}
		operation, err := getCreateOperationWithQuerier(
			ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, priorSandboxID,
		)
		if err != nil {
			return contracts.Sandbox{}, contracts.Operation{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox idempotency replay commit failed: %w", err)
		}
		return sandbox, operation, false, nil
	}
	if !errors.Is(idempotencyErr, pgx.ErrNoRows) {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox idempotency lookup failed: %w", idempotencyErr)
	}

	profile, err := scanProfile(tx.QueryRow(ctx, profileSelect+`
		WHERE profile.name=$1 FOR UPDATE OF profile`, input.Sandbox.Profile))
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, mapNotFound(err, ports.ErrProfileNotFound)
	}
	if profile.State != contracts.ProfileStateEnabled {
		return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrProfileDisabled
	}
	if err := ensureCompatibleRunnerPool(ctx, tx, profile.CurrentRevision.Spec); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	if err := ensureSubjectQuota(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.SubjectQuota, input.Sandbox.CreatedAt); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	subjectQuota, err := readSubjectQuota(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	subjectUsage, err := readSubjectQuotaUsage(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	requestedCPU := profile.CurrentRevision.Spec.Resources.CPUMillis
	requestedMemory := profile.CurrentRevision.Spec.Resources.MemoryBytes
	requestedActiveInstances := int64(0)
	if profile.CurrentRevision.Spec.Lifecycle.InitialState == contracts.SandboxDesiredStateRunning {
		requestedActiveInstances = 1
	}
	if quotaWouldExceed(
		subjectQuota, subjectUsage, requestedCPU, requestedMemory, requestedActiveInstances,
	) {
		return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrQuotaExceeded
	}

	sandbox := input.Sandbox
	sandbox.ProjectID = input.Principal.ProjectID
	sandbox.TenantRef = input.Principal.TenantRef
	sandbox.SubjectRef = input.Principal.SubjectRef
	sandbox.ProfileRevisionID = profile.CurrentRevision.ID
	sandbox.Workspace = input.Workspace
	sandbox.Workspace.TenantRef = input.Principal.TenantRef
	sandbox.Workspace.SubjectRef = input.Principal.SubjectRef
	sandbox.Workspace.Generation = sandbox.Generation
	initialLifecycleIntent := ""
	if profile.CurrentRevision.Spec.Lifecycle.InitialState == contracts.SandboxDesiredStateRunning {
		sandbox.DesiredState = contracts.SandboxDesiredStateRunning
		sandbox.State = contracts.SandboxStateCreating
		input.Operation.State = contracts.OperationStatePending
		initialLifecycleIntent = "create"
	} else {
		sandbox.DesiredState = contracts.SandboxDesiredStateStopped
		sandbox.State = contracts.SandboxStateStopped
		input.Operation.State = contracts.OperationStateSucceeded
		input.Operation.Sandbox = &sandbox
	}
	metadataJSON, err := json.Marshal(sandbox.Metadata)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox metadata encoding failed: %w", err)
	}
	compatibilityJSON, err := json.Marshal(map[string]any{
		"pool": profile.CurrentRevision.Spec.Pool, "architecture": profile.CurrentRevision.Spec.Architecture,
		"backend": profile.CurrentRevision.Spec.Backend,
	})
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox compatibility encoding failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,generation,retained_bytes,current_checkpoint_id,
			current_checkpoint_sha256,current_checkpoint_size_bytes,retention_state,
			garbage_collection_state,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		sandbox.Workspace.ID, sandbox.TenantRef, sandbox.SubjectRef,
		sandbox.ID, sandbox.Workspace.Generation,
		sandbox.Workspace.RetainedBytes, "", "", 0, "retained", "reachable",
		sandbox.Workspace.CreatedAt, sandbox.Workspace.UpdatedAt,
	); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Workspace insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,generation,workspace_id,
			current_instance_id,metadata_json,compatibility_summary_json,last_activity_at,revision,
			lifecycle_termination_reason,lifecycle_failure_class,lifecycle_failure_message,lifecycle_intent_kind,
			reconcile_owner,reconcile_claim_expires_at,next_reconcile_at,reconcile_retry_count,
			reconcile_retry_limit,created_at,updated_at,deleted_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		sandbox.ID, sandbox.TenantRef, sandbox.SubjectRef,
		sandbox.Profile, sandbox.ProfileRevisionID, sandbox.State,
		sandbox.DesiredState, sandbox.Generation, sandbox.Workspace.ID, "", metadataJSON,
		compatibilityJSON, sandbox.LastActivityAt, sandbox.Revision, "", "", "", initialLifecycleIntent,
		"", nil, sandbox.UpdatedAt, 0, 8, sandbox.CreatedAt, sandbox.UpdatedAt, sandbox.DeletedAt,
	); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox insert failed: %w", err)
	}
	input.Operation.SandboxID = sandbox.ID
	if err := insertOperation(
		ctx, tx, sandbox.TenantRef, sandbox.SubjectRef, input.Operation,
	); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,
			request_hash,response_resource_id,created_at,expires_at
		) VALUES ($1,$2,'sandbox.create','',$3,$4,$5,$6,$7)`,
		sandbox.TenantRef, sandbox.SubjectRef,
		input.IdempotencyKey, input.RequestHash, sandbox.ID,
		sandbox.CreatedAt, input.IdempotencyEnds,
	); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox idempotency insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox create commit failed: %w", err)
	}
	return sandbox, input.Operation, true, nil
}

func (store *PostgresControlPlaneStore) GetSandbox(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
) (contracts.Sandbox, error) {
	return getSandboxWithQuerier(ctx, store.pool, tenantRef, subjectRef, sandboxID)
}

// GetSubjectUsage reads one subject's current quota and aggregate reservations.
func (store *PostgresControlPlaneStore) GetSubjectUsage(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
) (contracts.SubjectUsage, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.SubjectUsage{}, fmt.Errorf("SecondBox subject usage transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	limits, err := readSubjectQuota(ctx, tx, tenantRef, subjectRef)
	if err != nil {
		return contracts.SubjectUsage{}, err
	}
	usage, err := readSubjectQuotaUsage(ctx, tx, tenantRef, subjectRef)
	if err != nil {
		return contracts.SubjectUsage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.SubjectUsage{}, fmt.Errorf("SecondBox subject usage commit failed: %w", err)
	}
	return contracts.SubjectUsage{
		TenantRef: tenantRef, SubjectRef: subjectRef, Limits: limits,
		Usage: contracts.QuotaUsage{
			Sandboxes: usage.sandboxes, ActiveInstances: usage.activeInstances,
			CPUMillis: usage.cpuMillis, MemoryBytes: usage.memoryBytes,
			RetainedBytes: usage.retainedBytes, Snapshots: usage.snapshots,
			Artifacts: usage.artifacts, PortSessions: usage.portSessions,
			ConcurrentOperations: usage.concurrentOperations,
		},
	}, nil
}

func (store *PostgresControlPlaneStore) ListSandboxes(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	limit int,
	cursor string,
) (contracts.SandboxPage, error) {
	scope := "tenant=" + tenantRef + "\x1fsubject=" + subjectRef
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		sandboxListCursorResource,
		scope,
		cursor,
		`SELECT created_at FROM secondbox.sandboxes
		 WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef,
		subjectRef,
	)
	if err != nil {
		return contracts.SandboxPage{}, err
	}
	rows, err := store.pool.Query(ctx, sandboxSelect+`
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2
		  AND (NOT $3 OR (sandbox.created_at,sandbox.id) > ($4,$5))
		ORDER BY sandbox.created_at,sandbox.id
		LIMIT $6`,
		tenantRef, subjectRef, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.SandboxPage{}, fmt.Errorf("SecondBox Sandbox list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.SandboxPage{Items: make([]contracts.Sandbox, 0)}
	for rows.Next() {
		sandbox, scanErr := scanSandbox(rows)
		if scanErr != nil {
			return contracts.SandboxPage{}, fmt.Errorf("SecondBox Sandbox list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, sandbox)
	}
	if err := rows.Err(); err != nil {
		return contracts.SandboxPage{}, fmt.Errorf("SecondBox Sandbox list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(
			sandboxListCursorResource, scope, page.Items[limit-1].ID,
		)
		if err != nil {
			return contracts.SandboxPage{}, err
		}
	}
	return page, nil
}

func (store *PostgresControlPlaneStore) GetOperation(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	operationID string,
) (contracts.Operation, error) {
	return getOperationWithQuerier(
		ctx, store.pool, tenantRef, subjectRef, `id=$3`, operationID,
	)
}

func getCreateOperationWithQuerier(
	ctx context.Context,
	querier queryRower,
	tenantRef string,
	subjectRef string,
	sandboxID string,
) (contracts.Operation, error) {
	return getOperationWithQuerier(
		ctx, querier, tenantRef, subjectRef,
		`sandbox_id=$3 AND kind='create'`, sandboxID,
	)
}

func getOperationWithQuerier(
	ctx context.Context,
	querier queryRower,
	tenantRef string,
	subjectRef string,
	predicate string,
	identifier string,
) (contracts.Operation, error) {
	queries := map[string]string{
		"id=$3": `WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		"sandbox_id=$3 AND kind='create'": `WHERE tenant_ref=$1 AND subject_ref=$2
			AND sandbox_id=$3 AND kind='create'`,
	}
	where, ok := queries[predicate]
	if !ok {
		return contracts.Operation{}, errors.New("SecondBox Operation lookup predicate is invalid")
	}
	var operation contracts.Operation
	var errorCode, errorMessage string
	var retryable bool
	var requestMetadataJSON []byte
	err := querier.QueryRow(ctx, `
		SELECT id,tenant_ref,subject_ref,sandbox_id,kind,state,request_id,request_metadata_json,error_code,error_message,retryable,
		       created_at,started_at,completed_at,updated_at
		FROM secondbox.operations `+where+` ORDER BY created_at LIMIT 1`,
		tenantRef, subjectRef, identifier,
	).Scan(
		&operation.ID, &operation.TenantRef, &operation.SubjectRef,
		&operation.SandboxID, &operation.Kind, &operation.State, &operation.RequestID,
		&requestMetadataJSON,
		&errorCode, &errorMessage, &retryable, &operation.CreatedAt, &operation.StartedAt,
		&operation.CompletedAt, &operation.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Operation{}, ports.ErrSandboxNotFound
	}
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox Operation lookup failed: %w", err)
	}
	if errorCode != "" {
		operation.Error = &contracts.Problem{
			Type:  "https://secondbox.dev/problems/" + errorCode,
			Title: errorMessage, Status: 503, Code: errorCode,
			RequestID: operation.RequestID, Retryable: retryable,
		}
	}
	if len(requestMetadataJSON) > 0 {
		if err := json.Unmarshal(requestMetadataJSON, &operation.RequestMetadata); err != nil {
			return contracts.Operation{}, fmt.Errorf("SecondBox Operation request metadata decoding failed: %w", err)
		}
	}
	return operation, nil
}

func (store *PostgresControlPlaneStore) ListAuditEvents(
	ctx context.Context,
	tenantRef string,
	limit int,
) ([]contracts.AuditEvent, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,resource_id,
		       outcome,request_id,details_json,created_at
		FROM secondbox.audit_events
		WHERE ($1='' OR tenant_ref=$1)
		ORDER BY created_at DESC,id DESC LIMIT $2`, tenantRef, limit)
	if err != nil {
		return nil, fmt.Errorf("SecondBox audit list failed: %w", err)
	}
	defer rows.Close()
	events := make([]contracts.AuditEvent, 0)
	for rows.Next() {
		var event contracts.AuditEvent
		var detailsJSON []byte
		if err := rows.Scan(
			&event.ID, &event.TenantRef, &event.SubjectRef,
			&event.ActorKind, &event.ActorID, &event.Action,
			&event.ResourceKind, &event.ResourceID, &event.Outcome, &event.RequestID,
			&detailsJSON, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("SecondBox audit list scan failed: %w", err)
		}
		event.ProjectID = event.TenantRef
		if err := json.Unmarshal(detailsJSON, &event.Details); err != nil {
			return nil, fmt.Errorf("SecondBox audit details decoding failed: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// AppendAuditEvent persists service-layer mutation evidence.
func (store *PostgresControlPlaneStore) AppendAuditEvent(
	ctx context.Context,
	event contracts.AuditEvent,
) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox audit transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := insertAuditEvent(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox audit commit failed: %w", err)
	}
	return nil
}

func (store *PostgresControlPlaneStore) ReadMetricsSnapshot(
	ctx context.Context,
) (contracts.MetricsSnapshot, error) {
	snapshot := contracts.MetricsSnapshot{
		SandboxStates: map[string]int64{
			contracts.SandboxStateCreating: 0, contracts.SandboxStateStopped: 0,
			contracts.SandboxStateStarting: 0, contracts.SandboxStateReady: 0,
			contracts.SandboxStateDraining: 0, contracts.SandboxStateStopping: 0,
			contracts.SandboxStateCheckpointing: 0,
			contracts.SandboxStateFailed:        0, contracts.SandboxStateDeleting: 0,
			contracts.SandboxStateDeleted: 0,
		},
		OperationStates: map[string]int64{
			contracts.OperationStatePending: 0, contracts.OperationStateRunning: 0,
			contracts.OperationStateSucceeded: 0, contracts.OperationStateFailed: 0,
			contracts.OperationStateCancelled: 0,
		},
	}
	for query, destination := range map[string]map[string]int64{
		`SELECT state,count(*) FROM secondbox.sandboxes GROUP BY state`:  snapshot.SandboxStates,
		`SELECT state,count(*) FROM secondbox.operations GROUP BY state`: snapshot.OperationStates,
	} {
		rows, err := store.pool.Query(ctx, query)
		if err != nil {
			return contracts.MetricsSnapshot{}, fmt.Errorf("SecondBox metrics projection failed: %w", err)
		}
		for rows.Next() {
			var state string
			var count int64
			if err := rows.Scan(&state, &count); err != nil {
				rows.Close()
				return contracts.MetricsSnapshot{}, fmt.Errorf("SecondBox metrics projection scan failed: %w", err)
			}
			if _, fixed := destination[state]; fixed {
				destination[state] = count
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return contracts.MetricsSnapshot{}, fmt.Errorf("SecondBox metrics projection iteration failed: %w", err)
		}
		rows.Close()
	}
	return snapshot, nil
}

type rowScanner interface {
	Scan(...any) error
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const profileSelect = `
	SELECT profile.name,profile.state,profile.revision,profile.created_at,profile.updated_at,
	       revision.id,revision.revision_number,revision.spec_json,revision.created_at
	FROM secondbox.profiles AS profile
	JOIN secondbox.profile_revisions AS revision ON revision.id=profile.current_revision_id`

func scanProfile(row rowScanner) (contracts.Profile, error) {
	var profile contracts.Profile
	var specJSON []byte
	if err := row.Scan(
		&profile.Name, &profile.State, &profile.Revision, &profile.CreatedAt, &profile.UpdatedAt,
		&profile.CurrentRevision.ID, &profile.CurrentRevision.Number, &specJSON,
		&profile.CurrentRevision.CreatedAt,
	); err != nil {
		return contracts.Profile{}, err
	}
	if err := json.Unmarshal(specJSON, &profile.CurrentRevision.Spec); err != nil {
		return contracts.Profile{}, fmt.Errorf("SecondBox ProfileRevision spec decoding failed: %w", err)
	}
	return profile, nil
}

const sandboxSelect = `
	SELECT sandbox.id,sandbox.tenant_ref,sandbox.subject_ref,
	       sandbox.profile_name,sandbox.profile_revision_id,
	       sandbox.state,sandbox.desired_state,sandbox.generation,sandbox.metadata_json,
	       sandbox.last_activity_at,sandbox.revision,sandbox.created_at,sandbox.updated_at,sandbox.deleted_at,
	       workspace.id,workspace.tenant_ref,workspace.subject_ref,
	       workspace.generation,workspace.retained_bytes,workspace.current_checkpoint_id,
	       COALESCE(workspace.current_checkpoint_sha256,''),COALESCE(workspace.current_checkpoint_size_bytes,0),
	       COALESCE(workspace.retention_state,''),workspace.created_at,workspace.updated_at,
	       instance.id,instance.state,COALESCE(instance.guest_liveness,''),instance.termination_reason,
	       instance.created_at,instance.updated_at,instance.ready_at,instance.guest_heartbeat_at,instance.stopped_at
	FROM secondbox.sandboxes AS sandbox
	JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
	LEFT JOIN secondbox.instances AS instance ON instance.id=sandbox.current_instance_id`

func scanSandbox(row rowScanner) (contracts.Sandbox, error) {
	var sandbox contracts.Sandbox
	var metadataJSON []byte
	var instanceID, instanceState, guestLiveness, terminationReason sql.NullString
	var instanceCreatedAt, instanceUpdatedAt sql.NullTime
	var readyAt, guestHeartbeatAt, stoppedAt sql.NullTime
	if err := row.Scan(
		&sandbox.ID, &sandbox.TenantRef, &sandbox.SubjectRef,
		&sandbox.Profile, &sandbox.ProfileRevisionID,
		&sandbox.State, &sandbox.DesiredState, &sandbox.Generation, &metadataJSON,
		&sandbox.LastActivityAt, &sandbox.Revision, &sandbox.CreatedAt, &sandbox.UpdatedAt,
		&sandbox.DeletedAt, &sandbox.Workspace.ID,
		&sandbox.Workspace.TenantRef, &sandbox.Workspace.SubjectRef,
		&sandbox.Workspace.Generation,
		&sandbox.Workspace.RetainedBytes, &sandbox.Workspace.CurrentCheckpointID,
		&sandbox.Workspace.CurrentCheckpointHash, &sandbox.Workspace.CurrentCheckpointSize,
		&sandbox.Workspace.RetentionState, &sandbox.Workspace.CreatedAt, &sandbox.Workspace.UpdatedAt,
		&instanceID, &instanceState, &guestLiveness, &terminationReason,
		&instanceCreatedAt, &instanceUpdatedAt, &readyAt, &guestHeartbeatAt, &stoppedAt,
	); err != nil {
		return contracts.Sandbox{}, err
	}
	sandbox.ProjectID = sandbox.TenantRef
	if err := json.Unmarshal(metadataJSON, &sandbox.Metadata); err != nil {
		return contracts.Sandbox{}, fmt.Errorf("SecondBox Sandbox metadata decoding failed: %w", err)
	}
	if instanceID.Valid {
		sandbox.Instance = &contracts.Instance{
			ID: instanceID.String, SandboxID: sandbox.ID, Generation: sandbox.Generation,
			State: instanceState.String, GuestLiveness: guestLiveness.String,
			TerminationReason: terminationReason.String, CreatedAt: instanceCreatedAt.Time,
			UpdatedAt: instanceUpdatedAt.Time,
		}
		if readyAt.Valid {
			sandbox.Instance.ReadyAt = &readyAt.Time
		}
		if guestHeartbeatAt.Valid {
			sandbox.Instance.GuestHeartbeatAt = &guestHeartbeatAt.Time
		}
		if stoppedAt.Valid {
			sandbox.Instance.StoppedAt = &stoppedAt.Time
		}
	}
	return sandbox, nil
}

func getSandboxWithQuerier(
	ctx context.Context,
	querier queryRower,
	tenantRef string,
	subjectRef string,
	sandboxID string,
) (contracts.Sandbox, error) {
	sandbox, err := scanSandbox(querier.QueryRow(ctx, sandboxSelect+`
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$3`,
		tenantRef, subjectRef, sandboxID))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Sandbox{}, ports.ErrSandboxNotFound
	}
	if err != nil {
		return contracts.Sandbox{}, fmt.Errorf("SecondBox Sandbox lookup failed: %w", err)
	}
	return sandbox, nil
}

func encodeAuthorityLists(scopes []string, grants []string) ([]byte, []byte, error) {
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox scopes encoding failed: %w", err)
	}
	grantsJSON, err := json.Marshal(grants)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox profile grants encoding failed: %w", err)
	}
	return scopesJSON, grantsJSON, nil
}

func insertAuditEvent(ctx context.Context, tx pgx.Tx, event contracts.AuditEvent) error {
	if event.TenantRef == "" {
		event.TenantRef = event.ProjectID
	}
	if event.TenantRef == "" {
		event.TenantRef = "secondbox"
	}
	if event.SubjectRef == "" {
		event.SubjectRef = event.ActorID
	}
	if event.SubjectRef == "" {
		event.SubjectRef = "secondbox"
	}
	if event.Details == nil {
		event.Details = map[string]string{}
	}
	detailsJSON, err := json.Marshal(event.Details)
	if err != nil {
		return fmt.Errorf("SecondBox audit details encoding failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.audit_events (
			id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,resource_id,
			outcome,request_id,details_json,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		event.ID, event.TenantRef, event.SubjectRef,
		event.ActorKind, event.ActorID, event.Action,
		event.ResourceKind, event.ResourceID, event.Outcome, event.RequestID,
		detailsJSON, event.CreatedAt,
	); err != nil {
		return fmt.Errorf("SecondBox audit insert failed: %w", err)
	}
	return nil
}

func insertOperation(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	operation contracts.Operation,
) error {
	var errorCode, errorMessage string
	var retryable bool
	if operation.Error != nil {
		errorCode = operation.Error.Code
		errorMessage = operation.Error.Title
		retryable = operation.Error.Retryable
	}
	requestMetadataJSON, err := json.Marshal(operation.RequestMetadata)
	if err != nil {
		return fmt.Errorf("SecondBox Operation request metadata encoding failed: %w", err)
	}
	completedAt := operation.CompletedAt
	if completedAt == nil && (operation.State == contracts.OperationStateSucceeded || operation.State == contracts.OperationStateFailed) {
		value := operation.UpdatedAt
		completedAt = &value
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,kind,state,request_id,request_metadata_json,error_code,error_message,retryable,
			created_at,started_at,completed_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		operation.ID, tenantRef, subjectRef,
		operation.SandboxID, operation.Kind, operation.State, operation.RequestID,
		requestMetadataJSON, errorCode, errorMessage, retryable, operation.CreatedAt,
		operation.StartedAt, completedAt, operation.UpdatedAt,
	); err != nil {
		return fmt.Errorf("SecondBox Operation insert failed: %w", err)
	}
	return nil
}

func ensureCompatibleRunnerPool(ctx context.Context, tx pgx.Tx, spec contracts.ProfileRevisionSpec) error {
	var state string
	var architecturesJSON, capabilitiesJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT state,architectures_json,capabilities_json
		FROM secondbox.runner_pools WHERE name=$1 AND ready_runner_count>0`, spec.Pool).Scan(
		&state, &architecturesJSON, &capabilitiesJSON,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.ErrRunnerPoolUnavailable
		}
		return fmt.Errorf("SecondBox runner-pool compatibility lookup failed: %w", err)
	}
	if state != contracts.RunnerPoolStateReady {
		return ports.ErrRunnerPoolUnavailable
	}
	var architectures, capabilities []string
	if err := json.Unmarshal(architecturesJSON, &architectures); err != nil {
		return fmt.Errorf("SecondBox runner-pool architectures decoding failed: %w", err)
	}
	if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil {
		return fmt.Errorf("SecondBox runner-pool capabilities decoding failed: %w", err)
	}
	requiredCapabilities := []string{"firecracker"}
	if spec.Checkpoint.OnStop {
		requiredCapabilities = append(requiredCapabilities, "checkpoint")
	}
	if !contains(architectures, spec.Architecture) || !isScopeSubset(requiredCapabilities, capabilities) {
		return ports.ErrRunnerPoolUnavailable
	}
	return nil
}

type quotaUsage struct {
	sandboxes, activeInstances, cpuMillis, memoryBytes int64
	retainedBytes, snapshots, artifacts, portSessions  int64
	concurrentOperations                               int64
}

func ensureSubjectQuota(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	quota contracts.QuotaLimits,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
			max_retained_bytes,max_snapshots,max_artifacts,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant_ref,subject_ref) DO NOTHING`,
		tenantRef, subjectRef, quota.MaxSandboxes, quota.MaxActiveInstances,
		quota.MaxCPUMillis, quota.MaxMemoryBytes, quota.MaxRetainedBytes,
		quota.MaxSnapshots, quota.MaxArtifacts, quota.MaxPortSessions,
		quota.MaxConcurrentOperations, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox subject quota initialization failed: %w", err)
	}
	return nil
}

func readSubjectQuota(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
) (contracts.QuotaLimits, error) {
	var quota contracts.QuotaLimits
	if err := tx.QueryRow(ctx, `
		SELECT max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
		       max_retained_bytes,max_snapshots,max_artifacts,max_port_sessions,max_concurrent_operations
		FROM secondbox.subject_quotas
		WHERE tenant_ref=$1 AND subject_ref=$2
		FOR UPDATE`, tenantRef, subjectRef).Scan(
		&quota.MaxSandboxes, &quota.MaxActiveInstances, &quota.MaxCPUMillis,
		&quota.MaxMemoryBytes, &quota.MaxRetainedBytes, &quota.MaxSnapshots,
		&quota.MaxArtifacts, &quota.MaxPortSessions, &quota.MaxConcurrentOperations,
	); err != nil {
		return contracts.QuotaLimits{}, fmt.Errorf("SecondBox quota lookup failed: %w", err)
	}
	return quota, nil
}

func readSubjectQuotaUsage(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
) (quotaUsage, error) {
	var usage quotaUsage
	if err := tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE sandbox.state IN ('starting','ready','draining','stopping')),
		       COALESCE(sum((revision.spec_json->'resources'->>'cpuMillis')::bigint),0),
		       COALESCE(sum((revision.spec_json->'resources'->>'memoryBytes')::bigint),0),
		       COALESCE(sum(workspace.retained_bytes),0)
		         +(SELECT COALESCE(sum(size_bytes),0) FROM secondbox.artifacts
		           WHERE tenant_ref=$1 AND subject_ref=$2 AND state<>'deleted')
		         +(SELECT COALESCE(sum(size_bytes),0) FROM secondbox.workspace_checkpoints
		           WHERE tenant_ref=$1 AND subject_ref=$2 AND state IN ('staging','verified')),
		       (SELECT count(*) FROM secondbox.snapshots
		        WHERE tenant_ref=$1 AND subject_ref=$2 AND state='published'),
		       (SELECT count(*) FROM secondbox.artifacts
		        WHERE tenant_ref=$1 AND subject_ref=$2 AND state<>'deleted'),
		       (SELECT count(*) FROM secondbox.port_sessions
		        WHERE tenant_ref=$1 AND subject_ref=$2 AND state='open'),
		       (SELECT count(*) FROM secondbox.data_plane_sessions
		        WHERE tenant_ref=$1 AND subject_ref=$2 AND state IN ('pending','running','cancelling'))
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.state<>'deleted'`,
		tenantRef, subjectRef).Scan(
		&usage.sandboxes, &usage.activeInstances, &usage.cpuMillis, &usage.memoryBytes,
		&usage.retainedBytes, &usage.snapshots, &usage.artifacts, &usage.portSessions,
		&usage.concurrentOperations,
	); err != nil {
		return quotaUsage{}, fmt.Errorf("SecondBox quota usage lookup failed: %w", err)
	}
	return usage, nil
}

func quotaWouldExceed(
	quota contracts.QuotaLimits,
	usage quotaUsage,
	requestedCPU int64,
	requestedMemory int64,
	requestedActiveInstances int64,
) bool {
	return usage.sandboxes+1 > quota.MaxSandboxes ||
		usage.activeInstances+requestedActiveInstances > quota.MaxActiveInstances ||
		usage.cpuMillis+requestedCPU > quota.MaxCPUMillis ||
		usage.memoryBytes+requestedMemory > quota.MaxMemoryBytes ||
		usage.retainedBytes > quota.MaxRetainedBytes ||
		usage.snapshots > quota.MaxSnapshots ||
		usage.artifacts > quota.MaxArtifacts ||
		usage.portSessions > quota.MaxPortSessions ||
		usage.concurrentOperations > quota.MaxConcurrentOperations
}

func mapNotFound(err error, notFound error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound
	}
	return err
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func isScopeSubset(subset []string, superset []string) bool {
	for _, candidate := range subset {
		if !contains(superset, candidate) {
			return false
		}
	}
	return true
}

func intersectScopes(first []string, second []string) []string {
	result := make([]string, 0)
	for _, scope := range first {
		if contains(second, scope) {
			result = append(result, scope)
		}
	}
	sort.Strings(result)
	return result
}
