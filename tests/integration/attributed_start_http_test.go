package integration_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAttributedStartHTTPAdmissionAndReplay(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "attributed-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-attributed-http")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, "attributed-http-create", contracts.CreateSandboxRequest{
		Profile: profile.Name, Metadata: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, sandbox.ID)
	sandbox, err = controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var runnerID string
	var capabilities []byte
	if err := pool.QueryRow(t.Context(), `SELECT runner.id,runner.capabilities_json
		FROM secondbox.workspaces workspace JOIN secondbox.runners runner ON runner.id=workspace.home_runner_id
		WHERE workspace.sandbox_id=$1`, sandbox.ID).Scan(&runnerID, &capabilities); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `UPDATE secondbox.runners SET capabilities_json=$2 WHERE id=$1`, runnerID, capabilities); err != nil {
			t.Error(err)
		}
	})
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.runners
		SET capabilities_json=capabilities_json || '["attributed-execution"]'::jsonb WHERE id=$1`, runnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.profile_revisions
		SET spec_json=jsonb_set(spec_json,'{network,requiresTenantEgressContext}','true') ||
		'{"attributedExecution":{"gateway":"gateway","maximumConnections":32}}'::jsonb
		WHERE id=$1`, sandbox.ProfileRevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET egress_context='installation' WHERE id=$1`, sandbox.ID); err != nil {
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
	for _, invalid := range []any{
		map[string]any{"attributedExecution": nil},
		map[string]any{"attributedExecution": map[string]any{}},
		map[string]any{"gateway": "caller-selected"},
		map[string]any{"attributedExecution": map[string]any{"authorizationRef": "command", "expiresAt": "2026-07-28T12:00:45Z", "gateway": "caller-selected"}},
	} {
		response := lifecycleHTTPRequest(t, server.URL, credential, http.MethodPost,
			"/v1/sandboxes/"+sandbox.ID+":start", "attributed-http-invalid", strconv.FormatInt(sandbox.Revision, 10), "", invalid)
		assertProblem(t, response, http.StatusBadRequest, "invalid_request")
	}
	body := contracts.StartSandboxRequest{AttributedExecution: &contracts.AttributedExecutionRequest{
		AuthorizationRef: "command-http", ExpiresAt: time.Date(2026, 7, 28, 12, 0, 45, 0, time.UTC),
	}}
	var operationID string
	for _, replay := range []bool{false, true} {
		response := lifecycleHTTPRequest(t, server.URL, credential, http.MethodPost,
			"/v1/sandboxes/"+sandbox.ID+":start", "attributed-http-start", strconv.FormatInt(sandbox.Revision, 10), "", body)
		if response.StatusCode != http.StatusAccepted || response.Header.Get("Idempotency-Replayed") != strconv.FormatBool(replay) {
			t.Fatalf("attributed start status=%d replay=%q body=%s", response.StatusCode, response.Header.Get("Idempotency-Replayed"), readResponse(t, response))
		}
		var operation contracts.Operation
		decodeResponseJSON(t, response, &operation)
		if replay && operation.ID != operationID {
			t.Fatal("replay changed the Operation")
		}
		operationID = operation.ID
	}
	stored, err := databaseStore.GetOperation(t.Context(), principal.TenantRef, principal.SubjectRef, operationID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := contracts.ParseAttributedExecutionMetadata(stored.RequestMetadata)
	if err != nil || actual == nil || *actual != *body.AttributedExecution {
		t.Fatalf("stored attribution=%+v error=%v", actual, err)
	}
	body.AttributedExecution.AuthorizationRef = "different-command"
	conflict := lifecycleHTTPRequest(t, server.URL, credential, http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+":start", "attributed-http-start", strconv.FormatInt(sandbox.Revision, 10), "", body)
	assertProblem(t, conflict, http.StatusConflict, "idempotency_conflict")
}
