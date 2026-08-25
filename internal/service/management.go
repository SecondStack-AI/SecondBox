package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var managementMetadataKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
var managementOwnershipRefPattern = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

var applicationScopeSet = map[string]bool{
	"sandbox:read": true, "sandbox:lifecycle": true, "sandbox:exec": true,
	"sandbox:files": true, "sandbox:ports": true, "sandbox:ports:direct": true,
}

// CreateTenant creates one explicit operator-owned Tenant boundary.
func (service *ControlPlaneService) CreateTenant(ctx context.Context, principal contracts.Principal, idempotencyKey string, request contracts.CreateTenantRequest) (contracts.Tenant, bool, error) {
	if err := validateCreateTenantRequest(request, service.now().UTC()); err != nil {
		return contracts.Tenant{}, false, service.managementDenied(ctx, principal, "tenant.created", "tenant", request.Ref, request.Ref, err)
	}
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(principal, "tenant.create", request.Ref, idempotencyKey, request, now)
	if err != nil {
		return contracts.Tenant{}, false, err
	}
	tenant := contracts.Tenant{
		Ref: request.Ref, State: contracts.TenantStateActive,
		AllowedProfileGrants:     sortedUnique(request.AllowedProfileGrants),
		AllowedApplicationScopes: sortedUnique(request.AllowedApplicationScopes),
		AggregateQuota:           request.AggregateQuota, ExpiryPolicy: request.ExpiryPolicy,
		Metadata: cloneMetadata(request.Metadata), ExpiresAt: request.ExpiresAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	tenant, result, err := service.store.CreateManagedTenant(ctx, tenant, idempotency)
	if err != nil {
		return contracts.Tenant{}, false, service.managementDenied(ctx, principal, "tenant.created", "tenant", request.Ref, request.Ref, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "tenant.created", "tenant", tenant.Ref, tenant.Ref, now)); err != nil {
		return contracts.Tenant{}, false, err
	}
	return tenant, result.Replayed, nil
}

func (service *ControlPlaneService) GetTenant(ctx context.Context, _ contracts.Principal, tenantRef string) (contracts.Tenant, error) {
	if err := validateOwnershipRef("Tenant", tenantRef); err != nil {
		return contracts.Tenant{}, err
	}
	return service.store.GetTenant(ctx, tenantRef)
}

func (service *ControlPlaneService) ListTenants(ctx context.Context, _ contracts.Principal, limit int, cursor string) (contracts.TenantPage, error) {
	return service.store.ListTenants(ctx, boundedLimit(limit), cursor)
}

func (service *ControlPlaneService) SuspendTenant(ctx context.Context, principal contracts.Principal, tenantRef, idempotencyKey string, expectedRevision int64) (contracts.Tenant, bool, error) {
	return service.setTenantState(ctx, principal, tenantRef, contracts.TenantStateSuspended, "tenant.suspend", "tenant.suspended", idempotencyKey, expectedRevision)
}

func (service *ControlPlaneService) ReactivateTenant(ctx context.Context, principal contracts.Principal, tenantRef, idempotencyKey string, expectedRevision int64) (contracts.Tenant, bool, error) {
	return service.setTenantState(ctx, principal, tenantRef, contracts.TenantStateActive, "tenant.reactivate", "tenant.reactivated", idempotencyKey, expectedRevision)
}

func (service *ControlPlaneService) setTenantState(ctx context.Context, principal contracts.Principal, tenantRef, targetState, operation, auditAction, idempotencyKey string, expectedRevision int64) (contracts.Tenant, bool, error) {
	if err := validateOwnershipRef("Tenant", tenantRef); err != nil {
		return contracts.Tenant{}, false, err
	}
	if expectedRevision < 1 {
		return contracts.Tenant{}, false, invalidRequest(errors.New("SecondBox Tenant revision must be positive"))
	}
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(principal, operation, tenantRef, idempotencyKey, struct {
		ExpectedRevision int64 `json:"expectedRevision"`
	}{expectedRevision}, now)
	if err != nil {
		return contracts.Tenant{}, false, err
	}
	tenant, result, err := service.store.SetTenantState(ctx, tenantRef, targetState, expectedRevision, now, idempotency)
	if err != nil {
		return contracts.Tenant{}, false, service.managementDenied(ctx, principal, auditAction, "tenant", tenantRef, tenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, auditAction, "tenant", tenantRef, tenantRef, now)); err != nil {
		return contracts.Tenant{}, false, err
	}
	return tenant, result.Replayed, nil
}

func (service *ControlPlaneService) CreateTenantControllerAuthority(ctx context.Context, principal contracts.Principal, tenantRef, idempotencyKey string, request contracts.CreateTenantControllerAuthorityRequest) (contracts.TenantControllerCredentialResponse, bool, error) {
	now := service.now().UTC()
	if err := validateOwnershipRef("Tenant", tenantRef); err != nil {
		return contracts.TenantControllerCredentialResponse{}, false, err
	}
	if err := validateManagementMetadata(request.Metadata); err != nil || !request.ExpiresAt.After(now) {
		if err == nil {
			err = invalidRequest(errors.New("SecondBox TenantControllerAuthority expiry must be in the future"))
		}
		return contracts.TenantControllerCredentialResponse{}, false, service.managementDenied(ctx, principal, "tenant_controller_authority.created", "tenant_controller_authority", tenantRef, tenantRef, err)
	}
	scoped := principal
	scoped.TenantRef = tenantRef
	idempotency, err := service.adminIdempotency(scoped, "tenant_controller_authority.create", tenantRef, idempotencyKey, request, now)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, false, err
	}
	authority := contracts.TenantControllerAuthority{
		ID: service.newID("tca"), TenantRef: tenantRef, State: contracts.AuthorityStateActive,
		Metadata: cloneMetadata(request.Metadata), ExpiresAt: &request.ExpiresAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	response, result, err := service.store.CreateManagedTenantControllerAuthority(ctx, authority, idempotency)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, false, service.managementDenied(ctx, principal, "tenant_controller_authority.created", "tenant_controller_authority", authority.ID, tenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "tenant_controller_authority.created", "tenant_controller_authority", response.Authority.ID, tenantRef, now)); err != nil {
		return contracts.TenantControllerCredentialResponse{}, false, err
	}
	return response, result.Replayed, nil
}

