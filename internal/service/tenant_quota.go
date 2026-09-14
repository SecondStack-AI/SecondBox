package service

import (
	"context"
	"errors"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (service *ControlPlaneService) UpdateTenantQuota(ctx context.Context, principal contracts.Principal, tenantRef, idempotencyKey string, expectedRevision int64, request contracts.UpdateTenantQuotaRequest) (contracts.Tenant, bool, error) {
	if principal.Kind != contracts.AuthorityKindPlatform {
		return contracts.Tenant{}, false, ports.ErrAuthorizationDenied
	}
	if err := validateOwnershipRef("Tenant", tenantRef); err != nil {
		return contracts.Tenant{}, false, err
	}
	if expectedRevision < 1 || !validTenantQuota(request.AggregateQuota) {
		return contracts.Tenant{}, false, invalidRequest(errors.New("SecondBox Tenant quota must be non-negative and revision positive"))
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
