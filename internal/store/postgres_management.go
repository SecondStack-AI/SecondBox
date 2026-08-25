package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const (
	tenantControllerLookupPrefix = "tca_"
	applicationLookupPrefix      = "apa_"
)

const tenantSelect = `SELECT ref,state,allowed_profile_grants_json,allowed_application_scopes_json,
	aggregate_quota_json,expiry_policy_json,metadata_json,expires_at,revision,created_at,updated_at
	FROM secondbox.tenants`

const subjectSelect = `SELECT tenant_ref,ref,state,cleanup_state,quota_json,metadata_json,
	expires_at,revision,created_at,updated_at FROM secondbox.subjects`

// CreateTenant persists one explicit tenant management boundary.
func (store *PostgresControlPlaneStore) CreateTenant(
	ctx context.Context,
	tenant contracts.Tenant,
) (contracts.Tenant, error) {
	profileGrantsJSON, err := encodeManagementJSON("Tenant Profile grants", tenant.AllowedProfileGrants)
	if err != nil {
		return contracts.Tenant{}, err
	}
	scopesJSON, err := encodeManagementJSON("Tenant application scopes", tenant.AllowedApplicationScopes)
	if err != nil {
		return contracts.Tenant{}, err
	}
	quotaJSON, err := encodeManagementJSON("Tenant aggregate quota", tenant.AggregateQuota)
	if err != nil {
		return contracts.Tenant{}, err
	}
	expiryPolicyJSON, err := encodeManagementJSON("Tenant expiry policy", tenant.ExpiryPolicy)
	if err != nil {
		return contracts.Tenant{}, err
	}
	metadataJSON, err := encodeManagementJSON("Tenant metadata", tenant.Metadata)
	if err != nil {
		return contracts.Tenant{}, err
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO secondbox.tenants (
			ref,state,allowed_profile_grants_json,allowed_application_scopes_json,
			aggregate_quota_json,expiry_policy_json,metadata_json,expires_at,
			revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		tenant.Ref, tenant.State, profileGrantsJSON, scopesJSON, quotaJSON,
		expiryPolicyJSON, metadataJSON, tenant.ExpiresAt, tenant.Revision,
		tenant.CreatedAt.UTC(), tenant.UpdatedAt.UTC(),
	); err != nil {
		return contracts.Tenant{}, fmt.Errorf("SecondBox Tenant insert failed: %w", err)
	}
	return tenant, nil
}

// CreateManagedTenant creates or replays one exact operator-owned Tenant response.
func (store *PostgresControlPlaneStore) CreateManagedTenant(
	ctx context.Context,
	tenant contracts.Tenant,
	idempotency ports.AdminIdempotencyInput,
) (contracts.Tenant, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Tenant
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant idempotency replay commit failed: %w", err)
		}
		return replayed, result, nil
	}
	if err := insertTenant(ctx, tx, tenant); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, tenant)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant create commit failed: %w", err)
	}
	return tenant, result, nil
}

// GetTenant returns one operator-visible Tenant.
func (store *PostgresControlPlaneStore) GetTenant(ctx context.Context, tenantRef string) (contracts.Tenant, error) {
	tenant, err := scanTenant(store.pool.QueryRow(ctx, tenantSelect+` WHERE ref=$1`, tenantRef))
	return tenant, mapNotFound(err, ports.ErrManagementNotFound)
}

