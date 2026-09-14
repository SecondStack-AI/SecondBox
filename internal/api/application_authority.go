package api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const (
	applicationScopeSandboxRead      = "sandbox:read"
	applicationScopeSandboxLifecycle = "sandbox:lifecycle"
	applicationScopeSandboxExec      = "sandbox:exec"
	applicationScopeSandboxFiles     = "sandbox:files"
	applicationScopeSandboxPorts     = "sandbox:ports"
	// applicationScopeSandboxPortsDirect grants the direct Port transport. It is
	// never an implied consequence of sandbox:ports: only an authority holding
	// this exact scope ever learns a Runner data-plane address.
	applicationScopeSandboxPortsDirect = "sandbox:ports:direct"
)

type applicationAuthorityContextKey struct{}

func authorizeApplicationRequest(
	authority ports.AuthenticatedApplicationAuthority,
	request *http.Request,
) error {
	if request.Header.Get("X-SecondBox-Tenant-Ref") != authority.TenantRef ||
		request.Header.Get("X-SecondBox-Subject-Ref") != authority.SubjectRef {
		return ports.ErrAuthorizationDenied
	}
	requiredScope := applicationRequestScope(request.Pattern)
	if requiredScope == "" || !slices.Contains(authority.Scopes, requiredScope) {
		return ports.ErrAuthorizationDenied
	}
	if request.Pattern == "GET /v1/profiles/{profileName}" &&
		!slices.Contains(authority.ProfileGrants, request.PathValue("profileName")) {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func authorizeApplicationProfile(request *http.Request, profile string) error {
	authority, ok := request.Context().Value(applicationAuthorityContextKey{}).(ports.AuthenticatedApplicationAuthority)
	if !ok {
		return nil
	}
	if !slices.Contains(authority.ProfileGrants, profile) {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func applicationPrincipal(authority ports.AuthenticatedApplicationAuthority) contracts.Principal {
	return contracts.Principal{
		Kind: "service_account", ID: authority.ID,
		TenantRef: authority.TenantRef, SubjectRef: authority.SubjectRef,
	}
}

func applicationRequestScope(pattern string) string {
	switch {
	case pattern == "GET /v1/profiles/{profileName}":
		return applicationScopeSandboxRead
	case strings.HasPrefix(pattern, "GET /v1/profiles"),
		strings.HasPrefix(pattern, "POST /v1/profiles"),
		strings.HasPrefix(pattern, "GET /v1/runner"),
		strings.HasPrefix(pattern, "POST /v1/runner"),
		strings.HasPrefix(pattern, "PATCH /v1/runner"),
		pattern == "GET /v1/timings":
		return ""
	case pattern == "GET /v1/sandboxes",
		pattern == "GET /v1/subject-usage",
		pattern == "GET /v1/subject-policy",
		pattern == "GET /v1/sandboxes/{sandboxID}",
		pattern == "GET /v1/sandboxes/{sandboxID}/timings",
		pattern == "GET /v1/leases/{leaseID}",
		pattern == "GET /v1/operations/{operationID}",
		pattern == "GET /v1/operations/{operationID}/timings":
		return applicationScopeSandboxRead
	case pattern == "POST /v1/sandboxes",
		pattern == "PUT /v1/sandboxes/{sandboxID}/metadata",
		pattern == "DELETE /v1/sandboxes/{sandboxID}",
		pattern == "POST /v1/sandboxes/{sandboxAction}",
		pattern == "POST /v1/sandboxes/{sandboxID}/leases",
		pattern == "DELETE /v1/leases/{leaseID}",
		pattern == "POST /v1/leases/{leaseAction}",
		strings.Contains(pattern, "/snapshots"):
		return applicationScopeSandboxLifecycle
	case strings.Contains(pattern, "/exec"),
		strings.Contains(pattern, "/terminals"):
		return applicationScopeSandboxExec
	case strings.Contains(pattern, "/files"),
		strings.Contains(pattern, "/directories"):
		return applicationScopeSandboxFiles
	case strings.Contains(pattern, "/port-sessions"):
		return applicationScopeSandboxPorts
	default:
		return ""
	}
}

// portTransportForRequest resolves which Port transport this caller may receive.
// The direct transport is denied by default: a request carrying no application
// authority, or an authority without the exact scope, receives the proxied
// endpoint and never observes a Runner address.
func portTransportForRequest(request *http.Request) string {
	authority, ok := request.Context().Value(applicationAuthorityContextKey{}).(ports.AuthenticatedApplicationAuthority)
	if !ok {
		return contracts.PortTransportProxied
	}
	if !slices.Contains(authority.Scopes, applicationScopeSandboxPortsDirect) {
		return contracts.PortTransportProxied
	}
	return contracts.PortTransportDirect
}
