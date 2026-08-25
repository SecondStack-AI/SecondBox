package integration_test

import (
	"errors"
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
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestPersistedAuthorityHTTPRestartRevocationExpiryRotationAndTenantIsolation(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	now := time.Now().UTC().Truncate(time.Second)
	sequence := integrationIdentitySequence.Add(1)
	firstTenant := persistedHTTPTenant(fmt.Sprintf("persisted-http-a-%d", sequence), now)
	secondTenant := persistedHTTPTenant(fmt.Sprintf("persisted-http-b-%d", sequence), now)
	for _, tenant := range []contracts.Tenant{firstTenant, secondTenant} {
		if _, err := databaseStore.CreateTenant(t.Context(), tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := databaseStore.CreateSubject(t.Context(), contracts.Subject{
			TenantRef: tenant.Ref, Ref: "same-local-subject",
			State: contracts.SubjectStateActive, CleanupState: contracts.SubjectCleanupStateNone,
			Quota: generousQuota(), Metadata: map[string]string{}, Revision: 1,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	controller, err := databaseStore.CreateTenantControllerAuthority(t.Context(), contracts.TenantControllerAuthority{
		ID: fmt.Sprintf("controller-http-%d", sequence), TenantRef: firstTenant.Ref,
		State: contracts.AuthorityStateActive, Metadata: map[string]string{}, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewHandler(api.HandlerConfig{
		Service:                   controlPlane,
		PlatformToken:             ports.ApplicationBearerTokenPrefix + "reserved-platform-token-material-00000000000000000000",
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	}); err == nil {
		t.Fatal("platform token with a persisted application prefix was accepted")
	}
	firstApplication, err := databaseStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID:        fmt.Sprintf("application-http-a-%d", sequence),
		TenantRef: firstTenant.Ref, SubjectRef: "same-local-subject",
		State:  contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read", "sandbox:lifecycle"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondApplication, err := databaseStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID:        fmt.Sprintf("application-http-b-%d", sequence),
		TenantRef: secondTenant.Ref, SubjectRef: "same-local-subject",
		State:  contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		firstApplication.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusOK)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		secondApplication.BearerToken, secondTenant.Ref, "same-local-subject", "", nil,
	), http.StatusOK)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		firstApplication.BearerToken, secondTenant.Ref, "same-local-subject", "", nil,
	), http.StatusForbidden)
	assertHTTPStatusAndClose(t, bearerRequest(
		t, http.MethodGet, server.URL+"/v1/subjects", controller.BearerToken,
	), http.StatusOK)
	controllerAssertion := newBearerHTTPRequest(
		t, http.MethodGet, server.URL+"/v1/subjects", controller.BearerToken,
	)
	controllerAssertion.Header.Set("X-SecondBox-Tenant-Ref", secondTenant.Ref)
	assertHTTPStatusAndClose(t, doHTTP(t, controllerAssertion), http.StatusForbidden)
	assertHTTPStatusAndClose(t, bearerRequest(
		t, http.MethodGet, server.URL+"/v1/subjects", firstApplication.BearerToken,
	), http.StatusUnauthorized)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		controller.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusUnauthorized)
	server.Close()
	databaseStore.Close()

	restartedStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedStore.Close)
	restartedControlPlane := newControlPlaneService(t, restartedStore, generousQuota())
	restartedServer := contractServer(t, persistedHTTPHandler(t, restartedControlPlane, restartedStore))
	t.Cleanup(restartedServer.Close)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		firstApplication.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusOK)

	rotated, err := restartedStore.RotateApplicationAuthority(
		t.Context(), firstTenant.Ref, firstApplication.Authority.ID, 1, now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		firstApplication.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusUnauthorized)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		rotated.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusOK)
	if _, err := restartedStore.RevokeApplicationAuthority(
		t.Context(), firstTenant.Ref, firstApplication.Authority.ID, 2, now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		rotated.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusUnauthorized)

	expiredAt := now.Add(-time.Minute)
	expired, err := restartedStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID:        fmt.Sprintf("application-http-expired-%d", sequence),
		TenantRef: firstTenant.Ref, SubjectRef: "same-local-subject",
		State:  contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, ExpiresAt: &expiredAt, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		expired.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusUnauthorized)

	invalid, err := restartedStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID:        fmt.Sprintf("application-http-invalid-%d", sequence),
		TenantRef: firstTenant.Ref, SubjectRef: "same-local-subject",
		State:  contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.application_authorities
		SET token_verifier_sha256='\\x01'::bytea WHERE id=$1`, invalid.Authority.ID,
	); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, restartedServer.URL+"/v1/sandboxes",
		invalid.BearerToken, firstTenant.Ref, "same-local-subject", "", nil,
	), http.StatusUnauthorized)

	if _, err := restartedStore.GetApplicationAuthority(
		t.Context(), secondTenant.Ref, firstApplication.Authority.ID,
	); !errors.Is(err, ports.ErrManagementNotFound) {
		t.Fatalf("cross-tenant application read = %v", err)
	}
	if _, err := restartedStore.GetApplicationAuthority(
		t.Context(), secondTenant.Ref, "application-http-absent",
	); !errors.Is(err, ports.ErrManagementNotFound) {
		t.Fatalf("absent application read = %v", err)
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

func persistedHTTPTenant(ref string, now time.Time) contracts.Tenant {
	return contracts.Tenant{
		Ref: ref, State: contracts.TenantStateActive,
		AllowedProfileGrants:     []string{"coding"},
		AllowedApplicationScopes: []string{"sandbox:read", "sandbox:lifecycle"},
		AggregateQuota: contracts.TenantQuota{
			MaxSandboxes: 10, MaxActiveInstances: 10, MaxCPUMillis: 10000,
			MaxMemoryBytes: 10 << 30, MaxSnapshots: 10, MaxPortSessions: 10,
			MaxConcurrentOperations: 10, MaxActiveSubjects: 10, MaxApplicationAuthorities: 10,
		},
		ExpiryPolicy: contracts.TenantExpiryPolicy{
			MaximumSubjectLifetimeSeconds: 3600, MaximumAuthorityLifetimeSeconds: 3600,
		},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
