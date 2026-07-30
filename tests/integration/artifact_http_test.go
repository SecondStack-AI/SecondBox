package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublicArtifactsPublishListDownloadAndEndRetention(t *testing.T) {
	objects, s3Server := newArtifactObjectServer(t)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	immutableObjects, err := objectstore.NewS3Store(t.Context(), objectstore.S3Config{
		Endpoint: s3Server.URL, Region: "us-east-1", Bucket: "secondbox",
		AccessKeyID: "artifact-test-access", SecretAccessKey: "artifact-test-secret",
		UsePathStyle: true, RetryMaxAttempts: 1, HTTPTimeout: time.Second,
		TempDirectory: t.TempDir(), MaxObjectBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	controlPlane := newArtifactControlPlane(
		t, databaseStore, immutableObjects, generousQuota(),
		func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) },
	)
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "artifact-http")
	cleanupArtifactRows(t, project.ID)
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-artifact-http")
	scopes := []string{
		"sandbox:read", "sandbox:lifecycle", "sandbox:artifacts",
	}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "artifact-http", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "artifact-http-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	seedRelayReadyAssignment(t, sandbox, time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC))
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "artifact-http-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	firstContent := []byte{0, 1, 0xff, 'a'}
	firstResponse := uploadArtifact(
		t, server.URL, key.Credential, sandbox, lease.ID, "artifact-first-key",
		"first.bin", "application/octet-stream", map[string]string{"purpose": "result"}, firstContent,
	)
	assertHTTPStatus(t, firstResponse, http.StatusCreated)
	var first contracts.Artifact
	decodeHTTPJSON(t, firstResponse, &first)
	firstHash := sha256.Sum256(firstContent)
	if first.ID == "" || first.SandboxID != sandbox.ID ||
		first.SourceGeneration != sandbox.Generation ||
		first.Name != "first.bin" || first.MediaType != "application/octet-stream" ||
		first.SizeBytes != int64(len(firstContent)) ||
		first.SHA256 != hex.EncodeToString(firstHash[:]) ||
		first.State != "" || first.TenantRef != "" ||
		!first.RetainUntil.Equal(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("published Artifact = %#v", first)
	}
	if objects.puts != 1 {
		t.Fatalf("immutable uploads = %d, want 1", objects.puts)
	}

	replay := uploadArtifact(
		t, server.URL, key.Credential, sandbox, lease.ID, "artifact-first-key",
		"first.bin", "application/octet-stream", map[string]string{"purpose": "result"}, firstContent,
	)
	assertHTTPStatus(t, replay, http.StatusCreated)
	var replayed contracts.Artifact
	decodeHTTPJSON(t, replay, &replayed)
	if replayed.ID != first.ID || objects.puts != 1 {
		t.Fatalf("replayed Artifact = %#v, immutable uploads=%d", replayed, objects.puts)
	}
	conflict := uploadArtifact(
		t, server.URL, key.Credential, sandbox, lease.ID, "artifact-first-key",
		"different.bin", "application/octet-stream", map[string]string{"purpose": "result"}, firstContent,
	)
	assertHTTPStatus(t, conflict, http.StatusConflict)
	conflict.Body.Close()

	secondContent := []byte("second")
	secondResponse := uploadArtifact(
		t, server.URL, key.Credential, sandbox, lease.ID, "artifact-second-key",
		"second.txt", "text/plain", map[string]string{}, secondContent,
	)
	assertHTTPStatus(t, secondResponse, http.StatusCreated)
	var second contracts.Artifact
	decodeHTTPJSON(t, secondResponse, &second)

	pageResponse := artifactGET(
		t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/artifacts?limit=1",
		key.Credential,
	)
	assertHTTPStatus(t, pageResponse, http.StatusOK)
	var firstPage contracts.ArtifactPage
	decodeHTTPJSON(t, pageResponse, &firstPage)
	if len(firstPage.Items) != 1 || firstPage.Items[0].ID != second.ID || firstPage.NextCursor == nil {
		t.Fatalf("first Artifact page = %#v", firstPage)
	}
	nextPageResponse := artifactGET(
		t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/artifacts?limit=1&cursor="+*firstPage.NextCursor,
		key.Credential,
	)
	assertHTTPStatus(t, nextPageResponse, http.StatusOK)
	var nextPage contracts.ArtifactPage
	decodeHTTPJSON(t, nextPageResponse, &nextPage)
	if len(nextPage.Items) != 1 || nextPage.Items[0].ID != first.ID || nextPage.NextCursor != nil {
		t.Fatalf("next Artifact page = %#v", nextPage)
	}

	metadataResponse := artifactGET(t, server.URL+"/v1/artifacts/"+first.ID, key.Credential)
	assertHTTPStatus(t, metadataResponse, http.StatusOK)
	var metadata contracts.Artifact
	decodeHTTPJSON(t, metadataResponse, &metadata)
	if metadata.ID != first.ID {
		t.Fatalf("Artifact metadata = %#v", metadata)
	}
	otherSubject, err := createFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID,
		fixtureCreateServiceAccountRequest{
			Name: "artifact-http-other-subject", Scopes: scopes,
			ProfileGrants: []string{profile.Name},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	otherSubjectKey, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, otherSubject.ID,
		fixtureCreateAPIKeyRequest{
			Name: "artifact-http-other-subject", Scopes: scopes,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	crossSubject := artifactGET(
		t, server.URL+"/v1/artifacts/"+first.ID, otherSubjectKey.Credential,
	)
	assertHTTPStatus(t, crossSubject, http.StatusNotFound)
	crossSubject.Body.Close()
	downloadResponse := artifactGET(t, server.URL+"/v1/artifacts/"+first.ID+"/content", key.Credential)
	assertHTTPStatus(t, downloadResponse, http.StatusOK)
	downloaded, err := io.ReadAll(downloadResponse.Body)
	downloadResponse.Body.Close()
	if err != nil || !bytes.Equal(downloaded, firstContent) ||
		downloadResponse.Header.Get("Content-Type") != "application/octet-stream" ||
		downloadResponse.Header.Get("Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(firstHash[:])+":" {
		t.Fatalf("Artifact download bytes=%v headers=%v error=%v", downloaded, downloadResponse.Header, err)
	}

	deleteResponse := artifactDELETE(
		t, server.URL+"/v1/artifacts/"+first.ID, key.Credential, "artifact-delete-key",
	)
	assertHTTPStatus(t, deleteResponse, http.StatusNoContent)
	deleteResponse.Body.Close()
	deleteReplay := artifactDELETE(
		t, server.URL+"/v1/artifacts/"+first.ID, key.Credential, "artifact-delete-key",
	)
	assertHTTPStatus(t, deleteReplay, http.StatusNoContent)
	deleteReplay.Body.Close()
	assertHTTPStatus(t, artifactGET(t, server.URL+"/v1/artifacts/"+first.ID, key.Credential), http.StatusNotFound)
	assertHTTPStatus(t, artifactGET(t, server.URL+"/v1/artifacts/"+first.ID+"/content", key.Credential), http.StatusNotFound)
	if objects.deletes != 0 {
		t.Fatalf("public retention end synchronously deleted provider object")
	}
}

func TestPublicArtifactsEnforceAuthorityIntegrityBoundsQuotaAndExpiry(t *testing.T) {
	t.Run("authority integrity and expiry", func(t *testing.T) {
		fixture := newArtifactHTTPFixture(t, "artifact-negative", generousQuota(), 1<<20)
		content := []byte("retained")
		response := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-negative-base", "base.bin", "application/octet-stream",
			map[string]string{}, content,
		)
		assertHTTPStatus(t, response, http.StatusCreated)
		var artifact contracts.Artifact
		decodeHTTPJSON(t, response, &artifact)

		readOnlyKey, err := createFixtureAPIKey(t, fixture.controlPlane,
			t.Context(), fixture.admin, fixture.project.ID, fixture.account.ID,
			fixtureCreateAPIKeyRequest{
				Name: "artifact-missing-scope", Scopes: []string{"sandbox:read"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		missingScope := artifactGET(
			t, fixture.server.URL+"/v1/artifacts/"+artifact.ID, readOnlyKey.Credential,
		)
		assertHTTPStatus(t, missingScope, http.StatusOK)
		missingScope.Body.Close()

		otherProject, otherAccount, _ := createProjectAccountAndCredential(
			t, fixture.controlPlane, fixture.admin, "artifact-cross-project",
		)
		otherScopes := []string{"sandbox:read", "sandbox:artifacts"}
		if _, err := updateFixtureServiceAccount(t, fixture.controlPlane,
			t.Context(), fixture.admin, otherProject.ID, otherAccount.ID,
			fixtureUpdateServiceAccountRequest{Scopes: &otherScopes},
		); err != nil {
			t.Fatal(err)
		}
		otherKey, err := createFixtureAPIKey(t, fixture.controlPlane,
			t.Context(), fixture.admin, otherProject.ID, otherAccount.ID,
			fixtureCreateAPIKeyRequest{Name: "artifact-cross-project", Scopes: otherScopes},
		)
		if err != nil {
			t.Fatal(err)
		}
		crossProject := artifactGET(
			t, fixture.server.URL+"/v1/artifacts/"+artifact.ID, otherKey.Credential,
		)
		assertHTTPStatus(t, crossProject, http.StatusNotFound)
		crossProject.Body.Close()

		staleSandbox := fixture.sandbox
		staleSandbox.Generation++
		stale := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, staleSandbox, fixture.lease.ID,
			"artifact-stale-generation", "stale.bin", "application/octet-stream",
			map[string]string{}, []byte("stale"),
		)
		assertHTTPStatus(t, stale, http.StatusConflict)
		stale.Body.Close()

		badChecksum := strings.Repeat("0", 64)
		checksum := uploadArtifactWithSHA(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-bad-checksum", "bad.bin", "application/octet-stream",
			map[string]string{}, []byte("checksum"), badChecksum,
		)
		assertHTTPStatus(t, checksum, http.StatusConflict)
		checksum.Body.Close()
		if fixture.objects.puts != 1 {
			t.Fatalf("integrity failure published bytes: immutable uploads=%d, want 1", fixture.objects.puts)
		}

		nullMetadata := uploadArtifactWithRawMetadata(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-null-metadata", "null.bin", "application/octet-stream",
			[]byte("null"), []byte("metadata"),
		)
		assertHTTPStatus(t, nullMetadata, http.StatusBadRequest)
		nullMetadata.Body.Close()

		if _, err := fixture.controlPlane.ReleaseSandboxLease(
			t.Context(), contracts.Principal{
				Kind: "service_account", ID: fixture.account.ID,
				TenantRef: fixture.project.ID, SubjectRef: fixture.account.ID,
			},
			fixture.lease.ID, "artifact-release-lease",
		); err != nil {
			t.Fatal(err)
		}
		expiredLease := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-expired-lease", "expired.bin", "application/octet-stream",
			map[string]string{}, []byte("expired"),
		)
		assertHTTPStatus(t, expiredLease, http.StatusConflict)
		expiredLease.Body.Close()

		oversized := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, "",
			"artifact-oversized", "oversized.bin", "application/octet-stream",
			map[string]string{}, bytes.Repeat([]byte{1}, (1<<20)+1),
		)
		assertHTTPStatus(t, oversized, http.StatusRequestEntityTooLarge)
		oversized.Body.Close()

		*fixture.now = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
		expiredMetadata := artifactGET(
			t, fixture.server.URL+"/v1/artifacts/"+artifact.ID, fixture.key.Credential,
		)
		assertHTTPStatus(t, expiredMetadata, http.StatusNotFound)
		expiredMetadata.Body.Close()
		expiredContent := artifactGET(
			t, fixture.server.URL+"/v1/artifacts/"+artifact.ID+"/content", fixture.key.Credential,
		)
		assertHTTPStatus(t, expiredContent, http.StatusNotFound)
		expiredContent.Body.Close()
	})

	t.Run("large Artifact streams through staging", func(t *testing.T) {
		fixture := newArtifactHTTPFixture(t, "artifact-large", generousQuota(), 4<<20)
		content := bytes.Repeat([]byte("secondbox-streaming-artifact"), 120000)
		response := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-large", "large.bin", "application/octet-stream",
			map[string]string{"path": "streamed"}, content,
		)
		assertHTTPStatus(t, response, http.StatusCreated)
		var artifact contracts.Artifact
		decodeHTTPJSON(t, response, &artifact)
		download := artifactGET(
			t, fixture.server.URL+"/v1/artifacts/"+artifact.ID+"/content",
			fixture.key.Credential,
		)
		assertHTTPStatus(t, download, http.StatusOK)
		received, err := io.ReadAll(download.Body)
		download.Body.Close()
		if err != nil || !bytes.Equal(received, content) {
			t.Fatalf("large Artifact round-trip bytes=%d error=%v", len(received), err)
		}
	})

	t.Run("maximum Artifact count", func(t *testing.T) {
		quota := generousQuota()
		quota.MaxArtifacts = 1
		fixture := newArtifactHTTPFixture(t, "artifact-count-quota", quota, 1<<20)
		first := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-count-first", "first.bin", "application/octet-stream",
			map[string]string{}, []byte("one"),
		)
		assertHTTPStatus(t, first, http.StatusCreated)
		first.Body.Close()
		second := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-count-second", "second.bin", "application/octet-stream",
			map[string]string{}, []byte("two"),
		)
		assertHTTPStatus(t, second, http.StatusTooManyRequests)
		second.Body.Close()
	})

	t.Run("retained byte quota", func(t *testing.T) {
		quota := generousQuota()
		quota.MaxArtifactBytes = 4
		fixture := newArtifactHTTPFixture(t, "artifact-byte-quota", quota, 1<<20)
		first := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-bytes-first", "first.bin", "application/octet-stream",
			map[string]string{}, []byte("1234"),
		)
		assertHTTPStatus(t, first, http.StatusCreated)
		first.Body.Close()
		second := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-bytes-second", "second.bin", "application/octet-stream",
			map[string]string{}, []byte("5"),
		)
		assertHTTPStatus(t, second, http.StatusTooManyRequests)
		second.Body.Close()
	})

	t.Run("subject Artifact quota", func(t *testing.T) {
		fixture := newArtifactHTTPFixture(t, "artifact-profile-quota", generousQuota(), 1<<20)
		pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		if _, err := pool.Exec(t.Context(), `
			UPDATE secondbox.subject_quotas SET max_artifacts=0
			WHERE tenant_ref=$1 AND subject_ref=$2`,
			fixture.sandbox.TenantRef, fixture.sandbox.SubjectRef,
		); err != nil {
			t.Fatal(err)
		}
		response := uploadArtifact(
			t, fixture.server.URL, fixture.key.Credential, fixture.sandbox, fixture.lease.ID,
			"artifact-profile-quota", "profile.bin", "application/octet-stream",
			map[string]string{}, []byte("profile"),
		)
		assertHTTPStatus(t, response, http.StatusTooManyRequests)
		response.Body.Close()
	})
}

