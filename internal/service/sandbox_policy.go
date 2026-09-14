package service

import (
	"context"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (service *ControlPlaneService) GetSubjectSandboxPolicy(ctx context.Context, principal contracts.Principal, subjectRef, profile string) (contracts.SubjectSandboxPolicyObservation, error) {
	if principal.Kind != contracts.AuthorityKindTenantController {
		return contracts.SubjectSandboxPolicyObservation{}, ports.ErrAuthorizationDenied
	}
	if err := validateOwnershipRef("Subject", subjectRef); err != nil {
		return contracts.SubjectSandboxPolicyObservation{}, err
	}
	return service.store.GetSubjectSandboxPolicy(ctx, principal.TenantRef, subjectRef, profile, service.now().UTC())
}

func (service *ControlPlaneService) UpdateSubjectSandboxPolicy(ctx context.Context, principal contracts.Principal, subjectRef, key string, revision int64, request contracts.SubjectSandboxPolicy) (contracts.SubjectSandboxPolicyObservation, bool, error) {
	if principal.Kind != contracts.AuthorityKindTenantController {
		return contracts.SubjectSandboxPolicyObservation{}, false, ports.ErrAuthorizationDenied
	}
	if err := validateOwnershipRef("Subject", subjectRef); err != nil {
		return contracts.SubjectSandboxPolicyObservation{}, false, err
	}
	if !profileNamePattern.MatchString(request.Profile) || revision < 1 {
		return contracts.SubjectSandboxPolicyObservation{}, false, ports.ErrInvalidRequest
	}
	if err := request.Lifecycle.Validate(); err != nil {
		return contracts.SubjectSandboxPolicyObservation{}, false, invalidRequest(err)
	}
	now := service.now().UTC()
	input, err := service.adminIdempotency(principal, "subject.sandbox_policy.update", subjectRef, key, struct {
		Revision int64
		Policy   contracts.SubjectSandboxPolicy
	}{revision, request}, now)
	if err != nil {
		return contracts.SubjectSandboxPolicyObservation{}, false, err
	}
	input.AuditEvent = auditEventPointer(service.newAudit(ctx, principal, "subject.sandbox_policy_updated", "subject", subjectRef, principal.TenantRef, now))
	result, receipt, err := service.store.UpdateSubjectSandboxPolicy(ctx, principal.TenantRef, subjectRef, request, revision, now, input)
	return result, receipt.Replayed, err
}

func (service *ControlPlaneService) GetApplicationSandboxPolicy(ctx context.Context, principal contracts.Principal, profile string) (contracts.SubjectSandboxPolicyObservation, error) {
	return service.store.GetSubjectSandboxPolicy(ctx, principal.TenantRef, principal.SubjectRef, profile, service.now().UTC())
}
