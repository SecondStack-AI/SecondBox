package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

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
// an application authority keeps the live data-plane broker.
func TestDirectPortTransportRequiresTheExactGrant(t *testing.T) {
	for name, testCase := range map[string]struct {
		scopes []string
		absent bool
		want   string
	}{
		"no_application_authority": {absent: true, want: contracts.PortTransportProxied},
		"port_scope_only": {
			scopes: []string{applicationScopeSandboxPorts},
			want:   contracts.PortTransportProxied,
		},
		"unrelated_scopes": {
			scopes: []string{applicationScopeSandboxRead, applicationScopeSandboxExec},
			want:   contracts.PortTransportProxied,
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

// TestDirectPortScopeKeepsPortEndpointsOnThePortScope proves the direct grant
// remains additive and does not change which scope Port endpoints require.
func TestDirectPortScopeKeepsPortEndpointsOnThePortScope(t *testing.T) {
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
