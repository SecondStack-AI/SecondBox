package subjectcleanup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	controlstore "github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var cleanupTestDatabaseURL string

func TestMain(m *testing.M) {
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		fmt.Fprintln(os.Stderr, "SECONDBOX_TEST_DATABASE_URL is required for Subject cleanup tests")
		os.Exit(2)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	databaseName := fmt.Sprintf("secondbox_subject_cleanup_test_%d", os.Getpid())
	adminURL := *parsed
	adminURL.Path = "/postgres"
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		panic(err)
	}
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		panic(err)
	}
	parsed.Path = "/" + databaseName
	cleanupTestDatabaseURL = parsed.String()
	if err := postgresmigrations.Apply(ctx, cleanupTestDatabaseURL); err != nil {
		panic(err)
	}
	code := m.Run()
	if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
		panic(err)
	}
	if err := admin.Close(ctx); err != nil {
		panic(err)
	}
	os.Exit(code)
}

func TestExpiryCleanupSurvivesRestartAndKeepsAnotherTenantUsable(t *testing.T) {
	pool, databaseURL := cleanupTestPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprintf("%d", now.UnixNano())
	expiredTenant := "expiry-cleanup-tenant-" + suffix
	expiredSubject := "expiry-cleanup-subject-" + suffix
	otherTenant := "expiry-cleanup-other-tenant-" + suffix
	otherSubject := "expiry-cleanup-other-subject-" + suffix
	insertCleanupTestSubject(t, pool, expiredTenant, expiredSubject, "active", "none", now.Add(-time.Second), now)
	insertCleanupTestSubject(t, pool, otherTenant, otherSubject, "active", "none", now.Add(time.Hour), now)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.application_authorities (
			id,lookup_id,tenant_ref,subject_ref,state,scopes_json,profile_grants_json,
			metadata_json,expires_at,revision,token_verifier_sha256,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'active','[]','[]','{}',$5,1,$6,$7,$7)`,
		"expiry-cleanup-authority-"+suffix, "apa_expiry_cleanup_"+suffix,
		expiredTenant, expiredSubject, now.Add(time.Hour), make([]byte, 32), now,
	); err != nil {
		t.Fatal(err)
	}

	worker := newCleanupTestWorker(t, databaseURL, "expiry-worker-one-"+suffix)
	if found, err := worker.RunOnce(t.Context(), now); err != nil || !found {
		t.Fatalf("expiry cleanup first pass found=%t error=%v", found, err)
	}
	worker.Close()
	worker = newCleanupTestWorker(t, databaseURL, "expiry-worker-two-"+suffix)
	defer worker.Close()
	for index := 0; index < 64; index++ {
		if _, err := worker.RunOnce(t.Context(), now.Add(time.Duration(index+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := pool.QueryRow(t.Context(), `
			SELECT cleanup_state FROM secondbox.subjects WHERE tenant_ref=$1 AND ref=$2`,
			expiredTenant, expiredSubject,
		).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "succeeded" {
			break
		}
	}

	var expiredState, cleanupState, cleanupOperationID string
	if err := pool.QueryRow(t.Context(), `
		SELECT state,cleanup_state,cleanup_operation_id FROM secondbox.subjects
		WHERE tenant_ref=$1 AND ref=$2`, expiredTenant, expiredSubject,
	).Scan(&expiredState, &cleanupState, &cleanupOperationID); err != nil {
		t.Fatal(err)
	}
	if expiredState != "expired" || cleanupState != "succeeded" || cleanupOperationID == "" {
		t.Fatalf("expired Subject state=%q cleanup=%q operation=%q", expiredState, cleanupState, cleanupOperationID)
	}
	var operationState string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM secondbox.operations WHERE id=$1`, cleanupOperationID).Scan(&operationState); err != nil {
		t.Fatal(err)
	}
	if operationState != "succeeded" {
		t.Fatalf("expiry cleanup Operation state = %q", operationState)
	}
	var authorityState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.application_authorities WHERE id=$1`,
		"expiry-cleanup-authority-"+suffix,
	).Scan(&authorityState); err != nil {
		t.Fatal(err)
	}
	if authorityState != "revoked" {
		t.Fatalf("expired Subject authority state = %q", authorityState)
	}
	var otherState, otherCleanup string
	if err := pool.QueryRow(t.Context(), `
		SELECT state,cleanup_state FROM secondbox.subjects WHERE tenant_ref=$1 AND ref=$2`,
		otherTenant, otherSubject,
	).Scan(&otherState, &otherCleanup); err != nil {
		t.Fatal(err)
	}
	if otherState != "active" || otherCleanup != "none" {
		t.Fatalf("isolated tenant Subject state=%q cleanup=%q", otherState, otherCleanup)
	}
}

func TestCleanupCancelsActiveWorkAndContinuesPartialSandboxDeletion(t *testing.T) {
	pool, databaseURL := cleanupTestPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprintf("%d", now.UnixNano())
	tenantRef := "active-cleanup-tenant-" + suffix
	subjectRef := "active-cleanup-subject-" + suffix
	operationID := "op_active_cleanup_" + suffix
	activeSandbox := "sandbox-active-cleanup-" + suffix
	deletedSandbox := "sandbox-deleted-cleanup-" + suffix
	insertCleanupTestSubject(t, pool, tenantRef, subjectRef, "closed", "pending", now.Add(time.Hour), now)
	insertCleanupTestOperation(t, pool, operationID, tenantRef, subjectRef, "pending", now)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.subjects SET cleanup_operation_id=$3
		WHERE tenant_ref=$1 AND ref=$2;
		INSERT INTO secondbox.subject_cleanup_operations (
			operation_id,tenant_ref,subject_ref,stage,reconcile_owner,
			reconcile_claim_expires_at,next_reconcile_at,retry_count,retry_limit,
			created_at,updated_at
		) VALUES ($3,$1,$2,'cancel_work','',$4,$4,0,20,$4,$4);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,created_at,updated_at
		) VALUES
			($5,$1,$2,$6,'runner-active','ready',1024,3,'','','','',3,3,'','{}',$4,$4),
			($7,$1,$2,$8,'runner-deleted','deleted',1024,1,'','','','',1,1,'','{}',$4,$4);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES
			($6,$1,$2,'profile','revision','ready','running',3,$5,'','{}','{}',1,$4,$4),
			($8,$1,$2,'profile','revision','deleted','deleted',1,$7,'','{}','{}',1,$4,$4);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,started_at,updated_at
		) VALUES
			($9,$1,$2,$6,'','start','running',$10,'{}','','',false,$4,$4,$4),
			($11,$1,$2,$6,'','delete','running',$12,'{}','','',false,$4,$4,$4)`,
		pgx.QueryExecModeSimpleProtocol,
		tenantRef, subjectRef, operationID, now,
		"workspace-active-cleanup-"+suffix, activeSandbox,
		"workspace-deleted-cleanup-"+suffix, deletedSandbox,
		"operation-active-cleanup-"+suffix, "request-active-cleanup-"+suffix,
		"operation-existing-delete-"+suffix, "request-existing-delete-"+suffix,
	); err != nil {
		t.Fatal(err)
	}
	canceller := &recordingCleanupCanceller{}
	worker, err := NewWorker(
		t.Context(), databaseURL, canceller, "active-worker-"+suffix, time.Minute, time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	for index := 0; index < 3; index++ {
		if found, err := worker.RunOnce(t.Context(), now.Add(time.Duration(index)*time.Second)); err != nil || !found {
			t.Fatalf("cleanup pass %d found=%t error=%v", index, found, err)
		}
	}
	if len(canceller.calls) != 1 || canceller.calls[0] != activeSandbox+"/3" {
		t.Fatalf("active session cancellation calls = %#v", canceller.calls)
	}
	var activeState, deleteState string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM secondbox.operations WHERE id=$1`,
		"operation-active-cleanup-"+suffix).Scan(&activeState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT state FROM secondbox.operations WHERE id=$1`,
		"operation-existing-delete-"+suffix).Scan(&deleteState); err != nil {
		t.Fatal(err)
	}
	if activeState != "cancelled" || deleteState != "running" {
		t.Fatalf("concurrent Operation states active=%q delete=%q", activeState, deleteState)
	}
	var desiredState string
	if err := pool.QueryRow(t.Context(), `SELECT desired_state FROM secondbox.sandboxes WHERE id=$1`, activeSandbox).Scan(&desiredState); err != nil {
		t.Fatal(err)
	}
	if desiredState != "deleted" {
		t.Fatalf("remaining Sandbox desired state = %q", desiredState)
	}
	var cleanupDeletes int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.operations
		WHERE tenant_ref=$1 AND subject_ref=$2 AND kind='delete' AND id<>$3`,
		tenantRef, subjectRef, "operation-existing-delete-"+suffix,
	).Scan(&cleanupDeletes); err != nil {
		t.Fatal(err)
	}
	if cleanupDeletes != 1 {
		t.Fatalf("cleanup-created delete Operations = %d", cleanupDeletes)
	}
}

func TestConcurrentExecAdmissionLifecycleAndCleanupHaveNoDatabaseContentionErrors(t *testing.T) {
	pool, databaseURL := cleanupTestPool(t)
	now := time.Now().UTC()
	suffix := fmt.Sprintf("%d", now.UnixNano())
	tenantRef := "contention-tenant-" + suffix
	subjectRef := "contention-subject-" + suffix
	operationID := "op_contention_cleanup_" + suffix
	sandboxID := "sandbox-contention-" + suffix
	workspaceID := "workspace-contention-" + suffix
	runnerID := "runner-contention-" + suffix
	profileRevisionID := "revision-contention-" + suffix
	instanceID := "instance-contention-" + suffix
	assignmentID := "assignment-contention-" + suffix
	connectionID := "connection-contention-" + suffix
	insertCleanupTestSubject(t, pool, tenantRef, subjectRef, "closed", "pending", now.Add(time.Hour), now)
	insertCleanupTestOperation(t, pool, operationID, tenantRef, subjectRef, "pending", now)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.tenants (
			ref,state,allowed_profile_grants_json,allowed_application_scopes_json,
			aggregate_quota_json,expiry_policy_json,metadata_json,revision,created_at,updated_at
		) VALUES ($1,'active','[]','[]',
			'{"maxSandboxes":100,"maxActiveInstances":100,"maxCpuMillis":100000,"maxMemoryBytes":1099511627776,"maxSnapshots":100,"maxPortSessions":100,"maxConcurrentOperations":100,"maxActiveSubjects":100,"maxApplicationAuthorities":100}',
			'{"maximumSubjectLifetimeSeconds":3600,"maximumAuthorityLifetimeSeconds":3600}',
			'{}',1,$2,$2);
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ($3,'contention-profile-' || $9,1,
			'{"pool":"contention-pool","architecture":"amd64","resources":{"cpuMillis":1000,"memoryBytes":1073741824,"workspaceBytes":1073741824,"processLimit":128,"concurrentOperations":1024},"startup":{"mode":"cold_boot"},"lifecycle":{"initialState":"stopped","drainGraceSeconds":30,"idleSeconds":300,"maximumDurationSeconds":3600,"leaseSeconds":60},"retention":{"snapshotLimit":8,"snapshotRetentionSeconds":86400},"execution":{"maximumDeadlineMilliseconds":60000,"maximumBufferedOutputBytes":1048576,"streamWindowBytes":65536,"maximumTransferBytes":1048576,"terminalDetachSeconds":30,"dataPlaneTransport":"proxied"}}',$2);
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES ($4,'contention-pool',$4,'ready','["amd64"]','["compute","local-workspace"]',
			'{}','[1]',1,1,'test',$11,0,'active','{}','[]',0,0,$2,1,$2,$2);
		INSERT INTO secondbox.runner_connections (
			id,runner_id,credential_serial,protocol_version,state,last_sequence,
			last_control_sequence,connected_at,last_seen_at,disconnected_at
		) VALUES ($11,$4,'serial-contention-' || $9,1,'active',0,0,$2,$2,NULL);
		UPDATE secondbox.subjects SET cleanup_operation_id=$5
		WHERE tenant_ref=$1 AND ref=$6;
		INSERT INTO secondbox.subject_cleanup_operations (
			operation_id,tenant_ref,subject_ref,stage,reconcile_owner,
			reconcile_claim_expires_at,next_reconcile_at,retry_count,retry_limit,
			created_at,updated_at
		) VALUES ($5,$1,$6,'release_resources','',$2,$2,0,20,$2,$2);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,created_at,updated_at
		) VALUES ($7,$1,$6,$8,$4,'ready',1073741824,1,'','','','',NULL,NULL,'','{}',$2,$2);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES ($8,$1,$6,'contention-profile-' || $9,$3,'ready','running',1,$7,$12,'{}','{}',1,$2,$2);
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES ($12,$8,1,'ready','ready','',$2,$2,$2,$2,$2::timestamptz + interval '1 hour',NULL);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES ($13,$8,$12,$4,$3,'firecracker','contention-backend',1,$14,'ready',
			'{}','{}','{}','',0,8,$2::timestamptz + interval '1 hour',$2::timestamptz + interval '1 hour','',
			$2::timestamptz + interval '1 hour',$2::timestamptz + interval '1 hour',1,$2,$2);
		UPDATE secondbox.tenant_quotas
		SET max_concurrent_operations=2048,max_application_authorities=2048
		WHERE tenant_ref=$1;
		UPDATE secondbox.subject_quotas SET max_concurrent_operations=2048
		WHERE tenant_ref=$1 AND subject_ref=$6;
		INSERT INTO secondbox.application_authorities (
			id,lookup_id,tenant_ref,subject_ref,state,scopes_json,profile_grants_json,
			metadata_json,expires_at,revision,token_verifier_sha256,created_at,updated_at
		)
		SELECT 'contention-authority-' || value || '-' || $9,
		       'apa_contention_' || value || '_' || $9,$1,$6,'active','[]','[]','{}',
		       $2::timestamptz - interval '1 second',1,$10,$2,$2
		FROM generate_series(1,64) AS value`,
		pgx.QueryExecModeSimpleProtocol,
		tenantRef, now, profileRevisionID, runnerID, operationID, subjectRef,
		workspaceID, sandboxID, suffix, make([]byte, 32), connectionID,
		instanceID, assignmentID, []byte("01234567890123456789012345678901"),
	); err != nil {
		t.Fatal(err)
	}
	controlPlaneStore, err := controlstore.NewPostgresControlPlaneStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer controlPlaneStore.Close()
	dataPlaneStore, err := runnercontrol.NewPostgresDataPlaneStore(
		t.Context(),
		runnercontrol.PostgresDataPlaneStoreConfig{
			DatabaseURL: databaseURL, Retention: time.Hour, MaximumSessionBytes: 4 << 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer dataPlaneStore.Close()
	worker := newCleanupTestWorker(t, databaseURL, "contention-worker-"+suffix)
	defer worker.Close()

	admission := func(index int, observedAt time.Time) runnercontrol.DataPlaneAdmission {
		identity := fmt.Sprintf("%d-%s", index, suffix)
		return runnercontrol.DataPlaneAdmission{
			ID: "session-contention-" + identity, StreamID: "stream-contention-" + identity,
			TenantRef: tenantRef, SubjectRef: subjectRef, SandboxID: sandboxID,
			Generation: 1, Kind: "exec", Operation: "exec",
			RequestID:      "request-exec-contention-" + identity,
			IdempotencyKey: "exec-contention-" + identity,
			RequestHash:    "exec-contention-hash-" + identity,
			DeadlineAt:     observedAt.Add(time.Minute), MaximumResponseBytes: 1024,
			ExecOpen: &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "true"},
				DeadlineUnixMs:   uint64(observedAt.Add(time.Minute).UnixMilli()),
				OutputLimitBytes: 1024,
			},
			Now: observedAt,
		}
	}

	// Force the exact qualified-host interleaving. Admission must wait on the
	// Tenant quota row without already owning the Sandbox row, leaving a
	// quota-first lifecycle transaction free to lock that Sandbox.
	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var blockerPID int32
	if err := blocker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		blocker.Rollback(t.Context())
		t.Fatal(err)
	}
	if _, err := blocker.Exec(t.Context(), `
		SELECT tenant_ref FROM secondbox.tenant_quotas
		WHERE tenant_ref=$1 FOR UPDATE`, tenantRef); err != nil {
		blocker.Rollback(t.Context())
		t.Fatal(err)
	}
	type admissionResult struct{ err error }
	admissionDone := make(chan admissionResult, 1)
	go func() {
		_, _, err := dataPlaneStore.AdmitDataPlane(t.Context(), admission(-1, now))
		admissionDone <- admissionResult{err: err}
	}()
	waitDeadline := time.Now().Add(5 * time.Second)
	waiting := false
	for time.Now().Before(waitDeadline) {
		if err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE $1=ANY(pg_blocking_pids(pid))
			)`, blockerPID).Scan(&waiting); err != nil {
			blocker.Rollback(t.Context())
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	probe, err := pool.Begin(t.Context())
	if err != nil {
		blocker.Rollback(t.Context())
		t.Fatal(err)
	}
	_, probeErr := probe.Exec(t.Context(), `
		SELECT id FROM secondbox.sandboxes WHERE id=$1 FOR UPDATE NOWAIT`, sandboxID)
	_ = probe.Rollback(t.Context())
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	exactAdmission := <-admissionDone
	if !waiting {
		t.Fatal("data-plane admission did not wait on the blocked Tenant quota ledger")
	}
	if probeErr != nil {
		t.Fatalf("data-plane admission locked the Sandbox before its quota ledger: %v", probeErr)
	}
	if exactAdmission.err != nil {
		t.Fatalf("data-plane admission after quota release: %v", exactAdmission.err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	errorsFound := make(chan error, 512)
	var admitted atomic.Int64
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		for index := 0; index < 128; index++ {
			kind := "stop"
			desiredState := contracts.SandboxDesiredStateStopped
			initialState := contracts.SandboxStateReady
			initialDesiredState := contracts.SandboxDesiredStateRunning
			if index%2 == 0 {
				kind = "start"
				desiredState = contracts.SandboxDesiredStateRunning
				initialState = contracts.SandboxStateStopped
				initialDesiredState = contracts.SandboxDesiredStateStopped
			}
			mutationAt := now.Add(time.Duration(index+1) * time.Millisecond)
			if err := resetContentionSandboxState(
				ctx, pool, tenantRef, subjectRef, sandboxID, workspaceID,
				initialState, initialDesiredState, mutationAt,
			); err != nil {
				errorsFound <- err
				return
			}
			var revision int64
			if err := pool.QueryRow(ctx, `SELECT revision FROM secondbox.sandboxes WHERE id=$1`, sandboxID).Scan(&revision); err != nil {
				errorsFound <- err
				return
			}
			_, err := controlPlaneStore.SetSandboxDesiredState(ctx, ports.LifecycleIntentInput{
				Principal: contracts.Principal{TenantRef: tenantRef, SubjectRef: subjectRef},
				SandboxID: sandboxID, DesiredState: desiredState,
				Operation: contracts.Operation{
					ID:   fmt.Sprintf("operation-contention-%d-%s", index, suffix),
					Kind: kind, RequestID: fmt.Sprintf("request-contention-%d-%s", index, suffix),
					CreatedAt: mutationAt, UpdatedAt: mutationAt,
				},
				Now: mutationAt, ExpectedRevision: revision,
				IdempotencyKey:  fmt.Sprintf("contention-%d", index),
				RequestHash:     fmt.Sprintf("contention-hash-%d", index),
				IdempotencyEnds: mutationAt.Add(time.Hour),
			})
			if err != nil {
				errorsFound <- fmt.Errorf("lifecycle %s iteration %d: %w", kind, index, err)
				return
			}
			if err := resetContentionSandboxState(
				ctx, pool, tenantRef, subjectRef, sandboxID, workspaceID,
				contracts.SandboxStateReady, contracts.SandboxDesiredStateRunning, mutationAt,
			); err != nil {
				errorsFound <- err
				return
			}
		}
	}()
	go func() {
		defer wait.Done()
		for index := 0; index < 128; index++ {
			_, _, err := dataPlaneStore.AdmitDataPlane(
				ctx, admission(index, now.Add(time.Duration(index+1)*time.Millisecond)),
			)
			if err == nil {
				admitted.Add(1)
				continue
			}
			if errors.Is(err, ports.ErrLifecycleUnavailable) {
				continue
			}
			errorsFound <- fmt.Errorf("exec admission iteration %d: %w", index, err)
			return
		}
	}()
	go func() {
		defer wait.Done()
		for index := 0; index < 128; index++ {
			if _, err := worker.RunOnce(ctx, now.Add(time.Duration(index)*time.Second)); err != nil {
				errorsFound <- fmt.Errorf("cleanup iteration %d: %w", index, err)
				return
			}
		}
	}()
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if admitted.Load() == 0 {
		t.Fatal("concurrent data-plane admissions never reached the ready Sandbox")
	}
}

func resetContentionSandboxState(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	workspaceID string,
	sandboxState string,
	desiredState string,
	now time.Time,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := rowlock.SandboxWorkspaceForSubject(ctx, tx, tenantRef, subjectRef, sandboxID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',
			mutation_expected_generation=NULL,mutation_target_generation=NULL,mutation_state='',updated_at=$2
		WHERE id=$1`, workspaceID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET state=$2,desired_state=$3,revision=revision+1,updated_at=$4
		WHERE id=$1`, sandboxID, sandboxState, desiredState, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestCleanupWaitsForWorkspaceAcknowledgementAndSurfacesTerminalLoss(t *testing.T) {
	pool, databaseURL := cleanupTestPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprintf("%d", now.UnixNano())
	tenantRef := "ack-cleanup-tenant-" + suffix
	subjectRef := "ack-cleanup-subject-" + suffix
	operationID := "op_ack_cleanup_" + suffix
	sandboxID := "sandbox-ack-cleanup-" + suffix
	workspaceID := "workspace-ack-cleanup-" + suffix
	insertCleanupTestSubject(t, pool, tenantRef, subjectRef, "closed", "running", now.Add(time.Hour), now)
	insertCleanupTestOperation(t, pool, operationID, tenantRef, subjectRef, "running", now)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.subjects SET cleanup_operation_id=$3
		WHERE tenant_ref=$1 AND ref=$2;
		INSERT INTO secondbox.subject_cleanup_operations (
			operation_id,tenant_ref,subject_ref,stage,reconcile_owner,
			reconcile_claim_expires_at,next_reconcile_at,retry_count,retry_limit,
			created_at,updated_at
		) VALUES ($3,$1,$2,'await_acknowledgements','',$4,$4,0,20,$4,$4);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,created_at,updated_at
		) VALUES ($5,$1,$2,$6,'runner-disconnected','ready',1024,1,
			'workspace_delete','effect','','',1,1,'deleting','{}',$4,$4);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES ($6,$1,$2,'profile','revision','deleting','deleted',1,$5,'','{}','{}',1,$4,$4)`,
		pgx.QueryExecModeSimpleProtocol, tenantRef, subjectRef, operationID, now, workspaceID, sandboxID,
	); err != nil {
		t.Fatal(err)
	}
	worker := newCleanupTestWorker(t, databaseURL, "ack-worker-one-"+suffix)
	for index := 0; index < 64; index++ {
		if _, err := worker.RunOnce(t.Context(), now.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
		var retries int64
		if err := pool.QueryRow(t.Context(), `
			SELECT retry_count FROM secondbox.subject_cleanup_operations WHERE operation_id=$1`,
			operationID,
		).Scan(&retries); err != nil {
			t.Fatal(err)
		}
		if retries != 0 {
			break
		}
	}
	worker.Close()
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM secondbox.operations WHERE id=$1`, operationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("cleanup completed without Runner acknowledgement: %q", state)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.workspaces SET state='deleted',mutation_kind='',mutation_state='',
			local_receipt_json='{"acknowledged":true}',updated_at=$2 WHERE id=$1;
		UPDATE secondbox.sandboxes SET state='deleted',deleted_at=$2,updated_at=$2 WHERE id=$3`,
		pgx.QueryExecModeSimpleProtocol, workspaceID, now.Add(time.Second), sandboxID,
	); err != nil {
		t.Fatal(err)
	}
	worker = newCleanupTestWorker(t, databaseURL, "ack-worker-two-"+suffix)
	defer worker.Close()
	for index := 0; index < 64; index++ {
		if _, err := worker.RunOnce(t.Context(), now.Add(time.Duration(index+65)*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT state FROM secondbox.operations WHERE id=$1`, operationID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "succeeded" {
			break
		}
	}
	if err := pool.QueryRow(t.Context(), `SELECT state FROM secondbox.operations WHERE id=$1`, operationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" {
		t.Fatalf("acknowledged cleanup Operation state = %q", state)
	}

	lostSubject := "lost-cleanup-subject-" + suffix
	lostOperation := "op_lost_cleanup_" + suffix
	lostSandbox := "sandbox-lost-cleanup-" + suffix
	lostWorkspace := "workspace-lost-cleanup-" + suffix
	insertCleanupTestSubject(t, pool, tenantRef, lostSubject, "closed", "running", now.Add(time.Hour), now)
	insertCleanupTestOperation(t, pool, lostOperation, tenantRef, lostSubject, "running", now)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.subjects SET cleanup_operation_id=$3
		WHERE tenant_ref=$1 AND ref=$2;
		INSERT INTO secondbox.subject_cleanup_operations (
			operation_id,tenant_ref,subject_ref,stage,reconcile_owner,
			reconcile_claim_expires_at,next_reconcile_at,retry_count,retry_limit,
			created_at,updated_at
		) VALUES ($3,$1,$2,'await_acknowledgements','',$4,$4,0,20,$4,$4);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,created_at,updated_at
		) VALUES ($5,$1,$2,$6,'runner-lost','failed',1024,1,
			'workspace_delete','effect','','',1,1,'failed','{}',$4,$4);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES ($6,$1,$2,'profile','revision','deleting','deleted',1,$5,'','{}','{}',1,$4,$4)`,
		pgx.QueryExecModeSimpleProtocol, tenantRef, lostSubject, lostOperation, now.Add(3*time.Second), lostWorkspace, lostSandbox,
	); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 64; index++ {
		if _, err := worker.RunOnce(t.Context(), now.Add(time.Duration(index+130)*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT state FROM secondbox.operations WHERE id=$1`, lostOperation).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "failed" {
			break
		}
	}
	var errorCode string
	if err := pool.QueryRow(t.Context(), `SELECT state,error_code FROM secondbox.operations WHERE id=$1`, lostOperation).Scan(&state, &errorCode); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || errorCode != "workspace_cleanup_failed" {
		t.Fatalf("workspace loss Operation state=%q code=%q", state, errorCode)
	}
}

type cleanupNoOpCanceller struct{}

func (cleanupNoOpCanceller) CancelSandboxSessions(context.Context, string, int64, string, time.Time) (int64, error) {
	return 0, nil
}

type recordingCleanupCanceller struct {
	calls []string
}

func (canceller *recordingCleanupCanceller) CancelSandboxSessions(
	_ context.Context,
	sandboxID string,
	generation int64,
	_ string,
	_ time.Time,
) (int64, error) {
	canceller.calls = append(canceller.calls, fmt.Sprintf("%s/%d", sandboxID, generation))
	return 1, nil
}

func cleanupTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	databaseURL := cleanupTestDatabaseURL
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, databaseURL
}

func newCleanupTestWorker(t *testing.T, databaseURL string, workerID string) *Worker {
	t.Helper()
	worker, err := NewWorker(t.Context(), databaseURL, cleanupNoOpCanceller{}, workerID, time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func insertCleanupTestSubject(t *testing.T, pool *pgxpool.Pool, tenantRef, subjectRef, state, cleanupState string, expiresAt, now time.Time) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.tenant_quotas (
			tenant_ref,max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
			max_snapshots,max_port_sessions,max_concurrent_operations,max_active_subjects,
			max_application_authorities,updated_at
		) VALUES ($1,10,10,10000,10737418240,10,10,10,10,10,$7)
		ON CONFLICT (tenant_ref) DO NOTHING;
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_cpu_millis,
			max_memory_bytes,max_snapshots,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ($1,$2,4,4,4000,4294967296,4,4,4,$7);
		INSERT INTO secondbox.subjects (
			tenant_ref,ref,state,cleanup_state,cleanup_operation_id,quota_json,metadata_json,
			expires_at,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'',$5,'{}',$6,1,$7,$7)`,
		pgx.QueryExecModeSimpleProtocol, tenantRef, subjectRef, state, cleanupState,
		`{"maxSandboxes":4,"maxActiveInstances":4,"maxCpuMillis":4000,"maxMemoryBytes":4294967296,"maxSnapshots":4,"maxPortSessions":4,"maxConcurrentOperations":4}`,
		expiresAt, now,
	); err != nil {
		t.Fatal(err)
	}
}

func insertCleanupTestOperation(t *testing.T, pool *pgxpool.Pool, operationID, tenantRef, subjectRef, state string, now time.Time) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,started_at,updated_at
		) VALUES ($1,$2,$3,'','','subject_cleanup',$4,$5,'{}','','',false,$6,$6,$6)`,
		operationID, tenantRef, subjectRef, state, strings.Replace(operationID, "op_", "req_", 1), now,
	); err != nil {
		t.Fatal(err)
	}
}
