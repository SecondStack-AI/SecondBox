package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"slices"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
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

// TestDirectPortTransportRequiresTheExactGrant proves the direct transport is
// denied by default: it is never implied by sandbox:ports, and a request without
// an application authority keeps the durable relay.
func TestDirectPortTransportRequiresTheExactGrant(t *testing.T) {
	for name, testCase := range map[string]struct {
		scopes []string
		absent bool
		want   string
	}{
		"no_application_authority": {absent: true, want: contracts.PortTransportRelay},
		"port_scope_only": {
			scopes: []string{applicationScopeSandboxPorts},
			want:   contracts.PortTransportRelay,
		},
		"unrelated_scopes": {
			scopes: []string{applicationScopeSandboxRead, applicationScopeSandboxExec},
			want:   contracts.PortTransportRelay,
		},
		"exact_direct_grant": {
			scopes: []string{applicationScopeSandboxPorts, applicationScopeSandboxPortsDirect},
			want:   contracts.PortTransportDirect,
		},
	} {
		t.Run(name, func(t *testing.T) {
			request, err := http.NewRequest(
				http.MethodPost, "/v1/sandboxes/sandbox-1/port-sessions", nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !testCase.absent {
				request = request.WithContext(context.WithValue(
					request.Context(),
					applicationAuthorityContextKey{},
					resolvedApplicationAuthority{
						id: "ingress", tenantRef: "secondstack", subjectRef: "ingress",
						scopes: testCase.scopes, profileGrants: []string{"secondstack-agent"},
					},
				))
			}
			if got := portTransportForRequest(request); got != testCase.want {
				t.Fatalf("Port transport = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestDirectPortScopeIsAcceptedAndPortEndpointsStayOnThePortScope keeps the
// direct grant additive: it is a valid configured scope, and it does not change
// which scope the Port endpoints themselves require.
func TestDirectPortScopeIsAcceptedAndPortEndpointsStayOnThePortScope(t *testing.T) {
	resolved, err := resolveApplicationAuthorities(
		[]ApplicationAuthority{{
			ID: "ingress", Token: "ingress-token-00000000000000000000",
			TenantRef: "secondstack", SubjectRef: "ingress",
			Scopes: []string{
				applicationScopeSandboxPorts, applicationScopeSandboxPortsDirect,
			},
			ProfileGrants: []string{"secondstack-agent"},
		}},
		sha256.Sum256([]byte("platform-token-000000000000000000")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 ||
		!slices.Contains(resolved[0].scopes, applicationScopeSandboxPortsDirect) {
		t.Fatalf("resolved direct-grant authority = %+v", resolved)
	}
	scope, platformOnly := applicationRequestScope(
		http.MethodPost, "POST /v1/sandboxes/{sandboxID}/port-sessions",
	)
	if platformOnly || scope != applicationScopeSandboxPorts {
		t.Fatalf(
			"PortSession creation scope = (%q, %t), want (%q, false)",
			scope, platformOnly, applicationScopeSandboxPorts,
		)
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
