package service

import (
	"context"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestNewAuditResolvesOwnershipAttribution(t *testing.T) {
	service := &ControlPlaneService{newID: func(string) string { return "audit-id" }}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		principal   contracts.Principal
		projectID   string
		wantTenant  string
		wantSubject string
	}{
		"admin": {
			principal:   contracts.Principal{Kind: "platform", ID: "operator-admin"},
			wantTenant:  "secondbox",
			wantSubject: "operator-admin",
		},
		"subject": {
			principal: contracts.Principal{
				Kind: "service_account", ID: "application-agent",
				TenantRef: "asserted-tenant", SubjectRef: "asserted-subject",
			},
			projectID:   "project-fallback",
			wantTenant:  "asserted-tenant",
			wantSubject: "asserted-subject",
		},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			event := service.newAudit(
				context.Background(), testCase.principal, "resource.created", "resource",
				"resource-id", testCase.projectID, now,
			)
			if event.TenantRef != testCase.wantTenant || event.SubjectRef != testCase.wantSubject {
				t.Fatalf(
					"audit attribution = %q/%q, want %q/%q",
					event.TenantRef, event.SubjectRef, testCase.wantTenant, testCase.wantSubject,
				)
			}
		})
	}
}
