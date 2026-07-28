package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestAdministrativeMutationsReplayExactDurableResponses(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	projectResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, "/v1/projects",
		"admin-project-idempotency", "", map[string]any{"name": "idempotent project"},
	)
	assertHTTPStatus(t, projectResponse, http.StatusCreated)
	if projectResponse.Header.Get("Idempotency-Replayed") != "false" {
		t.Fatalf("Project create replay header = %q", projectResponse.Header.Get("Idempotency-Replayed"))
	}
	var project contracts.Project
	decodeHTTPJSON(t, projectResponse, &project)

	projectReplayResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, "/v1/projects",
		"admin-project-idempotency", "", map[string]any{"name": "idempotent project"},
	)
	assertHTTPStatus(t, projectReplayResponse, http.StatusCreated)
	if projectReplayResponse.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("Project create replay header = %q", projectReplayResponse.Header.Get("Idempotency-Replayed"))
	}
	var replayedProject contracts.Project
	decodeHTTPJSON(t, projectReplayResponse, &replayedProject)
	if replayedProject.ID != project.ID || replayedProject.Revision != project.Revision {
		t.Fatalf("Project replay = %#v, want %#v", replayedProject, project)
	}
	projectConflict := adminJSONRequest(
		t, server.URL, http.MethodPost, "/v1/projects",
		"admin-project-idempotency", "", map[string]any{"name": "changed project"},
	)
	assertHTTPStatus(t, projectConflict, http.StatusConflict)
	projectConflict.Body.Close()

	projectUpdatePath := "/v1/projects/" + project.ID
	projectUpdateBody := map[string]any{"name": "updated idempotent project"}
	projectUpdateResponse := adminJSONRequest(
		t, server.URL, http.MethodPatch, projectUpdatePath,
		"admin-project-update-idempotency", strconv.FormatInt(project.Revision, 10), projectUpdateBody,
	)
	assertHTTPStatus(t, projectUpdateResponse, http.StatusOK)
	var updatedProject contracts.Project
	decodeHTTPJSON(t, projectUpdateResponse, &updatedProject)
	projectUpdateReplay := adminJSONRequest(
		t, server.URL, http.MethodPatch, projectUpdatePath,
		"admin-project-update-idempotency", strconv.FormatInt(project.Revision, 10), projectUpdateBody,
	)
	assertHTTPStatus(t, projectUpdateReplay, http.StatusOK)
	if projectUpdateReplay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("Project update replay header = %q", projectUpdateReplay.Header.Get("Idempotency-Replayed"))
	}
	var replayedProjectUpdate contracts.Project
	decodeHTTPJSON(t, projectUpdateReplay, &replayedProjectUpdate)
	if replayedProjectUpdate.Revision != updatedProject.Revision {
		t.Fatalf("Project update replay revision = %d, want %d", replayedProjectUpdate.Revision, updatedProject.Revision)
	}

	accountBody := map[string]any{
		"name":          "idempotent key owner",
		"scopes":        []string{contracts.ScopeSandboxRead},
		"profileGrants": []string{},
	}
	accountPath := "/v1/projects/" + project.ID + "/service-accounts"
	accountResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, accountPath,
		"admin-account-idempotency", "", accountBody,
	)
	assertHTTPStatus(t, accountResponse, http.StatusCreated)
	var account contracts.ServiceAccount
	decodeHTTPJSON(t, accountResponse, &account)
	accountReplay := adminJSONRequest(
		t, server.URL, http.MethodPost, accountPath,
		"admin-account-idempotency", "", accountBody,
	)
	assertHTTPStatus(t, accountReplay, http.StatusCreated)
	if accountReplay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("ServiceAccount create replay header = %q", accountReplay.Header.Get("Idempotency-Replayed"))
	}
	accountReplay.Body.Close()

	accountUpdatePath := accountPath + "/" + account.ID
	accountUpdateBody := map[string]any{"name": "updated idempotent key owner"}
	accountUpdateResponse := adminJSONRequest(
		t, server.URL, http.MethodPatch, accountUpdatePath,
		"admin-account-update-idempotency", strconv.FormatInt(account.Revision, 10), accountUpdateBody,
	)
	assertHTTPStatus(t, accountUpdateResponse, http.StatusOK)
	var updatedAccount contracts.ServiceAccount
	decodeHTTPJSON(t, accountUpdateResponse, &updatedAccount)
	accountUpdateReplay := adminJSONRequest(
		t, server.URL, http.MethodPatch, accountUpdatePath,
		"admin-account-update-idempotency", strconv.FormatInt(account.Revision, 10), accountUpdateBody,
	)
	assertHTTPStatus(t, accountUpdateReplay, http.StatusOK)
	if accountUpdateReplay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("ServiceAccount update replay header = %q", accountUpdateReplay.Header.Get("Idempotency-Replayed"))
	}
	accountUpdateReplay.Body.Close()

	keyBody := map[string]any{
		"name": "idempotent key",
		"scopes": []string{
			contracts.ScopeSandboxRead,
		},
	}
	keyPath := "/v1/projects/" + project.ID + "/service-accounts/" + account.ID + "/api-keys"
	keyResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, keyPath, "admin-key-idempotency", "", keyBody,
	)
	assertHTTPStatus(t, keyResponse, http.StatusCreated)
	var key contracts.CreateAPIKeyResponse
	decodeHTTPJSON(t, keyResponse, &key)
	if key.Credential == "" {
		t.Fatal("APIKey creation returned no credential")
	}
	keyReplayResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, keyPath, "admin-key-idempotency", "", keyBody,
	)
	assertHTTPStatus(t, keyReplayResponse, http.StatusCreated)
	if keyReplayResponse.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("APIKey create replay header = %q", keyReplayResponse.Header.Get("Idempotency-Replayed"))
	}
	var replayedKey contracts.CreateAPIKeyResponse
	decodeHTTPJSON(t, keyReplayResponse, &replayedKey)
	if replayedKey.APIKey.ID != key.APIKey.ID || replayedKey.Credential != key.Credential {
		t.Fatalf("APIKey replay changed response: first=%#v replay=%#v", key, replayedKey)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var storedResponse string
	var storedSecret []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT response_json::text,response_secret
		FROM secondbox.idempotency_records
		WHERE operation='api_key.create' AND idempotency_key='admin-key-idempotency'`,
	).Scan(&storedResponse, &storedSecret); err != nil {
		t.Fatal(err)
	}
	if len(storedSecret) == 0 {
		t.Fatal("APIKey idempotency response has no sealed credential")
	}
	if strings.Contains(storedResponse, key.Credential) ||
		bytes.Contains(storedSecret, []byte(key.Credential)) {
		t.Fatal("APIKey idempotency storage exposed plaintext credential material")
	}

	revokePath := keyPath + "/" + key.APIKey.ID + ":revoke"
	revokeResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, revokePath, "admin-key-revoke-idempotency",
		strconv.FormatInt(key.APIKey.Revision, 10), nil,
	)
	assertHTTPStatus(t, revokeResponse, http.StatusOK)
	var revokedKey contracts.APIKey
	decodeHTTPJSON(t, revokeResponse, &revokedKey)
	revokeReplay := adminJSONRequest(
		t, server.URL, http.MethodPost, revokePath, "admin-key-revoke-idempotency",
		strconv.FormatInt(key.APIKey.Revision, 10), nil,
	)
	assertHTTPStatus(t, revokeReplay, http.StatusOK)
	if revokeReplay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("APIKey revoke replay header = %q", revokeReplay.Header.Get("Idempotency-Replayed"))
	}
	revokeReplay.Body.Close()

	profileBody := contracts.CreateProfileRequest{Name: "idempotent-revise", Spec: testProfileSpec(1000)}
	profileResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, "/v1/profiles",
		"admin-profile-create-idempotency", "", profileBody,
	)
	assertHTTPStatus(t, profileResponse, http.StatusCreated)
	var profile contracts.Profile
	decodeHTTPJSON(t, profileResponse, &profile)
	profileReplay := adminJSONRequest(
		t, server.URL, http.MethodPost, "/v1/profiles",
		"admin-profile-create-idempotency", "", profileBody,
	)
	assertHTTPStatus(t, profileReplay, http.StatusCreated)
	if profileReplay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("Profile create replay header = %q", profileReplay.Header.Get("Idempotency-Replayed"))
	}
	profileReplay.Body.Close()

	revisionBody := contracts.ReviseProfileRequest{Spec: testProfileSpec(2000)}
	revisionPath := "/v1/profiles/" + profile.Name + ":revise"
	revisionResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, revisionPath, "admin-profile-revise-idempotency",
		strconv.FormatInt(profile.Revision, 10), revisionBody,
	)
	assertHTTPStatus(t, revisionResponse, http.StatusOK)
	var firstRevision contracts.Profile
	decodeHTTPJSON(t, revisionResponse, &firstRevision)
	if _, err := controlPlane.ReviseProfile(
		t.Context(),
		admin,
		profile.Name,
		contracts.ReviseProfileRequest{Spec: testProfileSpec(3000)},
	); err != nil {
		t.Fatal(err)
	}
	revisionReplayResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, revisionPath, "admin-profile-revise-idempotency",
		strconv.FormatInt(profile.Revision, 10), revisionBody,
	)
	assertHTTPStatus(t, revisionReplayResponse, http.StatusOK)
	if revisionReplayResponse.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("Profile revise replay header = %q", revisionReplayResponse.Header.Get("Idempotency-Replayed"))
	}
	var replayedRevision contracts.Profile
	decodeHTTPJSON(t, revisionReplayResponse, &replayedRevision)
	if replayedRevision.Revision != firstRevision.Revision ||
		replayedRevision.CurrentRevision.ID != firstRevision.CurrentRevision.ID {
		t.Fatalf("Profile revision replay changed response: first=%#v replay=%#v", firstRevision, replayedRevision)
	}
	currentProfile, err := controlPlane.GetProfile(t.Context(), admin, profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	disablePath := "/v1/profiles/" + profile.Name + ":disable"
	disableResponse := adminJSONRequest(
		t, server.URL, http.MethodPost, disablePath, "admin-profile-disable-idempotency",
		strconv.FormatInt(currentProfile.Revision, 10), nil,
	)
	assertHTTPStatus(t, disableResponse, http.StatusOK)
	var disabledProfile contracts.Profile
	decodeHTTPJSON(t, disableResponse, &disabledProfile)
	disableReplay := adminJSONRequest(
		t, server.URL, http.MethodPost, disablePath, "admin-profile-disable-idempotency",
		strconv.FormatInt(currentProfile.Revision, 10), nil,
	)
	assertHTTPStatus(t, disableReplay, http.StatusOK)
	if disableReplay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("Profile disable replay header = %q", disableReplay.Header.Get("Idempotency-Replayed"))
	}
	var replayedDisabledProfile contracts.Profile
	decodeHTTPJSON(t, disableReplay, &replayedDisabledProfile)
	if replayedDisabledProfile.Revision != disabledProfile.Revision {
		t.Fatalf("Profile disable replay revision = %d, want %d", replayedDisabledProfile.Revision, disabledProfile.Revision)
	}
}

func TestProfileRevisionHTTPUpdatesTheExistingResource(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	profile, err := controlPlane.CreateProfile(
		t.Context(),
		admin,
		contracts.CreateProfileRequest{Name: "admin-http-revise", Spec: testProfileSpec(1000)},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	body, err := json.Marshal(contracts.ReviseProfileRequest{Spec: testProfileSpec(2000)})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL+"/v1/profiles/"+profile.Name+":revise",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer bootstrap-administrator-secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "admin-http-revise-request")
	request.Header.Set("If-Match", `"revision-`+strconv.FormatInt(profile.Revision, 10)+`"`)
	response := doHTTP(t, request)
	assertHTTPStatus(t, response, http.StatusOK)
	var revised contracts.Profile
	decodeHTTPJSON(t, response, &revised)
	if revised.Name != profile.Name || revised.Revision != profile.Revision+1 {
		t.Fatalf("revised Profile = %#v", revised)
	}
}

func TestAPIKeyRevocationRejectsAStaleObservedRevision(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "admin-http-key-revoke")
	created, err := controlPlane.CreateAPIKey(
		t.Context(),
		admin,
		project.ID,
		account.ID,
		contracts.CreateAPIKeyRequest{
			Name: "stale-revoke",
			Scopes: []string{
				contracts.ScopeSandboxRead,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := controlPlane.RotateAPIKey(
		t.Context(),
		admin,
		project.ID,
		account.ID,
		created.APIKey.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL+"/v1/projects/"+project.ID+"/service-accounts/"+account.ID+
			"/api-keys/"+created.APIKey.ID+":revoke",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer bootstrap-administrator-secret")
	request.Header.Set("Idempotency-Key", "admin-http-stale-revoke")
	request.Header.Set("If-Match", `"revision-`+strconv.FormatInt(created.APIKey.Revision, 10)+`"`)
	response := doHTTP(t, request)
	assertHTTPStatus(t, response, http.StatusPreconditionFailed)
	var problem contracts.Problem
	decodeHTTPJSON(t, response, &problem)
	if problem.Code != "precondition_failed" {
		t.Fatalf("stale APIKey revoke problem = %#v", problem)
	}
	if _, err := controlPlane.AuthenticateCredential(t.Context(), rotated.Credential); err != nil {
		t.Fatalf("stale revoke invalidated the rotated credential: %v", err)
	}
}

func TestAPIKeyRotationIsRevisionFencedAndReturnsTheUpdatedAuthority(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "admin-http-key-rotate")
	created, err := controlPlane.CreateAPIKey(
		t.Context(),
		admin,
		project.ID,
		account.ID,
		contracts.CreateAPIKeyRequest{
			Name: "revision-fenced-rotate",
			Scopes: []string{
				contracts.ScopeSandboxRead,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	rotateURL := server.URL + "/v1/projects/" + project.ID + "/service-accounts/" +
		account.ID + "/api-keys/" + created.APIKey.ID + ":rotate"

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, rotateURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer bootstrap-administrator-secret")
	request.Header.Set("Idempotency-Key", "admin-http-rotate")
	request.Header.Set("If-Match", `"revision-`+strconv.FormatInt(created.APIKey.Revision, 10)+`"`)
	response := doHTTP(t, request)
	assertHTTPStatus(t, response, http.StatusOK)
	if response.Header.Get("ETag") != `"revision-2"` {
		t.Fatalf("rotated APIKey ETag = %q", response.Header.Get("ETag"))
	}
	var rotated contracts.CreateAPIKeyResponse
	decodeHTTPJSON(t, response, &rotated)
	if rotated.APIKey.ID != created.APIKey.ID || rotated.APIKey.Revision != 2 || rotated.Credential == "" {
		t.Fatalf("rotated APIKey response = %#v", rotated)
	}

	replayRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, rotateURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	replayRequest.Header.Set("Authorization", "Bearer bootstrap-administrator-secret")
	replayRequest.Header.Set("Idempotency-Key", "admin-http-rotate")
	replayRequest.Header.Set("If-Match", `"revision-`+strconv.FormatInt(created.APIKey.Revision, 10)+`"`)
	replayResponse := doHTTP(t, replayRequest)
	assertHTTPStatus(t, replayResponse, http.StatusOK)
	if replayResponse.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("APIKey rotation replay header = %q", replayResponse.Header.Get("Idempotency-Replayed"))
	}
	var replayedRotation contracts.CreateAPIKeyResponse
	decodeHTTPJSON(t, replayResponse, &replayedRotation)
	if replayedRotation.APIKey.ID != rotated.APIKey.ID ||
		replayedRotation.APIKey.Revision != rotated.APIKey.Revision ||
		replayedRotation.Credential != rotated.Credential {
		t.Fatalf("APIKey rotation replay changed response: first=%#v replay=%#v", rotated, replayedRotation)
	}

	staleRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, rotateURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	staleRequest.Header.Set("Authorization", "Bearer bootstrap-administrator-secret")
	staleRequest.Header.Set("Idempotency-Key", "admin-http-stale-rotate")
	staleRequest.Header.Set("If-Match", `"revision-`+strconv.FormatInt(created.APIKey.Revision, 10)+`"`)
	staleResponse := doHTTP(t, staleRequest)
	assertHTTPStatus(t, staleResponse, http.StatusPreconditionFailed)
	if _, err := controlPlane.AuthenticateCredential(t.Context(), rotated.Credential); err != nil {
		t.Fatalf("stale rotation invalidated the current credential: %v", err)
	}
}

func adminJSONRequest(
	t *testing.T,
	baseURL string,
	method string,
	path string,
	idempotencyKey string,
	ifMatch string,
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
	request, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer bootstrap-administrator-secret")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		request.Header.Set("If-Match", `"revision-`+ifMatch+`"`)
	}
	return doHTTP(t, request)
}