// ListTenants returns one stable operator-visible Tenant page.
func (store *PostgresControlPlaneStore) ListTenants(ctx context.Context, limit int, cursor string) (contracts.TenantPage, error) {
	boundary, err := store.resolvePostgresListCursor(ctx, tenantListCursorResource, "", cursor,
		`SELECT created_at FROM secondbox.tenants WHERE ref=$1`)
	if err != nil {
		return contracts.TenantPage{}, err
	}
	rows, err := store.pool.Query(ctx, tenantSelect+`
		WHERE NOT $1 OR (created_at,ref) > ($2,$3)
		ORDER BY created_at,ref LIMIT $4`, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.TenantPage{}, fmt.Errorf("SecondBox Tenant list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.TenantPage{Items: make([]contracts.Tenant, 0)}
	for rows.Next() {
		tenant, scanErr := scanTenant(rows)
		if scanErr != nil {
			return contracts.TenantPage{}, fmt.Errorf("SecondBox Tenant list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, tenant)
	}
	if err := rows.Err(); err != nil {
		return contracts.TenantPage{}, fmt.Errorf("SecondBox Tenant list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(tenantListCursorResource, "", page.Items[limit-1].Ref)
	}
	return page, err
}

// SetTenantState applies one revision-fenced Tenant lifecycle transition.
func (store *PostgresControlPlaneStore) SetTenantState(
	ctx context.Context, tenantRef, targetState string, expectedRevision int64, now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.Tenant, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant lifecycle transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Tenant
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant lifecycle replay commit failed: %w", err)
		}
		return replayed, result, nil
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1 FOR UPDATE`, tenantRef))
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if tenant.Revision != expectedRevision {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if tenant.State == contracts.TenantStateExpired {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, ports.ErrResourceExpired
	}
	valid := tenant.State == contracts.TenantStateActive && targetState == contracts.TenantStateSuspended ||
		tenant.State == contracts.TenantStateSuspended && targetState == contracts.TenantStateActive
	if !valid {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, ports.ErrInvalidLifecycleTransition
	}
	tenant.State = targetState
	tenant.Revision++
	tenant.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `UPDATE secondbox.tenants SET state=$2,revision=$3,updated_at=$4 WHERE ref=$1`,
		tenant.Ref, tenant.State, tenant.Revision, tenant.UpdatedAt); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant lifecycle update failed: %w", err)
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, tenant)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant lifecycle commit failed: %w", err)
	}
	return tenant, result, nil
}

// CreateSubject persists one tenant-scoped subject identity.
func (store *PostgresControlPlaneStore) CreateSubject(
	ctx context.Context,
	subject contracts.Subject,
) (contracts.Subject, error) {
	quotaJSON, err := encodeManagementJSON("Subject quota", subject.Quota)
	if err != nil {
		return contracts.Subject{}, err
	}
	metadataJSON, err := encodeManagementJSON("Subject metadata", subject.Metadata)
	if err != nil {
		return contracts.Subject{}, err
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO secondbox.subjects (
			tenant_ref,ref,state,cleanup_state,quota_json,metadata_json,
			expires_at,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		subject.TenantRef, subject.Ref, subject.State, subject.CleanupState,
		quotaJSON, metadataJSON, subject.ExpiresAt, subject.Revision,
		subject.CreatedAt.UTC(), subject.UpdatedAt.UTC(),
	); err != nil {
		return contracts.Subject{}, fmt.Errorf("SecondBox Subject insert failed: %w", err)
	}
	return subject, nil
}

// CreateManagedSubject creates or replays one tenant-scoped Subject after ceiling checks.
func (store *PostgresControlPlaneStore) CreateManagedSubject(
	ctx context.Context, subject contracts.Subject, idempotency ports.AdminIdempotencyInput,
) (contracts.Subject, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Subject create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Subject
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Subject{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Subject idempotency replay commit failed: %w", err)
		}
		return replayed, result, nil
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1 FOR SHARE`, subject.TenantRef))
	if err != nil {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if err := validateManagedTenantAdmission(tenant, subject.CreatedAt); err != nil {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, err
	}
	if !subjectQuotaWithinTenant(subject.Quota, tenant.AggregateQuota) {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
	}
	if subject.ExpiresAt != nil && (subject.ExpiresAt.After(subject.CreatedAt.Add(time.Duration(tenant.ExpiryPolicy.MaximumSubjectLifetimeSeconds)*time.Second)) ||
		tenant.ExpiresAt != nil && subject.ExpiresAt.After(*tenant.ExpiresAt)) {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
	}
	if err := insertSubject(ctx, tx, subject); err != nil {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, err
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, subject)
	if err != nil {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Subject{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Subject create commit failed: %w", err)
	}
	return subject, result, nil
}

// GetSubject returns one non-enumerating tenant-scoped Subject.
func (store *PostgresControlPlaneStore) GetSubject(ctx context.Context, tenantRef, subjectRef string) (contracts.Subject, error) {
	subject, err := scanSubject(store.pool.QueryRow(ctx, subjectSelect+` WHERE tenant_ref=$1 AND ref=$2`, tenantRef, subjectRef))
	return subject, mapNotFound(err, ports.ErrManagementNotFound)
}

// ListSubjects returns one stable tenant-scoped Subject page.
func (store *PostgresControlPlaneStore) ListSubjects(ctx context.Context, tenantRef string, limit int, cursor string) (contracts.SubjectPage, error) {
	boundary, err := store.resolvePostgresListCursor(ctx, subjectListCursorResource, tenantRef, cursor,
		`SELECT created_at FROM secondbox.subjects WHERE tenant_ref=$1 AND ref=$2`, tenantRef)
	if err != nil {
		return contracts.SubjectPage{}, err
	}
	rows, err := store.pool.Query(ctx, subjectSelect+`
		WHERE tenant_ref=$1 AND (NOT $2 OR (created_at,ref) > ($3,$4))
		ORDER BY created_at,ref LIMIT $5`, tenantRef, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.SubjectPage{}, fmt.Errorf("SecondBox Subject list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.SubjectPage{Items: make([]contracts.Subject, 0)}
	for rows.Next() {
		subject, scanErr := scanSubject(rows)
		if scanErr != nil {
			return contracts.SubjectPage{}, fmt.Errorf("SecondBox Subject list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, subject)
	}
	if err := rows.Err(); err != nil {
		return contracts.SubjectPage{}, fmt.Errorf("SecondBox Subject list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(subjectListCursorResource, tenantRef, page.Items[limit-1].Ref)
	}
	return page, err
}

// CreateTenantControllerAuthority generates and persists one tenant-controller credential.
func (store *PostgresControlPlaneStore) CreateTenantControllerAuthority(
	ctx context.Context,
	authority contracts.TenantControllerAuthority,
) (contracts.TenantControllerCredentialResponse, error) {
	if authority.LookupID != "" {
		return contracts.TenantControllerCredentialResponse{}, errors.New("SecondBox TenantControllerAuthority lookup identity is server-generated")
	}
	authority.Kind = contracts.AuthorityKindTenantController
	authority.Grant = contracts.TenantControllerGrantManagement
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(
		tenantControllerLookupPrefix,
		ports.TenantControllerBearerTokenPrefix,
	)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, err
	}
	authority.LookupID = lookupID
	metadataJSON, err := encodeManagementJSON("TenantControllerAuthority metadata", authority.Metadata)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := insertAuthorityIdentity(ctx, tx, authority.ID, authority.Kind, authority.LookupID); err != nil {
		return contracts.TenantControllerCredentialResponse{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.tenant_controller_authorities (
			id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,
			revision,token_verifier_sha256,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		authority.ID, authority.LookupID, authority.TenantRef, authority.Grant,
		authority.State, metadataJSON, authority.ExpiresAt, authority.Revision,
		verifier[:], authority.CreatedAt.UTC(), authority.UpdatedAt.UTC(),
	); err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority create commit failed: %w", err)
	}
	return contracts.TenantControllerCredentialResponse{Authority: authority, BearerToken: bearerToken}, nil
}

// CreateManagedTenantControllerAuthority creates one idempotency-protected controller credential.
func (store *PostgresControlPlaneStore) CreateManagedTenantControllerAuthority(
	ctx context.Context, authority contracts.TenantControllerAuthority, idempotency ports.AdminIdempotencyInput,
) (contracts.TenantControllerCredentialResponse, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.TenantControllerAuthority
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		return contracts.TenantControllerCredentialResponse{}, result, ports.ErrCredentialResponseUnavailable
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1 FOR SHARE`, authority.TenantRef))
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if err := validateManagedTenantAdmission(tenant, authority.CreatedAt); err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if !managedAuthorityExpiryAllowed(tenant, authority.CreatedAt, authority.ExpiresAt) {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
	}
	response, err := insertTenantControllerCredential(ctx, tx, authority)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, response.Authority)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority create commit failed: %w", err)
	}
	return response, result, nil
}

// ListTenantControllerAuthorities returns one stable tenant-scoped controller page.
func (store *PostgresControlPlaneStore) ListTenantControllerAuthorities(ctx context.Context, tenantRef string, limit int, cursor string) (contracts.TenantControllerAuthorityPage, error) {
	if _, err := store.GetTenant(ctx, tenantRef); err != nil {
		return contracts.TenantControllerAuthorityPage{}, err
	}
	boundary, err := store.resolvePostgresListCursor(ctx, tenantControllerAuthorityListCursorResource, tenantRef, cursor,
		`SELECT created_at FROM secondbox.tenant_controller_authorities WHERE tenant_ref=$1 AND id=$2`, tenantRef)
	if err != nil {
		return contracts.TenantControllerAuthorityPage{}, err
	}
	rows, err := store.pool.Query(ctx, `SELECT id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,revision,created_at,updated_at
		FROM secondbox.tenant_controller_authorities WHERE tenant_ref=$1 AND (NOT $2 OR (created_at,id)>($3,$4))
		ORDER BY created_at,id LIMIT $5`, tenantRef, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.TenantControllerAuthorityPage{}, fmt.Errorf("SecondBox TenantControllerAuthority list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.TenantControllerAuthorityPage{Items: make([]contracts.TenantControllerAuthority, 0)}
	for rows.Next() {
		authority, scanErr := scanTenantControllerAuthority(rows)
		if scanErr != nil {
			return contracts.TenantControllerAuthorityPage{}, fmt.Errorf("SecondBox TenantControllerAuthority list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, authority)
	}
	if err := rows.Err(); err != nil {
		return contracts.TenantControllerAuthorityPage{}, fmt.Errorf("SecondBox TenantControllerAuthority list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(tenantControllerAuthorityListCursorResource, tenantRef, page.Items[limit-1].ID)
	}
	return page, err
}

// CreateApplicationAuthority generates and persists one application credential.
func (store *PostgresControlPlaneStore) CreateApplicationAuthority(
	ctx context.Context,
	authority contracts.ApplicationAuthority,
) (contracts.ApplicationCredentialResponse, error) {
	if authority.LookupID != "" {
		return contracts.ApplicationCredentialResponse{}, errors.New("SecondBox ApplicationAuthority lookup identity is server-generated")
	}
	authority.Kind = contracts.AuthorityKindApplication
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(
		applicationLookupPrefix,
		ports.ApplicationBearerTokenPrefix,
	)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	authority.LookupID = lookupID
	scopesJSON, err := encodeManagementJSON("ApplicationAuthority scopes", authority.Scopes)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	profileGrantsJSON, err := encodeManagementJSON("ApplicationAuthority Profile grants", authority.ProfileGrants)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	metadataJSON, err := encodeManagementJSON("ApplicationAuthority metadata", authority.Metadata)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := insertAuthorityIdentity(ctx, tx, authority.ID, authority.Kind, authority.LookupID); err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.application_authorities (
			id,lookup_id,tenant_ref,subject_ref,state,scopes_json,profile_grants_json,
			metadata_json,expires_at,revision,token_verifier_sha256,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		authority.ID, authority.LookupID, authority.TenantRef, authority.SubjectRef,
		authority.State, scopesJSON, profileGrantsJSON, metadataJSON,
		authority.ExpiresAt, authority.Revision, verifier[:],
		authority.CreatedAt.UTC(), authority.UpdatedAt.UTC(),
	); err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority insert failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority create commit failed: %w", err)
	}
	return contracts.ApplicationCredentialResponse{Authority: authority, BearerToken: bearerToken}, nil
}

// CreateManagedApplicationAuthority creates one idempotency-protected ceiling-checked application credential.
func (store *PostgresControlPlaneStore) CreateManagedApplicationAuthority(
	ctx context.Context, authority contracts.ApplicationAuthority, idempotency ports.AdminIdempotencyInput,
) (contracts.ApplicationCredentialResponse, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.ApplicationAuthority
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		return contracts.ApplicationCredentialResponse{}, result, ports.ErrCredentialResponseUnavailable
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1 FOR SHARE`, authority.TenantRef))
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if err := validateManagedTenantAdmission(tenant, authority.CreatedAt); err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if !isStringSubset(authority.Scopes, tenant.AllowedApplicationScopes) ||
		!isStringSubset(authority.ProfileGrants, tenant.AllowedProfileGrants) ||
		!managedAuthorityExpiryAllowed(tenant, authority.CreatedAt, authority.ExpiresAt) {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
	}
	subject, err := scanSubject(tx.QueryRow(ctx, subjectSelect+` WHERE tenant_ref=$1 AND ref=$2 FOR SHARE`, authority.TenantRef, authority.SubjectRef))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
		}
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority Subject lookup failed: %w", err)
	}
	if subject.State != contracts.SubjectStateActive || isExpired(subject.ExpiresAt, authority.CreatedAt) {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
	}
	if subject.ExpiresAt != nil && authority.ExpiresAt != nil && authority.ExpiresAt.After(*subject.ExpiresAt) {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
	}
	response, err := insertApplicationCredential(ctx, tx, authority)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, response.Authority)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority create commit failed: %w", err)
	}
	return response, result, nil
}

// ListApplicationAuthorities returns one stable non-secret tenant-scoped page.
func (store *PostgresControlPlaneStore) ListApplicationAuthorities(ctx context.Context, tenantRef, subjectRef string, limit int, cursor string) (contracts.ApplicationAuthorityPage, error) {
	scope := tenantRef + "\x1f" + subjectRef
	lookup := `SELECT created_at FROM secondbox.application_authorities WHERE tenant_ref=$1 AND id=$2`
	lookupArgs := []any{tenantRef}
	if subjectRef != "" {
		lookup = `SELECT created_at FROM secondbox.application_authorities WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`
		lookupArgs = append(lookupArgs, subjectRef)
	}
	boundary, err := store.resolvePostgresListCursor(ctx, applicationAuthorityListCursorResource, scope, cursor, lookup, lookupArgs...)
	if err != nil {
		return contracts.ApplicationAuthorityPage{}, err
	}
	rows, err := store.pool.Query(ctx, `SELECT id,lookup_id,tenant_ref,subject_ref,state,scopes_json,profile_grants_json,metadata_json,expires_at,revision,created_at,updated_at
		FROM secondbox.application_authorities WHERE tenant_ref=$1 AND ($2='' OR subject_ref=$2)
		AND (NOT $3 OR (created_at,id)>($4,$5)) ORDER BY created_at,id LIMIT $6`,
		tenantRef, subjectRef, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.ApplicationAuthorityPage{}, fmt.Errorf("SecondBox ApplicationAuthority list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.ApplicationAuthorityPage{Items: make([]contracts.ApplicationAuthority, 0)}
	for rows.Next() {
		authority, scanErr := scanApplicationAuthority(rows)
		if scanErr != nil {
			return contracts.ApplicationAuthorityPage{}, fmt.Errorf("SecondBox ApplicationAuthority list scan failed: %w", scanErr)
		}
		page.Items = append(page.Items, authority)
	}
	if err := rows.Err(); err != nil {
		return contracts.ApplicationAuthorityPage{}, fmt.Errorf("SecondBox ApplicationAuthority list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(applicationAuthorityListCursorResource, scope, page.Items[limit-1].ID)
	}
	return page, err
}

// AuthenticateTenantControllerAuthority resolves and verifies current durable controller authority.
func (store *PostgresControlPlaneStore) AuthenticateTenantControllerAuthority(
	ctx context.Context,
	bearerToken string,
	now time.Time,
) (contracts.Principal, error) {
	lookupID, ok := parsePersistedAuthorityCredential(
		bearerToken,
		ports.TenantControllerBearerTokenPrefix,
		tenantControllerLookupPrefix,
	)
	if !ok {
		return contracts.Principal{}, ports.ErrAuthenticationFailed
	}
	var id, tenantRef, grant, authorityState, tenantState string
	var verifier []byte
	var authorityExpiresAt, tenantExpiresAt *time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT authority.id,authority.tenant_ref,authority.grant_name,authority.state,
		       authority.expires_at,authority.token_verifier_sha256,
		       tenant.state,tenant.expires_at
		FROM secondbox.tenant_controller_authorities AS authority
		JOIN secondbox.tenants AS tenant ON tenant.ref=authority.tenant_ref
		WHERE authority.lookup_id=$1`, lookupID,
	).Scan(
		&id, &tenantRef, &grant, &authorityState, &authorityExpiresAt, &verifier,
		&tenantState, &tenantExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Principal{}, ports.ErrAuthenticationFailed
	}
	if err != nil {
		return contracts.Principal{}, fmt.Errorf("SecondBox TenantControllerAuthority authentication lookup failed: %w", err)
	}
	if !verifyPersistedAuthorityCredential(bearerToken, verifier) ||
		authorityState != contracts.AuthorityStateActive ||
		grant != contracts.TenantControllerGrantManagement ||
		tenantState != contracts.TenantStateActive ||
		isExpired(authorityExpiresAt, now) || isExpired(tenantExpiresAt, now) {
		return contracts.Principal{}, ports.ErrAuthenticationFailed
	}
	return contracts.Principal{
		Kind: contracts.AuthorityKindTenantController,
		ID:   id, TenantRef: tenantRef,
	}, nil
}

// AuthenticateApplicationAuthority resolves and verifies current durable application authority.
func (store *PostgresControlPlaneStore) AuthenticateApplicationAuthority(
	ctx context.Context,
	bearerToken string,
	now time.Time,
) (ports.AuthenticatedApplicationAuthority, error) {
	lookupID, ok := parsePersistedAuthorityCredential(
		bearerToken,
		ports.ApplicationBearerTokenPrefix,
		applicationLookupPrefix,
	)
	if !ok {
		return ports.AuthenticatedApplicationAuthority{}, ports.ErrAuthenticationFailed
	}
	var authenticated ports.AuthenticatedApplicationAuthority
	var authorityState, subjectState, tenantState string
	var scopesJSON, profileGrantsJSON, tenantScopesJSON, tenantProfileGrantsJSON []byte
	var verifier []byte
	var authorityExpiresAt, subjectExpiresAt, tenantExpiresAt *time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT authority.id,authority.tenant_ref,authority.subject_ref,authority.state,
		       authority.scopes_json,authority.profile_grants_json,authority.expires_at,
		       authority.token_verifier_sha256,
		       subject.state,subject.expires_at,
		       tenant.state,tenant.expires_at,
		       tenant.allowed_application_scopes_json,tenant.allowed_profile_grants_json
		FROM secondbox.application_authorities AS authority
		JOIN secondbox.subjects AS subject
		  ON subject.tenant_ref=authority.tenant_ref AND subject.ref=authority.subject_ref
		JOIN secondbox.tenants AS tenant ON tenant.ref=authority.tenant_ref
		WHERE authority.lookup_id=$1`, lookupID,
	).Scan(
		&authenticated.ID, &authenticated.TenantRef, &authenticated.SubjectRef,
		&authorityState, &scopesJSON, &profileGrantsJSON, &authorityExpiresAt, &verifier,
		&subjectState, &subjectExpiresAt, &tenantState, &tenantExpiresAt,
		&tenantScopesJSON, &tenantProfileGrantsJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.AuthenticatedApplicationAuthority{}, ports.ErrAuthenticationFailed
	}
	if err != nil {
		return ports.AuthenticatedApplicationAuthority{}, fmt.Errorf("SecondBox ApplicationAuthority authentication lookup failed: %w", err)
	}
	if !verifyPersistedAuthorityCredential(bearerToken, verifier) ||
		authorityState != contracts.AuthorityStateActive ||
		subjectState != contracts.SubjectStateActive ||
		tenantState != contracts.TenantStateActive ||
		isExpired(authorityExpiresAt, now) || isExpired(subjectExpiresAt, now) ||
		isExpired(tenantExpiresAt, now) {
		return ports.AuthenticatedApplicationAuthority{}, ports.ErrAuthenticationFailed
	}
	var tenantScopes, tenantProfileGrants []string
	if err := decodeManagementJSON("ApplicationAuthority scopes", scopesJSON, &authenticated.Scopes); err != nil {
		return ports.AuthenticatedApplicationAuthority{}, err
	}
	if err := decodeManagementJSON("ApplicationAuthority Profile grants", profileGrantsJSON, &authenticated.ProfileGrants); err != nil {
		return ports.AuthenticatedApplicationAuthority{}, err
	}
	if err := decodeManagementJSON("Tenant application scopes", tenantScopesJSON, &tenantScopes); err != nil {
		return ports.AuthenticatedApplicationAuthority{}, err
	}
	if err := decodeManagementJSON("Tenant Profile grants", tenantProfileGrantsJSON, &tenantProfileGrants); err != nil {
		return ports.AuthenticatedApplicationAuthority{}, err
	}
	if !isStringSubset(authenticated.Scopes, tenantScopes) ||
		!isStringSubset(authenticated.ProfileGrants, tenantProfileGrants) {
		return ports.AuthenticatedApplicationAuthority{}, ports.ErrAuthenticationFailed
	}
	return authenticated, nil
}

// GetTenantControllerAuthority returns no bearer or verifier material.
func (store *PostgresControlPlaneStore) GetTenantControllerAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
) (contracts.TenantControllerAuthority, error) {
	authority, err := scanTenantControllerAuthority(store.pool.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.tenant_controller_authorities
		WHERE tenant_ref=$1 AND id=$2`, tenantRef, authorityID))
	return authority, mapNotFound(err, ports.ErrManagementNotFound)
}

// GetApplicationAuthority returns no bearer or verifier material.
func (store *PostgresControlPlaneStore) GetApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
) (contracts.ApplicationAuthority, error) {
	authority, err := scanApplicationAuthority(store.pool.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,subject_ref,state,scopes_json,
		       profile_grants_json,metadata_json,expires_at,revision,created_at,updated_at
		FROM secondbox.application_authorities
		WHERE tenant_ref=$1 AND id=$2`, tenantRef, authorityID))
	return authority, mapNotFound(err, ports.ErrManagementNotFound)
}

// RotateTenantControllerAuthority invalidates the previous bearer and returns one replacement.
func (store *PostgresControlPlaneStore) RotateTenantControllerAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
) (contracts.TenantControllerCredentialResponse, error) {
	response, _, err := store.RotateManagedTenantControllerAuthority(
		ctx, tenantRef, authorityID, expectedRevision, now, ports.AdminIdempotencyInput{},
	)
	return response, err
}

// RotateManagedTenantControllerAuthority performs one idempotency-protected controller credential rotation.
func (store *PostgresControlPlaneStore) RotateManagedTenantControllerAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.TenantControllerCredentialResponse, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority rotate transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.TenantControllerAuthority
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		return contracts.TenantControllerCredentialResponse{}, result, ports.ErrCredentialResponseUnavailable
	}
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(
		tenantControllerLookupPrefix,
		ports.TenantControllerBearerTokenPrefix,
	)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	authority, err := scanTenantControllerAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.tenant_controller_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if authority.Revision != expectedRevision {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if authority.State != contracts.AuthorityStateActive || isExpired(authority.ExpiresAt, now) {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrInvalidLifecycleTransition
	}
	authority.LookupID = lookupID
	authority.Revision++
	authority.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.authority_identities SET lookup_id=$2
		WHERE id=$1 AND kind=$3`, authority.ID, lookupID, contracts.AuthorityKindTenantController,
	); err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority identity rotation failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.tenant_controller_authorities
		SET lookup_id=$2,token_verifier_sha256=$3,revision=$4,updated_at=$5
		WHERE id=$1`, authority.ID, lookupID, verifier[:], authority.Revision, authority.UpdatedAt,
	); err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority rotation failed: %w", err)
	}
	response := contracts.TenantControllerCredentialResponse{Authority: authority, BearerToken: bearerToken}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, response.Authority)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.TenantControllerCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority rotate commit failed: %w", err)
	}
	return response, result, nil
}

