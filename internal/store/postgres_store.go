// Package store implements PostgreSQL-backed SecondBox control-plane authority.
package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// InitializeBootstrapAdmin creates the first operator authority exactly once.
func (store *PostgresControlPlaneStore) InitializeBootstrapAdmin(
	ctx context.Context,
	credentialHash []byte,
	now time.Time,
	audit contracts.AuditEvent,
) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox bootstrap transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('secondbox-bootstrap-admin',0))`); err != nil {
		return fmt.Errorf("SecondBox bootstrap lock failed: %w", err)
	}
	var storedHash []byte
	err = tx.QueryRow(ctx, `
		SELECT credential_hash FROM secondbox.operator_credentials
		WHERE id='bootstrap_operator_credential' FOR UPDATE`).Scan(&storedHash)
	if err == nil {
		if subtle.ConstantTimeCompare(storedHash, credentialHash) != 1 {
			return errors.New("SecondBox configured bootstrap credential does not match durable operator authority")
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("SecondBox bootstrap authority lookup failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.operators (id,name,state,revision,created_at,updated_at)
		VALUES ('bootstrap_operator','Bootstrap operator','active',1,$1,$1)`, now.UTC()); err != nil {
		return fmt.Errorf("SecondBox bootstrap operator insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.operator_credentials (
			id,operator_id,credential_hash,state,last_used_at,revision,created_at,updated_at
		) VALUES ('bootstrap_operator_credential','bootstrap_operator',$1,'active',NULL,1,$2,$2)`,
		credentialHash, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox bootstrap credential insert failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox bootstrap commit failed: %w", err)
	}
	return nil
}

// AuthenticateBootstrapAdmin validates the durable operator credential hash.
func (store *PostgresControlPlaneStore) AuthenticateBootstrapAdmin(
	ctx context.Context,
	presentedHash []byte,
	now time.Time,
	audit contracts.AuditEvent,
) (contracts.Principal, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox operator authentication transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var operatorID, operatorState, credentialState string
	var storedHash []byte
	if err := tx.QueryRow(ctx, `
		SELECT operator.id,operator.state,credential.credential_hash,credential.state
		FROM secondbox.operator_credentials AS credential
		JOIN secondbox.operators AS operator ON operator.id=credential.operator_id
		WHERE credential.id='bootstrap_operator_credential'
		FOR UPDATE OF credential`).Scan(
		&operatorID, &operatorState, &storedHash, &credentialState,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.Principal{}, ports.ErrAuthenticationFailed
		}
		return contracts.Principal{}, fmt.Errorf("SecondBox operator authentication lookup failed: %w", err)
	}
	if operatorState != "active" || credentialState != "active" ||
		subtle.ConstantTimeCompare(storedHash, presentedHash) != 1 {
		return contracts.Principal{}, ports.ErrAuthenticationFailed
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.operator_credentials
		SET last_used_at=$1,updated_at=$1 WHERE id='bootstrap_operator_credential'`, now.UTC()); err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox operator last-use update failed: %w", err)
	}
	audit.ActorID = operatorID
	audit.ResourceID = operatorID
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Principal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox operator authentication commit failed: %w", err)
	}
	return contracts.Principal{
		Kind: "operator", ID: operatorID, BootstrapAdmin: true,
		Scopes: []string{
			contracts.ScopeAdminProjects, contracts.ScopeAdminKeys, contracts.ScopeAdminProfiles,
			contracts.ScopeAdminAudit, contracts.ScopeDiagnostics,
		},
	}, nil
}