func (service *ControlPlaneService) GetTenantControllerAuthority(ctx context.Context, _ contracts.Principal, tenantRef, authorityID string) (contracts.TenantControllerAuthority, error) {
	return service.store.GetTenantControllerAuthority(ctx, tenantRef, authorityID)
}

func (service *ControlPlaneService) ListTenantControllerAuthorities(ctx context.Context, _ contracts.Principal, tenantRef string, limit int, cursor string) (contracts.TenantControllerAuthorityPage, error) {
	return service.store.ListTenantControllerAuthorities(ctx, tenantRef, boundedLimit(limit), cursor)
}

func (service *ControlPlaneService) RotateTenantControllerAuthority(ctx context.Context, principal contracts.Principal, tenantRef, authorityID, idempotencyKey string, expectedRevision int64) (contracts.TenantControllerCredentialResponse, bool, error) {
	now := service.now().UTC()
	scoped := principal
	scoped.TenantRef = tenantRef
	idempotency, err := service.adminIdempotency(scoped, "tenant_controller_authority.rotate", authorityID, idempotencyKey, struct {
		ExpectedRevision int64 `json:"expectedRevision"`
	}{expectedRevision}, now)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, false, err
	}
	response, result, err := service.store.RotateManagedTenantControllerAuthority(ctx, tenantRef, authorityID, expectedRevision, now, idempotency)
	if err != nil {
		return contracts.TenantControllerCredentialResponse{}, false, service.managementDenied(ctx, principal, "tenant_controller_authority.rotated", "tenant_controller_authority", authorityID, tenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "tenant_controller_authority.rotated", "tenant_controller_authority", authorityID, tenantRef, now)); err != nil {
		return contracts.TenantControllerCredentialResponse{}, false, err
	}
	return response, result.Replayed, nil
}

