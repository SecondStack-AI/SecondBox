package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

type attributedExecFixture struct {
	controlPlane *service.ControlPlaneService
	store        *store.PostgresControlPlaneStore
	sandbox      contracts.Sandbox
	principal    contracts.Principal
	credential   string
	pool         *pgxpool.Pool
	now          time.Time
}

// seedAttributedExecSandbox readies a Sandbox whose pinned Profile permits
// attributed execution, pinned to a Tenant egress context, on a Runner that
// advertises per-exec attribution.
func seedAttributedExecSandbox(t *testing.T, suffix, transport string) attributedExecFixture {
	t.Helper()
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, suffix)
	profile := createGrantedProfileWithDataPlaneTransport(t, controlPlane, databaseStore, admin, account, "profile-"+suffix, transport)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, suffix+"-create", contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedDataPlaneReadyAssignment(t, sandbox, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.profile_revisions
		SET spec_json=jsonb_set(spec_json,'{network,requiresTenantEgressContext}','true') ||
		'{"attributedExecution":{"gateway":"gateway","maximumConnections":32}}'::jsonb
		WHERE id=$1`, sandbox.ProfileRevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET egress_context='installation' WHERE id=$1`, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO secondbox.runners
		SELECT (jsonb_populate_record(NULL::secondbox.runners,to_jsonb(runner) ||
		jsonb_build_object('id',$1::text,'name',$1::text,
		  'capabilities_json',runner.capabilities_json || '["per-exec-attribution"]'::jsonb))).*
		FROM secondbox.runners runner WHERE id=$2`, seed.RunnerID, "runner-fixture-profile-"+suffix); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM secondbox.runners WHERE id=$1`, seed.RunnerID); err != nil {
			t.Error(err)
		}
	})
	return attributedExecFixture{
		controlPlane: controlPlane, store: databaseStore, sandbox: sandbox, principal: principal,
		credential: credential, pool: pool, now: now,
	}
}

