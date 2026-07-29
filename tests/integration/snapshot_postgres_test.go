package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestSnapshotHTTPContractEnforcesAuthRevisionAndStoppedCheckpointAuthority(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "snapshot-http",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-snapshot-http",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "snapshot-http-sandbox",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='stopped',desired_state='stopped',revision=revision+1,updated_at=$2
		WHERE id=$1`,
		sandbox.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	publishSnapshotTestCheckpoint(t, databaseStore, contracts.WorkspaceCheckpoint{
		ID: "chk_snapshot_http", WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: sandbox.Generation,
		SHA256:           "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		SizeBytes:        1024, Compatibility: map[string]string{"formatVersion": "1"},
		RetainUntil: now.Add(24 * time.Hour), CreatedAt: now,
	}, sandbox.Generation, now)

	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	unauthenticated, err := http.Get(server.URL + "/v1/sandboxes/" + sandbox.ID + "/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, unauthenticated, http.StatusUnauthorized)
	unauthenticated.Body.Close()

	createdResponse := snapshotJSONRequest(
		t, http.MethodPost, server.URL+"/v1/sandboxes/"+sandbox.ID+"/snapshots",
		credential, "snapshot-http-create", sandbox.Revision,
		contracts.CreateSnapshotRequest{Name: "http-snapshot", Metadata: map[string]string{}},
	)
	assertHTTPStatus(t, createdResponse, http.StatusCreated)
	var created contracts.Snapshot
	decodeHTTPJSON(t, createdResponse, &created)
	if created.ID == "" || created.TenantRef != "" || created.WorkspaceID != "" ||
		created.CheckpointID != "" || created.State != "" ||
		created.Name != "http-snapshot" {
		t.Fatalf("public Snapshot = %#v", created)
	}

	listResponse := snapshotJSONRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes/"+sandbox.ID+"/snapshots",
		credential, "", 0, nil,
	)
	assertHTTPStatus(t, listResponse, http.StatusOK)
	var page contracts.SnapshotPage
	decodeHTTPJSON(t, listResponse, &page)
	if len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("public Snapshot page = %#v", page)
	}

	snapshotScopes := []string{
		"sandbox:read", "sandbox:lifecycle",
	}
	otherSubject, err := createFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID,
		fixtureCreateServiceAccountRequest{
			Name: "snapshot-http-other-subject", Scopes: snapshotScopes,
			ProfileGrants: []string{profile.Name},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	otherSubjectKey, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, otherSubject.ID,
		fixtureCreateAPIKeyRequest{
			Name: "snapshot-http-other-subject", Scopes: snapshotScopes,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	crossSubject := snapshotJSONRequest(
		t, http.MethodGet, server.URL+"/v1/snapshots/"+created.ID,
		otherSubjectKey.Credential, "", 0, nil,
	)
	assertHTTPStatus(t, crossSubject, http.StatusNotFound)
	crossSubject.Body.Close()

	_, _, otherCredential := createProjectAccountAndCredential(
		t, controlPlane, admin, "snapshot-http-other",
	)
	crossProject := snapshotJSONRequest(
		t, http.MethodGet, server.URL+"/v1/snapshots/"+created.ID,
		otherCredential, "", 0, nil,
	)
	assertHTTPStatus(t, crossProject, http.StatusNotFound)
	crossProject.Body.Close()

	staleRevision := snapshotJSONRequest(
		t, http.MethodPost, server.URL+"/v1/sandboxes/"+sandbox.ID+"/snapshots",
		credential, "snapshot-http-stale", sandbox.Revision-1,
		contracts.CreateSnapshotRequest{Name: "stale", Metadata: map[string]string{}},
	)
	assertHTTPStatus(t, staleRevision, http.StatusPreconditionFailed)
	staleRevision.Body.Close()

	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',desired_state='running',revision=revision+1,updated_at=$2
		WHERE id=$1`,
		sandbox.ID, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	running := snapshotJSONRequest(
		t, http.MethodPost, server.URL+"/v1/sandboxes/"+sandbox.ID+"/snapshots",
		credential, "snapshot-http-running", sandbox.Revision,
		contracts.CreateSnapshotRequest{Name: "running", Metadata: map[string]string{}},
	)
	assertHTTPStatus(t, running, http.StatusConflict)
	running.Body.Close()

	deleteResponse := snapshotJSONRequest(
		t, http.MethodDelete, server.URL+"/v1/snapshots/"+created.ID,
		credential, "snapshot-http-delete", 0, nil,
	)
	assertHTTPStatus(t, deleteResponse, http.StatusNoContent)
	deleteResponse.Body.Close()
	getDeleted := snapshotJSONRequest(
		t, http.MethodGet, server.URL+"/v1/snapshots/"+created.ID,
		credential, "", 0, nil,
	)
	assertHTTPStatus(t, getDeleted, http.StatusNotFound)
	getDeleted.Body.Close()
	if project.ID == "" {
		t.Fatal("Snapshot HTTP Project identity is empty")
	}
}

