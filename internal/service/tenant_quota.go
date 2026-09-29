package service

import (
	"context"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (service *ControlPlaneService) UpdateTenantQuota(ctx context.Context, principal contracts.Principal, tenantRef, idempotencyKey string, expectedRevision int64, request contracts.UpdateTenantQuotaRequest) (contracts.Tenant, bool, error) {
	if principal.Kind != contracts.AuthorityKindPlatform {
		return contracts.Tenant{}, false, ports.ErrAuthorizationDenied
	}
	if err := validateOwnershipRef("tenantRef", tenantRef); err != nil {
		return contracts.Tenant{}, false, err
	}
	if expectedRevision < 1 {
		return contracts.Tenant{}, false, invalidField("If-Match", "must contain a positive revision ETag")
	}
	if !validTenantQuota(request.AggregateQuota) {
		return contracts.Tenant{}, false, invalidField("aggregateQuota", "must have nonnegative or null limits")
	}
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(principal, "tenant.quota.update", tenantRef, idempotencyKey, struct {
		ExpectedRevision int64                 `json:"expectedRevision"`
		AggregateQuota   contracts.TenantQuota `json:"aggregateQuota"`
	}{expectedRevision, request.AggregateQuota}, now)
	if err != nil {
		return contracts.Tenant{}, false, err
	}
	idempotency.AuditEvent = auditEventPointer(service.newAudit(ctx, principal, "tenant.quota_updated", "tenant", tenantRef, tenantRef, now))
	tenant, result, err := service.store.UpdateManagedTenantQuota(ctx, tenantRef, request.AggregateQuota, expectedRevision, now, idempotency)
	if err != nil {
		return contracts.Tenant{}, false, service.managementDenied(ctx, principal, "tenant.quota_updated", "tenant", tenantRef, tenantRef, err)
	}
	return tenant, result.Replayed, nil
}
