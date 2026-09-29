package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// managedProfileGrantLimit mirrors the public ProfileGrantList bound.
const managedProfileGrantLimit = 32

// ExtendManagedTenantCeiling adds Profile grants and application scopes to one
// Tenant ceiling. Extension is add-only, so existing authorities stay within it.
// A request whose entries are all present commits no new Tenant revision.
func (store *PostgresControlPlaneStore) ExtendManagedTenantCeiling(
	ctx context.Context,
	tenantRef string,
	profileGrants []string,
	applicationScopes []string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.Tenant, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant ceiling extension transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var replayed contracts.Tenant
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant ceiling extension replay commit failed: %w", err)
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
	extendedGrants, grantsAdded := extendGrantSet(tenant.AllowedProfileGrants, profileGrants)
	extendedScopes, scopesAdded := extendGrantSet(tenant.AllowedApplicationScopes, applicationScopes)
	if len(extendedGrants) > managedProfileGrantLimit {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, errors.Join(
			ports.ErrManagementConflict,
			fmt.Errorf("SecondBox Tenant Profile grant ceiling cannot exceed %d values", managedProfileGrantLimit),
		)
	}
	if grantsAdded || scopesAdded {
		profileGrantsJSON, err := encodeManagementJSON("Tenant Profile grants", extendedGrants)
		if err != nil {
			return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
		}
		scopesJSON, err := encodeManagementJSON("Tenant application scopes", extendedScopes)
		if err != nil {
			return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
		}
		tenant.AllowedProfileGrants = extendedGrants
		tenant.AllowedApplicationScopes = extendedScopes
		tenant.Revision++
		tenant.UpdatedAt = now.UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.tenants
			SET allowed_profile_grants_json=$2,allowed_application_scopes_json=$3,revision=$4,updated_at=$5
			WHERE ref=$1`,
			tenant.Ref, profileGrantsJSON, scopesJSON, tenant.Revision, tenant.UpdatedAt,
		); err != nil {
			return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant ceiling extension failed: %w", err)
		}
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, tenant)
	if err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Tenant{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox Tenant ceiling extension commit failed: %w", err)
	}
	return tenant, result, nil
}

// ExtendManagedApplicationAuthority adds Profile grants and scopes to one active
// application authority without rotating its credential. The resulting grants
// must remain within the Tenant's current ceiling. A request whose entries are
// all present commits no new authority revision.
func (store *PostgresControlPlaneStore) ExtendManagedApplicationAuthority(
	ctx context.Context,
	tenantRef string,
	authorityID string,
	profileGrants []string,
	scopes []string,
	expectedRevision int64,
	now time.Time,
	idempotency ports.AdminIdempotencyInput,
) (contracts.ApplicationAuthority, ports.AdminIdempotencyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority extension transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	var subjectRef string
	if err := tx.QueryRow(ctx, `
		SELECT subject_ref FROM secondbox.application_authorities
		WHERE tenant_ref=$1 AND id=$2`, tenantRef, authorityID,
	).Scan(&subjectRef); err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
	}
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	var replayed contracts.ApplicationAuthority
	result, found, err := lookupAdminIdempotency(ctx, tx, idempotency, &replayed)
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority extension replay commit failed: %w", err)
		}
		return replayed, result, nil
	}
	tenant, err := scanTenant(tx.QueryRow(ctx, tenantSelect+` WHERE ref=$1 FOR SHARE`, tenantRef))
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, mapNotFound(err, ports.ErrManagementNotFound)
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
	if authority.State != contracts.AuthorityStateActive || isExpired(authority.ExpiresAt, now) {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, ports.ErrInvalidLifecycleTransition
	}
	if err := validateManagedTenantAdmission(tenant, now); err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	extendedGrants, grantsAdded := extendGrantSet(authority.ProfileGrants, profileGrants)
	extendedScopes, scopesAdded := extendGrantSet(authority.Scopes, scopes)
	if !isStringSubset(extendedGrants, tenant.AllowedProfileGrants) ||
		!isStringSubset(extendedScopes, tenant.AllowedApplicationScopes) {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, ports.ErrGrantEscalationDenied
	}
	if grantsAdded || scopesAdded {
		scopesJSON, err := encodeManagementJSON("ApplicationAuthority scopes", extendedScopes)
		if err != nil {
			return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
		}
		profileGrantsJSON, err := encodeManagementJSON("ApplicationAuthority Profile grants", extendedGrants)
		if err != nil {
			return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
		}
		authority.Scopes = extendedScopes
		authority.ProfileGrants = extendedGrants
		authority.Revision++
		authority.UpdatedAt = now.UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.application_authorities
			SET scopes_json=$2,profile_grants_json=$3,revision=$4,updated_at=$5
			WHERE id=$1`,
			authority.ID, scopesJSON, profileGrantsJSON, authority.Revision, authority.UpdatedAt,
		); err != nil {
			return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority extension failed: %w", err)
		}
	}
	result, err = insertAdminIdempotency(ctx, tx, idempotency, authority)
	if err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplicationAuthority{}, ports.AdminIdempotencyResult{}, fmt.Errorf("SecondBox ApplicationAuthority extension commit failed: %w", err)
	}
	return authority, result, nil
}

// extendGrantSet returns the sorted union of existing and requested values and
// whether any requested value was absent. Without an addition it returns the
// existing slice unchanged, so a no-op preserves its stored order.
func extendGrantSet(existing []string, requested []string) ([]string, bool) {
	present := make(map[string]bool, len(existing)+len(requested))
	for _, value := range existing {
		present[value] = true
	}
	added := false
	for _, value := range requested {
		if !present[value] {
			present[value] = true
			added = true
		}
	}
	if !added {
		return existing, false
	}
	union := make([]string, 0, len(present))
	for value := range present {
		union = append(union, value)
	}
	sort.Strings(union)
	return union, true
}
