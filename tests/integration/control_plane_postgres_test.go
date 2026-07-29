package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var integrationDatabaseURL string
var integrationIdentitySequence atomic.Int64

func TestMain(m *testing.M) {
	if os.Getenv(freshRunnerRestoreHelperEnvironment) == "1" {
		integrationDatabaseURL = os.Getenv("SECONDBOX_RESTORE_VERIFICATION_DATABASE_URL")
		if strings.TrimSpace(integrationDatabaseURL) == "" {
			fmt.Fprintln(os.Stderr, "SECONDBOX_RESTORE_VERIFICATION_DATABASE_URL is required for the fresh-Runner restore verifier")
			os.Exit(2)
		}
		os.Exit(m.Run())
	}
	integrationDatabaseURL = os.Getenv("SECONDBOX_TEST_DATABASE_URL")
	if strings.TrimSpace(integrationDatabaseURL) == "" {
		fmt.Fprintln(os.Stderr, "SECONDBOX_TEST_DATABASE_URL is required for PostgreSQL integration tests")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, integrationDatabaseURL)
	if err != nil {
		panic(err)
	}
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS secondbox CASCADE"); err != nil {
		panic(err)
	}
	pool.Close()
	if err := postgresmigrations.Apply(ctx, integrationDatabaseURL); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestConcurrentSandboxCreationIsIdempotentAndPinsProfileRevision(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, contracts.QuotaLimits{
		MaxSandboxes: 20, MaxActiveInstances: 20, MaxCPUMillis: 40000,
		MaxMemoryBytes: 40 << 30, MaxRetainedBytes: 100 << 30,
		MaxSnapshots: 100, MaxArtifacts: 100, MaxPortSessions: 20,
		MaxConcurrentOperations: 20,
	})
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "pinning")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "coding")
	principal := authenticateCredential(t, controlPlane, credential)

	const contenders = 16
	results := make(chan contracts.Sandbox, contenders)
	failures := make(chan error, contenders)
	var waitGroup sync.WaitGroup
	for index := 0; index < contenders; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, "same-request", contracts.CreateSandboxRequest{
				Profile: profile.Name, Metadata: map[string]string{"purpose": "concurrency"},
			})
			if err != nil {
				failures <- err
				return
			}
			results <- sandbox
		}()
	}
	waitGroup.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Errorf("concurrent CreateSandbox failed: %v", err)
	}
	var sandboxID string
	for sandbox := range results {
		if sandboxID == "" {
			sandboxID = sandbox.ID
		}
		if sandbox.ID != sandboxID {
			t.Errorf("concurrent idempotency returned Sandbox %q, want %q", sandbox.ID, sandboxID)
		}
		if sandbox.ProfileRevisionID != profile.CurrentRevision.ID {
			t.Errorf("Sandbox pinned ProfileRevision %q, want %q", sandbox.ProfileRevisionID, profile.CurrentRevision.ID)
		}
	}
	if _, _, err := controlPlane.CreateSandbox(t.Context(), principal, "same-request", contracts.CreateSandboxRequest{
		Profile: profile.Name, Metadata: map[string]string{"purpose": "different-payload"},
	}); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("idempotency payload mismatch error = %v, want ErrIdempotencyConflict", err)
	}

	revised, err := controlPlane.ReviseProfile(t.Context(), admin, profile.Name, contracts.ReviseProfileRequest{
		Spec: testProfileSpec(2000),
	})
	if err != nil {
		t.Fatal(err)
	}
	existing, err := controlPlane.GetSandbox(t.Context(), principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if existing.ProfileRevisionID != profile.CurrentRevision.ID {
		t.Fatalf("existing Sandbox revision changed to %q after profile revision", existing.ProfileRevisionID)
	}
	future, _, err := controlPlane.CreateSandbox(t.Context(), principal, "future-request", contracts.CreateSandboxRequest{
		Profile: profile.Name, Metadata: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if future.TenantRef != project.ID || future.ProfileRevisionID != revised.CurrentRevision.ID {
		t.Fatalf("future Sandbox = project %q revision %q, want %q and %q", future.TenantRef, future.ProfileRevisionID, project.ID, revised.CurrentRevision.ID)
	}
}

func TestProfileIdempotencyReplaysAfterIdentityTablesAreRemoved(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	request := contracts.CreateProfileRequest{
		Name: "profile-replay-without-identity", Spec: testProfileSpec(1000),
	}
	first, replayed, err := controlPlane.CreateProfileIdempotent(
		t.Context(), admin, "profile-replay-without-identity", request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("initial Profile create reported a replay")
	}
	second, replayed, err := controlPlane.CreateProfileIdempotent(
		t.Context(), admin, "profile-replay-without-identity", request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || second.Name != first.Name ||
		second.CurrentRevision.ID != first.CurrentRevision.ID {
		t.Fatalf("Profile replay = %#v replayed=%t, want original %#v", second, replayed, first)
	}
}

func TestSandboxReadsAreScopedToTenantAndSubject(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, ownerAccount, ownerCredential := createProjectAccountAndCredential(
		t, controlPlane, admin, "subject-isolation",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, ownerAccount, "subject-isolation-profile",
	)
	owner := authenticateCredential(t, controlPlane, ownerCredential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(),
		owner,
		"subject-isolation-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	scopes := []string{"sandbox:read", "sandbox:lifecycle"}
	otherAccount, err := createFixtureServiceAccount(t, controlPlane,
		t.Context(),
		admin,
		project.ID,
		fixtureCreateServiceAccountRequest{
			Name: "subject-isolation-other", Scopes: scopes,
			ProfileGrants: []string{profile.Name},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := createFixtureAPIKey(t, controlPlane,
		t.Context(),
		admin,
		project.ID,
		otherAccount.ID,
		fixtureCreateAPIKeyRequest{Name: "subject-isolation-other", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	other := authenticateCredential(t, controlPlane, otherKey.Credential)

	otherSandbox, _, err := controlPlane.CreateSandbox(
		t.Context(),
		other,
		"subject-isolation-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if otherSandbox.ID == sandbox.ID {
		t.Fatalf("cross-subject idempotency replay returned owner Sandbox %q", sandbox.ID)
	}
	if otherSandbox.TenantRef != other.TenantRef || otherSandbox.SubjectRef != other.SubjectRef {
		t.Fatalf("cross-subject idempotency Sandbox ownership = %#v", otherSandbox)
	}

	seedRelayReadyAssignment(
		t, sandbox, time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	)
	sandbox, err = controlPlane.GetSandbox(t.Context(), owner, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), owner, sandbox.ID, sandbox.Generation,
		"subject-isolation-lease", 30,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.RenewSandboxLease(
		t.Context(), other, lease.ID, "subject-isolation-cross-renew", 30,
	); !errors.Is(err, ports.ErrLeaseNotFound) {
		t.Fatalf("cross-subject Lease renewal error = %v, want ErrLeaseNotFound", err)
	}
	if _, err := controlPlane.GetSandbox(
		t.Context(), other, sandbox.ID,
	); !errors.Is(err, ports.ErrSandboxNotFound) {
		t.Fatalf("cross-subject Sandbox read error = %v, want ErrSandboxNotFound", err)
	}
	if _, err := databaseStore.GetSandbox(
		t.Context(), other.TenantRef, other.SubjectRef, sandbox.ID,
	); !errors.Is(err, ports.ErrSandboxNotFound) {
		t.Fatalf("cross-subject store read error = %v, want ErrSandboxNotFound", err)
	}
	ownerView, err := databaseStore.GetSandbox(
		t.Context(), owner.TenantRef, owner.SubjectRef, sandbox.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if ownerView.TenantRef != owner.TenantRef ||
		ownerView.SubjectRef != owner.SubjectRef ||
		ownerView.Workspace.TenantRef != owner.TenantRef ||
		ownerView.Workspace.SubjectRef != owner.SubjectRef {
		t.Fatalf("Sandbox ownership projection = %#v", ownerView)
	}
}

func TestSandboxAdmissionRejectsMissingDisabledAndIncompatibleProfiles(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, _, credential := createProjectAccountAndCredential(t, controlPlane, admin, "admission")
	principal := authenticateCredential(t, controlPlane, credential)

	if _, _, err := controlPlane.CreateSandbox(t.Context(), principal, "absent-profile", contracts.CreateSandboxRequest{
		Profile: "absent-profile", Metadata: map[string]string{},
	}); !errors.Is(err, ports.ErrProfileNotFound) {
		t.Fatalf("absent Profile admission error = %v, want ErrProfileNotFound", err)
	}

	disabled, err := controlPlane.CreateProfile(t.Context(), admin, contracts.CreateProfileRequest{
		Name: "disabled-profile", Spec: testProfileSpec(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.DisableProfile(t.Context(), admin, disabled.Name); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controlPlane.CreateSandbox(t.Context(), principal, "disabled-profile", contracts.CreateSandboxRequest{
		Profile: disabled.Name, Metadata: map[string]string{},
	}); !errors.Is(err, ports.ErrProfileDisabled) {
		t.Fatalf("disabled Profile admission error = %v, want ErrProfileDisabled", err)
	}

	incompatibleSpec := testProfileSpec(1000)
	incompatibleSpec.Pool = "unavailable-pool"
	incompatible, err := controlPlane.CreateProfile(t.Context(), admin, contracts.CreateProfileRequest{
		Name: "incompatible-profile", Spec: incompatibleSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controlPlane.CreateSandbox(t.Context(), principal, "incompatible-profile", contracts.CreateSandboxRequest{
		Profile: incompatible.Name, Metadata: map[string]string{},
	}); !errors.Is(err, ports.ErrRunnerPoolUnavailable) {
		t.Fatalf("incompatible Profile admission error = %v, want ErrRunnerPoolUnavailable", err)
	}
}

func TestConcurrentSubjectQuotaAdmissionNeverOvercommits(t *testing.T) {
	quota := generousQuota()
	quota.MaxSandboxes = 1
	controlPlane, databaseStore := newControlPlaneFixture(t, quota)
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "quota")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "quota-profile")
	principal := authenticateCredential(t, controlPlane, credential)

	start := make(chan struct{})
	results := make(chan error, 2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			<-start
			_, _, err := controlPlane.CreateSandbox(t.Context(), principal, fmt.Sprintf("quota-race-%d", index), contracts.CreateSandboxRequest{
				Profile: profile.Name, Metadata: map[string]string{},
			})
			results <- err
		}(index)
	}
	close(start)
	successes, quotaFailures := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ports.ErrQuotaExceeded):
			quotaFailures++
		default:
			t.Errorf("quota race returned unexpected error: %v", err)
		}
	}
	if successes != 1 || quotaFailures != 1 {
		t.Fatalf("quota race results = %d successes and %d quota failures", successes, quotaFailures)
	}
	usage, err := controlPlane.GetSubjectUsage(t.Context(), principal)
	if err != nil {
		t.Fatal(err)
	}
	if usage.TenantRef != principal.TenantRef || usage.SubjectRef != principal.SubjectRef ||
		usage.Limits.MaxSandboxes != 1 || usage.Usage.Sandboxes != 1 {
		t.Fatalf("subject usage = %#v", usage)
	}
}

func TestPostgresRestartRecoversCredentialIdempotencyAndSandboxState(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "restart")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "restart-profile")
	principal := authenticateCredential(t, controlPlane, credential)
	created, _, err := controlPlane.CreateSandbox(t.Context(), principal, "restart-idempotency", contracts.CreateSandboxRequest{
		Profile: profile.Name, Metadata: map[string]string{"restart": "durable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	databaseStore.Close()

	reopenedStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopenedStore.Close)
	restarted := newControlPlaneService(t, reopenedStore, generousQuota())
	restartedPrincipal := authenticateCredential(t, restarted, credential)
	replayed, createdAgain, err := restarted.CreateSandbox(t.Context(), restartedPrincipal, "restart-idempotency", contracts.CreateSandboxRequest{
		Profile: profile.Name, Metadata: map[string]string{"restart": "durable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if createdAgain || replayed.ID != created.ID {
		t.Fatalf("restart idempotency returned (%q, %t), want (%q, false)", replayed.ID, createdAgain, created.ID)
	}
}

func TestHTTPAuthenticationStrictCreateAndFixedCardinalityMetrics(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "http-profile")
	handler, err := api.NewHandler(api.HandlerConfig{
		Service:                   controlPlane,
		PlatformToken:             testPlatformToken,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	unauthenticated, err := http.Get(server.URL + "/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, unauthenticated, http.StatusUnauthorized)

	wrongToken, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongToken.Header.Set("Authorization", "Bearer wrong-platform-token-at-least-24-bytes")
	wrongToken.Header.Set("X-SecondBox-Tenant-Ref", project.ID)
	wrongToken.Header.Set("X-SecondBox-Subject-Ref", account.ID)
	assertHTTPStatusAndClose(t, doHTTP(t, wrongToken), http.StatusUnauthorized)

	missingRefs, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	missingRefs.Header.Set("Authorization", "Bearer "+testPlatformToken)
	assertHTTPStatusAndClose(t, doHTTP(t, missingRefs), http.StatusBadRequest)

	malformedRefs, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	malformedRefs.Header.Set("Authorization", "Bearer "+testPlatformToken)
	malformedRefs.Header.Set("X-SecondBox-Tenant-Ref", "malformed tenant")
	malformedRefs.Header.Set("X-SecondBox-Subject-Ref", account.ID)
	assertHTTPStatusAndClose(t, doHTTP(t, malformedRefs), http.StatusBadRequest)

	response := authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "http-create", map[string]any{
		"profile": profile.Name, "metadata": map[string]string{"project-name": project.Name},
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/sandboxes status = %d body=%s", response.StatusCode, readResponse(t, response))
	}
	if requestID := response.Header.Get("X-Request-ID"); requestID == "" {
		t.Fatal("POST /v1/sandboxes response has no X-Request-ID")
	}
	response.Body.Close()

	rejected := authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "http-reject", map[string]any{
		"profile": profile.Name, "metadata": map[string]string{}, "memoryBytes": 1024,
	})
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("strict create status = %d body=%s", rejected.StatusCode, readResponse(t, rejected))
	}
	rejected.Body.Close()

	metricsResponse, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody := readResponse(t, metricsResponse)
	metricsResponse.Body.Close()
	for _, forbidden := range []string{project.Name, profile.Name, account.ID, "project=", "sandbox_id=", "api_key=", "backend=", "workspace_path="} {
		if strings.Contains(metricsBody, forbidden) {
			t.Errorf("metrics contain forbidden high-cardinality value %q: %s", forbidden, metricsBody)
		}
	}
}

func authenticatedJSONRequest(t *testing.T, method, url, credential, idempotencyKey string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
