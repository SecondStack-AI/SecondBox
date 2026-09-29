package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestGrantExtensionsAreAddOnlyRevisionFencedIdempotentAndCeilingBound(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	t.Cleanup(server.Close)
	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	tenantRef := secondboxclient.OwnershipRef(newFixtureID("grant-extension-tenant"))
	tenant, err := operator.CreateTenant(t.Context(), persistedHTTPTenantRequest(tenantRef), "grant-extension-tenant-create")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(time.Hour)
	controller, err := operator.CreateTenantControllerAuthority(t.Context(), tenant.Ref, secondboxclient.CreateTenantControllerAuthorityRequest{
		ExpiresAt: expiresAt, Metadata: map[string]string{},
	}, "grant-extension-controller-create")
	if err != nil {
		t.Fatal(err)
	}
	controllerClient, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controller.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	subject, err := controllerClient.CreateSubject(t.Context(), secondboxclient.CreateSubjectRequest{
		Ref: "grant-extension-subject", Metadata: map[string]string{}, ExpiresAt: &expiresAt,
		Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 2, MaxActiveInstances: 2, MaxVcpuCount: 2, MaxMemoryBytes: 2 << 30,
			MaxSnapshots: 2, MaxPortSessions: 2, MaxConcurrentOperations: 2,
		},
	}, "grant-extension-subject-create")
	if err != nil {
		t.Fatal(err)
	}
	application, err := controllerClient.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef: subject.Ref, Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, ExpiresAt: expiresAt,
	}, "grant-extension-application-create")
	if err != nil {
		t.Fatal(err)
	}
	authority := application.Authority

	portScope := secondboxclient.ExtendApplicationAuthorityRequest{ProfileGrants: []string{}, Scopes: []string{"sandbox:ports"}}
	if _, err := controllerClient.ExtendApplicationAuthority(
		t.Context(), authority.ID, portScope, authority.Revision, "grant-extension-authority-escalation",
	); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeGrantEscalationDenied {
		t.Fatalf("out-of-ceiling ApplicationAuthority extension error = %v", err)
	}

	ceilingPath := server.URL + "/v1/tenants/" + string(tenant.Ref) + ":extend-ceiling"
	ceilingBody := `{"profileGrants":["agent-compartment"],"applicationScopes":["sandbox:ports"]}`
	for _, rejected := range []struct {
		name, token, ifMatch, body string
		status                     int
		code                       secondboxclient.ProblemCode
	}{
		{"controller authority", controller.BearerToken, revisionETag(tenant.Revision), ceilingBody, http.StatusUnauthorized, secondboxclient.ProblemCodeAuthenticationFailed},
		{"application authority", application.BearerToken, revisionETag(tenant.Revision), ceilingBody, http.StatusUnauthorized, secondboxclient.ProblemCodeAuthenticationFailed},
		{"missing If-Match", testPlatformToken, "", ceilingBody, http.StatusBadRequest, secondboxclient.ProblemCodeInvalidRequest},
		{"stale revision", testPlatformToken, revisionETag(tenant.Revision + 1), ceilingBody, http.StatusPreconditionFailed, secondboxclient.ProblemCodePreconditionFailed},
		{"empty extension", testPlatformToken, revisionETag(tenant.Revision), `{"profileGrants":[],"applicationScopes":[]}`, http.StatusBadRequest, secondboxclient.ProblemCodeInvalidRequest},
		{"missing list", testPlatformToken, revisionETag(tenant.Revision), `{"profileGrants":["agent-compartment"]}`, http.StatusBadRequest, secondboxclient.ProblemCodeInvalidRequest},
		{"null list", testPlatformToken, revisionETag(tenant.Revision), `{"profileGrants":["agent-compartment"],"applicationScopes":null}`, http.StatusBadRequest, secondboxclient.ProblemCodeInvalidRequest},
		{"unknown scope", testPlatformToken, revisionETag(tenant.Revision), `{"profileGrants":[],"applicationScopes":["sandbox:admin"]}`, http.StatusBadRequest, secondboxclient.ProblemCodeInvalidRequest},
		{"duplicate grant", testPlatformToken, revisionETag(tenant.Revision), `{"profileGrants":["coding","coding"],"applicationScopes":[]}`, http.StatusBadRequest, secondboxclient.ProblemCodeInvalidRequest},
		{"unknown field", testPlatformToken, revisionETag(tenant.Revision), `{"profileGrants":[],"applicationScopes":["sandbox:ports"],"removeScopes":["sandbox:read"]}`, http.StatusBadRequest, secondboxclient.ProblemCodeInvalidRequest},
	} {
		t.Run("tenant ceiling rejects "+rejected.name, func(t *testing.T) {
			response := grantExtensionRequest(t, ceilingPath, rejected.token, rejected.ifMatch, "grant-extension-rejected-"+strings.ReplaceAll(rejected.name, " ", "-"), rejected.body)
			defer response.Body.Close()
			var problem secondboxclient.Problem
			if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != rejected.status || problem.Code != rejected.code {
				t.Fatalf("Tenant ceiling extension = %d %#v", response.StatusCode, problem)
			}
		})
	}

	response := grantExtensionRequest(t, ceilingPath, testPlatformToken, revisionETag(tenant.Revision), "grant-extension-ceiling", ceilingBody)
	extendedTenant := decodeGrantExtensionResponse[secondboxclient.Tenant](t, response, "false")
	if extendedTenant.Revision != tenant.Revision+1 || response.Header.Get("ETag") != revisionETag(extendedTenant.Revision) ||
		!reflect.DeepEqual(extendedTenant.AllowedProfileGrants, []string{"agent-compartment", "coding"}) ||
		!reflect.DeepEqual(extendedTenant.AllowedApplicationScopes, []string{"sandbox:lifecycle", "sandbox:ports", "sandbox:read"}) {
		t.Fatalf("extended Tenant = %#v ETag=%q", extendedTenant, response.Header.Get("ETag"))
	}
	response = grantExtensionRequest(t, ceilingPath, testPlatformToken, revisionETag(tenant.Revision), "grant-extension-ceiling", ceilingBody)
	replayedTenant := decodeGrantExtensionResponse[secondboxclient.Tenant](t, response, "true")
	if !reflect.DeepEqual(replayedTenant, extendedTenant) {
		t.Fatalf("Tenant ceiling extension replay = %#v", replayedTenant)
	}
	response = grantExtensionRequest(t, ceilingPath, testPlatformToken, revisionETag(tenant.Revision), "grant-extension-ceiling", `{"profileGrants":["durable-coding"],"applicationScopes":[]}`)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("Tenant ceiling extension key reuse status = %d", response.StatusCode)
	}
	unchangedTenant, err := operator.ExtendTenantCeiling(t.Context(), tenant.Ref, secondboxclient.ExtendTenantCeilingRequest{
		ProfileGrants: []string{"coding"}, ApplicationScopes: []string{"sandbox:read"},
	}, extendedTenant.Revision, "grant-extension-ceiling-no-op")
	if err != nil || unchangedTenant.Revision != extendedTenant.Revision || !unchangedTenant.UpdatedAt.Equal(extendedTenant.UpdatedAt) {
		t.Fatalf("no-op Tenant ceiling extension = %#v error=%v", unchangedTenant, err)
	}
	if _, err := operator.ExtendTenantCeiling(t.Context(), tenant.Ref, secondboxclient.ExtendTenantCeilingRequest{
		ProfileGrants: []string{"coding"},
	}, extendedTenant.Revision, "grant-extension-ceiling-nil"); err == nil || !strings.Contains(err.Error(), "requires non-nil") {
		t.Fatalf("nil Tenant ceiling extension list error = %v", err)
	}

	assertApplicationAuthenticationStatus(t, server.URL+"/v1/sandboxes", application.BearerToken, string(tenant.Ref), string(subject.Ref), http.StatusOK)

	authorityPath := server.URL + "/v1/application-authorities/" + string(authority.ID) + ":extend"
	response = grantExtensionRequest(t, authorityPath, testPlatformToken, revisionETag(authority.Revision), "grant-extension-authority-platform", `{"profileGrants":[],"scopes":["sandbox:ports"]}`)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("platform ApplicationAuthority extension status = %d", response.StatusCode)
	}
	response = grantExtensionRequest(t, authorityPath, controller.BearerToken, revisionETag(authority.Revision), "grant-extension-authority", `{"profileGrants":["agent-compartment"],"scopes":["sandbox:ports"]}`)
	extendedAuthority := decodeGrantExtensionResponse[secondboxclient.ApplicationAuthority](t, response, "false")
	if extendedAuthority.Revision != authority.Revision+1 || extendedAuthority.LookupID != authority.LookupID ||
		!reflect.DeepEqual(extendedAuthority.ProfileGrants, []string{"agent-compartment", "coding"}) ||
		!reflect.DeepEqual(extendedAuthority.Scopes, []string{"sandbox:ports", "sandbox:read"}) {
		t.Fatalf("extended ApplicationAuthority = %#v", extendedAuthority)
	}
	response = grantExtensionRequest(t, authorityPath, controller.BearerToken, revisionETag(authority.Revision), "grant-extension-authority", `{"profileGrants":["agent-compartment"],"scopes":["sandbox:ports"]}`)
	if replayedAuthority := decodeGrantExtensionResponse[secondboxclient.ApplicationAuthority](t, response, "true"); !reflect.DeepEqual(replayedAuthority, extendedAuthority) {
		t.Fatalf("ApplicationAuthority extension replay = %#v", replayedAuthority)
	}
	if _, err := controllerClient.ExtendApplicationAuthority(
		t.Context(), authority.ID, portScope, authority.Revision, "grant-extension-authority-stale",
	); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodePreconditionFailed {
		t.Fatalf("stale ApplicationAuthority extension error = %v", err)
	}
	unchangedAuthority, err := controllerClient.ExtendApplicationAuthority(
		t.Context(), authority.ID, portScope, extendedAuthority.Revision, "grant-extension-authority-no-op",
	)
	if err != nil || unchangedAuthority.Revision != extendedAuthority.Revision {
		t.Fatalf("no-op ApplicationAuthority extension = %#v error=%v", unchangedAuthority, err)
	}
	assertApplicationAuthenticationStatus(t, server.URL+"/v1/sandboxes", application.BearerToken, string(tenant.Ref), string(subject.Ref), http.StatusOK)

	revoked, err := controllerClient.RevokeApplicationAuthority(t.Context(), authority.ID, extendedAuthority.Revision, "grant-extension-authority-revoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controllerClient.ExtendApplicationAuthority(
		t.Context(), authority.ID, secondboxclient.ExtendApplicationAuthorityRequest{ProfileGrants: []string{}, Scopes: []string{"sandbox:lifecycle"}},
		revoked.Revision, "grant-extension-authority-revoked",
	); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeInvalidLifecycleTransition {
		t.Fatalf("revoked ApplicationAuthority extension error = %v", err)
	}

	auditPool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer auditPool.Close()
	for _, expected := range []struct {
		action, outcome string
		count           int
	}{
		{"tenant.ceiling_extended", "accepted", 2},
		{"tenant.ceiling_extended", "denied", 5},
		{"application_authority.extended", "accepted", 2},
		{"application_authority.extended", "denied", 3},
	} {
		var count int
		if err := auditPool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.audit_events
			WHERE tenant_ref=$1 AND action=$2 AND outcome=$3`, tenant.Ref, expected.action, expected.outcome,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != expected.count {
			t.Errorf("%s %s audit events = %d, want %d", expected.action, expected.outcome, count, expected.count)
		}
	}
}

func revisionETag(revision int64) string {
	return fmt.Sprintf(`"revision-%d"`, revision)
}

func grantExtensionRequest(t *testing.T, url, token, ifMatch, idempotencyKey, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeGrantExtensionResponse[T any](t *testing.T, response *http.Response, replayed string) T {
	t.Helper()
	defer response.Body.Close()
	var value T
	if response.StatusCode != http.StatusOK {
		var problem secondboxclient.Problem
		_ = json.NewDecoder(response.Body).Decode(&problem)
		t.Fatalf("grant extension status = %d %#v", response.StatusCode, problem)
	}
	if response.Header.Get("Idempotency-Replayed") != replayed {
		t.Fatalf("grant extension Idempotency-Replayed = %q, want %q", response.Header.Get("Idempotency-Replayed"), replayed)
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
