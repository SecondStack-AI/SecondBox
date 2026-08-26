package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestPersistedAuthorityHTTPRestartRevocationExpiryRotationAndTenantIsolation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	sequence := integrationIdentitySequence.Add(1)
	firstTenant := secondboxclient.OwnershipRef(fmt.Sprintf("persisted-http-a-%d", sequence))
	secondTenant := secondboxclient.OwnershipRef(fmt.Sprintf("persisted-http-b-%d", sequence))
	if _, err := api.NewHandler(api.HandlerConfig{
		Service:                   controlPlane,
		PlatformToken:             ports.ApplicationBearerTokenPrefix + "reserved-platform-token-material-00000000000000000000",
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	}); err == nil {
		t.Fatal("platform token with a persisted application prefix was accepted")
	}

	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	controller, firstApplication := bootstrapPersistedHTTPAuthority(
		t, operator, server.URL, server.Client(), firstTenant, "sandbox:lifecycle", now, "a",
	)
	secondController, secondApplication := bootstrapPersistedHTTPAuthority(
		t, operator, server.URL, server.Client(), secondTenant, "", now, "b",
	)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		firstApplication.BearerToken, string(firstTenant), "same-local-subject", "", nil,
	), http.StatusOK)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		secondApplication.BearerToken, string(secondTenant), "same-local-subject", "", nil,
	), http.StatusOK)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		firstApplication.BearerToken, string(secondTenant), "same-local-subject", "", nil,
	), http.StatusForbidden)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodPost, server.URL+"/v1/sandboxes",
		secondApplication.BearerToken, string(secondTenant), "same-local-subject", "persisted-scope-denied",
		map[string]any{"profile": "coding", "metadata": map[string]string{}},
	), http.StatusForbidden)
	assertCrossAuthorityDenials(
		t, server.URL, firstApplication.BearerToken, controller.BearerToken,
		string(firstTenant), string(secondTenant), "same-local-subject",
	)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		"retired-static-token-material-000000", string(firstTenant), "same-local-subject", "", nil,
	), http.StatusUnauthorized)
	assertHTTPStatusAndClose(t, bearerRequest(
		t, http.MethodGet, server.URL+"/v1/subjects", controller.BearerToken,
	), http.StatusOK)
	controllerAssertion := newBearerHTTPRequest(
		t, http.MethodGet, server.URL+"/v1/subjects", controller.BearerToken,
	)
	controllerAssertion.Header.Set("X-SecondBox-Tenant-Ref", string(secondTenant))
	assertHTTPStatusAndClose(t, doHTTP(t, controllerAssertion), http.StatusForbidden)
	server.Close()
	databaseStore.Close()

	restartedStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedStore.Close)
	restartedControlPlane := newManagementControlPlane(t, restartedStore, now)
	restartedServer := contractServer(t, persistedHTTPHandler(t, restartedControlPlane, restartedStore))
	t.Cleanup(restartedServer.Close)
	restartedController, err := secondboxclient.NewSecondBoxTenantControllerClient(restartedServer.URL, controller.BearerToken, restartedServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		firstApplication.BearerToken, string(firstTenant), "same-local-subject", "", nil,
	), http.StatusOK)

	rotated, err := restartedController.RotateApplicationAuthority(t.Context(), firstApplication.Authority.ID, firstApplication.Authority.Revision, "persisted-http-rotate")
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		firstApplication.BearerToken, string(firstTenant), "same-local-subject", "", nil,
	), http.StatusUnauthorized)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		rotated.BearerToken, string(firstTenant), "same-local-subject", "", nil,
	), http.StatusOK)
	if _, err := restartedController.RevokeApplicationAuthority(t.Context(), firstApplication.Authority.ID, rotated.Authority.Revision, "persisted-http-revoke"); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		rotated.BearerToken, string(firstTenant), "same-local-subject", "", nil,
	), http.StatusUnauthorized)

	expired, err := restartedController.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef: "same-local-subject", Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{"case": "expired"}, ExpiresAt: now.Add(30 * time.Minute),
	}, "persisted-http-expired-create")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.application_authorities SET expires_at=$2 WHERE id=$1`, expired.Authority.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		expired.BearerToken, string(firstTenant), "same-local-subject", "", nil,
	), http.StatusUnauthorized)

	invalid, err := restartedController.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef: "same-local-subject", Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{"case": "invalid-verifier"}, ExpiresAt: now.Add(30 * time.Minute),
	}, "persisted-http-invalid-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.application_authorities
		SET token_verifier_sha256='\\x01'::bytea WHERE id=$1`, invalid.Authority.ID,
	); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		invalid.BearerToken, string(firstTenant), "same-local-subject", "", nil,
	), http.StatusUnauthorized)

	secondControllerClient, err := secondboxclient.NewSecondBoxTenantControllerClient(restartedServer.URL, secondController.BearerToken, restartedServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondControllerClient.GetApplicationAuthority(t.Context(), firstApplication.Authority.ID); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeNotFound {
		t.Fatalf("cross-tenant application read = %v", err)
	}
	if _, err := secondControllerClient.GetApplicationAuthority(t.Context(), "application-http-absent"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeNotFound {
		t.Fatalf("absent application read = %v", err)
	}
}

// assertCrossAuthorityDenials keeps the complete fixed authority hierarchy in
// one greppable integration matrix. The routes intentionally cross each public
// administrative boundary before proving reference binding on ordinary routes.
func assertCrossAuthorityDenials(
	t *testing.T,
	baseURL string,
	applicationToken string,
	controllerToken string,
	tenantRef string,
	otherTenantRef string,
	subjectRef string,
) {
	t.Helper()
	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		tenantRef  string
		subjectRef string
		body       any
		want       int
	}{
		{name: "application_cannot_call_platform_management", method: http.MethodGet, path: "/v1/tenants", token: applicationToken, want: http.StatusUnauthorized},
		{name: "application_cannot_call_tenant_management", method: http.MethodGet, path: "/v1/subjects", token: applicationToken, want: http.StatusUnauthorized},
		{name: "application_cannot_mutate_profiles", method: http.MethodPost, path: "/v1/profiles", token: applicationToken, tenantRef: tenantRef, subjectRef: subjectRef, body: map[string]any{}, want: http.StatusForbidden},
		{name: "application_cannot_read_aggregate_timing", method: http.MethodGet, path: "/v1/timings?windowSeconds=60", token: applicationToken, tenantRef: tenantRef, subjectRef: subjectRef, want: http.StatusForbidden},
		{name: "application_cannot_administer_runner_pools", method: http.MethodGet, path: "/v1/runner-pools", token: applicationToken, tenantRef: tenantRef, subjectRef: subjectRef, want: http.StatusForbidden},
		{name: "application_cannot_administer_runners", method: http.MethodGet, path: "/v1/runners", token: applicationToken, tenantRef: tenantRef, subjectRef: subjectRef, want: http.StatusForbidden},
		{name: "application_cannot_assert_another_tenant", method: http.MethodGet, path: "/v1/sandboxes", token: applicationToken, tenantRef: otherTenantRef, subjectRef: subjectRef, want: http.StatusForbidden},
		{name: "application_cannot_assert_another_subject", method: http.MethodGet, path: "/v1/sandboxes", token: applicationToken, tenantRef: tenantRef, subjectRef: "another-subject", want: http.StatusForbidden},
		{name: "controller_cannot_call_sandbox_routes", method: http.MethodGet, path: "/v1/sandboxes", token: controllerToken, tenantRef: tenantRef, subjectRef: subjectRef, want: http.StatusUnauthorized},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.tenantRef == "" {
				assertHTTPStatusAndClose(t, bearerRequest(
					t, testCase.method, baseURL+testCase.path, testCase.token,
				), testCase.want)
				return
			}
			assertHTTPStatusAndClose(t, applicationRequest(
				t, testCase.method, baseURL+testCase.path, testCase.token,
				testCase.tenantRef, testCase.subjectRef,
				"cross-authority-"+testCase.name, testCase.body,
			), testCase.want)
		})
	}
}