// RotateApplicationAuthority invalidates the previous bearer and returns one replacement.
func (store *PostgresControlPlaneStore) RotateApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
) (contracts.ApplicationCredentialResponse, error) {
	response, _, err := store.RotateManagedApplicationAuthority(
		ctx, tenantRef, authorityID, expectedRevision, now, ports.AdminIdempotencyInput{},
	)
	return response, err
}

// RotateManagedApplicationAuthority performs one idempotency-protected application credential rotation.
func (store *PostgresControlPlaneStore) RotateManagedApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.ApplicationCredentialResponse, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority rotate transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.ApplicationAuthority
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		return contracts.ApplicationCredentialResponse{}, result, ports.ErrCredentialResponseUnavailable
	}
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(
		applicationLookupPrefix,
		ports.ApplicationBearerTokenPrefix,
	)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	authority, err := scanApplicationAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,subject_ref,state,scopes_json,
		       profile_grants_json,metadata_json,expires_at,revision,created_at,updated_at
		FROM secondbox.application_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if authority.Revision != expectedRevision {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if authority.State != contracts.AuthorityStateActive || isExpired(authority.ExpiresAt, now) {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, ports.ErrInvalidLifecycleTransition
	}
	authority.LookupID = lookupID
	authority.Revision++
	authority.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.authority_identities SET lookup_id=$2
		WHERE id=$1 AND kind=$3`, authority.ID, lookupID, contracts.AuthorityKindApplication,
	); err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority identity rotation failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.application_authorities
		SET lookup_id=$2,token_verifier_sha256=$3,revision=$4,updated_at=$5
		WHERE id=$1`, authority.ID, lookupID, verifier[:], authority.Revision, authority.UpdatedAt,
	); err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority rotation failed: %w", err)
	}
	response := contracts.ApplicationCredentialResponse{Authority: authority, BearerToken: bearerToken}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, response.Authority)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplicationCredentialResponse{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority rotate commit failed: %w", err)
	}
	return response, result, nil
}

