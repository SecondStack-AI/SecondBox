package config

import (
	"strings"
	"testing"
)

func TestRequiredApplicationAuthoritiesParsesExplicitAuthorities(t *testing.T) {
	t.Setenv("SECONDBOX_APPLICATION_AUTHORITIES_JSON", `[
		{
			"id": "agent-service",
			"token": "agent-service-token-000000000000",
			"tenantRef": "secondstack",
			"subjectRef": "agent-service",
			"scopes": ["sandbox:read", "sandbox:lifecycle"],
			"profileGrants": ["secondstack-agent"]
		}
	]`)

	authorities, err := requiredApplicationAuthorities()
	if err != nil {
		t.Fatalf("parse application authorities: %v", err)
	}
	if len(authorities) != 1 {
		t.Fatalf("authority count = %d, want 1", len(authorities))
	}
	authority := authorities[0]
	if authority.ID != "agent-service" || authority.TenantRef != "secondstack" ||
		authority.SubjectRef != "agent-service" {
		t.Fatalf("unexpected authority identity: %#v", authority)
	}
	if len(authority.Scopes) != 2 || authority.ProfileGrants[0] != "secondstack-agent" {
		t.Fatalf("unexpected authority grants: %#v", authority)
	}
}

func TestRequiredApplicationAuthoritiesAcceptsExplicitEmptyList(t *testing.T) {
	t.Setenv("SECONDBOX_APPLICATION_AUTHORITIES_JSON", `[]`)

	authorities, err := requiredApplicationAuthorities()
	if err != nil {
		t.Fatalf("parse explicit empty application authorities: %v", err)
	}
	if authorities == nil || len(authorities) != 0 {
		t.Fatalf("authorities = %#v, want explicit empty list", authorities)
	}
}

func TestRequiredApplicationAuthoritiesRejectsUnknownFields(t *testing.T) {
	t.Setenv("SECONDBOX_APPLICATION_AUTHORITIES_JSON", `[
		{
			"id": "agent-service",
			"token": "agent-service-token-000000000000",
			"tenantRef": "secondstack",
			"subjectRef": "agent-service",
			"scopes": ["sandbox:read"],
			"profileGrants": ["secondstack-agent"],
			"unbounded": true
		}
	]`)

	_, err := requiredApplicationAuthorities()
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown-field rejection", err)
	}
}

func TestRequiredApplicationAuthoritiesRejectsTrailingJSON(t *testing.T) {
	t.Setenv("SECONDBOX_APPLICATION_AUTHORITIES_JSON", `[] {}`)

	_, err := requiredApplicationAuthorities()
	if err == nil || !strings.Contains(err.Error(), "single JSON array") {
		t.Fatalf("error = %v, want trailing-JSON rejection", err)
	}
}
