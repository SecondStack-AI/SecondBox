package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
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
	applicationScopeSandboxArtifacts = "sandbox:artifacts"
	applicationScopeSandboxPorts     = "sandbox:ports"
)

var applicationProfileNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,79}$`)

// ApplicationAuthority binds one bearer credential to a fixed SecondBox ownership and capability scope.
type ApplicationAuthority struct {
	ID            string
	Token         string
	TenantRef     string
	SubjectRef    string
	Scopes        []string
	ProfileGrants []string
}

type applicationAuthorityContextKey struct{}

type resolvedApplicationAuthority struct {
	id            string
	tokenHash     [sha256.Size]byte
	tenantRef     string
	subjectRef    string
	scopes        []string
	profileGrants []string
}

func resolveApplicationAuthorities(
	authorities []ApplicationAuthority,
	platformTokenHash [sha256.Size]byte,
) ([]resolvedApplicationAuthority, error) {
	resolved := make([]resolvedApplicationAuthority, 0, len(authorities))
	identifiers := map[string]struct{}{}
	tokenHashes := map[[sha256.Size]byte]struct{}{platformTokenHash: {}}
	for _, authority := range authorities {
		if !ownershipRefPattern.MatchString(authority.ID) ||
			!ownershipRefPattern.MatchString(authority.TenantRef) ||
			!ownershipRefPattern.MatchString(authority.SubjectRef) {
			return nil, errors.New("SecondBox application authority identity and ownership references must contain 1 to 128 visible ASCII characters")
		}
		if len(authority.Token) < 24 {
			return nil, errors.New("SecondBox application authority token must contain at least 24 bytes")
		}
		if len(authority.Scopes) == 0 || len(authority.Scopes) > 6 {
			return nil, errors.New("SecondBox application authority requires between 1 and 6 scopes")
		}
		if len(authority.ProfileGrants) == 0 || len(authority.ProfileGrants) > 32 {
			return nil, errors.New("SecondBox application authority requires between 1 and 32 Profile grants")
		}
		if _, duplicate := identifiers[authority.ID]; duplicate {
			return nil, errors.New("SecondBox application authority identifiers must be unique")
		}
		identifiers[authority.ID] = struct{}{}
		tokenHash := sha256.Sum256([]byte(authority.Token))
		if _, duplicate := tokenHashes[tokenHash]; duplicate {
			return nil, errors.New("SecondBox application authority tokens must be unique and differ from the platform token")
		}
		tokenHashes[tokenHash] = struct{}{}
		scopes := append([]string(nil), authority.Scopes...)
		slices.Sort(scopes)
		if slices.ContainsFunc(scopes, func(scope string) bool {
			return !isApplicationScope(scope)
		}) || hasAdjacentDuplicate(scopes) {
			return nil, errors.New("SecondBox application authority contains an invalid or duplicate scope")
		}
		profileGrants := append([]string(nil), authority.ProfileGrants...)
		slices.Sort(profileGrants)
		if slices.ContainsFunc(profileGrants, func(profile string) bool {
			return !applicationProfileNamePattern.MatchString(profile)
		}) || hasAdjacentDuplicate(profileGrants) {
			return nil, errors.New("SecondBox application authority contains an invalid or duplicate Profile grant")
		}
		resolved = append(resolved, resolvedApplicationAuthority{
			id: authority.ID, tokenHash: tokenHash,
			tenantRef: authority.TenantRef, subjectRef: authority.SubjectRef,
			scopes: scopes, profileGrants: profileGrants,
		})
	}
	return resolved, nil
}

func authenticateApplicationAuthority(
	authorities []resolvedApplicationAuthority,
	presentedHash [sha256.Size]byte,
) (resolvedApplicationAuthority, bool) {
	for _, authority := range authorities {
		if subtle.ConstantTimeCompare(presentedHash[:], authority.tokenHash[:]) == 1 {
			return authority, true
		}
	}
	return resolvedApplicationAuthority{}, false
}

func authorizeApplicationRequest(
	authority resolvedApplicationAuthority,
	request *http.Request,
) error {
	if request.Header.Get("X-SecondBox-Tenant-Ref") != authority.tenantRef ||
		request.Header.Get("X-SecondBox-Subject-Ref") != authority.subjectRef {
		return ports.ErrAuthorizationDenied
	}
	requiredScope, administrative := applicationRequestScope(request.Method, request.Pattern)
	if administrative || requiredScope == "" || !slices.Contains(authority.scopes, requiredScope) {
		return ports.ErrAuthorizationDenied
	}
	if request.Pattern == "GET /v1/profiles/{profileName}" &&
		!slices.Contains(authority.profileGrants, request.PathValue("profileName")) {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func authorizeApplicationProfile(request *http.Request, profile string) error {
	authority, ok := request.Context().Value(applicationAuthorityContextKey{}).(resolvedApplicationAuthority)
	if !ok {
		return nil
	}
	if !slices.Contains(authority.profileGrants, profile) {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func applicationPrincipal(authority resolvedApplicationAuthority) contracts.Principal {
	return contracts.Principal{
		Kind: "service_account", ID: authority.id,
		TenantRef: authority.tenantRef, SubjectRef: authority.subjectRef,
	}
}

func applicationRequestScope(method string, pattern string) (string, bool) {
	switch {
	case pattern == "GET /v1/profiles/{profileName}":
		return applicationScopeSandboxRead, false
	case strings.HasPrefix(pattern, "GET /v1/profiles"),
		strings.HasPrefix(pattern, "POST /v1/profiles"),
		strings.HasPrefix(pattern, "GET /v1/runner"),
		strings.HasPrefix(pattern, "POST /v1/runner"),
		strings.HasPrefix(pattern, "PATCH /v1/runner"),
		pattern == "GET /v1/timings":
		return "", true
	case pattern == "GET /v1/sandboxes",
		pattern == "GET /v1/sandboxes/{sandboxID}",
		pattern == "GET /v1/sandboxes/{sandboxID}/timings",
		pattern == "GET /v1/leases/{leaseID}",
		pattern == "GET /v1/operations/{operationID}",
		pattern == "GET /v1/operations/{operationID}/timings":
		return applicationScopeSandboxRead, false
	case pattern == "POST /v1/sandboxes",
		pattern == "PUT /v1/sandboxes/{sandboxID}/metadata",
		pattern == "DELETE /v1/sandboxes/{sandboxID}",
		pattern == "POST /v1/sandboxes/{sandboxAction}",
		pattern == "POST /v1/sandboxes/{sandboxID}/leases",
		pattern == "DELETE /v1/leases/{leaseID}",
		pattern == "POST /v1/leases/{leaseAction}",
		strings.Contains(pattern, "/snapshots"):
		return applicationScopeSandboxLifecycle, false
	case strings.Contains(pattern, "/exec"),
		strings.Contains(pattern, "/terminals"):
		return applicationScopeSandboxExec, false
	case strings.Contains(pattern, "/files"),
		strings.Contains(pattern, "/directories"):
		return applicationScopeSandboxFiles, false
	case strings.Contains(pattern, "/artifacts") || strings.HasPrefix(pattern, "GET /v1/artifacts") ||
		strings.HasPrefix(pattern, "DELETE /v1/artifacts"):
		return applicationScopeSandboxArtifacts, false
	case strings.Contains(pattern, "/port-sessions"):
		return applicationScopeSandboxPorts, false
	default:
		_ = method
		return "", false
	}
}

func isApplicationScope(scope string) bool {
	return scope == applicationScopeSandboxRead ||
		scope == applicationScopeSandboxLifecycle ||
		scope == applicationScopeSandboxExec ||
		scope == applicationScopeSandboxFiles ||
		scope == applicationScopeSandboxArtifacts ||
		scope == applicationScopeSandboxPorts
}

func hasAdjacentDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}
