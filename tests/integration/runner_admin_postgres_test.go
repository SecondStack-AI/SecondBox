package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunnerPoolAdministrationIsAuditedAndRevisionGuarded(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)

	created, err := controlPlane.CreateRunnerPool(
		t.Context(),
		admin,
		contracts.CreateRunnerPoolRequest{
			Name:          "qualified-amd64",
			State:         contracts.RunnerPoolStateReady,
			Architectures: []string{"amd64"},
			Capabilities:  []string{"checkpoint", "firecracker"},
			CapacityPolicy: map[string]int64{
				"maximumInstances": 32,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.ReadyRunnerCount != 0 {
		t.Fatalf("created RunnerPool = %#v", created)
	}

	listed, err := controlPlane.ListRunnerPools(t.Context(), admin, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var listedCreated bool
	for _, runnerPool := range listed.Items {
		listedCreated = listedCreated || runnerPool.Name == created.Name
	}
	if !listedCreated {
		t.Fatalf("listed RunnerPools = %#v", listed)
	}

	draining := contracts.RunnerPoolStateDraining
	updated, err := controlPlane.UpdateRunnerPool(
		t.Context(),
		admin,
		created.Name,
		contracts.UpdateRunnerPoolRequest{State: &draining},
		created.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != contracts.RunnerPoolStateDraining || updated.Revision != 2 {
		t.Fatalf("updated RunnerPool = %#v", updated)
	}
	if _, err := controlPlane.UpdateRunnerPool(
		t.Context(),
		admin,
		created.Name,
		contracts.UpdateRunnerPoolRequest{State: &draining},
		created.Revision,
	); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale RunnerPool revision error = %v, want ErrRevisionConflict", err)
	}

	project, _, credential := createProjectAccountAndCredential(t, controlPlane, admin, "runner-admin-denied")
	principal := authenticateCredential(t, controlPlane, credential)
	if principal.ProjectID != project.ID {
		t.Fatalf("application Principal project = %q, want %q", principal.ProjectID, project.ID)
	}
	if _, err := controlPlane.ListRunnerPools(t.Context(), principal, 10, ""); err != nil {
		t.Fatalf("platform RunnerPool list error = %v", err)
	}

	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var createdAudit, updatedAudit bool
	err = pool.QueryRow(t.Context(), `
		SELECT
			EXISTS (
				SELECT 1
				FROM secondbox.audit_events
				WHERE action = 'runner_pool.created'
				  AND resource_kind = 'runner_pool'
				  AND resource_id = $1
				  AND actor_kind = 'platform'
				  AND outcome = 'accepted'
			),
			EXISTS (
				SELECT 1
				FROM secondbox.audit_events
				WHERE action = 'runner_pool.updated'
				  AND resource_kind = 'runner_pool'
				  AND resource_id = $1
				  AND actor_kind = 'platform'
				  AND outcome = 'accepted'
			)`,
		created.Name,
	).Scan(&createdAudit, &updatedAudit)
	if err != nil {
		t.Fatal(err)
	}
	if !createdAudit || !updatedAudit {
		t.Fatalf("RunnerPool audit evidence: created=%t updated=%t", createdAudit, updatedAudit)
	}
}

func TestRunnerAdministrationProjectsIdentityWithoutCredentialMaterial(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_pools (
			name,state,architectures_json,capabilities_json,capacity_policy_json,
			ready_runner_count,revision,created_at,updated_at
		) VALUES ('runner-admin-pool','ready','["amd64"]','["firecracker"]',
		          '{"maxInstances":4}',1,1,$1,$1)`,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,last_seen_at,revision,
			created_at,updated_at
		) VALUES (
			'runner-admin-1','runner-admin-pool','qualified-runner','ready',
			'["amd64"]','["firecracker"]','{"instances":4}','["1"]',
			1,1,'1.0.0','connection-admin',7,'active','{}','[]',$1,3,$1,$1
		)`,
		now,
	); err != nil {
		t.Fatal(err)
	}
	runner, err := controlPlane.GetRunner(t.Context(), admin, "runner-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if runner.CredentialState != "pre_shared" || runner.PoolName != "runner-admin-pool" ||
		runner.Capacity["instances"] != 4 {
		t.Fatalf("Runner administrative projection = %#v", runner)
	}
	runners, err := controlPlane.ListRunners(t.Context(), admin, "runner-admin-pool", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runners.Items) != 1 || runners.Items[0].ID != runner.ID {
		t.Fatalf("Runner administrative list = %#v", runners)
	}
}

func TestRunnerPoolAdministrationIsAvailableThroughPublicHTTPContract(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
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

	createdResponse := authenticatedJSONRequest(
		t,
		http.MethodPost,
		server.URL+"/v1/runner-pools",
		"bootstrap-administrator-secret",
		"runner-pool-http-create",
		contracts.CreateRunnerPoolRequest{
			Name:           "http-runner-pool",
			State:          contracts.RunnerPoolStateReady,
			Architectures:  []string{"amd64"},
			Capabilities:   []string{"firecracker"},
			CapacityPolicy: map[string]int64{"maxInstances": 4},
		},
	)
	if createdResponse.StatusCode != http.StatusCreated {
		t.Fatalf(
			"POST /v1/runner-pools status = %d body=%s",
			createdResponse.StatusCode,
			readResponse(t, createdResponse),
		)
	}
	var created contracts.RunnerPool
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	createdResponse.Body.Close()
	if created.Name != "http-runner-pool" || createdResponse.Header.Get("ETag") != `"revision-1"` {
		t.Fatalf("created RunnerPool = %#v headers=%v", created, createdResponse.Header)
	}

	drainingState := contracts.RunnerPoolStateDraining
	updateBody, err := json.Marshal(contracts.UpdateRunnerPoolRequest{State: &drainingState})
	if err != nil {
		t.Fatal(err)
	}
	updateRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPatch,
		server.URL+"/v1/runner-pools/http-runner-pool",
		bytes.NewReader(updateBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, updateRequest, "bootstrap-administrator-secret")
	updateRequest.Header.Set("Content-Type", "application/json")
	updateRequest.Header.Set("Idempotency-Key", "runner-pool-http-update")
	updateRequest.Header.Set("If-Match", `"revision-1"`)
	updateResponse, err := http.DefaultClient.Do(updateRequest)
	if err != nil {
		t.Fatal(err)
	}
	if updateResponse.StatusCode != http.StatusOK {
		t.Fatalf(
			"PATCH /v1/runner-pools status = %d body=%s",
			updateResponse.StatusCode,
			readResponse(t, updateResponse),
		)
	}
	var updated contracts.RunnerPool
	if err := json.NewDecoder(updateResponse.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	updateResponse.Body.Close()
	if updated.State != contracts.RunnerPoolStateDraining || updateResponse.Header.Get("ETag") != `"revision-2"` {
		t.Fatalf("updated RunnerPool = %#v headers=%v", updated, updateResponse.Header)
	}
}