type artifactHTTPFixture struct {
	controlPlane *service.ControlPlaneService
	server       *httptest.Server
	objects      *artifactObjectServer
	admin        contracts.Principal
	project      fixtureProject
	account      fixtureServiceAccount
	profile      contracts.Profile
	key          fixtureCreateAPIKeyResponse
	sandbox      contracts.Sandbox
	lease        contracts.Lease
	now          *time.Time
}

func newArtifactHTTPFixture(
	t *testing.T,
	suffix string,
	projectQuota contracts.QuotaLimits,
	maximumBodyBytes int64,
) artifactHTTPFixture {
	t.Helper()
	objects, s3Server := newArtifactObjectServer(t)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	immutableObjects, err := objectstore.NewS3Store(t.Context(), objectstore.S3Config{
		Endpoint: s3Server.URL, Region: "us-east-1", Bucket: "secondbox",
		AccessKeyID: "artifact-test-access", SecretAccessKey: "artifact-test-secret",
		UsePathStyle: true, RetryMaxAttempts: 1, HTTPTimeout: time.Second,
		TempDirectory: t.TempDir(), MaxObjectBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	controlPlane := newArtifactControlPlane(
		t, databaseStore, immutableObjects, projectQuota, func() time.Time { return now },
	)
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, suffix)
	cleanupArtifactRows(t, project.ID)
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-"+suffix)
	scopes := []string{
		"sandbox:read", "sandbox:lifecycle", "sandbox:artifacts",
	}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: suffix, Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, suffix+"-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, suffix+"-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: maximumBodyBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)
	return artifactHTTPFixture{
		controlPlane: controlPlane, server: server, objects: objects,
		admin: admin, project: project, account: account, key: key,
		profile: profile, sandbox: sandbox, lease: lease, now: &now,
	}
}