func TestSnapshotsRetainPublishedStoppedStateAndProtectCheckpointGarbageCollection(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "snapshot-retention",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-snapshot-retention",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "snapshot-sandbox",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='stopped',desired_state='stopped',revision=revision+1,updated_at=$2
		WHERE id=$1`,
		sandbox.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstCheckpoint := contracts.WorkspaceCheckpoint{
		ID: "chk_snapshot_first", WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: sandbox.Generation,
		SHA256:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes:        4096,
		Compatibility: map[string]string{
			"architecture": "amd64", "formatVersion": "1",
		},
		RetainUntil: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour),
	}
	publishSnapshotTestCheckpoint(t, databaseStore, firstCheckpoint, sandbox.Generation, now)

	created, err := controlPlane.CreateSandboxSnapshot(
		t.Context(), principal, sandbox.ID, "snapshot-create", sandbox.Revision,
		contracts.CreateSnapshotRequest{
			Name: "before-refactor", Metadata: map[string]string{"purpose": "restore-point"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.TenantRef != project.ID ||
		created.SandboxID != sandbox.ID || created.WorkspaceID != sandbox.Workspace.ID ||
		created.CheckpointID != firstCheckpoint.ID ||
		created.SourceGeneration != firstCheckpoint.SourceGeneration ||
		created.Name != "before-refactor" || created.SHA256 != firstCheckpoint.SHA256 ||
		created.SizeBytes != firstCheckpoint.SizeBytes ||
		created.State != contracts.ObjectStatePublished ||
		created.Metadata["purpose"] != "restore-point" ||
		created.Compatibility["formatVersion"] != "1" ||
		!created.RetainUntil.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("created Snapshot = %#v", created)
	}
	replayed, err := controlPlane.CreateSandboxSnapshot(
		t.Context(), principal, sandbox.ID, "snapshot-create", sandbox.Revision,
		contracts.CreateSnapshotRequest{
			Name: "before-refactor", Metadata: map[string]string{"purpose": "restore-point"},
		},
	)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("Snapshot replay = %#v, %v", replayed, err)
	}
	if _, err := controlPlane.CreateSandboxSnapshot(
		t.Context(), principal, sandbox.ID, "snapshot-create", sandbox.Revision,
		contracts.CreateSnapshotRequest{Name: "different", Metadata: map[string]string{}},
	); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("Snapshot idempotency conflict = %v", err)
	}
	page, err := controlPlane.ListSandboxSnapshots(
		t.Context(), principal, sandbox.ID, 100, "",
	)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("Snapshot page = %#v, %v", page, err)
	}
	got, err := controlPlane.GetSnapshot(t.Context(), principal, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("Snapshot get = %#v, %v", got, err)
	}
	if _, err := databaseStore.GetSnapshot(
		t.Context(), principal.TenantRef, principal.SubjectRef,
		created.ID, created.RetainUntil,
	); !errors.Is(err, ports.ErrSnapshotNotFound) {
		t.Fatalf("retention-expired Snapshot get error = %v, want ErrSnapshotNotFound", err)
	}

	secondCheckpoint := contracts.WorkspaceCheckpoint{
		ID: "chk_snapshot_second", WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: sandbox.Generation,
		SHA256:           "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SizeBytes:        8192,
		Compatibility: map[string]string{
			"architecture": "amd64", "formatVersion": "1",
		},
		RetainUntil: now.Add(24 * time.Hour), CreatedAt: now,
	}
	publishSnapshotTestCheckpoint(t, databaseStore, secondCheckpoint, sandbox.Generation, now)
	candidates, err := databaseStore.ListGarbageObjectsDue(
		t.Context(), now.Add(2*time.Hour), time.Hour, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertGarbageCandidateAbsent(t, candidates, firstCheckpoint.ID)

	if err := controlPlane.DeleteSnapshot(
		t.Context(), principal, created.ID, "snapshot-delete",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.GetSnapshot(t.Context(), principal, created.ID); !errors.Is(err, ports.ErrSnapshotNotFound) {
		t.Fatalf("ended Snapshot retention get = %v", err)
	}
	if err := controlPlane.DeleteSnapshot(
		t.Context(), principal, created.ID, "snapshot-delete",
	); err != nil {
		t.Fatalf("Snapshot retention replay = %v", err)
	}
	if _, err := databaseStore.ListGarbageObjectsDue(
		t.Context(), now.Add(2*time.Hour), time.Hour, 100,
	); err != nil {
		t.Fatal(err)
	}
	candidates, err = databaseStore.ListGarbageObjectsDue(
		t.Context(), now.Add(4*time.Hour), time.Hour, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertGarbageCandidatePresent(t, candidates, firstCheckpoint.ID)
}

func TestSnapshotPolicyLimitIsTransactionalAndRetentionReleaseRestoresCapacity(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "snapshot-capacity",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-snapshot-capacity",
	)
	spec := testProfileSpec(1000)
	spec.Checkpoint.SnapshotLimit = 1
	profile, err := controlPlane.ReviseProfile(
		t.Context(), admin, profile.Name, contracts.ReviseProfileRequest{Spec: spec},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "snapshot-capacity-sandbox",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='stopped',desired_state='stopped',revision=revision+1,updated_at=$2
		WHERE id=$1`,
		sandbox.ID, now,
	); err != nil {
		t.Fatal(err)
	}
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	publishSnapshotTestCheckpoint(t, databaseStore, contracts.WorkspaceCheckpoint{
		ID: "chk_snapshot_capacity", WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: sandbox.Generation,
		SHA256:           "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		SizeBytes:        2048, Compatibility: map[string]string{"formatVersion": "1"},
		RetainUntil: now.Add(24 * time.Hour), CreatedAt: now,
	}, sandbox.Generation, now)

	type snapshotResult struct {
		snapshot contracts.Snapshot
		err      error
	}
	results := make(chan snapshotResult, 2)
	var waitGroup sync.WaitGroup
	for _, suffix := range []string{"a", "b"} {
		suffix := suffix
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			snapshot, err := controlPlane.CreateSandboxSnapshot(
				t.Context(), principal, sandbox.ID, "snapshot-capacity-"+suffix,
				sandbox.Revision,
				contracts.CreateSnapshotRequest{
					Name: "capacity-" + suffix, Metadata: map[string]string{},
				},
			)
			results <- snapshotResult{snapshot: snapshot, err: err}
		}()
	}
	waitGroup.Wait()
	close(results)
	var retained contracts.Snapshot
	var createdCount, rejectedCount int
	for result := range results {
		switch {
		case result.err == nil:
			createdCount++
			retained = result.snapshot
		case errors.Is(result.err, ports.ErrQuotaExceeded):
			rejectedCount++
		default:
			t.Fatalf("concurrent Snapshot capacity result = %#v, %v", result.snapshot, result.err)
		}
	}
	if createdCount != 1 || rejectedCount != 1 {
		t.Fatalf("Snapshot capacity results: created=%d rejected=%d", createdCount, rejectedCount)
	}
	if err := controlPlane.DeleteSnapshot(
		t.Context(), principal, retained.ID, "snapshot-capacity-delete",
	); err != nil {
		t.Fatal(err)
	}
	replacement, err := controlPlane.CreateSandboxSnapshot(
		t.Context(), principal, sandbox.ID, "snapshot-capacity-replacement",
		sandbox.Revision,
		contracts.CreateSnapshotRequest{
			Name: "capacity-replacement", Metadata: map[string]string{},
		},
	)
	if err != nil || replacement.ID == "" {
		t.Fatalf("Snapshot capacity after retention release = %#v, %v", replacement, err)
	}
}