// RevokeTenantControllerAuthority immediately denies its bearer credential.
func (store *PostgresControlPlaneStore) RevokeTenantControllerAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
) (contracts.TenantControllerAuthority, error) {
	authority, _, err := store.RevokeManagedTenantControllerAuthority(
		ctx, tenantRef, authorityID, expectedRevision, now, ports.AdminIdempotencyInput{},
	)
	return authority, err
}

// RevokeManagedTenantControllerAuthority revokes or replays one controller mutation.
func (store *PostgresControlPlaneStore) RevokeManagedTenantControllerAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.TenantControllerAuthority, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority revoke transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.TenantControllerAuthority
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority revocation replay commit failed: %w", err)
		}
		return replayed, result, nil
	}
	authority, err := scanTenantControllerAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.tenant_controller_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if authority.Revision != expectedRevision {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if authority.State != contracts.AuthorityStateActive {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, ports.ErrInvalidLifecycleTransition
	}
	authority.State = contracts.AuthorityStateRevoked
	authority.Revision++
	authority.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.tenant_controller_authorities
		SET state=$2,revision=$3,updated_at=$4 WHERE id=$1`,
		authority.ID, authority.State, authority.Revision, authority.UpdatedAt,
	); err != nil {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority revocation failed: %w", err)
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, authority)
	if err != nil {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.TenantControllerAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox TenantControllerAuthority revoke commit failed: %w", err)
	}
	return authority, result, nil
}

// RevokeApplicationAuthority immediately denies its bearer credential.
func (store *PostgresControlPlaneStore) RevokeApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
) (contracts.ApplicationAuthority, error) {
	authority, _, err := store.RevokeManagedApplicationAuthority(
		ctx, tenantRef, authorityID, expectedRevision, now, ports.AdminIdempotencyInput{},
	)
	return authority, err
}

// RevokeManagedApplicationAuthority revokes or replays one application mutation.
func (store *PostgresControlPlaneStore) RevokeManagedApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.ApplicationAuthority, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority revoke transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.ApplicationAuthority
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority revocation replay commit failed: %w", err)
		}
		return replayed, result, nil
	}
	authority, err := scanApplicationAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,subject_ref,state,scopes_json,
		       profile_grants_json,metadata_json,expires_at,revision,created_at,updated_at
		FROM secondbox.application_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if authority.Revision != expectedRevision {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, ports.ErrRevisionConflict
	}
	if authority.State != contracts.AuthorityStateActive {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, ports.ErrInvalidLifecycleTransition
	}
	authority.State = contracts.AuthorityStateRevoked
	authority.Revision++
	authority.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.application_authorities
		SET state=$2,revision=$3,updated_at=$4 WHERE id=$1`,
		authority.ID, authority.State, authority.Revision, authority.UpdatedAt,
	); err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority revocation failed: %w", err)
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, authority)
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority revoke commit failed: %w", err)
	}
	return authority, result, nil
}