func TestAttributedExecAdmissionBindsOrdinaryGeneration(t *testing.T) {
	fixture := seedAttributedExecSandbox(t, "attributed-exec", contracts.DataPlaneTransportProxied)
	sandbox, principal, now := fixture.sandbox, fixture.principal, fixture.now
	relay, err := runnercontrol.NewPostgresDataPlaneStore(t.Context(), runnercontrol.PostgresDataPlaneStoreConfig{
		DatabaseURL: integrationDatabaseURL, Retention: time.Hour, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	admission := func(id string, attribution *contracts.AttributedExecutionRequest) runnercontrol.DataPlaneAdmission {
		return runnercontrol.DataPlaneAdmission{
			ID: id + sandbox.ID, StreamID: "stream-" + id + sandbox.ID,
			TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef, SandboxID: sandbox.ID,
			Generation: sandbox.Generation, RequestID: id, IdempotencyKey: id, RequestHash: id,
			Kind: "exec", Operation: "exec", DeadlineAt: now.Add(10 * time.Second), MaximumResponseBytes: 1024,
			ExecOpen:            &runnerv1.ExecOpen{Command: &runnerv1.ExecOpen_Shell{Shell: "true"}, DeadlineUnixMs: uint64(now.Add(10 * time.Second).UnixMilli()), OutputLimitBytes: 1024},
			AttributedExecution: attribution,
			Request:             map[string]string{"command": "true"}, Now: now,
		}
	}
	binding := func(reference string, lifetime time.Duration) *contracts.AttributedExecutionRequest {
		return &contracts.AttributedExecutionRequest{AuthorizationRef: reference, ExpiresAt: now.Add(lifetime)}
	}
	var invalidField *ports.InvalidFieldError
	for _, refused := range []struct {
		name  string
		input runnercontrol.DataPlaneAdmission
		field string
	}{
		{"expired", admission("expired", binding("expired", 0)), "attributedExecution.expiresAt"},
		{"beyond Profile deadline", admission("beyond", binding("beyond", 61*time.Second)), "attributedExecution.expiresAt"},
		{"deadline outlives expiry", admission("outlives", binding("outlives", 5*time.Second)), "deadlineMilliseconds"},
	} {
		if _, _, err := relay.AdmitDataPlane(t.Context(), refused.input); !errors.As(err, &invalidField) || invalidField.Field != refused.field {
			t.Fatalf("%s admission = %v; want field %s", refused.name, err, refused.field)
		}
	}
	pty := admission("pty", binding("pty", 20*time.Second))
	pty.ExecOpen.AllocatePty = true
	if _, _, err := relay.AdmitDataPlane(t.Context(), pty); err == nil {
		t.Fatal("admitted an attributed PTY")
	}

	// Attribution does not claim the generation: sequential and concurrent
	// attributed execs, ordinary execs, and file writes all share it.
	first, replayed, err := relay.AdmitDataPlane(t.Context(), admission("first", binding("command-first", 20*time.Second)))
	if err != nil || replayed || first.AttributedExecution == nil || first.AttributedExecution.AuthorizationRef != "command-first" ||
		!first.AttributedExecution.ExpiresAt.Equal(now.Add(20*time.Second)) {
		t.Fatalf("first attributed admission = %+v replayed=%v error=%v", first.AttributedExecution, replayed, err)
	}
	second, _, err := relay.AdmitDataPlane(t.Context(), admission("second", binding("command-second", 30*time.Second)))
	if err != nil || second.AttributedExecution == nil || second.AttributedExecution.AuthorizationRef != "command-second" {
		t.Fatalf("second attributed admission = %+v error=%v", second.AttributedExecution, err)
	}
	if _, _, err := relay.AdmitDataPlane(t.Context(), admission("ordinary", nil)); err != nil {
		t.Fatalf("ordinary exec beside attributed execs = %v", err)
	}
	write := admission("write", nil)
	write.Kind, write.Operation, write.ExecOpen = "file", "write", nil
	write.FileOpen = &runnerv1.FileOpen{Operation: runnerv1.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "file", ExpectedSize: 1}
	write.FileContent = []byte("x")
	if _, _, err := relay.AdmitDataPlane(t.Context(), write); err != nil {
		t.Fatalf("file write beside attributed execs = %v", err)
	}
	replay, found, err := relay.AdmitDataPlane(t.Context(), admission("first", binding("command-first", 20*time.Second)))
	if err != nil || !found || replay.ID != first.ID || replay.AttributedExecution == nil ||
		replay.AttributedExecution.AuthorizationRef != "command-first" {
		t.Fatalf("attributed replay = %+v found=%v error=%v", replay, found, err)
	}
	stored, err := relay.GetDataPlaneSession(t.Context(), principal.TenantRef, principal.SubjectRef, second.ID)
	if err != nil || stored.AttributedExecution == nil || *stored.AttributedExecution != *second.AttributedExecution {
		t.Fatalf("stored attribution = %+v error=%v", stored.AttributedExecution, err)
	}

	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners
		SET capabilities_json=capabilities_json - 'per-exec-attribution'
		WHERE id=(SELECT runner_id FROM secondbox.assignments WHERE sandbox_id=$1)`, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := relay.AdmitDataPlane(t.Context(), admission("unsupported", binding("unsupported", 20*time.Second))); !errors.Is(err, ports.ErrHomeRunnerUnavailable) {
		t.Fatalf("admission on an unsupported Runner = %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.profile_revisions SET spec_json=spec_json - 'attributedExecution' WHERE id=$1`, sandbox.ProfileRevisionID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := relay.AdmitDataPlane(t.Context(), admission("unpermitted", binding("unpermitted", 20*time.Second))); !errors.As(err, &invalidField) || invalidField.Field != "attributedExecution" {
		t.Fatalf("admission without Profile permission = %v", err)
	}
}

func TestAttributedExecHTTPAdmissionAndReplay(t *testing.T) {
	fixture := seedAttributedExecSandbox(t, "attributed-exec-http", contracts.DataPlaneTransportDirect)
	sandbox, credential := fixture.sandbox, fixture.credential
	relay, err := runnercontrol.NewPostgresDataPlaneStore(t.Context(), runnercontrol.PostgresDataPlaneStoreConfig{
		DatabaseURL: integrationDatabaseURL, Retention: time.Hour, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	dataPlaneService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: fixture.store, PlatformToken: testPlatformToken,
		Now: func() time.Time { return fixture.now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneStore:        relay, DataPlanePollInterval: time.Millisecond,
		LiveDataPlane: runnercontrol.NewLiveDataPlaneBroker(),
		PublicBaseURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: dataPlaneService, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The contract checker does not implement the WebSocket-frame schema
	// extension of the 201 exec-stream response.
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint := server.URL + "/v1/sandboxes/" + sandbox.ID + "/exec-streams"
	request := func(attribution any) map[string]any {
		return map[string]any{
			"command":     map[string]any{"mode": "shell", "command": "true"},
			"environment": map[string]string{}, "deadlineMilliseconds": 10000,
			"maximumOutputBytes": 16, "windowBytes": 4096,
			"attributedExecution": attribution,
		}
	}
	expiresAt := fixture.now.Add(45 * time.Second).Format(time.RFC3339)
	for name, invalid := range map[string]map[string]any{
		"null":             request(nil),
		"empty":            request(map[string]any{}),
		"caller gateway":   request(map[string]any{"authorizationRef": "command", "expiresAt": expiresAt, "gateway": "caller-selected"}),
		"spaced reference": request(map[string]any{"authorizationRef": " command", "expiresAt": expiresAt}),
	} {
		t.Run(name, func(t *testing.T) {
			assertProblem(t, dataPlaneJSONRequest(t, endpoint, credential, sandbox.Generation, "attributed-http-invalid", invalid),
				http.StatusBadRequest, "invalid_request")
		})
	}
	body := request(map[string]any{"authorizationRef": "command-http", "expiresAt": expiresAt})
	var sessionID string
	for _, replay := range []string{"false", "true"} {
		response := dataPlaneJSONRequest(t, endpoint, credential, sandbox.Generation, "attributed-http-stream", body)
		if response.StatusCode != http.StatusCreated || response.Header.Get("Idempotency-Replayed") != replay {
			responseBody, _ := io.ReadAll(response.Body)
			t.Fatalf("attributed stream status=%d replay=%q body=%s", response.StatusCode, response.Header.Get("Idempotency-Replayed"), responseBody)
		}
		var session contracts.ExecStreamSession
		decodeHTTPJSON(t, response, &session)
		if replay == "true" && session.ID != sessionID {
			t.Fatal("replay changed the exec session")
		}
		sessionID = session.ID
	}
	stored, err := dataPlaneService.GetSandboxExecStream(t.Context(), fixture.principal, sandbox.ID, sessionID, sandbox.Generation)
	if err != nil || stored.AttributedExecution == nil || stored.AttributedExecution.AuthorizationRef != "command-http" ||
		stored.Transport != contracts.DataPlaneTransportProxied {
		t.Fatalf("stored attributed stream = %+v transport=%q error=%v", stored.AttributedExecution, stored.Transport, err)
	}
	assertProblem(t, dataPlaneJSONRequest(t, endpoint, credential, sandbox.Generation, "attributed-http-stream",
		request(map[string]any{"authorizationRef": "different-command", "expiresAt": expiresAt})),
		http.StatusConflict, "idempotency_conflict")
}