func bootstrapPersistedHTTPAuthority(
	t *testing.T,
	operator *secondboxclient.Client,
	baseURL string,
	httpClient *http.Client,
	tenantRef secondboxclient.OwnershipRef,
	extraScope string,
	now time.Time,
	suffix string,
) (secondboxclient.TenantControllerCredentialResponse, secondboxclient.ApplicationCredentialResponse) {
	t.Helper()
	scopes := []string{"sandbox:read"}
	if extraScope != "" {
		scopes = append(scopes, extraScope)
	}
	if _, err := operator.CreateTenant(t.Context(), persistedHTTPTenantRequest(tenantRef), "persisted-http-tenant-"+suffix); err != nil {
		t.Fatal(err)
	}
	controller, err := operator.CreateTenantControllerAuthority(t.Context(), tenantRef, secondboxclient.CreateTenantControllerAuthorityRequest{
		ExpiresAt: now.Add(45 * time.Minute), Metadata: map[string]string{"case": suffix},
	}, "persisted-http-controller-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	controllerClient, err := secondboxclient.NewSecondBoxTenantControllerClient(baseURL, controller.BearerToken, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controllerClient.CreateSubject(t.Context(), secondboxclient.CreateSubjectRequest{
		Ref: "same-local-subject", Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 10, MaxActiveInstances: 10, MaxVcpuCount: 10, MaxMemoryBytes: 10 << 30,
			MaxSnapshots: 10, MaxPortSessions: 10, MaxConcurrentOperations: 10,
		}, Metadata: map[string]string{"case": suffix},
	}, "persisted-http-subject-"+suffix); err != nil {
		t.Fatal(err)
	}
	application, err := controllerClient.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef: "same-local-subject", Scopes: scopes, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{"case": suffix}, ExpiresAt: now.Add(30 * time.Minute),
	}, "persisted-http-application-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	return controller, application
}

