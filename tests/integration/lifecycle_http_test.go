package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestLifecycleHTTPContractAndProjectIsolation(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "lifecycle-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-lifecycle-http")
	_, _, otherCredential := createProjectAccountAndCredential(t, controlPlane, admin, "lifecycle-http-other")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "lifecycle-http-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, sandbox.ID)
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	metadataUpdate := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPut,
		"/v1/sandboxes/"+sandbox.ID+"/metadata", "",
		strconv.FormatInt(sandbox.Revision, 10), "",
		contracts.UpdateSandboxMetadataRequest{
			Metadata: map[string]string{"runtime-container-id": "container-bound"},
		},
	)
	if metadataUpdate.StatusCode != http.StatusOK {
		t.Fatalf(
			"metadata update status=%d body=%s",
			metadataUpdate.StatusCode,
			readResponse(t, metadataUpdate),
		)
	}
	if metadataUpdate.Header.Get("ETag") != `"revision-`+strconv.FormatInt(sandbox.Revision+1, 10)+`"` {
		t.Fatalf("metadata update ETag=%q", metadataUpdate.Header.Get("ETag"))
	}
	var metadataUpdatedSandbox contracts.Sandbox
	decodeResponseJSON(t, metadataUpdate, &metadataUpdatedSandbox)
	if metadataUpdatedSandbox.Metadata["runtime-container-id"] != "container-bound" {
		t.Fatalf("metadata update returned %#v", metadataUpdatedSandbox.Metadata)
	}
	if metadataUpdatedSandbox.State != sandbox.State ||
		metadataUpdatedSandbox.Generation != sandbox.Generation ||
		metadataUpdatedSandbox.Revision != sandbox.Revision+1 {
		t.Fatalf(
			"metadata update changed lifecycle: before=%#v after=%#v",
			sandbox,
			metadataUpdatedSandbox,
		)
	}
	auditEvents, err := databaseStore.ListAuditEvents(t.Context(), project.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	metadataAuditFound := false
	for _, event := range auditEvents {
		if event.Action == "sandbox.metadata.updated" &&
			event.ResourceKind == "sandbox" &&
			event.ResourceID == sandbox.ID &&
			event.ActorID == principal.ID {
			metadataAuditFound = true
			break
		}
	}
	if !metadataAuditFound {
		t.Fatalf("Sandbox metadata update audit is absent: %#v", auditEvents)
	}
	staleMetadataUpdate := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPut,
		"/v1/sandboxes/"+sandbox.ID+"/metadata", "",
		strconv.FormatInt(sandbox.Revision, 10), "",
		contracts.UpdateSandboxMetadataRequest{Metadata: map[string]string{}},
	)
	assertProblem(t, staleMetadataUpdate, http.StatusPreconditionFailed, "precondition_failed")
	sandbox = metadataUpdatedSandbox

	missingHeaders := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":start", "", "", "", nil,
	)
	assertProblem(t, missingHeaders, http.StatusBadRequest, "invalid_request")

	start := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":start", "lifecycle-http-start",
		strconv.FormatInt(sandbox.Revision, 10), "", nil,
	)
	if start.StatusCode != http.StatusAccepted || start.Header.Get("Idempotency-Replayed") != "false" {
		t.Fatalf("start status=%d replay=%q body=%s", start.StatusCode, start.Header.Get("Idempotency-Replayed"), readResponse(t, start))
	}
	var startOperation contracts.Operation
	decodeResponseJSON(t, start, &startOperation)
	replay := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":start", "lifecycle-http-start",
		strconv.FormatInt(sandbox.Revision, 10), "", nil,
	)
	if replay.StatusCode != http.StatusAccepted || replay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("start replay status=%d replay=%q body=%s", replay.StatusCode, replay.Header.Get("Idempotency-Replayed"), readResponse(t, replay))
	}
	var replayedOperation contracts.Operation
	decodeResponseJSON(t, replay, &replayedOperation)
	if replayedOperation.ID != startOperation.ID {
		t.Fatalf("start replay operation=%q want %q", replayedOperation.ID, startOperation.ID)
	}
	isolated := lifecycleHTTPRequest(
		t, server.URL, otherCredential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":start", "lifecycle-http-isolated",
		strconv.FormatInt(sandbox.Revision, 10), "", nil,
	)
	assertProblem(t, isolated, http.StatusNotFound, "not_found")

	operationResponse := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodGet,
		"/v1/operations/"+startOperation.ID, "", "", "", nil,
	)
	if operationResponse.StatusCode != http.StatusOK {
		t.Fatalf("operation GET status=%d body=%s", operationResponse.StatusCode, readResponse(t, operationResponse))
	}
	operationResponse.Body.Close()

	reloaded := getHTTPSandbox(t, server.URL, credential, sandbox.ID)
	for _, mutation := range []struct {
		method string
		action string
		key    string
		body   any
	}{
		{http.MethodPost, "drain", "lifecycle-http-drain", nil},
		{http.MethodPost, "stop", "lifecycle-http-stop", nil},
	} {
		response := lifecycleHTTPRequest(
			t, server.URL, credential, mutation.method,
			"/v1/sandboxes/"+sandbox.ID+":"+mutation.action, mutation.key,
			strconv.FormatInt(reloaded.Revision, 10), "", mutation.body,
		)
		assertProblem(
			t,
			response,
			http.StatusConflict,
			"workspace_mutation_conflict",
		)
	}
	deleteSandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "lifecycle-http-delete-create",
		contracts.CreateSandboxRequest{
			Profile: profile.Name, Metadata: map[string]string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, deleteSandbox.ID)
	deleteSandbox, err = controlPlane.GetSandbox(
		t.Context(),
		principal,
		deleteSandbox.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	deleteResponse := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodDelete,
		"/v1/sandboxes/"+deleteSandbox.ID,
		"lifecycle-http-delete",
		strconv.FormatInt(deleteSandbox.Revision, 10),
		"",
		nil,
	)
	if deleteResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status=%d body=%s", deleteResponse.StatusCode, readResponse(t, deleteResponse))
	}
	deleteResponse.Body.Close()
}

