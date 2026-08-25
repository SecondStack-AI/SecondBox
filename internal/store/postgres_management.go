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

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const (
	tenantControllerLookupPrefix = "tca_"
	applicationLookupPrefix      = "apa_"
)

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
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(
		tenantControllerLookupPrefix,
		ports.TenantControllerBearerTokenPrefix,
	)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority rotate transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	authority, err := scanTenantControllerAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.tenant_controller_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if expectedRevision > 0 && authority.Revision != expectedRevision {
		return contracts.TenantControllerCredentialResponse{}, ports.ErrRevisionConflict
	}
	authority.LookupID = lookupID
	authority.Revision++
	authority.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.authority_identities SET lookup_id=$2
		WHERE id=$1 AND kind=$3`, authority.ID, lookupID, contracts.AuthorityKindTenantController,
	); err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority identity rotation failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.tenant_controller_authorities
		SET lookup_id=$2,token_verifier_sha256=$3,revision=$4,updated_at=$5
		WHERE id=$1`, authority.ID, lookupID, verifier[:], authority.Revision, authority.UpdatedAt,
	); err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority rotation failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.TenantControllerCredentialResponse{}, fmt.Errorf("SecondBox TenantControllerAuthority rotate commit failed: %w", err)
	}
	return contracts.TenantControllerCredentialResponse{Authority: authority, BearerToken: bearerToken}, nil
}

// RotateApplicationAuthority invalidates the previous bearer and returns one replacement.
func (store *PostgresControlPlaneStore) RotateApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
) (contracts.ApplicationCredentialResponse, error) {
	lookupID, bearerToken, verifier, err := generatePersistedAuthorityCredential(
		applicationLookupPrefix,
		ports.ApplicationBearerTokenPrefix,
	)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority rotate transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	authority, err := scanApplicationAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,subject_ref,state,scopes_json,
		       profile_grants_json,metadata_json,expires_at,revision,created_at,updated_at
		FROM secondbox.application_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if expectedRevision > 0 && authority.Revision != expectedRevision {
		return contracts.ApplicationCredentialResponse{}, ports.ErrRevisionConflict
	}
	authority.LookupID = lookupID
	authority.Revision++
	authority.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.authority_identities SET lookup_id=$2
		WHERE id=$1 AND kind=$3`, authority.ID, lookupID, contracts.AuthorityKindApplication,
	); err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority identity rotation failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.application_authorities
		SET lookup_id=$2,token_verifier_sha256=$3,revision=$4,updated_at=$5
		WHERE id=$1`, authority.ID, lookupID, verifier[:], authority.Revision, authority.UpdatedAt,
	); err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority rotation failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplicationCredentialResponse{}, fmt.Errorf("SecondBox ApplicationAuthority rotate commit failed: %w", err)
	}
	return contracts.ApplicationCredentialResponse{Authority: authority, BearerToken: bearerToken}, nil
}

// RevokeTenantControllerAuthority immediately denies its bearer credential.
func (store *PostgresControlPlaneStore) RevokeTenantControllerAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
) (contracts.TenantControllerAuthority, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.TenantControllerAuthority{}, fmt.Errorf("SecondBox TenantControllerAuthority revoke transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	authority, err := scanTenantControllerAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,grant_name,state,metadata_json,expires_at,
		       revision,created_at,updated_at
		FROM secondbox.tenant_controller_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.TenantControllerAuthority{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if expectedRevision > 0 && authority.Revision != expectedRevision {
		return contracts.TenantControllerAuthority{}, ports.ErrRevisionConflict
	}
	if authority.State != contracts.AuthorityStateRevoked {
		authority.State = contracts.AuthorityStateRevoked
		authority.Revision++
		authority.UpdatedAt = now.UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.tenant_controller_authorities
			SET state=$2,revision=$3,updated_at=$4 WHERE id=$1`,
			authority.ID, authority.State, authority.Revision, authority.UpdatedAt,
		); err != nil {
			return contracts.TenantControllerAuthority{}, fmt.Errorf("SecondBox TenantControllerAuthority revocation failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.TenantControllerAuthority{}, fmt.Errorf("SecondBox TenantControllerAuthority revoke commit failed: %w", err)
	}
	return authority, nil
}

// RevokeApplicationAuthority immediately denies its bearer credential.
func (store *PostgresControlPlaneStore) RevokeApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	expectedRevision int64,
	now time.Time,
) (contracts.ApplicationAuthority, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ApplicationAuthority{}, fmt.Errorf("SecondBox ApplicationAuthority revoke transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	authority, err := scanApplicationAuthority(tx.QueryRow(ctx, `
		SELECT id,lookup_id,tenant_ref,subject_ref,state,scopes_json,
		       profile_grants_json,metadata_json,expires_at,revision,created_at,updated_at
		FROM secondbox.application_authorities
		WHERE tenant_ref=$1 AND id=$2 FOR UPDATE`, tenantRef, authorityID))
	if err != nil {
		return contracts.ApplicationAuthority{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if expectedRevision > 0 && authority.Revision != expectedRevision {
		return contracts.ApplicationAuthority{}, ports.ErrRevisionConflict
	}
	if authority.State != contracts.AuthorityStateRevoked {
		authority.State = contracts.AuthorityStateRevoked
		authority.Revision++
		authority.UpdatedAt = now.UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.application_authorities
			SET state=$2,revision=$3,updated_at=$4 WHERE id=$1`,
			authority.ID, authority.State, authority.Revision, authority.UpdatedAt,
		); err != nil {
			return contracts.ApplicationAuthority{}, fmt.Errorf("SecondBox ApplicationAuthority revocation failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplicationAuthority{}, fmt.Errorf("SecondBox ApplicationAuthority revoke commit failed: %w", err)
	}
	return authority, nil
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
		return fmt.Errorf("SecondBox authority identity insert failed: %w", err)
	}
	return nil
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
