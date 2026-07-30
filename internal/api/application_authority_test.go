package api

import (
	"crypto/sha256"
	"net/http"
	"testing"
)

func TestResolveApplicationAuthoritiesRejectsCredentialAndGrantAmbiguity(t *testing.T) {
	platformToken := "platform-token-000000000000000000"
	valid := ApplicationAuthority{
		ID: "agent-service", Token: "agent-service-token-000000000000",
		TenantRef: "secondstack", SubjectRef: "agent-service",
		Scopes: []string{applicationScopeSandboxRead}, ProfileGrants: []string{"secondstack-agent"},
	}
	tests := map[string][]ApplicationAuthority{
		"platform token reuse": {{
			ID: "agent-service", Token: platformToken,
			TenantRef: "secondstack", SubjectRef: "agent-service",
			Scopes: []string{applicationScopeSandboxRead}, ProfileGrants: []string{"secondstack-agent"},
		}},
		"duplicate application token": {
			valid,
			{
				ID: "agent-runtime", Token: valid.Token,
				TenantRef: "secondstack", SubjectRef: "agent-runtime",
				Scopes: []string{applicationScopeSandboxRead}, ProfileGrants: []string{"secondstack-runtime"},
			},
		},
		"unknown scope": {{
			ID: "agent-service", Token: valid.Token,
			TenantRef: "secondstack", SubjectRef: "agent-service",
			Scopes: []string{"sandbox:administrator"}, ProfileGrants: []string{"secondstack-agent"},
		}},
		"duplicate profile grant": {{
			ID: "agent-service", Token: valid.Token,
			TenantRef: "secondstack", SubjectRef: "agent-service",
			Scopes:        []string{applicationScopeSandboxRead},
			ProfileGrants: []string{"secondstack-agent", "secondstack-agent"},
		}},
	}

	for name, authorities := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveApplicationAuthorities(authorities, sha256.Sum256([]byte(platformToken))); err == nil {
				t.Fatal("resolve authorities succeeded, want rejection")
			}
		})
	}
}

func TestAuthorizeApplicationRequestRequiresExactScope(t *testing.T) {
	authority := resolvedApplicationAuthority{
		id: "agent-service", tenantRef: "secondstack", subjectRef: "agent-service",
		scopes: []string{applicationScopeSandboxRead}, profileGrants: []string{"secondstack-agent"},
	}

	request, err := http.NewRequest(http.MethodPut, "/v1/sandboxes/sandbox-1/files", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Pattern = "PUT /v1/sandboxes/{sandboxID}/files"
	request.Header.Set("X-SecondBox-Tenant-Ref", authority.tenantRef)
	request.Header.Set("X-SecondBox-Subject-Ref", authority.subjectRef)

	if err := authorizeApplicationRequest(authority, request); err == nil {
		t.Fatal("authorize files request succeeded without sandbox:files scope")
	}
}

func TestSandboxMetadataUpdateRequiresLifecycleScope(t *testing.T) {
	scope, platformOnly := applicationRequestScope(
		http.MethodPut,
		"PUT /v1/sandboxes/{sandboxID}/metadata",
	)
	if platformOnly || scope != applicationScopeSandboxLifecycle {
		t.Fatalf(
			"Sandbox metadata update scope = (%q, %t), want (%q, false)",
			scope,
			platformOnly,
			applicationScopeSandboxLifecycle,
		)
	}
}