func TestHTTPRequestIDCorrelatesOperationAuditAndStructuredLog(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "request-correlation")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-request-correlation")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "request-correlation-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, sandbox.ID)
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}

	var logOutput bytes.Buffer
	handler, err := api.NewHandler(api.HandlerConfig{
		Service:                   controlPlane,
		PlatformToken:             testPlatformToken,
		Logger:                    slog.New(slog.NewJSONHandler(&logOutput, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	const requestID = "request-correlation-http-1"
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL+"/v1/sandboxes/"+sandbox.ID+":start",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, credential)
	request.Header.Set("Idempotency-Key", "request-correlation-start")
	request.Header.Set("If-Match", `"revision-`+strconv.FormatInt(sandbox.Revision, 10)+`"`)
	request.Header.Set("X-Request-ID", requestID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted || response.Header.Get("X-Request-ID") != requestID {
		t.Fatalf(
			"correlated start status=%d request=%q body=%s",
			response.StatusCode,
			response.Header.Get("X-Request-ID"),
			readResponse(t, response),
		)
	}
	var operation contracts.Operation
	decodeResponseJSON(t, response, &operation)
	if operation.RequestID != requestID {
		t.Fatalf("Operation requestId = %q, want %q", operation.RequestID, requestID)
	}

	auditEvents, err := databaseStore.ListAuditEvents(t.Context(), project.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var correlatedAudit bool
	for _, event := range auditEvents {
		if event.Action == "sandbox.start" && event.ResourceID == sandbox.ID {
			correlatedAudit = event.RequestID == requestID
		}
	}
	if !correlatedAudit {
		t.Fatalf("sandbox.start audit did not retain requestId %q: %#v", requestID, auditEvents)
	}
	logBytes := logOutput.Bytes()
	for _, required := range [][]byte{
		[]byte(`"msg":"SecondBox HTTP request completed"`),
		[]byte(`"request_id":"` + requestID + `"`),
		[]byte(`"method":"POST"`),
		[]byte(`"route":"POST /v1/sandboxes/{sandboxAction}"`),
		[]byte(`"status":202`),
		[]byte(`"duration_ms":`),
	} {
		if !bytes.Contains(logBytes, required) {
			t.Fatalf("structured request log lacks %s: %s", required, logBytes)
		}
	}
	if bytes.Contains(logBytes, []byte(credential)) {
		t.Fatal("structured request log exposed the bearer credential")
	}
}

func TestWaitInspectLeasePingAndTouchHTTPContract(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "activity-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-activity-http")
	_, _, otherCredential := createProjectAccountAndCredential(t, controlPlane, admin, "activity-http-other")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "activity-http-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	instanceID := "ins_activity_http"
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES ($1,$2,$3,'ready','ready','',$4,$4,$4,$4,$5,NULL)`,
		instanceID, sandbox.ID, sandbox.Generation, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes SET state='ready',desired_state='running',
		    current_instance_id=$2,last_activity_at=$3,updated_at=$3
		WHERE id=$1`,
		sandbox.ID, instanceID, now,
	); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)
	generation := strconv.FormatInt(sandbox.Generation, 10)

	wait := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":wait", "", "", "",
		map[string]any{"states": []string{"ready"}, "deadlineMilliseconds": 100},
	)
	if wait.StatusCode != http.StatusOK || wait.Header.Get("ETag") == "" {
		t.Fatalf("wait status=%d etag=%q body=%s", wait.StatusCode, wait.Header.Get("ETag"), readResponse(t, wait))
	}
	wait.Body.Close()
	expiredWait := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":wait", "", "", "",
		map[string]any{"states": []string{"deleted"}, "deadlineMilliseconds": 5},
	)
	assertProblem(t, expiredWait, http.StatusRequestTimeout, "wait_expired")

	inspect := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":inspect", "", "", generation, nil,
	)
	if inspect.StatusCode != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", inspect.StatusCode, readResponse(t, inspect))
	}
	inspect.Body.Close()
	isolatedInspect := lifecycleHTTPRequest(
		t, server.URL, otherCredential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":inspect", "", "", generation, nil,
	)
	assertProblem(t, isolatedInspect, http.StatusNotFound, "not_found")
	ping := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":ping", "", "", generation, nil,
	)
	if ping.StatusCode != http.StatusOK {
		t.Fatalf("ping status=%d body=%s", ping.StatusCode, readResponse(t, ping))
	}
	var pingResult contracts.PingResult
	decodeResponseJSON(t, ping, &pingResult)
	if !pingResult.Healthy || pingResult.Generation != sandbox.Generation {
		t.Fatalf("ping result=%#v", pingResult)
	}

	acquire := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+"/leases", "activity-http-acquire", "", generation,
		map[string]any{"durationSeconds": 30},
	)
	if acquire.StatusCode != http.StatusCreated {
		t.Fatalf("lease acquire status=%d body=%s", acquire.StatusCode, readResponse(t, acquire))
	}
	var lease contracts.Lease
	decodeResponseJSON(t, acquire, &lease)
	acquireReplay := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+"/leases", "activity-http-acquire", "", generation,
		map[string]any{"durationSeconds": 30},
	)
	if acquireReplay.StatusCode != http.StatusCreated {
		t.Fatalf("lease acquire replay status=%d body=%s", acquireReplay.StatusCode, readResponse(t, acquireReplay))
	}
	var replayedLease contracts.Lease
	decodeResponseJSON(t, acquireReplay, &replayedLease)
	if replayedLease.ID != lease.ID {
		t.Fatalf("lease replay=%q want %q", replayedLease.ID, lease.ID)
	}
	getLease := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodGet, "/v1/leases/"+lease.ID, "", "", "", nil,
	)
	if getLease.StatusCode != http.StatusOK {
		t.Fatalf("lease GET status=%d body=%s", getLease.StatusCode, readResponse(t, getLease))
	}
	getLease.Body.Close()
	isolatedLease := lifecycleHTTPRequest(
		t, server.URL, otherCredential, http.MethodGet, "/v1/leases/"+lease.ID, "", "", "", nil,
	)
	assertProblem(t, isolatedLease, http.StatusNotFound, "not_found")
	renew := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost, "/v1/leases/"+lease.ID+":renew",
		"activity-http-renew", "", "", map[string]any{"durationSeconds": 20},
	)
	if renew.StatusCode != http.StatusOK {
		t.Fatalf("lease renew status=%d body=%s", renew.StatusCode, readResponse(t, renew))
	}
	renew.Body.Close()
	touch := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":touch", "activity-http-touch", "", generation, nil,
	)
	if touch.StatusCode != http.StatusOK {
		t.Fatalf("touch without Lease status=%d body=%s", touch.StatusCode, readResponse(t, touch))
	}
	touch.Body.Close()
	touch = lifecycleHTTPRequestWithLease(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":touch", "activity-http-touch-with-lease",
		generation, lease.ID, nil,
	)
	if touch.StatusCode != http.StatusOK {
		t.Fatalf("touch status=%d body=%s", touch.StatusCode, readResponse(t, touch))
	}
	var touchResult contracts.TouchResult
	decodeResponseJSON(t, touch, &touchResult)
	touchReplay := lifecycleHTTPRequestWithLease(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":touch", "activity-http-touch-with-lease",
		generation, lease.ID, nil,
	)
	var replayedTouch contracts.TouchResult
	if touchReplay.StatusCode != http.StatusOK {
		t.Fatalf("touch replay status=%d body=%s", touchReplay.StatusCode, readResponse(t, touchReplay))
	}
	decodeResponseJSON(t, touchReplay, &replayedTouch)
	if !replayedTouch.LastActivityAt.Equal(touchResult.LastActivityAt) {
		t.Fatalf("touch replay time=%s want %s", replayedTouch.LastActivityAt, touchResult.LastActivityAt)
	}
	stalePing := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":ping", "", "", strconv.FormatInt(sandbox.Generation+1, 10), nil,
	)
	assertProblem(t, stalePing, http.StatusConflict, "generation_fenced")
	release := lifecycleHTTPRequest(
		t, server.URL, credential, http.MethodDelete, "/v1/leases/"+lease.ID,
		"activity-http-release", "", "", nil,
	)
	if release.StatusCode != http.StatusOK {
		t.Fatalf("lease release status=%d body=%s", release.StatusCode, readResponse(t, release))
	}
	release.Body.Close()
}