func (service *ControlPlaneService) RevokeTenantControllerAuthority(ctx context.Context, principal contracts.Principal, tenantRef, authorityID, idempotencyKey string, expectedRevision int64) (contracts.TenantControllerAuthority, bool, error) {
	now := service.now().UTC()
	scoped := principal
	scoped.TenantRef = tenantRef
	idempotency, err := service.adminIdempotency(scoped, "tenant_controller_authority.revoke", authorityID, idempotencyKey, struct {
		ExpectedRevision int64 `json:"expectedRevision"`
	}{expectedRevision}, now)
	if err != nil {
		return contracts.TenantControllerAuthority{}, false, err
	}
	authority, result, err := service.store.RevokeManagedTenantControllerAuthority(ctx, tenantRef, authorityID, expectedRevision, now, idempotency)
	if err != nil {
		return contracts.TenantControllerAuthority{}, false, service.managementDenied(ctx, principal, "tenant_controller_authority.revoked", "tenant_controller_authority", authorityID, tenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "tenant_controller_authority.revoked", "tenant_controller_authority", authorityID, tenantRef, now)); err != nil {
		return contracts.TenantControllerAuthority{}, false, err
	}
	return authority, result.Replayed, nil
}

func (service *ControlPlaneService) CreateSubject(ctx context.Context, principal contracts.Principal, idempotencyKey string, request contracts.CreateSubjectRequest) (contracts.Subject, bool, error) {
	now := service.now().UTC()
	if err := validateCreateSubjectRequest(request, now); err != nil {
		return contracts.Subject{}, false, service.managementDenied(ctx, principal, "subject.created", "subject", request.Ref, principal.TenantRef, err)
	}
	idempotency, err := service.adminIdempotency(principal, "subject.create", request.Ref, idempotencyKey, request, now)
	if err != nil {
		return contracts.Subject{}, false, err
	}
	subject := contracts.Subject{
		TenantRef: principal.TenantRef, Ref: request.Ref, State: contracts.SubjectStateActive,
		CleanupState: contracts.SubjectCleanupStateNone, Quota: request.Quota,
		Metadata: cloneMetadata(request.Metadata), ExpiresAt: request.ExpiresAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	subject, result, err := service.store.CreateManagedSubject(ctx, subject, idempotency)
	if err != nil {
		return contracts.Subject{}, false, service.managementDenied(ctx, principal, "subject.created", "subject", request.Ref, principal.TenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "subject.created", "subject", subject.Ref, principal.TenantRef, now)); err != nil {
		return contracts.Subject{}, false, err
	}
	return subject, result.Replayed, nil
}

func (service *ControlPlaneService) GetSubject(ctx context.Context, principal contracts.Principal, subjectRef string) (contracts.Subject, error) {
	return service.store.GetSubject(ctx, principal.TenantRef, subjectRef)
}

func (service *ControlPlaneService) ListSubjects(ctx context.Context, principal contracts.Principal, limit int, cursor string) (contracts.SubjectPage, error) {
	return service.store.ListSubjects(ctx, principal.TenantRef, boundedLimit(limit), cursor)
}

// UpdateSubjectQuota replaces one Subject quota under revision and usage fences.
func (service *ControlPlaneService) UpdateSubjectQuota(
	ctx context.Context,
	principal contracts.Principal,
	subjectRef string,
	idempotencyKey string,
	expectedRevision int64,
	request contracts.UpdateSubjectQuotaRequest,
) (contracts.Subject, bool, error) {
	if err := validateOwnershipRef("Subject", subjectRef); err != nil {
		return contracts.Subject{}, false, err
	}
	if !validSubjectQuota(request.Quota) {
		return contracts.Subject{}, false, invalidRequest(errors.New("SecondBox Subject quota must be non-negative"))
	}
	if expectedRevision < 1 {
		return contracts.Subject{}, false, invalidRequest(errors.New("SecondBox Subject revision must be positive"))
	}
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(principal, "subject.quota.update", subjectRef, idempotencyKey, struct {
		ExpectedRevision int64                 `json:"expectedRevision"`
		Quota            contracts.QuotaLimits `json:"quota"`
	}{ExpectedRevision: expectedRevision, Quota: request.Quota}, now)
	if err != nil {
		return contracts.Subject{}, false, err
	}
	subject, result, err := service.store.UpdateManagedSubjectQuota(
		ctx, principal.TenantRef, subjectRef, request.Quota, expectedRevision, now, idempotency,
	)
	if err != nil {
		return contracts.Subject{}, false, service.managementDenied(
			ctx, principal, "subject.quota_updated", "subject", subjectRef, principal.TenantRef, err,
		)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(
		ctx, principal, "subject.quota_updated", "subject", subjectRef, principal.TenantRef, now,
	)); err != nil {
		return contracts.Subject{}, false, err
	}
	return subject, result.Replayed, nil
}

// GetTenantUsage returns aggregate and per-Subject usage for the authenticated tenant.
func (service *ControlPlaneService) GetTenantUsage(ctx context.Context, principal contracts.Principal) (contracts.TenantUsage, error) {
	if principal.Kind != contracts.AuthorityKindTenantController || principal.TenantRef == "" {
		return contracts.TenantUsage{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetTenantUsage(ctx, principal.TenantRef, service.now().UTC())
}

// GetDeploymentUsage returns deployment-wide usage to the platform operator.
func (service *ControlPlaneService) GetDeploymentUsage(
	ctx context.Context,
	principal contracts.Principal,
	limit int,
	cursor string,
) (contracts.DeploymentUsage, error) {
	if principal.Kind != contracts.AuthorityKindPlatform {
		return contracts.DeploymentUsage{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetDeploymentUsage(ctx, boundedLimit(limit), cursor, service.now().UTC())
}

func (service *ControlPlaneService) CreateApplicationAuthority(ctx context.Context, principal contracts.Principal, idempotencyKey string, request contracts.CreateApplicationAuthorityRequest) (contracts.ApplicationCredentialResponse, bool, error) {
	now := service.now().UTC()
	if err := validateCreateApplicationAuthorityRequest(request, now); err != nil {
		return contracts.ApplicationCredentialResponse{}, false, service.managementDenied(ctx, principal, "application_authority.created", "application_authority", request.SubjectRef, principal.TenantRef, err)
	}
	idempotency, err := service.adminIdempotency(principal, "application_authority.create", request.SubjectRef, idempotencyKey, request, now)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, false, err
	}
	authority := contracts.ApplicationAuthority{
		ID: service.newID("apa"), TenantRef: principal.TenantRef, SubjectRef: request.SubjectRef,
		State: contracts.AuthorityStateActive, Scopes: sortedUnique(request.Scopes),
		ProfileGrants: sortedUnique(request.ProfileGrants), Metadata: cloneMetadata(request.Metadata),
		ExpiresAt: &request.ExpiresAt, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	response, result, err := service.store.CreateManagedApplicationAuthority(ctx, authority, idempotency)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, false, service.managementDenied(ctx, principal, "application_authority.created", "application_authority", authority.ID, principal.TenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "application_authority.created", "application_authority", response.Authority.ID, principal.TenantRef, now)); err != nil {
		return contracts.ApplicationCredentialResponse{}, false, err
	}
	return response, result.Replayed, nil
}

func (service *ControlPlaneService) GetApplicationAuthority(ctx context.Context, principal contracts.Principal, authorityID string) (contracts.ApplicationAuthority, error) {
	return service.store.GetApplicationAuthority(ctx, principal.TenantRef, authorityID)
}

func (service *ControlPlaneService) ListApplicationAuthorities(ctx context.Context, principal contracts.Principal, subjectRef string, limit int, cursor string) (contracts.ApplicationAuthorityPage, error) {
	if subjectRef != "" {
		if err := validateOwnershipRef("Subject", subjectRef); err != nil {
			return contracts.ApplicationAuthorityPage{}, err
		}
	}
	return service.store.ListApplicationAuthorities(ctx, principal.TenantRef, subjectRef, boundedLimit(limit), cursor)
}

func (service *ControlPlaneService) RotateApplicationAuthority(ctx context.Context, principal contracts.Principal, authorityID, idempotencyKey string, expectedRevision int64) (contracts.ApplicationCredentialResponse, bool, error) {
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(principal, "application_authority.rotate", authorityID, idempotencyKey, struct {
		ExpectedRevision int64 `json:"expectedRevision"`
	}{expectedRevision}, now)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, false, err
	}
	response, result, err := service.store.RotateManagedApplicationAuthority(ctx, principal.TenantRef, authorityID, expectedRevision, now, idempotency)
	if err != nil {
		return contracts.ApplicationCredentialResponse{}, false, service.managementDenied(ctx, principal, "application_authority.rotated", "application_authority", authorityID, principal.TenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "application_authority.rotated", "application_authority", authorityID, principal.TenantRef, now)); err != nil {
		return contracts.ApplicationCredentialResponse{}, false, err
	}
	return response, result.Replayed, nil
}

func (service *ControlPlaneService) RevokeApplicationAuthority(ctx context.Context, principal contracts.Principal, authorityID, idempotencyKey string, expectedRevision int64) (contracts.ApplicationAuthority, bool, error) {
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(principal, "application_authority.revoke", authorityID, idempotencyKey, struct {
		ExpectedRevision int64 `json:"expectedRevision"`
	}{expectedRevision}, now)
	if err != nil {
		return contracts.ApplicationAuthority{}, false, err
	}
	authority, result, err := service.store.RevokeManagedApplicationAuthority(ctx, principal.TenantRef, authorityID, expectedRevision, now, idempotency)
	if err != nil {
		return contracts.ApplicationAuthority{}, false, service.managementDenied(ctx, principal, "application_authority.revoked", "application_authority", authorityID, principal.TenantRef, err)
	}
	if err := service.store.AppendAuditEvent(ctx, service.newAudit(ctx, principal, "application_authority.revoked", "application_authority", authorityID, principal.TenantRef, now)); err != nil {
		return contracts.ApplicationAuthority{}, false, err
	}
	return authority, result.Replayed, nil
}

func (service *ControlPlaneService) managementDenied(ctx context.Context, principal contracts.Principal, action, resourceKind, resourceID, tenantRef string, err error) error {
	if !isManagementDenial(err) || service.store == nil {
		return err
	}
	event := service.newAudit(ctx, principal, action, resourceKind, resourceID, tenantRef, service.now().UTC())
	event.Outcome = "denied"
	if auditErr := service.store.AppendAuditEvent(ctx, event); auditErr != nil {
		return auditErr
	}
	return err
}

func isManagementDenial(err error) bool {
	return errors.Is(err, ports.ErrInvalidRequest) || errors.Is(err, ports.ErrManagementNotFound) ||
		errors.Is(err, ports.ErrManagementConflict) || errors.Is(err, ports.ErrInvalidLifecycleTransition) ||
		errors.Is(err, ports.ErrResourceExpired) || errors.Is(err, ports.ErrTenantSuspended) ||
		errors.Is(err, ports.ErrGrantEscalationDenied) || errors.Is(err, ports.ErrRevisionConflict) ||
		errors.Is(err, ports.ErrIdempotencyConflict) || errors.Is(err, ports.ErrCredentialResponseUnavailable) ||
		errors.Is(err, ports.ErrQuotaExceeded)
}

func validateCreateTenantRequest(request contracts.CreateTenantRequest, now time.Time) error {
	if err := validateOwnershipRef("Tenant", request.Ref); err != nil {
		return err
	}
	if err := validateManagementMetadata(request.Metadata); err != nil {
		return err
	}
	if err := validateProfileGrants(request.AllowedProfileGrants); err != nil {
		return err
	}
	if err := validateApplicationScopes(request.AllowedApplicationScopes); err != nil {
		return err
	}
	if !validTenantQuota(request.AggregateQuota) {
		return invalidRequest(errors.New("SecondBox Tenant aggregate quota must be non-negative"))
	}
	if request.ExpiryPolicy.MaximumSubjectLifetimeSeconds < 1 || request.ExpiryPolicy.MaximumSubjectLifetimeSeconds > 31536000 ||
		request.ExpiryPolicy.MaximumAuthorityLifetimeSeconds < 1 || request.ExpiryPolicy.MaximumAuthorityLifetimeSeconds > 31536000 {
		return invalidRequest(errors.New("SecondBox Tenant expiry policy must be between 1 and 31536000 seconds"))
	}
	if request.ExpiresAt != nil && !request.ExpiresAt.After(now) {
		return invalidRequest(errors.New("SecondBox Tenant expiry must be in the future"))
	}
	return nil
}

func validateCreateSubjectRequest(request contracts.CreateSubjectRequest, now time.Time) error {
	if err := validateOwnershipRef("Subject", request.Ref); err != nil {
		return err
	}
	if err := validateManagementMetadata(request.Metadata); err != nil {
		return err
	}
	if !validSubjectQuota(request.Quota) {
		return invalidRequest(errors.New("SecondBox Subject quota must be non-negative"))
	}
	if request.ExpiresAt != nil && !request.ExpiresAt.After(now) {
		return invalidRequest(errors.New("SecondBox Subject expiry must be in the future"))
	}
	return nil
}

func validateCreateApplicationAuthorityRequest(request contracts.CreateApplicationAuthorityRequest, now time.Time) error {
	if err := validateOwnershipRef("Subject", request.SubjectRef); err != nil {
		return err
	}
	if err := validateApplicationScopes(request.Scopes); err != nil {
		return err
	}
	if err := validateProfileGrants(request.ProfileGrants); err != nil {
		return err
	}
	if err := validateManagementMetadata(request.Metadata); err != nil {
		return err
	}
	if !request.ExpiresAt.After(now) {
		return invalidRequest(errors.New("SecondBox ApplicationAuthority expiry must be in the future"))
	}
	return nil
}

func validateOwnershipRef(kind, value string) error {
	if !managementOwnershipRefPattern.MatchString(value) {
		return invalidRequest(fmt.Errorf("SecondBox %s reference must contain 1 to 128 visible ASCII characters", kind))
	}
	return nil
}

func validateManagementMetadata(metadata map[string]string) error {
	if metadata == nil || len(metadata) > 32 {
		return invalidRequest(errors.New("SecondBox management metadata must contain at most 32 entries"))
	}
	for key, value := range metadata {
		if len(key) < 1 || len(key) > 128 || !managementMetadataKeyPattern.MatchString(key) || len(value) > 1024 {
			return invalidRequest(errors.New("SecondBox management metadata key or value is invalid"))
		}
	}
	return nil
}

func validateApplicationScopes(scopes []string) error {
	if len(scopes) < 1 || len(scopes) > 6 {
		return invalidRequest(errors.New("SecondBox application scopes must contain 1 to 6 values"))
	}
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		if !applicationScopeSet[scope] || seen[scope] {
			return invalidRequest(errors.New("SecondBox application scopes contain an invalid or duplicate value"))
		}
		seen[scope] = true
	}
	return nil
}

func validateProfileGrants(grants []string) error {
	if len(grants) < 1 || len(grants) > 32 {
		return invalidRequest(errors.New("SecondBox Profile grants must contain 1 to 32 values"))
	}
	seen := make(map[string]bool, len(grants))
	for _, grant := range grants {
		if !profileNamePattern.MatchString(grant) || seen[grant] {
			return invalidRequest(errors.New("SecondBox Profile grants contain an invalid or duplicate value"))
		}
		seen[grant] = true
	}
	return nil
}

func validSubjectQuota(quota contracts.QuotaLimits) bool {
	return quota.MaxSandboxes >= 0 && quota.MaxActiveInstances >= 0 && quota.MaxCPUMillis >= 0 &&
		quota.MaxMemoryBytes >= 0 && quota.MaxSnapshots >= 0 && quota.MaxPortSessions >= 0 &&
		quota.MaxConcurrentOperations >= 0
}

func validTenantQuota(quota contracts.TenantQuota) bool {
	return validSubjectQuota(contracts.QuotaLimits{
		MaxSandboxes: quota.MaxSandboxes, MaxActiveInstances: quota.MaxActiveInstances,
		MaxCPUMillis: quota.MaxCPUMillis, MaxMemoryBytes: quota.MaxMemoryBytes,
		MaxSnapshots: quota.MaxSnapshots, MaxPortSessions: quota.MaxPortSessions,
		MaxConcurrentOperations: quota.MaxConcurrentOperations,
	}) && quota.MaxActiveSubjects >= 0 && quota.MaxApplicationAuthorities >= 0
}
