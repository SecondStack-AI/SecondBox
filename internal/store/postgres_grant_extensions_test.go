package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestManagedTenantCeilingExtensionIsAddOnlyRevisionFencedAndIdempotent(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("tenant-ceiling-extension", now)
	tenant.AllowedApplicationScopes = []string{"sandbox:lifecycle", "sandbox:read"}
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}

	input := adminTestInput(tenant.Ref, tenant.Ref, "tenant.ceiling.extend", "extend-ceiling", now)
	extended, result, err := controlPlaneStore.ExtendManagedTenantCeiling(
		t.Context(), tenant.Ref, []string{"agent-compartment", "coding"}, []string{"sandbox:ports"},
		tenant.Revision, now.Add(time.Second), input,
	)
	if err != nil || result.Replayed || extended.Revision != tenant.Revision+1 ||
		!extended.UpdatedAt.Equal(now.Add(time.Second)) ||
		!reflect.DeepEqual(extended.AllowedProfileGrants, []string{"agent-compartment", "coding"}) ||
		!reflect.DeepEqual(extended.AllowedApplicationScopes, []string{"sandbox:lifecycle", "sandbox:ports", "sandbox:read"}) {
		t.Fatalf("Tenant ceiling extension = %#v result=%#v error=%v", extended, result, err)
	}
	replayed, result, err := controlPlaneStore.ExtendManagedTenantCeiling(
		t.Context(), tenant.Ref, []string{"agent-compartment", "coding"}, []string{"sandbox:ports"},
		tenant.Revision, now.Add(2*time.Second), input,
	)
	if err != nil || !result.Replayed || !sameTenantCeiling(replayed, extended) {
		t.Fatalf("Tenant ceiling extension replay = %#v result=%#v error=%v", replayed, result, err)
	}
	conflicting := input
	conflicting.RequestHash = "different-extension-hash"
	if _, _, err := controlPlaneStore.ExtendManagedTenantCeiling(
		t.Context(), tenant.Ref, []string{"coding"}, []string{}, tenant.Revision, now, conflicting,
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Tenant ceiling extension replay error = %v", err)
	}
	stale := adminTestInput(tenant.Ref, tenant.Ref, "tenant.ceiling.extend", "stale-extend-ceiling", now)
	if _, _, err := controlPlaneStore.ExtendManagedTenantCeiling(
		t.Context(), tenant.Ref, []string{"durable-coding"}, []string{}, tenant.Revision, now, stale,
	); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale Tenant ceiling extension error = %v", err)
	}

	noOp := adminTestInput(tenant.Ref, tenant.Ref, "tenant.ceiling.extend", "no-op-extend-ceiling", now)
	unchanged, _, err := controlPlaneStore.ExtendManagedTenantCeiling(
		t.Context(), tenant.Ref, []string{"coding"}, []string{"sandbox:read"},
		extended.Revision, now.Add(3*time.Second), noOp,
	)
	if err != nil || !sameTenantCeiling(unchanged, extended) {
		t.Fatalf("no-op Tenant ceiling extension = %#v error=%v", unchanged, err)
	}
	var auditEvents int
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.audit_events WHERE id=$1`, noOp.AuditEvent.ID,
	).Scan(&auditEvents); err != nil {
		t.Fatal(err)
	}
	if auditEvents != 1 {
		t.Fatalf("no-op Tenant ceiling extension audit events = %d", auditEvents)
	}

	oversized := make([]string, 0, managedProfileGrantLimit)
	for index := range managedProfileGrantLimit {
		oversized = append(oversized, "profile-"+string(rune('a'+index/26))+string(rune('a'+index%26)))
	}
	overLimit := adminTestInput(tenant.Ref, tenant.Ref, "tenant.ceiling.extend", "over-limit-extend-ceiling", now)
	if _, _, err := controlPlaneStore.ExtendManagedTenantCeiling(
		t.Context(), tenant.Ref, oversized, []string{}, extended.Revision, now, overLimit,
	); !errors.Is(err, ports.ErrManagementConflict) {
		t.Fatalf("over-limit Tenant ceiling extension error = %v", err)
	}
	stored, err := controlPlaneStore.GetTenant(t.Context(), tenant.Ref)
	if err != nil || !sameTenantCeiling(stored, extended) {
		t.Fatalf("stored Tenant after rejected extension = %#v error=%v", stored, err)
	}
}

func TestManagedApplicationAuthorityExtensionStaysWithinTheTenantCeiling(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("authority-extension-tenant", now)
	tenant.AllowedProfileGrants = []string{"agent-compartment", "coding"}
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	subject := managementTestSubject(tenant.Ref, "authority-extension-subject", now)
	if _, err := controlPlaneStore.CreateSubject(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(30 * time.Minute)
	application, err := controlPlaneStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID: "authority-extension-application", TenantRef: tenant.Ref, SubjectRef: subject.Ref,
		State: contracts.AuthorityStateActive, Scopes: []string{"sandbox:read"},
		ProfileGrants: []string{"coding"}, Metadata: map[string]string{}, ExpiresAt: &expiresAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	authority := application.Authority

	escalation := adminTestInput(tenant.Ref, subject.Ref, "application_authority.extend", "escalating-extension", now)
	if _, _, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, authority.ID, []string{"durable-coding"}, []string{}, authority.Revision, now, escalation,
	); !errors.Is(err, ports.ErrGrantEscalationDenied) {
		t.Fatalf("out-of-ceiling ApplicationAuthority extension error = %v", err)
	}
	if _, _, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, authority.ID, []string{}, []string{"sandbox:ports:direct"}, authority.Revision, now,
		adminTestInput(tenant.Ref, subject.Ref, "application_authority.extend", "escalating-scope-extension", now),
	); !errors.Is(err, ports.ErrGrantEscalationDenied) {
		t.Fatalf("out-of-ceiling ApplicationAuthority scope extension error = %v", err)
	}

	input := adminTestInput(tenant.Ref, subject.Ref, "application_authority.extend", "authority-extension", now)
	extended, result, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, authority.ID, []string{"agent-compartment"}, []string{"sandbox:ports"},
		authority.Revision, now.Add(time.Second), input,
	)
	if err != nil || result.Replayed || extended.Revision != authority.Revision+1 ||
		extended.LookupID != authority.LookupID ||
		!reflect.DeepEqual(extended.ProfileGrants, []string{"agent-compartment", "coding"}) ||
		!reflect.DeepEqual(extended.Scopes, []string{"sandbox:ports", "sandbox:read"}) {
		t.Fatalf("ApplicationAuthority extension = %#v result=%#v error=%v", extended, result, err)
	}
	replayed, result, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, authority.ID, []string{"agent-compartment"}, []string{"sandbox:ports"},
		authority.Revision, now.Add(2*time.Second), input,
	)
	if err != nil || !result.Replayed || !sameAuthorityGrants(replayed, extended) {
		t.Fatalf("ApplicationAuthority extension replay = %#v result=%#v error=%v", replayed, result, err)
	}
	if _, _, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, authority.ID, []string{"agent-compartment"}, []string{}, authority.Revision, now,
		adminTestInput(tenant.Ref, subject.Ref, "application_authority.extend", "stale-authority-extension", now),
	); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale ApplicationAuthority extension error = %v", err)
	}
	unchanged, _, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, authority.ID, []string{"coding"}, []string{"sandbox:read"}, extended.Revision, now.Add(3*time.Second),
		adminTestInput(tenant.Ref, subject.Ref, "application_authority.extend", "no-op-authority-extension", now),
	)
	if err != nil || !sameAuthorityGrants(unchanged, extended) {
		t.Fatalf("no-op ApplicationAuthority extension = %#v error=%v", unchanged, err)
	}
	authenticated, err := controlPlaneStore.AuthenticateApplicationAuthority(t.Context(), application.BearerToken, now.Add(time.Minute))
	if err != nil || !reflect.DeepEqual(authenticated.Scopes, extended.Scopes) ||
		!reflect.DeepEqual(authenticated.ProfileGrants, extended.ProfileGrants) {
		t.Fatalf("extended ApplicationAuthority authentication = %#v error=%v", authenticated, err)
	}

	revoked, err := controlPlaneStore.RevokeApplicationAuthority(t.Context(), tenant.Ref, authority.ID, extended.Revision, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, authority.ID, []string{}, []string{"sandbox:exec"}, revoked.Revision, now.Add(5*time.Second),
		adminTestInput(tenant.Ref, subject.Ref, "application_authority.extend", "revoked-authority-extension", now),
	); !errors.Is(err, ports.ErrInvalidLifecycleTransition) {
		t.Fatalf("revoked ApplicationAuthority extension error = %v", err)
	}
	if _, _, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), "another-tenant", authority.ID, []string{}, []string{"sandbox:exec"}, revoked.Revision, now,
		adminTestInput("another-tenant", subject.Ref, "application_authority.extend", "cross-tenant-authority-extension", now),
	); !errors.Is(err, ports.ErrManagementNotFound) {
		t.Fatalf("cross-Tenant ApplicationAuthority extension error = %v", err)
	}
}

func TestManagedApplicationAuthorityExtensionRejectsAnExpiredAuthority(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("expired-authority-extension-tenant", now)
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	subject := managementTestSubject(tenant.Ref, "expired-authority-extension-subject", now)
	if _, err := controlPlaneStore.CreateSubject(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(time.Minute)
	application, err := controlPlaneStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID: "expired-authority-extension-application", TenantRef: tenant.Ref, SubjectRef: subject.Ref,
		State: contracts.AuthorityStateActive, Scopes: []string{"sandbox:read"},
		ProfileGrants: []string{"coding"}, Metadata: map[string]string{}, ExpiresAt: &expiresAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controlPlaneStore.ExtendManagedApplicationAuthority(
		t.Context(), tenant.Ref, application.Authority.ID, []string{}, []string{"sandbox:exec"},
		application.Authority.Revision, expiresAt,
		adminTestInput(tenant.Ref, subject.Ref, "application_authority.extend", "expired-authority-extension", now),
	); !errors.Is(err, ports.ErrInvalidLifecycleTransition) {
		t.Fatalf("expired ApplicationAuthority extension error = %v", err)
	}
}

func sameTenantCeiling(left, right contracts.Tenant) bool {
	return left.Ref == right.Ref && left.Revision == right.Revision && left.UpdatedAt.Equal(right.UpdatedAt) &&
		reflect.DeepEqual(left.AllowedProfileGrants, right.AllowedProfileGrants) &&
		reflect.DeepEqual(left.AllowedApplicationScopes, right.AllowedApplicationScopes)
}

func sameAuthorityGrants(left, right contracts.ApplicationAuthority) bool {
	return left.ID == right.ID && left.LookupID == right.LookupID && left.Revision == right.Revision &&
		left.UpdatedAt.Equal(right.UpdatedAt) && reflect.DeepEqual(left.Scopes, right.Scopes) &&
		reflect.DeepEqual(left.ProfileGrants, right.ProfileGrants)
}