func (store *PostgresControlPlaneStore) CreateProject(
	ctx context.Context,
	project contracts.Project,
	quota contracts.QuotaLimits,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
) (contracts.Project, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Project
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.projects (id,name,state,revision,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		project.ID, project.Name, project.State, project.Revision, project.CreatedAt, project.UpdatedAt,
	); err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.project_quotas (
			project_id,max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
			max_retained_bytes,max_snapshots,max_artifacts,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		project.ID, quota.MaxSandboxes, quota.MaxActiveInstances, quota.MaxCPUMillis,
		quota.MaxMemoryBytes, quota.MaxRetainedBytes, quota.MaxSnapshots, quota.MaxArtifacts,
		quota.MaxPortSessions, quota.MaxConcurrentOperations, project.UpdatedAt,
	); err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project quota insert failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, err
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, project)
	if err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project create commit failed: %w", err)
	}
	return project, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) UpdateProject(
	ctx context.Context,
	projectID string,
	update contracts.UpdateProjectRequest,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
) (contracts.Project, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project update transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Project
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	project, err := scanProject(tx.QueryRow(ctx, `
		SELECT id,name,state,revision,created_at,updated_at
		FROM secondbox.projects WHERE id=$1 FOR UPDATE`, projectID))
	if err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrProjectNotFound)
	}
	if expectedRevision > 0 && expectedRevision != project.Revision {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if update.Name != nil {
		project.Name = *update.Name
	}
	if update.State != nil {
		project.State = *update.State
	}
	project.Revision++
	project.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.projects SET name=$2,state=$3,revision=$4,updated_at=$5 WHERE id=$1`,
		project.ID, project.Name, project.State, project.Revision, project.UpdatedAt,
	); err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project update failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, err
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, project)
	if err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Project{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Project update commit failed: %w", err)
	}
	return project, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) GetProject(ctx context.Context, projectID string) (contracts.Project, error) {
	project, err := scanProject(store.pool.QueryRow(ctx, `
		SELECT id,name,state,revision,created_at,updated_at FROM secondbox.projects WHERE id=$1`, projectID))
	return project, mapNotFound(err, ports.ErrProjectNotFound)
}

func (store *PostgresControlPlaneStore) ListProjects(
	ctx context.Context,
	limit int,
	cursor string,
) (contracts.ProjectPage, error) {
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		projectListCursorResource,
		"",
		cursor,
		`SELECT created_at FROM secondbox.projects WHERE id=$1`,
	)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT id,name,state,revision,created_at,updated_at
		FROM secondbox.projects
		WHERE NOT $1 OR (created_at,id) > ($2,$3)
		ORDER BY created_at,id
		LIMIT $4`,
		boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.ProjectPage{}, fmt.Errorf("SecondBox Project list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.ProjectPage{Items: make([]contracts.Project, 0)}
	for rows.Next() {
		project, scanErr := scanProject(rows)
		if scanErr != nil {
			return contracts.ProjectPage{}, fmt.Errorf("SecondBox Project list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, project)
	}
	if err := rows.Err(); err != nil {
		return contracts.ProjectPage{}, fmt.Errorf("SecondBox Project list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(
			projectListCursorResource, "", page.Items[limit-1].ID,
		)
		if err != nil {
			return contracts.ProjectPage{}, err
		}
	}
	return page, nil
}

func (store *PostgresControlPlaneStore) CreateServiceAccount(
	ctx context.Context,
	account contracts.ServiceAccount,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
) (contracts.ServiceAccount, ports.AdminIdempotencyResult, error) {
	scopesJSON, grantsJSON, err := encodeAuthorityLists(account.Scopes, account.ProfileGrants)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.ServiceAccount
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	var projectState string
	if err := tx.QueryRow(ctx, `SELECT state FROM secondbox.projects WHERE id=$1 FOR UPDATE`, account.ProjectID).Scan(&projectState); err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrProjectNotFound)
	}
	if projectState != contracts.ProjectStateActive {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, ports.ErrAuthorizationDenied
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.service_accounts (
			id,project_id,name,state,scopes_json,profile_grants_json,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		account.ID, account.ProjectID, account.Name, account.State, scopesJSON, grantsJSON,
		account.Revision, account.CreatedAt, account.UpdatedAt,
	); err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount insert failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, account)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount create commit failed: %w", err)
	}
	return account, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) UpdateServiceAccount(
	ctx context.Context,
	projectID string,
	accountID string,
	update contracts.UpdateServiceAccountRequest,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
) (contracts.ServiceAccount, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount update transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.ServiceAccount
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	account, err := scanServiceAccount(tx.QueryRow(ctx, serviceAccountSelect+` WHERE project_id=$1 AND id=$2 FOR UPDATE`, projectID, accountID))
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrServiceAccountNotFound)
	}
	if expectedRevision > 0 && expectedRevision != account.Revision {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if update.Name != nil {
		account.Name = *update.Name
	}
	if update.State != nil {
		account.State = *update.State
	}
	if update.Scopes != nil {
		account.Scopes = append([]string(nil), (*update.Scopes)...)
	}
	if update.ProfileGrants != nil {
		account.ProfileGrants = append([]string(nil), (*update.ProfileGrants)...)
	}
	scopesJSON, grantsJSON, err := encodeAuthorityLists(account.Scopes, account.ProfileGrants)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	account.Revision++
	account.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.service_accounts
		SET name=$3,state=$4,scopes_json=$5,profile_grants_json=$6,revision=$7,updated_at=$8
		WHERE project_id=$1 AND id=$2`,
		projectID, accountID, account.Name, account.State, scopesJSON, grantsJSON,
		account.Revision, account.UpdatedAt,
	); err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount update failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, account)
	if err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ServiceAccount{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ServiceAccount update commit failed: %w", err)
	}
	return account, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) GetServiceAccount(
	ctx context.Context,
	projectID string,
	accountID string,
) (contracts.ServiceAccount, error) {
	account, err := scanServiceAccount(store.pool.QueryRow(ctx, serviceAccountSelect+` WHERE project_id=$1 AND id=$2`, projectID, accountID))
	return account, mapNotFound(err, ports.ErrServiceAccountNotFound)
}

func (store *PostgresControlPlaneStore) ListServiceAccounts(
	ctx context.Context,
	projectID string,
	limit int,
	cursor string,
) (contracts.ServiceAccountPage, error) {
	scope := "project=" + projectID
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		serviceAccountListCursorResource,
		scope,
		cursor,
		`SELECT created_at FROM secondbox.service_accounts WHERE project_id=$1 AND id=$2`,
		projectID,
	)
	if err != nil {
		return contracts.ServiceAccountPage{}, err
	}
	rows, err := store.pool.Query(ctx, serviceAccountSelect+`
		WHERE project_id=$1
		  AND (NOT $2 OR (created_at,id) > ($3,$4))
		ORDER BY created_at,id
		LIMIT $5`,
		projectID, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.ServiceAccountPage{}, fmt.Errorf("SecondBox ServiceAccount list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.ServiceAccountPage{Items: make([]contracts.ServiceAccount, 0)}
	for rows.Next() {
		account, scanErr := scanServiceAccount(rows)
		if scanErr != nil {
			return contracts.ServiceAccountPage{}, fmt.Errorf("SecondBox ServiceAccount list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, account)
	}
	if err := rows.Err(); err != nil {
		return contracts.ServiceAccountPage{}, fmt.Errorf("SecondBox ServiceAccount list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(
			serviceAccountListCursorResource, scope, page.Items[limit-1].ID,
		)
		if err != nil {
			return contracts.ServiceAccountPage{}, err
		}
	}
	return page, nil
}

func (store *PostgresControlPlaneStore) CreateAPIKey(
	ctx context.Context,
	storedKey ports.StoredAPIKey,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
) (contracts.APIKey, ports.AdminIdempotencyResult, error) {
	scopesJSON, err := json.Marshal(storedKey.APIKey.Scopes)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey scopes encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.APIKey
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	account, err := scanServiceAccount(tx.QueryRow(ctx, serviceAccountSelect+` WHERE project_id=$1 AND id=$2 FOR UPDATE`,
		storedKey.ProjectID, storedKey.APIKey.ServiceAccountID))
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrServiceAccountNotFound)
	}
	if account.State != contracts.ServiceAccountStateActive || !isScopeSubset(storedKey.APIKey.Scopes, account.Scopes) {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, ports.ErrAuthorizationDenied
	}
	key := storedKey.APIKey
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.api_keys (
			id,project_id,service_account_id,name,prefix,credential_hash,state,scopes_json,
			expires_at,revoked_at,last_used_at,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		key.ID, storedKey.ProjectID, key.ServiceAccountID, key.Name, key.Prefix,
		storedKey.CredentialHash, key.State, scopesJSON, key.ExpiresAt, key.RevokedAt,
		key.LastUsedAt, key.Revision, key.CreatedAt, storedKey.UpdatedAt,
	); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey insert failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, key)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey create commit failed: %w", err)
	}
	return key, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) RotateAPIKey(
	ctx context.Context,
	projectID string,
	accountID string,
	keyID string,
	prefix string,
	credentialHash []byte,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
) (contracts.APIKey, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey rotation transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.APIKey
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	key, err := scanAPIKey(tx.QueryRow(ctx, apiKeySelect+`
		WHERE project_id=$1 AND service_account_id=$2 AND id=$3 FOR UPDATE`, projectID, accountID, keyID))
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrAPIKeyNotFound)
	}
	if expectedRevision > 0 && expectedRevision != key.Revision {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	key.Prefix = prefix
	key.State = contracts.APIKeyStateActive
	key.RevokedAt = nil
	key.LastUsedAt = nil
	key.Revision++
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.api_keys
		SET prefix=$4,credential_hash=$5,state=$6,revoked_at=NULL,last_used_at=NULL,revision=$7,updated_at=$8
		WHERE project_id=$1 AND service_account_id=$2 AND id=$3`,
		projectID, accountID, keyID, prefix, credentialHash, key.State, key.Revision, now.UTC(),
	); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey rotation failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, key)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey rotation commit failed: %w", err)
	}
	return key, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) RevokeAPIKey(
	ctx context.Context,
	projectID string,
	accountID string,
	keyID string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
) (contracts.APIKey, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey revocation transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.APIKey
	idempotencyResult, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey idempotency replay commit failed: %w", err)
		}
		return replayed, idempotencyResult, nil
	}
	key, err := scanAPIKey(tx.QueryRow(ctx, apiKeySelect+`
		WHERE project_id=$1 AND service_account_id=$2 AND id=$3 FOR UPDATE`, projectID, accountID, keyID))
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrAPIKeyNotFound)
	}
	if expectedRevision > 0 && expectedRevision != key.Revision {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if key.State != contracts.APIKeyStateRevoked {
		revokedAt := now.UTC()
		key.State = contracts.APIKeyStateRevoked
		key.RevokedAt = &revokedAt
		key.Revision++
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.api_keys
			SET state=$4,revoked_at=$5,revision=$6,updated_at=$5
			WHERE project_id=$1 AND service_account_id=$2 AND id=$3`,
			projectID, accountID, keyID, key.State, revokedAt, key.Revision,
		); err != nil {
			return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey revocation failed: %w", err)
		}
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	idempotencyResult, err = insertAdminIdempotency(ctx, tx, idempotency, key)
	if err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.APIKey{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox APIKey revocation commit failed: %w", err)
	}
	return key, idempotencyResult, nil
}

func (store *PostgresControlPlaneStore) GetAPIKey(
	ctx context.Context,
	projectID string,
	accountID string,
	keyID string,
) (contracts.APIKey, error) {
	key, err := scanAPIKey(store.pool.QueryRow(ctx, apiKeySelect+`
		WHERE project_id=$1 AND service_account_id=$2 AND id=$3`, projectID, accountID, keyID))
	return key, mapNotFound(err, ports.ErrAPIKeyNotFound)
}

func (store *PostgresControlPlaneStore) ListAPIKeys(
	ctx context.Context,
	projectID string,
	accountID string,
	limit int,
	cursor string,
) (contracts.APIKeyPage, error) {
	scope := "project=" + projectID + "\x1fservice_account=" + accountID
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		apiKeyListCursorResource,
		scope,
		cursor,
		`SELECT created_at FROM secondbox.api_keys
		 WHERE project_id=$1 AND service_account_id=$2 AND id=$3`,
		projectID, accountID,
	)
	if err != nil {
		return contracts.APIKeyPage{}, err
	}
	rows, err := store.pool.Query(ctx, apiKeySelect+`
		WHERE project_id=$1
		  AND service_account_id=$2
		  AND (NOT $3 OR (created_at,id) > ($4,$5))
		ORDER BY created_at,id
		LIMIT $6`,
		projectID, accountID, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.APIKeyPage{}, fmt.Errorf("SecondBox APIKey list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.APIKeyPage{Items: make([]contracts.APIKey, 0)}
	for rows.Next() {
		key, scanErr := scanAPIKey(rows)
		if scanErr != nil {
			return contracts.APIKeyPage{}, fmt.Errorf("SecondBox APIKey list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, key)
	}
	if err := rows.Err(); err != nil {
		return contracts.APIKeyPage{}, fmt.Errorf("SecondBox APIKey list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(
			apiKeyListCursorResource, scope, page.Items[limit-1].ID,
		)
		if err != nil {
			return contracts.APIKeyPage{}, err
		}
	}
	return page, nil
}

func (store *PostgresControlPlaneStore) AuthenticateAPIKey(
	ctx context.Context,
	prefix string,
	presentedHash []byte,
	now time.Time,
	audit contracts.AuditEvent,
) (contracts.Principal, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox APIKey authentication transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var keyID, projectID, accountID, keyState, projectState, accountState string
	var storedHash, keyScopesJSON, accountScopesJSON []byte
	var expiresAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT key.id,key.project_id,key.service_account_id,key.credential_hash,key.state,key.scopes_json,key.expires_at,
		       project.state,account.state,account.scopes_json
		FROM secondbox.api_keys AS key
		JOIN secondbox.projects AS project ON project.id=key.project_id
		JOIN secondbox.service_accounts AS account ON account.id=key.service_account_id
		WHERE key.prefix=$1 FOR UPDATE OF key`, prefix).Scan(
		&keyID, &projectID, &accountID, &storedHash, &keyState, &keyScopesJSON, &expiresAt,
		&projectState, &accountState, &accountScopesJSON,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.Principal{}, ports.ErrAuthenticationFailed
		}
		return contracts.Principal{}, fmt.Errorf("SecondBox APIKey authentication lookup failed: %w", err)
	}
	if subtle.ConstantTimeCompare(storedHash, presentedHash) != 1 ||
		keyState != contracts.APIKeyStateActive ||
		projectState != contracts.ProjectStateActive ||
		accountState != contracts.ServiceAccountStateActive ||
		(expiresAt != nil && !expiresAt.After(now)) {
		return contracts.Principal{}, ports.ErrAuthenticationFailed
	}
	var keyScopes, accountScopes []string
	if err := json.Unmarshal(keyScopesJSON, &keyScopes); err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox APIKey scopes decoding failed: %w", err)
	}
	if err := json.Unmarshal(accountScopesJSON, &accountScopes); err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox ServiceAccount scopes decoding failed: %w", err)
	}
	principal := contracts.Principal{
		Kind: "service_account", ID: accountID, ProjectID: projectID,
		ServiceAccountID: accountID, Scopes: intersectScopes(keyScopes, accountScopes),
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.api_keys SET last_used_at=$2,updated_at=$2 WHERE id=$1`, keyID, now.UTC()); err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox APIKey last-use update failed: %w", err)
	}
	audit.ProjectID = projectID
	audit.ActorID = accountID
	audit.ResourceID = keyID
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Principal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox APIKey authentication commit failed: %w", err)
	}
	return principal, nil
}

func (store *PostgresControlPlaneStore) CreateProfile(
	ctx context.Context,
	profile contracts.Profile,
	quota contracts.QuotaLimits,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.profile_quotas (
			profile_name,max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
			max_retained_bytes,max_snapshots,max_artifacts,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		profile.Name, quota.MaxSandboxes, quota.MaxActiveInstances, quota.MaxCPUMillis,
		quota.MaxMemoryBytes, quota.MaxRetainedBytes, quota.MaxSnapshots, quota.MaxArtifacts,
		quota.MaxPortSessions, quota.MaxConcurrentOperations, profile.UpdatedAt,
	); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Profile quota insert failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
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

func (store *PostgresControlPlaneStore) ReviseProfile(
	ctx context.Context,
	name string,
	revision contracts.ProfileRevision,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
	audit contracts.AuditEvent,
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
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
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
	audit contracts.AuditEvent,
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
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.Profile{}, ports.AdminIdempotencyResult{}, err
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
	lockKey := input.Principal.ProjectID + "\x1fcreate-sandbox\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox idempotency lock failed: %w", err)
	}
	var priorHash, priorSandboxID string
	idempotencyErr := tx.QueryRow(ctx, `
		SELECT request_hash,response_resource_id
		FROM secondbox.idempotency_records
		WHERE project_id=$1 AND operation='sandbox.create' AND target_id='' AND idempotency_key=$2`,
		input.Principal.ProjectID, input.IdempotencyKey,
	).Scan(&priorHash, &priorSandboxID)
	if idempotencyErr == nil {
		if priorHash != input.RequestHash {
			return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrIdempotencyConflict
		}
		sandbox, err := getSandboxWithQuerier(ctx, tx, input.Principal.ProjectID, priorSandboxID)
		if err != nil {
			return contracts.Sandbox{}, contracts.Operation{}, false, err
		}
		operation, err := getCreateOperationWithQuerier(ctx, tx, input.Principal.ProjectID, priorSandboxID)
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

	var projectState string
	if err := tx.QueryRow(ctx, `SELECT state FROM secondbox.projects WHERE id=$1 FOR UPDATE`, input.Principal.ProjectID).Scan(&projectState); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, mapNotFound(err, ports.ErrProjectNotFound)
	}
	if projectState != contracts.ProjectStateActive {
		return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrAuthorizationDenied
	}
	account, err := scanServiceAccount(tx.QueryRow(ctx, serviceAccountSelect+`
		WHERE project_id=$1 AND id=$2`, input.Principal.ProjectID, input.Principal.ServiceAccountID))
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrAuthorizationDenied
	}
	if account.State != contracts.ServiceAccountStateActive || !contains(account.ProfileGrants, input.Sandbox.Profile) {
		return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrProfileNotGranted
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
	projectQuota, err := readQuota(ctx, tx, "project_quotas", "project_id", input.Principal.ProjectID)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	profileQuota, err := readQuota(ctx, tx, "profile_quotas", "profile_name", profile.Name)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	projectUsage, err := readQuotaUsage(ctx, tx, "sandbox.project_id=$1", input.Principal.ProjectID)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	profileUsage, err := readQuotaUsage(ctx, tx, "sandbox.profile_name=$1", profile.Name)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	requestedCPU := profile.CurrentRevision.Spec.Resources.CPUMillis
	requestedMemory := profile.CurrentRevision.Spec.Resources.MemoryBytes
	if quotaWouldExceed(projectQuota, projectUsage, requestedCPU, requestedMemory) ||
		quotaWouldExceed(profileQuota, profileUsage, requestedCPU, requestedMemory) {
		return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrQuotaExceeded
	}

	sandbox := input.Sandbox
	sandbox.ProjectID = input.Principal.ProjectID
	sandbox.ProfileRevisionID = profile.CurrentRevision.ID
	sandbox.Workspace = input.Workspace
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
			id,project_id,sandbox_id,generation,retained_bytes,current_checkpoint_id,
			current_checkpoint_sha256,current_checkpoint_size_bytes,retention_state,
			garbage_collection_state,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		sandbox.Workspace.ID, sandbox.ProjectID, sandbox.ID, sandbox.Workspace.Generation,
		sandbox.Workspace.RetainedBytes, "", "", 0, "retained", "reachable",
		sandbox.Workspace.CreatedAt, sandbox.Workspace.UpdatedAt,
	); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Workspace insert failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.sandboxes (
			id,project_id,profile_name,profile_revision_id,state,desired_state,generation,workspace_id,
			current_instance_id,metadata_json,compatibility_summary_json,last_activity_at,revision,
			lifecycle_termination_reason,lifecycle_failure_class,lifecycle_failure_message,lifecycle_intent_kind,
			reconcile_owner,reconcile_claim_expires_at,next_reconcile_at,reconcile_retry_count,
			reconcile_retry_limit,created_at,updated_at,deleted_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
		sandbox.ID, sandbox.ProjectID, sandbox.Profile, sandbox.ProfileRevisionID, sandbox.State,
		sandbox.DesiredState, sandbox.Generation, sandbox.Workspace.ID, "", metadataJSON,
		compatibilityJSON, sandbox.LastActivityAt, sandbox.Revision, "", "", "", initialLifecycleIntent,
		"", nil, sandbox.UpdatedAt, 0, 8, sandbox.CreatedAt, sandbox.UpdatedAt, sandbox.DeletedAt,
	); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox insert failed: %w", err)
	}
	input.Operation.SandboxID = sandbox.ID
	if err := insertOperation(ctx, tx, sandbox.ProjectID, input.Operation); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.idempotency_records (
			project_id,operation,target_id,idempotency_key,request_hash,response_resource_id,created_at,expires_at
		) VALUES ($1,'sandbox.create','',$2,$3,$4,$5,$6)`,
		sandbox.ProjectID, input.IdempotencyKey, input.RequestHash, sandbox.ID,
		sandbox.CreatedAt, input.IdempotencyEnds,
	); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox idempotency insert failed: %w", err)
	}
	input.Audit.ProjectID = sandbox.ProjectID
	input.Audit.ResourceID = sandbox.ID
	if err := insertAuditEvent(ctx, tx, input.Audit); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox create commit failed: %w", err)
	}
	return sandbox, input.Operation, true, nil
}