func newArtifactControlPlane(
	t *testing.T,
	databaseStore *store.PostgresControlPlaneStore,
	immutableObjects objectstore.Store,
	projectQuota contracts.QuotaLimits,
	now func() time.Time,
) *service.ControlPlaneService {
	t.Helper()
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		BuiltInProfiles: integrationBuiltInProfiles(t),
		Store:           databaseStore, ArtifactObjectStore: immutableObjects,
		PlatformToken:       testPlatformToken,
		DefaultSubjectQuota: projectQuota,
		Now:                 now,
		NewID:               newFixtureID,
		NewCredentialMaterial: func() string {
			return fmt.Sprintf("credential-material-%032d", integrationIdentitySequence.Add(1))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return controlPlane
}

type artifactObjectServer struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	deletes int
}

func newArtifactObjectServer(t *testing.T) (*artifactObjectServer, *httptest.Server) {
	t.Helper()
	state := &artifactObjectServer{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch request.Method {
		case http.MethodPut:
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			if _, exists := state.objects[request.URL.Path]; exists {
				writer.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			state.objects[request.URL.Path] = body
			state.puts++
			writer.Header().Set("ETag", `"immutable"`)
			writer.WriteHeader(http.StatusOK)
		case http.MethodHead:
			body, exists := state.objects[request.URL.Path]
			if !exists {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			sum := sha256.Sum256(body)
			writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
			writer.Header().Set("X-Amz-Meta-Sha256", hex.EncodeToString(sum[:]))
			writer.Header().Set("ETag", `"immutable"`)
			writer.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, exists := state.objects[request.URL.Path]
			if !exists {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = writer.Write(body)
		case http.MethodDelete:
			delete(state.objects, request.URL.Path)
			state.deletes++
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return state, server
}

func uploadArtifact(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	name string,
	mediaType string,
	metadata map[string]string,
	content []byte,
) *http.Response {
	t.Helper()
	sum := sha256.Sum256(content)
	return uploadArtifactWithSHA(
		t, baseURL, credential, sandbox, leaseID, idempotencyKey,
		name, mediaType, metadata, content, hex.EncodeToString(sum[:]),
	)
}

func uploadArtifactWithSHA(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	name string,
	mediaType string,
	metadata map[string]string,
	content []byte,
	declaredSHA256 string,
) *http.Response {
	t.Helper()
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return uploadArtifactMultipart(
		t, baseURL, credential, sandbox, leaseID, idempotencyKey,
		name, mediaType, metadataJSON, content, declaredSHA256,
	)
}

func uploadArtifactWithRawMetadata(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	name string,
	mediaType string,
	metadataJSON []byte,
	content []byte,
) *http.Response {
	t.Helper()
	sum := sha256.Sum256(content)
	return uploadArtifactMultipart(
		t, baseURL, credential, sandbox, leaseID, idempotencyKey,
		name, mediaType, metadataJSON, content, hex.EncodeToString(sum[:]),
	)
}

func uploadArtifactMultipart(
	t *testing.T,
	baseURL string,
	credential string,
	sandbox contracts.Sandbox,
	leaseID string,
	idempotencyKey string,
	name string,
	mediaType string,
	metadataJSON []byte,
	content []byte,
	declaredSHA256 string,
) *http.Response {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	writeArtifactField(t, form, "name", []byte(name), "text/plain")
	writeArtifactField(t, form, "mediaType", []byte(mediaType), "text/plain")
	writeArtifactField(t, form, "sha256", []byte(declaredSHA256), "text/plain")
	writeArtifactField(t, form, "metadata", metadataJSON, "application/json")
	writeArtifactField(t, form, "content", content, "application/octet-stream")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, baseURL+"/v1/sandboxes/"+sandbox.ID+"/artifacts", &body,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, request, credential, sandbox.Generation, idempotencyKey)
	request.Header.Set("SecondBox-Lease-ID", leaseID)
	request.Header.Set("Content-Type", form.FormDataContentType())
	return doHTTP(t, request)
}

func writeArtifactField(
	t *testing.T,
	form *multipart.Writer,
	name string,
	value []byte,
	contentType string,
) {
	t.Helper()
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="`+name+`"`)
	header.Set("Content-Type", contentType)
	part, err := form.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(value); err != nil {
		t.Fatal(err)
	}
}

func artifactGET(t *testing.T, endpoint string, credential string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, credential)
	return doHTTP(t, request)
}

func artifactDELETE(t *testing.T, endpoint string, credential string, idempotencyKey string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodDelete, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, credential)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	return doHTTP(t, request)
}

func cleanupArtifactRows(t *testing.T, tenantRef string) {
	t.Helper()
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), integrationDatabaseURL)
		if err != nil {
			t.Errorf("Artifact cleanup pool: %v", err)
			return
		}
		defer pool.Close()
		if _, err := pool.Exec(
			context.Background(),
			`DELETE FROM secondbox.artifacts WHERE tenant_ref=$1`,
			tenantRef,
		); err != nil {
			t.Errorf("Artifact cleanup metadata: %v", err)
		}
	})
}