func lifecycleHTTPRequest(
	t *testing.T,
	baseURL string,
	credential string,
	method string,
	path string,
	idempotencyKey string,
	ifMatch string,
	generation string,
	body any,
) *http.Response {
	t.Helper()
	return lifecycleHTTPRequestWithLeaseAndRevision(
		t, baseURL, credential, method, path, idempotencyKey, ifMatch, generation, "", body,
	)
}

func lifecycleHTTPRequestWithLease(
	t *testing.T,
	baseURL string,
	credential string,
	method string,
	path string,
	idempotencyKey string,
	generation string,
	leaseID string,
	body any,
) *http.Response {
	t.Helper()
	return lifecycleHTTPRequestWithLeaseAndRevision(
		t, baseURL, credential, method, path, idempotencyKey, "", generation, leaseID, body,
	)
}

func lifecycleHTTPRequestWithLeaseAndRevision(
	t *testing.T,
	baseURL string,
	credential string,
	method string,
	path string,
	idempotencyKey string,
	ifMatch string,
	generation string,
	leaseID string,
	body any,
) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, reader)
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
	if ifMatch != "" {
		request.Header.Set("If-Match", `"revision-`+ifMatch+`"`)
	}
	if generation != "" {
		request.Header.Set("SecondBox-Generation", generation)
	}
	if leaseID != "" {
		request.Header.Set("SecondBox-Lease-ID", leaseID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func getHTTPSandbox(t *testing.T, baseURL, credential, sandboxID string) contracts.Sandbox {
	t.Helper()
	response := lifecycleHTTPRequest(
		t, baseURL, credential, http.MethodGet, "/v1/sandboxes/"+sandboxID, "", "", "", nil,
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET Sandbox status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	var sandbox contracts.Sandbox
	decodeResponseJSON(t, response, &sandbox)
	return sandbox
}

func decodeResponseJSON(t *testing.T, response *http.Response, destination any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatal(err)
	}
}

func assertProblem(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != status {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("problem status=%d want=%d body=%s", response.StatusCode, status, body)
	}
	if response.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("problem Content-Type=%q", response.Header.Get("Content-Type"))
	}
	var problem contracts.Problem
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != code {
		t.Fatalf("problem code=%q want=%q", problem.Code, code)
	}
}
