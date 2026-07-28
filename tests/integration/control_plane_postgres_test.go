package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/SecondStack-AI/SecondBox/internal/service"
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
	admin := controlPlane.BootstrapAdmin()
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
	if future.ProjectID != project.ID || future.ProfileRevisionID != revised.CurrentRevision.ID {
		t.Fatalf("future Sandbox = project %q revision %q, want %q and %q", future.ProjectID, future.ProfileRevisionID, project.ID, revised.CurrentRevision.ID)
	}
}

func TestBootstrapAdminIsOneDurableKeyedOperatorAuthority(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	principal, err := controlPlane.AuthenticateCredential(t.Context(), "bootstrap-administrator-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !principal.BootstrapAdmin || principal.ID != "bootstrap_operator" {
		t.Fatalf("bootstrap Principal = %#v", principal)
	}
	mismatched, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store:                 databaseStore,
		BootstrapAdminToken:   "different-bootstrap-administrator-secret",
		APIKeyHashSecret:      []byte("test-keyed-api-hash-secret-at-least-32-bytes"),
		DefaultProjectQuota:   generousQuota(),
		DefaultProfileQuota:   generousQuota(),
		Now:                   func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) },
		NewID:                 func(prefix string) string { return fmt.Sprintf("%s_%d", prefix, integrationIdentitySequence.Add(1)) },
		NewCredentialMaterial: func() string { return fmt.Sprintf("credential-material-%032d", integrationIdentitySequence.Add(1)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mismatched.InitializeBootstrapAdmin(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "does not match durable operator authority") {
		t.Fatalf("mismatched bootstrap restart error = %v", err)
	}
}

func TestAPIKeyRotationRevocationIsolationAndAudit(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	projectA, accountA, credentialA := createProjectAccountAndCredential(t, controlPlane, admin, "alpha")
	_, _, credentialB := createProjectAccountAndCredential(t, controlPlane, admin, "bravo")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, accountA, "isolated")
	principalA := authenticateCredential(t, controlPlane, credentialA)
	sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principalA, "alpha-sandbox", contracts.CreateSandboxRequest{
		Profile: profile.Name, Metadata: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	principalB := authenticateCredential(t, controlPlane, credentialB)
	if _, err := controlPlane.GetSandbox(t.Context(), principalB, sandbox.ID); !errors.Is(err, ports.ErrSandboxNotFound) {
		t.Fatalf("cross-project GetSandbox error = %v, want ErrSandboxNotFound", err)
	}

	keys, err := controlPlane.ListAPIKeys(t.Context(), admin, projectA.ID, accountA.ID)
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("API key last-use evidence = %#v, %v", keys, err)
	}
	rotated, err := controlPlane.RotateAPIKey(t.Context(), admin, projectA.ID, accountA.ID, keys[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Credential == credentialA {
		t.Fatal("API key rotation returned the previous plaintext")
	}
	if _, err := controlPlane.AuthenticateCredential(t.Context(), credentialA); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("old rotated credential error = %v, want ErrAuthenticationFailed", err)
	}
	rotatedPrincipal := authenticateCredential(t, controlPlane, rotated.Credential)
	if _, err := controlPlane.GetSandbox(t.Context(), rotatedPrincipal, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.RevokeAPIKey(t.Context(), admin, projectA.ID, accountA.ID, rotated.APIKey.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.AuthenticateCredential(t.Context(), rotated.Credential); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("revoked credential error = %v, want ErrAuthenticationFailed", err)
	}
	auditEvents, err := databaseStore.ListAuditEvents(t.Context(), projectA.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	encodedAudit, _ := json.Marshal(auditEvents)
	if !bytes.Contains(encodedAudit, []byte("api_key.rotated")) || !bytes.Contains(encodedAudit, []byte("api_key.revoked")) {
		t.Fatalf("audit does not contain rotation and revocation evidence: %s", encodedAudit)
	}
	if bytes.Contains(encodedAudit, []byte(credentialA)) || bytes.Contains(encodedAudit, []byte(rotated.Credential)) {
		t.Fatal("audit stored plaintext API credential material")
	}
	hashPool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hashPool.Close)
	var persistedHash []byte
	if err := hashPool.QueryRow(t.Context(), `SELECT credential_hash FROM secondbox.api_keys WHERE id=$1`, rotated.APIKey.ID).Scan(&persistedHash); err != nil {
		t.Fatal(err)
	}
	if len(persistedHash) != sha256.Size || bytes.Contains(persistedHash, []byte(rotated.Credential)) {
		t.Fatalf("persisted API credential evidence is not one keyed SHA-256 hash: %x", persistedHash)
	}
}

func TestSandboxAdmissionRejectsMissingUnauthorizedDisabledAndIncompatibleProfiles(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "admission")
	principal := authenticateCredential(t, controlPlane, credential)

	if _, _, err := controlPlane.CreateSandbox(t.Context(), principal, "missing-profile", contracts.CreateSandboxRequest{
		Profile: "not-granted", Metadata: map[string]string{},
	}); !errors.Is(err, ports.ErrProfileNotGranted) {
		t.Fatalf("ungranted Profile admission error = %v, want ErrProfileNotGranted", err)
	}

	grants := append(account.ProfileGrants, "absent-profile", "disabled-profile", "incompatible-profile")
	if _, err := controlPlane.UpdateServiceAccount(t.Context(), admin, account.ProjectID, account.ID, contracts.UpdateServiceAccountRequest{
		ProfileGrants: &grants,
	}); err != nil {
		t.Fatal(err)
	}
	principal = authenticateCredential(t, controlPlane, credential)
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

func TestConcurrentProjectQuotaAdmissionNeverOvercommits(t *testing.T) {
	quota := generousQuota()
	quota.MaxSandboxes = 1
	controlPlane, databaseStore := newControlPlaneFixture(t, quota)
	admin := controlPlane.BootstrapAdmin()
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
}

func TestPostgresRestartRecoversCredentialIdempotencyAndSandboxState(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
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
	admin := controlPlane.BootstrapAdmin()
	project, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "http-profile")
	handler, err := api.NewHandler(api.HandlerConfig{
		Service:                   controlPlane,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

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

func newControlPlaneFixture(t *testing.T, projectQuota contracts.QuotaLimits) (*service.ControlPlaneService, *store.PostgresControlPlaneStore) {
	t.Helper()
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	return newControlPlaneService(t, databaseStore, projectQuota), databaseStore
}

func newControlPlaneService(t *testing.T, databaseStore *store.PostgresControlPlaneStore, projectQuota contracts.QuotaLimits) *service.ControlPlaneService {
	t.Helper()
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store:                 databaseStore,
		BootstrapAdminToken:   "bootstrap-administrator-secret",
		APIKeyHashSecret:      []byte("test-keyed-api-hash-secret-at-least-32-bytes"),
		DefaultProjectQuota:   projectQuota,
		DefaultProfileQuota:   generousQuota(),
		Now:                   func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) },
		NewID:                 func(prefix string) string { return fmt.Sprintf("%s_%d", prefix, integrationIdentitySequence.Add(1)) },
		NewCredentialMaterial: func() string { return fmt.Sprintf("credential-material-%032d", integrationIdentitySequence.Add(1)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controlPlane.InitializeBootstrapAdmin(t.Context()); err != nil {
		t.Fatal(err)
	}
	return controlPlane
}

func createProjectAccountAndCredential(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	admin contracts.Principal,
	suffix string,
) (contracts.Project, contracts.ServiceAccount, string) {
	t.Helper()
	project, err := controlPlane.CreateProject(t.Context(), admin, contracts.CreateProjectRequest{Name: "project-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	account, err := controlPlane.CreateServiceAccount(t.Context(), admin, project.ID, contracts.CreateServiceAccountRequest{
		Name: "service-" + suffix,
		Scopes: []string{
			contracts.ScopeSandboxLifecycle,
			contracts.ScopeSandboxRead,
		},
		ProfileGrants: []string{"profile-" + suffix, "coding", "isolated", "quota-profile", "restart-profile", "http-profile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	createdKey, err := controlPlane.CreateAPIKey(t.Context(), admin, project.ID, account.ID, contracts.CreateAPIKeyRequest{
		Name: "key-" + suffix,
		Scopes: []string{
			contracts.ScopeSandboxLifecycle,
			contracts.ScopeSandboxRead,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return project, account, createdKey.Credential
}

func createGrantedProfile(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	databaseStore *store.PostgresControlPlaneStore,
	admin contracts.Principal,
	account contracts.ServiceAccount,
	name string,
) contracts.Profile {
	t.Helper()
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: "default-pool", State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"}, Capabilities: []string{"firecracker", "checkpoint"},
		CapacityPolicy: map[string]int64{"maxInstances": 100}, ReadyRunnerCount: 1,
		Revision: 1, CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	profile, err := controlPlane.CreateProfile(t.Context(), admin, contracts.CreateProfileRequest{
		Name: name, Spec: testProfileSpec(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	grants := append([]string{}, account.ProfileGrants...)
	if !containsString(grants, name) {
		grants = append(grants, name)
	}
	if _, err := controlPlane.UpdateServiceAccount(t.Context(), admin, account.ProjectID, account.ID, contracts.UpdateServiceAccountRequest{
		ProfileGrants: &grants,
	}); err != nil {
		t.Fatal(err)
	}
	return profile
}

func testProfileSpec(cpuMillis int64) contracts.ProfileRevisionSpec {
	return contracts.ProfileRevisionSpec{
		Backend: "firecracker", Pool: "default-pool", Architecture: "amd64",
		RuntimeBundleDigest:   "sha256:" + strings.Repeat("a", 64),
		ToolchainBundleDigest: "sha256:" + strings.Repeat("b", 64),
		Resources: contracts.ResourcePolicy{
			CPUMillis: cpuMillis, MemoryBytes: 1 << 30, WorkspaceBytes: 8 << 30,
			ProcessLimit: 128, ConcurrentOperations: 4,
		},
		Lifecycle: contracts.LifecyclePolicy{
			InitialState: "stopped", DrainGraceSeconds: 30, IdleSeconds: 300,
			MaximumDurationSeconds: 3600, LeaseSeconds: 60,
		},
		Checkpoint: contracts.CheckpointPolicy{
			OnStop: true, RetentionSeconds: 86400,
			SnapshotLimit: 8, ArtifactRetentionSeconds: 86400,
		},
		Execution: contracts.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 60000, MaximumBufferedOutputBytes: 1 << 20,
			StreamWindowBytes: 65536, MaximumTransferBytes: 1 << 30,
			TerminalDetachSeconds: 30,
		},
		Network: contracts.NetworkPolicy{Mode: "deny_all", Destinations: []contracts.NetworkDestination{}},
		Ports: []contracts.PortPolicy{{
			Name: "web", Port: 8080, Protocol: "tcp", MaximumSessions: 2, MaximumSessionSeconds: 300,
		}},
	}
}

func generousQuota() contracts.QuotaLimits {
	return contracts.QuotaLimits{
		MaxSandboxes: 100, MaxActiveInstances: 100, MaxCPUMillis: 100000,
		MaxMemoryBytes: 100 << 30, MaxRetainedBytes: 1 << 40,
		MaxSnapshots: 1000, MaxArtifacts: 1000, MaxPortSessions: 100,
		MaxConcurrentOperations: 100,
	}
}

func authenticateCredential(t *testing.T, controlPlane *service.ControlPlaneService, credential string) contracts.Principal {
	t.Helper()
	principal, err := controlPlane.AuthenticateCredential(t.Context(), credential)
	if err != nil {
		t.Fatal(err)
	}
	return principal
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
	request.Header.Set("Authorization", "Bearer "+credential)
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
