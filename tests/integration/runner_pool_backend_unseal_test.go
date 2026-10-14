package integration_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRunnerPoolBackendUnsealSwitchesASameIdentityRunnerToGVisor walks the
// operator path that moves one Runner identity from Firecracker to gVisor: the
// seal refuses the other backend, unseal refuses while the Runner or one of its
// Instances is live, and after unseal the same runner_id reseals the pool.
func TestRunnerPoolBackendUnsealSwitchesASameIdentityRunnerToGVisor(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	poolName := task4ID("unseal-pool")
	task4InsertRunnerPool(t, poolName, now)
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := newTask4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	databasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databasePool.Close)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	t.Cleanup(server.Close)
	client, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	register := func(runnerID, connectionID string, registration *runnerv1.RunnerRegistration) error {
		issued, err := authority.Issue(runnerID, task4CertificateRequest(t))
		if err != nil {
			return err
		}
		if err := stateStore.OpenConnection(t.Context(), issued.Identity, connectionID, 1, now); err != nil {
			return err
		}
		_, err = stateStore.RecordRegistration(t.Context(), registration, now)
		return err
	}
	gvisorRegistration := func(runnerID, connectionID string) *runnerv1.RunnerRegistration {
		registration := task4Registration(runnerID, connectionID, poolName)
		registration.BackendKind = runnerv1.ComputeBackendKind_COMPUTE_BACKEND_KIND_GVISOR
		registration.Materializations[0].BackendKind = registration.BackendKind
		registration.Materializations[0].SourceOciManifestDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		registration.Materializations[0].FlatRootDigest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		registration.Materializations[0].HelperBuildId = "runsc-release-20260817.0"
		return registration
	}
	poolBackend := func() string {
		t.Helper()
		var backendKind string
		if err := databasePool.QueryRow(t.Context(), `SELECT backend_kind FROM secondbox.runner_pools WHERE name=$1`, poolName).Scan(&backendKind); err != nil {
			t.Fatal(err)
		}
		return backendKind
	}
	unseal := func(expectedRevision int64) (secondboxclient.RunnerPool, error) {
		return client.UnsealRunnerPoolBackend(t.Context(), poolName, expectedRevision)
	}
	requireBackendInUse := func(err error, situation string) {
		t.Helper()
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) || apiError.StatusCode != 409 || apiError.Problem == nil ||
			apiError.Problem.Code != secondboxclient.ProblemCodeRunnerPoolBackendInUse {
			t.Fatalf("unseal %s error = %v, want 409 runner_pool_backend_in_use", situation, err)
		}
	}

	runnerID := task4ID("unseal-runner")
	firecrackerConnection := task4ID("unseal-firecracker-connection")
	if err := register(runnerID, firecrackerConnection, task4Registration(runnerID, firecrackerConnection, poolName)); err != nil {
		t.Fatal(err)
	}
	if kind := poolBackend(); kind != "firecracker" {
		t.Fatalf("pool backend after Firecracker registration = %q", kind)
	}
	otherRunner, otherConnection := task4ID("unseal-gvisor-other"), task4ID("unseal-gvisor-other-connection")
	if err := register(otherRunner, otherConnection, gvisorRegistration(otherRunner, otherConnection)); err == nil ||
		!strings.Contains(err.Error(), "sealed to a different compute backend") {
		t.Fatalf("gVisor registration into a Firecracker pool error = %v", err)
	}
	if err := stateStore.CloseConnection(t.Context(), otherRunner, otherConnection, now); err != nil {
		t.Fatal(err)
	}

	_, err = unseal(1)
	requireBackendInUse(err, "with a ready Runner")

	// A Runner whose heartbeat expired is offline but can still heartbeat back
	// to ready on its connection, so an active connection also refuses.
	if _, err := databasePool.Exec(t.Context(), `UPDATE secondbox.runners SET state='offline' WHERE id=$1`, runnerID); err != nil {
		t.Fatal(err)
	}
	_, err = unseal(1)
	requireBackendInUse(err, "with an active Runner connection")

	if err := stateStore.CloseConnection(t.Context(), runnerID, firecrackerConnection, now); err != nil {
		t.Fatal(err)
	}
	if _, err := databasePool.Exec(t.Context(), `
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			$1,$2,$3,$4,'unseal-profile-revision','firecracker','',1,$5,'uncertain','{}','{}','{}',
			'transient',0,3,$6,$6,'',$6,$6,1,$6,$6
		)`,
		pgx.QueryExecModeSimpleProtocol,
		task4ID("unseal-assignment"), task4ID("unseal-sandbox"), task4ID("unseal-instance"), runnerID,
		[]byte("01234567890123456789012345678901"), now,
	); err != nil {
		t.Fatal(err)
	}
	_, err = unseal(1)
	requireBackendInUse(err, "with a live Assignment")
	if _, err := databasePool.Exec(t.Context(), `
		UPDATE secondbox.assignments SET state='failed_terminal',failure_class='fencing'
		WHERE runner_id=$1`, runnerID); err != nil {
		t.Fatal(err)
	}
	_, err = unseal(1)
	requireBackendInUse(err, "after fencing exhausted without termination evidence")
	// A start that failed on the Runner's own report left no compute behind.
	if _, err := databasePool.Exec(t.Context(), `
		UPDATE secondbox.assignments SET failure_class='compatibility' WHERE runner_id=$1`, runnerID); err != nil {
		t.Fatal(err)
	}

	var apiError *secondboxclient.APIError
	if _, err := unseal(7); !errors.As(err, &apiError) || apiError.StatusCode != 412 {
		t.Fatalf("stale-revision unseal error = %v, want 412", err)
	}
	unsealed, err := unseal(1)
	if err != nil {
		t.Fatal(err)
	}
	if unsealed.Revision != 2 || !unsealed.UpdatedAt.Equal(now) || unsealed.Name != poolName {
		t.Fatalf("unsealed RunnerPool = %#v", unsealed)
	}
	if kind := poolBackend(); kind != "" {
		t.Fatalf("pool backend after unseal = %q", kind)
	}
	var audited bool
	if err := databasePool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM secondbox.audit_events
			WHERE action='runner_pool.backend_unsealed' AND resource_kind='runner_pool'
			  AND resource_id=$1 AND actor_kind='platform' AND outcome='accepted'
		)`, poolName,
	).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if !audited {
		t.Fatal("RunnerPool backend unseal audit event is absent")
	}

	gvisorConnection := task4ID("unseal-gvisor-connection")
	if err := register(runnerID, gvisorConnection, gvisorRegistration(runnerID, gvisorConnection)); err != nil {
		t.Fatalf("same Runner identity gVisor registration after unseal: %v", err)
	}
	if kind := poolBackend(); kind != "gvisor" {
		t.Fatalf("pool backend after gVisor re-registration = %q", kind)
	}
	var runnerBackend, runnerState string
	var cacheJSON []byte
	if err := databasePool.QueryRow(t.Context(), `
		SELECT backend_kind,state,artifact_cache_json FROM secondbox.runners WHERE id=$1`, runnerID,
	).Scan(&runnerBackend, &runnerState, &cacheJSON); err != nil {
		t.Fatal(err)
	}
	var cache struct {
		Materializations []struct {
			BackendKind string `json:"backendKind"`
		} `json:"materializations"`
	}
	if err := json.Unmarshal(cacheJSON, &cache); err != nil {
		t.Fatal(err)
	}
	if runnerBackend != "gvisor" || runnerState != "ready" ||
		len(cache.Materializations) != 1 || cache.Materializations[0].BackendKind != "gvisor" {
		t.Fatalf("re-registered Runner backend=%q state=%q cache=%s", runnerBackend, runnerState, cacheJSON)
	}

	firecrackerRunner, firecrackerOther := task4ID("unseal-firecracker-other"), task4ID("unseal-firecracker-other-connection")
	if err := register(firecrackerRunner, firecrackerOther, task4Registration(firecrackerRunner, firecrackerOther, poolName)); err == nil ||
		!strings.Contains(err.Error(), "sealed to a different compute backend") {
		t.Fatalf("Firecracker registration into the resealed gVisor pool error = %v", err)
	}
}

// TestRunnerPoolBackendUnsealRollsBackWithoutItsAuditEvent proves the seal, the
// revision and the timestamp change only together with the audit event.
func TestRunnerPoolBackendUnsealRollsBackWithoutItsAuditEvent(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	poolName := task4ID("unseal-audit-pool")
	task4InsertRunnerPool(t, poolName, now)
	databasePool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databasePool.Close)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	if _, err := databasePool.Exec(t.Context(), `UPDATE secondbox.runner_pools SET backend_kind='firecracker' WHERE name=$1`, poolName); err != nil {
		t.Fatal(err)
	}
	audit := func(id string) contracts.AuditEvent {
		return contracts.AuditEvent{
			ID: id, ActorKind: "platform", ActorID: "platform", Action: "runner_pool.backend_unsealed",
			ResourceKind: "runner_pool", ResourceID: poolName, Outcome: "accepted", RequestID: id, CreatedAt: now,
		}
	}
	collidingID := task4ID("unseal-audit-colliding")
	if _, err := databasePool.Exec(t.Context(), `
		INSERT INTO secondbox.audit_events (
			id,tenant_ref,subject_ref,actor_kind,actor_id,action,resource_kind,resource_id,
			outcome,request_id,details_json,created_at
		) VALUES ($1,'secondbox','platform','platform','platform','fixture','fixture',$1,'accepted',$1,'{}',$2)`,
		collidingID, now,
	); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Minute)
	if _, err := databaseStore.UnsealRunnerPoolBackend(t.Context(), poolName, 1, audit(collidingID), later); err == nil {
		t.Fatal("unseal with a failing audit insert succeeded")
	}
	var backendKind string
	var revision int64
	var updatedAt time.Time
	if err := databasePool.QueryRow(t.Context(), `
		SELECT backend_kind,revision,updated_at FROM secondbox.runner_pools WHERE name=$1`, poolName,
	).Scan(&backendKind, &revision, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if backendKind != "firecracker" || revision != 1 || !updatedAt.Equal(now) {
		t.Fatalf("pool after failed unseal: backend=%q revision=%d updatedAt=%s", backendKind, revision, updatedAt)
	}
	unsealed, err := databaseStore.UnsealRunnerPoolBackend(t.Context(), poolName, 1, audit(task4ID("unseal-audit-retry")), later)
	if err != nil {
		t.Fatalf("retry with the original revision: %v", err)
	}
	if unsealed.Revision != 2 || !unsealed.UpdatedAt.Equal(later) {
		t.Fatalf("unsealed RunnerPool = %#v", unsealed)
	}
}