func snapshotJSONRequest(
	t *testing.T,
	method string,
	url string,
	credential string,
	idempotencyKey string,
	revision int64,
	body any,
) *http.Response {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, url, requestBody)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, credential)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if revision > 0 {
		request.Header.Set("If-Match", `"revision-`+strconv.FormatInt(revision, 10)+`"`)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func publishSnapshotTestCheckpoint(
	t *testing.T,
	databaseStore *store.PostgresControlPlaneStore,
	checkpoint contracts.WorkspaceCheckpoint,
	generation int64,
	now time.Time,
) {
	t.Helper()
	publication := ports.CheckpointPublicationInput{
		Checkpoint: checkpoint, StorageKey: "checkpoints/" + checkpoint.ID,
		ExpectedWorkspaceGeneration: generation,
	}
	if _, err := databaseStore.StageCheckpoint(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.VerifyCheckpoint(t.Context(), publication, now); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.PublishCheckpoint(t.Context(), publication, now); err != nil {
		t.Fatal(err)
	}
}

func assertGarbageCandidateAbsent(t *testing.T, candidates []ports.GarbageObject, objectID string) {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.ID == objectID {
			t.Fatalf("garbage candidates unexpectedly contain %q: %#v", objectID, candidates)
		}
	}
}

func assertGarbageCandidatePresent(t *testing.T, candidates []ports.GarbageObject, objectID string) {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.ID == objectID {
			return
		}
	}
	t.Fatalf("garbage candidates do not contain %q: %#v", objectID, candidates)
}
