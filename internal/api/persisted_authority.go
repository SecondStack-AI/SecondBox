package api

import (
	"context"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// PersistedAuthorityAuthenticator verifies durable authorities without exposing verifier material.
type PersistedAuthorityAuthenticator interface {
	AuthenticateTenantControllerAuthority(context.Context, string, time.Time) (contracts.Principal, error)
	AuthenticateApplicationAuthority(context.Context, string, time.Time) (ports.AuthenticatedApplicationAuthority, error)
}

func isTenantControllerBearerToken(credential string) bool {
	return strings.HasPrefix(credential, ports.TenantControllerBearerTokenPrefix)
}

func isApplicationBearerToken(credential string) bool {
	return strings.HasPrefix(credential, ports.ApplicationBearerTokenPrefix)
}

func resolvedPersistedApplicationAuthority(
	authority ports.AuthenticatedApplicationAuthority,
) resolvedApplicationAuthority {
	return resolvedApplicationAuthority{
		id: authority.ID, tenantRef: authority.TenantRef, subjectRef: authority.SubjectRef,
		scopes: authority.Scopes, profileGrants: authority.ProfileGrants,
	}
}