func (store *PostgresControlPlaneStore) GetSandbox(
	ctx context.Context,
	projectID string,
	sandboxID string,
) (contracts.Sandbox, error) {
	return getSandboxWithQuerier(ctx, store.pool, projectID, sandboxID)
}

func (store *PostgresControlPlaneStore) ListSandboxes(
	ctx context.Context,
	projectID string,
	limit int,
	cursor string,
) (contracts.SandboxPage, error) {
	scope := "project=" + projectID
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		sandboxListCursorResource,
		scope,
		cursor,
		`SELECT created_at FROM secondbox.sandboxes WHERE project_id=$1 AND id=$2`,
		projectID,
	)
	if err != nil {
		return contracts.SandboxPage{}, err
	}
	rows, err := store.pool.Query(ctx, sandboxSelect+`
		WHERE sandbox.project_id=$1
		  AND (NOT $2 OR (sandbox.created_at,sandbox.id) > ($3,$4))
		ORDER BY sandbox.created_at,sandbox.id
		LIMIT $5`,
		projectID, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
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
	projectID string,
	operationID string,
) (contracts.Operation, error) {
	return getOperationWithQuerier(ctx, store.pool, projectID, `id=$2`, operationID)
}

func getCreateOperationWithQuerier(
	ctx context.Context,
	querier queryRower,
	projectID string,
	sandboxID string,
) (contracts.Operation, error) {
	return getOperationWithQuerier(ctx, querier, projectID, `sandbox_id=$2 AND kind='create'`, sandboxID)
}