func insertAuthorityIdentity(
	ctx context.Context,
	tx pgx.Tx,
	authorityID string,
	kind string,
	lookupID string,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.authority_identities (id,kind,lookup_id)
		VALUES ($1,$2,$3)`, authorityID, kind, lookupID,
	); err != nil {
		return mapManagementConflict(fmt.Errorf("SecondBox authority identity insert failed: %w", err))
	}
	return nil
}

func insertTenant(ctx context.Context, tx pgx.Tx, tenant contracts.Tenant) error {
	profileGrantsJSON, err := encodeManagementJSON("Tenant Profile grants", tenant.AllowedProfileGrants)
	if err != nil {
		return err
	}
	scopesJSON, err := encodeManagementJSON("Tenant application scopes", tenant.AllowedApplicationScopes)
	if err != nil {
		return err
	}
	quotaJSON, err := encodeManagementJSON("Tenant aggregate quota", tenant.AggregateQuota)
	if err != nil {
		return err
	}
	expiryPolicyJSON, err := encodeManagementJSON("Tenant expiry policy", tenant.ExpiryPolicy)
	if err != nil {
		return err
	}
	metadataJSON, err := encodeManagementJSON("Tenant metadata", tenant.Metadata)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.tenants (
		ref,state,allowed_profile_grants_json,allowed_application_scopes_json,
		aggregate_quota_json,expiry_policy_json,metadata_json,expires_at,revision,created_at,updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, tenant.Ref, tenant.State,
		profileGrantsJSON, scopesJSON, quotaJSON, expiryPolicyJSON, metadataJSON,
		tenant.ExpiresAt, tenant.Revision, tenant.CreatedAt.UTC(), tenant.UpdatedAt.UTC()); err != nil {
		return mapManagementConflict(fmt.Errorf("SecondBox Tenant insert failed: %w", err))
	}
	return nil
}

func insertSubject(ctx context.Context, tx pgx.Tx, subject contracts.Subject) error {
	quotaJSON, err := encodeManagementJSON("Subject quota", subject.Quota)
	if err != nil {
		return err
	}
	metadataJSON, err := encodeManagementJSON("Subject metadata", subject.Metadata)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.subjects (
		tenant_ref,ref,state,cleanup_state,quota_json,metadata_json,expires_at,revision,created_at,updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, subject.TenantRef, subject.Ref,
		subject.State, subject.CleanupState, quotaJSON, metadataJSON, subject.ExpiresAt,
		subject.Revision, subject.CreatedAt.UTC(), subject.UpdatedAt.UTC()); err != nil {
		return mapManagementConflict(fmt.Errorf("SecondBox Subject insert failed: %w", err))
	}
	return nil
}

func mapManagementConflict(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return errors.Join(ports.ErrManagementConflict, err)
	}
	return err
}

func insertTenantControllerCredential(ctx context.Context, tx pgx.Tx, authority contracts.TenantControllerAuthority) (contracts.TenantControllerCredentialResponse, error) {
	authority.Kind = contracts.AuthorityKindTenantController
	authority.Grant = contracts.TenantControllerGrantManagement
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(tenantControllerLookupPrefix, ports.TenantControllerBearerTokenPrefix)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, err
	}
	authority.LookupID = lookupID
	metadataJSON, err := encodeManagementJSON("TenantControllerAuthority metadata", authority.Metadata)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, err
	}
	if err := insertAuthorityIdentity(ctx, tx, authority.ID, authority.Kind, authority.LookupID); err != nil {
		return contracts.TenantControllerCredentialResponse{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.tenant_controller_authorities (
		id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,revision,
		token_verifier_sha256,created_at,updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, authority.ID, authority.LookupID,
		authority.TenantRef, authority.Grant, authority.State, metadataJSON, authority.ExpiresAt,
		authority.Revision, verifier[:], authority.CreatedAt.UTC(), authority.UpdatedAt.UTC()); err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority insert failed: %w", err)
	}
	return contracts.TenantControllerCredentialResponse{Authority: authority, BearerToken: bearerToken}, nil
}

func insertApplicationCredential(ctx context.Context, tx pgx.Tx, authority contracts.ApplicationAuthority) (contracts.ApplicationCredentialResponse, error) {
	authority.Kind = contracts.AuthorityKindApplication
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(applicationLookupPrefix, ports.ApplicationBearerTokenPrefix)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	authority.LookupID = lookupID
	scopesJSON, err := encodeManagementJSON("ApplicationAuthority scopes", authority.Scopes)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	profileGrantsJSON, err := encodeManagementJSON("ApplicationAuthority Profile grants", authority.ProfileGrants)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	metadataJSON, err := encodeManagementJSON("ApplicationAuthority metadata", authority.Metadata)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	if err := insertAuthorityIdentity(ctx, tx, authority.ID, authority.Kind, authority.LookupID); err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.application_authorities (
		id,lookup_id,tenant_ref,subject_ref,state,scopes_json,profile_grants_json,metadata_json,
		expires_at,revision,token_verifier_sha256,created_at,updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, authority.ID, authority.LookupID,
		authority.TenantRef, authority.SubjectRef, authority.State, scopesJSON, profileGrantsJSON,
		metadataJSON, authority.ExpiresAt, authority.Revision, verifier[:], authority.CreatedAt.UTC(),
		authority.UpdatedAt.UTC()); err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority insert failed: %w", err)
	}
	return contracts.ApplicationCredentialResponse{Authority: authority, BearerToken: bearerToken}, nil
}

func validateManagedTenantAdmission(tenant contracts.Tenant, now time.Time) error {
	if tenant.State == contracts.TenantStateSuspended {
		return ports.ErrTenantSuspended
	}
	if tenant.State == contracts.TenantStateExpired || isExpired(tenant.ExpiresAt, now) {
		return ports.ErrResourceExpired
	}
	if tenant.State != contracts.TenantStateActive {
		return ports.ErrInvalidLifecycleTransition
	}
	return nil
}

func managedAuthorityExpiryAllowed(tenant contracts.Tenant, now time.Time, expiresAt *time.Time) bool {
	if expiresAt == nil || !expiresAt.After(now.UTC()) {
		return false
	}
	if expiresAt.After(now.Add(time.Duration(tenant.ExpiryPolicy.MaximumAuthorityLifetimeSeconds) * time.Second)) {
		return false
	}
	return tenant.ExpiresAt == nil || !expiresAt.After(*tenant.ExpiresAt)
}

func subjectQuotaWithinTenant(subject contracts.QuotaLimits, tenant contracts.TenantQuota) bool {
	return subject.MaxSandboxes <= tenant.MaxSandboxes &&
		subject.MaxActiveInstances <= tenant.MaxActiveInstances &&
		subject.MaxCPUMillis <= tenant.MaxCPUMillis &&
		subject.MaxMemoryBytes <= tenant.MaxMemoryBytes &&
		subject.MaxSnapshots <= tenant.MaxSnapshots &&
		subject.MaxPortSessions <= tenant.MaxPortSessions &&
		subject.MaxConcurrentOperations <= tenant.MaxConcurrentOperations
}

func generatePersistedAuthorityCredential(
	lookupPrefix string,
	tokenPrefix string,
) (string, string, [sha256.Size]byte, error) {
	lookupRandom := make([]byte, 18)
	if _, err := rand.Read(lookupRandom); err != nil {
		return "", "", [sha256.Size]byte{}, fmt.Errorf("SecondBox authority lookup identity generation failed: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", [sha256.Size]byte{}, fmt.Errorf("SecondBox authority bearer generation failed: %w", err)
	}
	lookupID := lookupPrefix + base64.RawURLEncoding.EncodeToString(lookupRandom)
	bearerToken := tokenPrefix + lookupID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	return lookupID, bearerToken, sha256.Sum256([]byte(bearerToken)), nil
}

func parsePersistedAuthorityCredential(
	bearerToken string,
	tokenPrefix string,
	lookupPrefix string,
) (string, bool) {
	tail, ok := strings.CutPrefix(bearerToken, tokenPrefix)
	if !ok {
		return "", false
	}
	lookupLength := len(lookupPrefix) + 24
	if len(tail) <= lookupLength || tail[lookupLength] != '_' {
		return "", false
	}
	lookupID := tail[:lookupLength]
	if !strings.HasPrefix(lookupID, lookupPrefix) {
		return "", false
	}
	return lookupID, true
}

func verifyPersistedAuthorityCredential(bearerToken string, verifier []byte) bool {
	presented := sha256.Sum256([]byte(bearerToken))
	return subtle.ConstantTimeCompare(presented[:], verifier) == 1
}

func isExpired(expiresAt *time.Time, now time.Time) bool {
	return expiresAt != nil && !expiresAt.After(now.UTC())
}

func isStringSubset(values []string, ceiling []string) bool {
	allowed := make(map[string]struct{}, len(ceiling))
	for _, value := range ceiling {
		allowed[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := allowed[value]; !ok {
			return false
		}
	}
	return true
}

func encodeManagementJSON(name string, value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("SecondBox %s encoding failed: %w", name, err)
	}
	return encoded, nil
}

func decodeManagementJSON(name string, encoded []byte, target any) error {
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("SecondBox %s decode failed: %w", name, err)
	}
	return nil
}

type managementRow interface {
	Scan(dest ...any) error
}

func scanTenant(row managementRow) (contracts.Tenant, error) {
	var tenant contracts.Tenant
	var profileGrantsJSON, scopesJSON, quotaJSON, expiryPolicyJSON, metadataJSON []byte
	if err := row.Scan(&tenant.Ref, &tenant.State, &profileGrantsJSON, &scopesJSON, &quotaJSON,
		&expiryPolicyJSON, &metadataJSON, &tenant.ExpiresAt, &tenant.Revision,
		&tenant.CreatedAt, &tenant.UpdatedAt); err != nil {
		return contracts.Tenant{}, err
	}
	for _, decoded := range []struct {
		name string
		raw  []byte
		into any
	}{
		{"Tenant Profile grants", profileGrantsJSON, &tenant.AllowedProfileGrants},
		{"Tenant application scopes", scopesJSON, &tenant.AllowedApplicationScopes},
		{"Tenant aggregate quota", quotaJSON, &tenant.AggregateQuota},
		{"Tenant expiry policy", expiryPolicyJSON, &tenant.ExpiryPolicy},
		{"Tenant metadata", metadataJSON, &tenant.Metadata},
	} {
		if err := decodeManagementJSON(decoded.name, decoded.raw, decoded.into); err != nil {
			return contracts.Tenant{}, err
		}
	}
	return tenant, nil
}

func scanSubject(row managementRow) (contracts.Subject, error) {
	var subject contracts.Subject
	var quotaJSON, metadataJSON []byte
	if err := row.Scan(&subject.TenantRef, &subject.Ref, &subject.State, &subject.CleanupState,
		&quotaJSON, &metadataJSON, &subject.ExpiresAt, &subject.Revision,
		&subject.CreatedAt, &subject.UpdatedAt); err != nil {
		return contracts.Subject{}, err
	}
	if err := decodeManagementJSON("Subject quota", quotaJSON, &subject.Quota); err != nil {
		return contracts.Subject{}, err
	}
	if err := decodeManagementJSON("Subject metadata", metadataJSON, &subject.Metadata); err != nil {
		return contracts.Subject{}, err
	}
	return subject, nil
}

func scanTenantControllerAuthority(row managementRow) (contracts.TenantControllerAuthority, error) {
	var authority contracts.TenantControllerAuthority
	var metadataJSON []byte
	if err := row.Scan(
		&authority.ID, &authority.LookupID, &authority.TenantRef, &authority.Grant,
		&authority.State, &metadataJSON, &authority.ExpiresAt, &authority.Revision,
		&authority.CreatedAt, &authority.UpdatedAt,
	); err != nil {
		return contracts.TenantControllerAuthority{}, err
	}
	authority.Kind = contracts.AuthorityKindTenantController
	if err := decodeManagementJSON("TenantControllerAuthority metadata", metadataJSON, &authority.Metadata); err != nil {
		return contracts.TenantControllerAuthority{}, err
	}
	return authority, nil
}

func scanApplicationAuthority(row managementRow) (contracts.ApplicationAuthority, error) {
	var authority contracts.ApplicationAuthority
	var scopesJSON, profileGrantsJSON, metadataJSON []byte
	if err := row.Scan(
		&authority.ID, &authority.LookupID, &authority.TenantRef, &authority.SubjectRef,
		&authority.State, &scopesJSON, &profileGrantsJSON, &metadataJSON,
		&authority.ExpiresAt, &authority.Revision, &authority.CreatedAt, &authority.UpdatedAt,
	); err != nil {
		return contracts.ApplicationAuthority{}, err
	}
	authority.Kind = contracts.AuthorityKindApplication
	if err := decodeManagementJSON("ApplicationAuthority scopes", scopesJSON, &authority.Scopes); err != nil {
		return contracts.ApplicationAuthority{}, err
	}
	if err := decodeManagementJSON("ApplicationAuthority Profile grants", profileGrantsJSON, &authority.ProfileGrants); err != nil {
		return contracts.ApplicationAuthority{}, err
	}
	if err := decodeManagementJSON("ApplicationAuthority metadata", metadataJSON, &authority.Metadata); err != nil {
		return contracts.ApplicationAuthority{}, err
	}
	return authority, nil
}
