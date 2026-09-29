package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// A Profile grant is a capability boundary, not only a creation filter. A
// Sandbox that the platform placed in an application's subject on a Profile the
// application was not granted stays visible and manageable to it, but its data
// plane is refused.
func TestApplicationDataPlaneRequiresTheSandboxProfileGrant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: "default-pool", State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"},
		ReadyRunnerCount: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	sequence := integrationIdentitySequence.Add(1)
	restricted := fmt.Sprintf("grant-restricted-%d", sequence)
	seedFixtureHomeRunner(t, "default-pool", "runner-"+restricted)
	if _, _, err := controlPlane.CreateProfileIdempotent(
		t.Context(), fixtureAdmin(t, controlPlane), "create-"+restricted,
		contracts.CreateProfileRequest{Name: restricted, Spec: testProfileSpec(1)},
	); err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	tenantRef := secondboxclient.OwnershipRef(fmt.Sprintf("grant-tenant-%d", sequence))
	tenant := persistedHTTPTenantRequest(tenantRef)
	tenant.AllowedProfileGrants = []string{"coding", restricted}
	tenant.AllowedApplicationScopes = []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports"}
	if _, err := operator.CreateTenant(t.Context(), tenant, "grant-tenant"); err != nil {
		t.Fatal(err)
	}
	controllerCredential, err := operator.CreateTenantControllerAuthority(t.Context(), tenantRef, secondboxclient.CreateTenantControllerAuthorityRequest{
		ExpiresAt: now.Add(45 * time.Minute), Metadata: map[string]string{},
	}, "grant-controller")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controllerCredential.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.CreateSubject(t.Context(), secondboxclient.CreateSubjectRequest{
		Ref: "shared-subject", Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 10, MaxActiveInstances: 10, MaxVcpuCount: 10, MaxMemoryBytes: 10 << 30,
			MaxSnapshots: 10, MaxPortSessions: 10, MaxConcurrentOperations: 10,
		}, Metadata: map[string]string{},
	}, "grant-subject"); err != nil {
		t.Fatal(err)
	}
	application := func(grant string) string {
		t.Helper()
		credential, err := controller.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{
			SubjectRef:    "shared-subject",
			Scopes:        []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports"},
			ProfileGrants: []string{grant}, Metadata: map[string]string{}, ExpiresAt: now.Add(30 * time.Minute),
		}, "grant-application-"+grant)
		if err != nil {
			t.Fatal(err)
		}
		return credential.BearerToken
	}
	ungranted, granted := application("coding"), application(restricted)

	created := applicationRequest(t, http.MethodPost, server.URL+"/v1/sandboxes",
		testPlatformToken, string(tenantRef), "shared-subject", "grant-platform-sandbox",
		map[string]any{"profile": restricted, "metadata": map[string]string{}},
	)
	if created.StatusCode != http.StatusAccepted && created.StatusCode != http.StatusCreated {
		t.Fatalf("platform Sandbox creation status = %d", created.StatusCode)
	}
	var operation struct {
		SandboxID string `json:"sandboxId"`
	}
	if err := json.NewDecoder(created.Body).Decode(&operation); err != nil || operation.SandboxID == "" {
		t.Fatalf("platform Sandbox creation = %#v, %v", operation, err)
	}
	created.Body.Close()
	sandboxURL := server.URL + "/v1/sandboxes/" + operation.SandboxID

	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/exec", map[string]any{"argv": []string{"true"}}},
		{http.MethodGet, "/files?path=README", nil},
		{http.MethodGet, "/directories?path=.", nil},
		{http.MethodPost, "/port-sessions", map[string]any{"name": "web", "durationSeconds": 30}},
	} {
		response := applicationRequest(t, request.method, sandboxURL+request.path,
			ungranted, string(tenantRef), "shared-subject", "grant-denied-"+request.path, request.body)
		assertHTTPStatusAndClose(t, response, http.StatusForbidden)
		response = applicationRequest(t, request.method, sandboxURL+request.path,
			granted, string(tenantRef), "shared-subject", "grant-allowed-"+request.path, request.body)
		if response.StatusCode == http.StatusForbidden {
			t.Fatalf("%s %s with the Profile grant was refused", request.method, request.path)
		}
		response.Body.Close()
	}
	// Reads and lifecycle remain subject-scoped.
	assertHTTPStatusAndClose(t, applicationRequest(t, http.MethodGet, sandboxURL,
		ungranted, string(tenantRef), "shared-subject", "", nil), http.StatusOK)
}