func getOperationWithQuerier(
	ctx context.Context,
	querier queryRower,
	projectID string,
	predicate string,
	identifier string,
) (contracts.Operation, error) {
	queries := map[string]string{
		"id=$2":                           `WHERE project_id=$1 AND id=$2`,
		"sandbox_id=$2 AND kind='create'": `WHERE project_id=$1 AND sandbox_id=$2 AND kind='create'`,
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
		SELECT id,sandbox_id,kind,state,request_id,request_metadata_json,error_code,error_message,retryable,
		       created_at,started_at,completed_at,updated_at
		FROM secondbox.operations `+where+` ORDER BY created_at LIMIT 1`, projectID, identifier).Scan(
		&operation.ID, &operation.SandboxID, &operation.Kind, &operation.State, &operation.RequestID,
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
	projectID string,
	limit int,
) ([]contracts.AuditEvent, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id,project_id,actor_kind,actor_id,action,resource_kind,resource_id,
		       outcome,request_id,details_json,created_at
		FROM secondbox.audit_events
		WHERE ($1='' OR project_id=$1)
		ORDER BY created_at DESC,id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("SecondBox audit list failed: %w", err)
	}
	defer rows.Close()
	events := make([]contracts.AuditEvent, 0)
	for rows.Next() {
		var event contracts.AuditEvent
		var detailsJSON []byte
		if err := rows.Scan(
			&event.ID, &event.ProjectID, &event.ActorKind, &event.ActorID, &event.Action,
			&event.ResourceKind, &event.ResourceID, &event.Outcome, &event.RequestID,
			&detailsJSON, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("SecondBox audit list scan failed: %w", err)
		}
		if err := json.Unmarshal(detailsJSON, &event.Details); err != nil {
			return nil, fmt.Errorf("SecondBox audit details decoding failed: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
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
		APIKeyStates: map[string]int64{
			contracts.APIKeyStateActive: 0, contracts.APIKeyStateRevoked: 0,
			contracts.APIKeyStateExpired: 0,
		},
	}
	for query, destination := range map[string]map[string]int64{
		`SELECT state,count(*) FROM secondbox.sandboxes GROUP BY state`:  snapshot.SandboxStates,
		`SELECT state,count(*) FROM secondbox.operations GROUP BY state`: snapshot.OperationStates,
		`SELECT state,count(*) FROM secondbox.api_keys GROUP BY state`:   snapshot.APIKeyStates,
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

func scanProject(row rowScanner) (contracts.Project, error) {
	var project contracts.Project
	err := row.Scan(&project.ID, &project.Name, &project.State, &project.Revision, &project.CreatedAt, &project.UpdatedAt)
	return project, err
}

const serviceAccountSelect = `
	SELECT id,project_id,name,state,scopes_json,profile_grants_json,revision,created_at,updated_at
	FROM secondbox.service_accounts`

func scanServiceAccount(row rowScanner) (contracts.ServiceAccount, error) {
	var account contracts.ServiceAccount
	var scopesJSON, grantsJSON []byte
	if err := row.Scan(
		&account.ID, &account.ProjectID, &account.Name, &account.State, &scopesJSON,
		&grantsJSON, &account.Revision, &account.CreatedAt, &account.UpdatedAt,
	); err != nil {
		return contracts.ServiceAccount{}, err
	}
	if err := json.Unmarshal(scopesJSON, &account.Scopes); err != nil {
		return contracts.ServiceAccount{}, fmt.Errorf("SecondBox ServiceAccount scopes decoding failed: %w", err)
	}
	if err := json.Unmarshal(grantsJSON, &account.ProfileGrants); err != nil {
		return contracts.ServiceAccount{}, fmt.Errorf("SecondBox ServiceAccount profile grants decoding failed: %w", err)
	}
	return account, nil
}

const apiKeySelect = `
	SELECT id,service_account_id,name,prefix,state,scopes_json,expires_at,revoked_at,last_used_at,revision,created_at
	FROM secondbox.api_keys`

func scanAPIKey(row rowScanner) (contracts.APIKey, error) {
	var key contracts.APIKey
	var scopesJSON []byte
	if err := row.Scan(
		&key.ID, &key.ServiceAccountID, &key.Name, &key.Prefix, &key.State, &scopesJSON,
		&key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt, &key.Revision, &key.CreatedAt,
	); err != nil {
		return contracts.APIKey{}, err
	}
	if err := json.Unmarshal(scopesJSON, &key.Scopes); err != nil {
		return contracts.APIKey{}, fmt.Errorf("SecondBox APIKey scopes decoding failed: %w", err)
	}
	return key, nil
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
	SELECT sandbox.id,sandbox.project_id,sandbox.profile_name,sandbox.profile_revision_id,
	       sandbox.state,sandbox.desired_state,sandbox.generation,sandbox.metadata_json,
	       sandbox.last_activity_at,sandbox.revision,sandbox.created_at,sandbox.updated_at,sandbox.deleted_at,
	       workspace.id,workspace.generation,workspace.retained_bytes,workspace.current_checkpoint_id,
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
		&sandbox.ID, &sandbox.ProjectID, &sandbox.Profile, &sandbox.ProfileRevisionID,
		&sandbox.State, &sandbox.DesiredState, &sandbox.Generation, &metadataJSON,
		&sandbox.LastActivityAt, &sandbox.Revision, &sandbox.CreatedAt, &sandbox.UpdatedAt,
		&sandbox.DeletedAt, &sandbox.Workspace.ID, &sandbox.Workspace.Generation,
		&sandbox.Workspace.RetainedBytes, &sandbox.Workspace.CurrentCheckpointID,
		&sandbox.Workspace.CurrentCheckpointHash, &sandbox.Workspace.CurrentCheckpointSize,
		&sandbox.Workspace.RetentionState, &sandbox.Workspace.CreatedAt, &sandbox.Workspace.UpdatedAt,
		&instanceID, &instanceState, &guestLiveness, &terminationReason,
		&instanceCreatedAt, &instanceUpdatedAt, &readyAt, &guestHeartbeatAt, &stoppedAt,
	); err != nil {
		return contracts.Sandbox{}, err
	}
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
	projectID string,
	sandboxID string,
) (contracts.Sandbox, error) {
	sandbox, err := scanSandbox(querier.QueryRow(ctx, sandboxSelect+`
		WHERE sandbox.project_id=$1 AND sandbox.id=$2`, projectID, sandboxID))
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
	if event.Details == nil {
		event.Details = map[string]string{}
	}
	detailsJSON, err := json.Marshal(event.Details)
	if err != nil {
		return fmt.Errorf("SecondBox audit details encoding failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.audit_events (
			id,project_id,actor_kind,actor_id,action,resource_kind,resource_id,
			outcome,request_id,details_json,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		event.ID, event.ProjectID, event.ActorKind, event.ActorID, event.Action,
		event.ResourceKind, event.ResourceID, event.Outcome, event.RequestID,
		detailsJSON, event.CreatedAt,
	); err != nil {
		return fmt.Errorf("SecondBox audit insert failed: %w", err)
	}
	return nil
}

func insertOperation(ctx context.Context, tx pgx.Tx, projectID string, operation contracts.Operation) error {
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
			id,project_id,sandbox_id,kind,state,request_id,request_metadata_json,error_code,error_message,retryable,
			created_at,started_at,completed_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		operation.ID, projectID, operation.SandboxID, operation.Kind, operation.State, operation.RequestID,
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

func readQuota(
	ctx context.Context,
	tx pgx.Tx,
	table string,
	column string,
	identifier string,
) (contracts.QuotaLimits, error) {
	allowed := map[string]string{
		"project_quotas.project_id":   "SELECT max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,max_retained_bytes,max_snapshots,max_artifacts,max_port_sessions,max_concurrent_operations FROM secondbox.project_quotas WHERE project_id=$1",
		"profile_quotas.profile_name": "SELECT max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,max_retained_bytes,max_snapshots,max_artifacts,max_port_sessions,max_concurrent_operations FROM secondbox.profile_quotas WHERE profile_name=$1",
	}
	query, exists := allowed[table+"."+column]
	if !exists {
		return contracts.QuotaLimits{}, errors.New("SecondBox quota lookup target is invalid")
	}
	var quota contracts.QuotaLimits
	if err := tx.QueryRow(ctx, query, identifier).Scan(
		&quota.MaxSandboxes, &quota.MaxActiveInstances, &quota.MaxCPUMillis,
		&quota.MaxMemoryBytes, &quota.MaxRetainedBytes, &quota.MaxSnapshots,
		&quota.MaxArtifacts, &quota.MaxPortSessions, &quota.MaxConcurrentOperations,
	); err != nil {
		return contracts.QuotaLimits{}, fmt.Errorf("SecondBox quota lookup failed: %w", err)
	}
	return quota, nil
}

func readQuotaUsage(
	ctx context.Context,
	tx pgx.Tx,
	predicate string,
	identifier string,
) (quotaUsage, error) {
	allowed := map[string]string{
		"sandbox.project_id=$1": `
			SELECT count(*),
			       count(*) FILTER (WHERE sandbox.state IN ('starting','ready','draining','stopping')),
			       COALESCE(sum((revision.spec_json->'resources'->>'cpuMillis')::bigint),0),
			       COALESCE(sum((revision.spec_json->'resources'->>'memoryBytes')::bigint),0),
			       COALESCE(sum(workspace.retained_bytes),0)
			         +(SELECT COALESCE(sum(size_bytes),0) FROM secondbox.artifacts
			           WHERE project_id=$1 AND state<>'deleted')
			         +(SELECT COALESCE(sum(size_bytes),0) FROM secondbox.workspace_checkpoints
			           WHERE project_id=$1 AND state IN ('staging','verified')),
			       (SELECT count(*) FROM secondbox.snapshots
			        WHERE project_id=$1 AND state='published'),
			       (SELECT count(*) FROM secondbox.artifacts
			        WHERE project_id=$1 AND state<>'deleted'),
			       (SELECT count(*) FROM secondbox.port_sessions WHERE project_id=$1 AND state='open'),
			       (SELECT count(*) FROM secondbox.data_plane_sessions WHERE project_id=$1 AND state IN ('pending','running','cancelling'))
			FROM secondbox.sandboxes AS sandbox
			JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
			JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
			WHERE sandbox.project_id=$1 AND sandbox.state<>'deleted'`,
		"sandbox.profile_name=$1": `
			SELECT count(*),
			       count(*) FILTER (WHERE sandbox.state IN ('starting','ready','draining','stopping')),
			       COALESCE(sum((revision.spec_json->'resources'->>'cpuMillis')::bigint),0),
			       COALESCE(sum((revision.spec_json->'resources'->>'memoryBytes')::bigint),0),
			       COALESCE(sum(workspace.retained_bytes),0)
			         +(SELECT COALESCE(sum(artifact.size_bytes),0)
			           FROM secondbox.artifacts AS artifact
			           JOIN secondbox.sandboxes AS owned ON owned.id=artifact.sandbox_id
			           WHERE owned.profile_name=$1 AND artifact.state<>'deleted')
			         +(SELECT COALESCE(sum(checkpoint.size_bytes),0)
			           FROM secondbox.workspace_checkpoints AS checkpoint
			           JOIN secondbox.sandboxes AS owned ON owned.id=checkpoint.sandbox_id
			           WHERE owned.profile_name=$1 AND checkpoint.state IN ('staging','verified')),
			       (SELECT count(*) FROM secondbox.snapshots AS snapshot
			        JOIN secondbox.sandboxes AS owned ON owned.id=snapshot.sandbox_id
			        WHERE owned.profile_name=$1 AND snapshot.state='published'),
			       (SELECT count(*) FROM secondbox.artifacts AS artifact
			        JOIN secondbox.sandboxes AS owned ON owned.id=artifact.sandbox_id
			        WHERE owned.profile_name=$1 AND artifact.state<>'deleted'),
			       (SELECT count(*) FROM secondbox.port_sessions AS session
			        JOIN secondbox.sandboxes AS owned ON owned.id=session.sandbox_id
			        WHERE owned.profile_name=$1 AND session.state='open'),
			       (SELECT count(*) FROM secondbox.data_plane_sessions AS session
			        JOIN secondbox.sandboxes AS owned ON owned.id=session.sandbox_id
			        WHERE owned.profile_name=$1 AND session.state IN ('pending','running','cancelling'))
			FROM secondbox.sandboxes AS sandbox
			JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
			JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
			WHERE sandbox.profile_name=$1 AND sandbox.state<>'deleted'`,
	}
	query, exists := allowed[predicate]
	if !exists {
		return quotaUsage{}, errors.New("SecondBox quota usage predicate is invalid")
	}
	var usage quotaUsage
	if err := tx.QueryRow(ctx, query, identifier).Scan(
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
) bool {
	return usage.sandboxes+1 > quota.MaxSandboxes ||
		usage.activeInstances > quota.MaxActiveInstances ||
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