func persistedHTTPTenantRequest(ref secondboxclient.OwnershipRef) secondboxclient.CreateTenantRequest {
	return secondboxclient.CreateTenantRequest{
		Ref: ref, AllowedProfileGrants: []string{"coding"},
		AllowedApplicationScopes: []string{"sandbox:read", "sandbox:lifecycle"},
		AggregateQuota: secondboxclient.TenantQuota{
			MaxSandboxes: 10, MaxActiveInstances: 10, MaxVcpuCount: 10,
			MaxMemoryBytes: 10 << 30, MaxSnapshots: 10, MaxPortSessions: 10,
			MaxConcurrentOperations: 10, MaxActiveSubjects: 10, MaxApplicationAuthorities: 10,
		},
		ExpiryPolicy: secondboxclient.TenantExpiryPolicy{
			MaximumSubjectLifetimeSeconds: 3600, MaximumAuthorityLifetimeSeconds: 3600,
		}, Metadata: map[string]string{},
	}
}

func persistedHTTPHandler(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	authorities api.PersistedAuthorityAuthenticator,
) http.Handler {
	t.Helper()
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken,
		PersistedAuthorities:      authorities,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func bearerRequest(t *testing.T, method string, url string, bearerToken string) *http.Response {
	t.Helper()
	return doHTTP(t, newBearerHTTPRequest(t, method, url, bearerToken))
}

func newBearerHTTPRequest(t *testing.T, method string, url string, bearerToken string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearerToken)
	return request
}

func applicationRequest(
	t *testing.T,
	method string,
	url string,
	token string,
	tenantRef string,
	subjectRef string,
	idempotencyKey string,
	body any,
) *http.Response {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-SecondBox-Tenant-Ref", tenantRef)
	request.Header.Set("X-SecondBox-Subject-Ref", subjectRef)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
