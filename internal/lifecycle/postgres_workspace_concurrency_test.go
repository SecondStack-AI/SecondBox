package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var lifecyclePostgresTestSequence atomic.Uint64

func TestAutomaticRestartBuildsStartAuthorityWithoutPublicOperation(t *testing.T) {
	databaseURL := openLifecyclePostgresTestDatabase(t)
	now := time.Date(2026, 7, 29, 21, 30, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runtimeDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	toolchainDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	spec := contracts.ProfileRevisionSpec{
		Pool:                  "pool",
		Architecture:          "amd64",
		RuntimeBundleDigest:   runtimeDigest,
		ToolchainBundleDigest: toolchainDigest,
		Resources: contracts.ResourcePolicy{
			VCPUCount: 1, MemoryBytes: 1 << 30, WorkspaceBytes: 8 << 30,
			ConcurrentOperations: 1,
		},
		Execution: contracts.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 60000,
			MaximumBufferedOutputBytes:  1 << 20,
			DataPlaneTransport:          contracts.DataPlaneTransportProxied,
		},
		Network: contracts.NetworkPolicy{
			Mode:                        "deny_all",
			Destinations:                []contracts.NetworkDestination{},
			RequiresTenantEgressContext: new(bool),
		},
		Ports: []contracts.PortPolicy{},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ('revision-automatic-start','profile-automatic-start',1,$1,$2);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-automatic-start','tenant','subject','sandbox-automatic-start',
			'runner-home','ready',8589934592,2,'','','','',NULL,NULL,'','{}',$2,$2
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,reconcile_claim_expires_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-automatic-start','tenant','subject','profile-automatic-start',
			'revision-automatic-start','stopped','running',2,
			'workspace-automatic-start','','{}','{}','worker-automatic-start',$3,5,$2,$2
		)`,
		pgx.QueryExecModeSimpleProtocol,
		string(specJSON),
		now,
		now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	schedulerFailure := errors.New("captured automatic restart")
	recordingScheduler := &recordingFailureScheduler{err: schedulerFailure}
	catalog := fixedLifecycleAssetCatalog{assets: map[string]lifecycle.Asset{
		runtimeDigest: {
			ArtifactID: "runtime", ManifestDigest: runtimeDigest,
			Architecture:            "amd64",
			GuestProtocolGeneration: 1,
		},
		toolchainDigest: {
			ArtifactID: "toolchain", ManifestDigest: toolchainDigest,
			Architecture:            "amd64",
			GuestProtocolGeneration: 1,
		},
	}}
	broker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(),
		databaseURL,
		recordingScheduler,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              2,
			SerializationRetryLimit: 2,
			AssetCatalog:            catalog,
			SessionCanceller:        noOpSessionCanceller{},
			NewID: func(prefix string) string {
				return prefix + "-automatic"
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
			Now: func() time.Time { return now },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	err = broker.ExecuteLifecycleEffect(
		t.Context(),
		ports.LifecycleReconcileClaim{
			SandboxID: "sandbox-automatic-start",
			WorkerID:  "worker-automatic-start",
			Revision:  5,
		},
		lifecycle.Decision{Action: lifecycle.ActionStartInstance},
		now,
		now.Add(time.Second),
	)
	if !errors.Is(err, schedulerFailure) {
		t.Fatalf("automatic restart error = %v, want captured scheduler failure", err)
	}
	command := recordingScheduler.request.AssignmentCommand
	if command == nil ||
		command.Correlation == nil ||
		!strings.HasPrefix(command.Correlation.OperationId, "automatic-start-") ||
		command.Correlation.RequestId != "request-"+command.Correlation.OperationId ||
		recordingScheduler.request.StartMutationID == "" ||
		!recordingScheduler.request.EffectStartedAt.Equal(now) ||
		!recordingScheduler.request.PlanReadyAt.Equal(now) {
		t.Fatalf(
			"automatic restart authority command=%#v mutation=%q",
			command,
			recordingScheduler.request.StartMutationID,
		)
	}
	recordingScheduler.err = scheduler.ErrHomeRunnerUnavailable
	nextReconcileAt := now.Add(2 * time.Second)
	if err := broker.ExecuteLifecycleEffect(
		t.Context(),
		ports.LifecycleReconcileClaim{
			SandboxID: "sandbox-automatic-start",
			WorkerID:  "worker-automatic-start",
			Revision:  5,
		},
		lifecycle.Decision{Action: lifecycle.ActionStartInstance},
		now.Add(time.Second),
		nextReconcileAt,
	); err != nil {
		t.Fatalf("unavailable home Runner deferral error = %v", err)
	}
	var (
		reconcileOwner       string
		reconcileClaimExpiry *time.Time
		persistedNext        time.Time
		revision             int64
	)
	if err := pool.QueryRow(t.Context(), `
		SELECT reconcile_owner,reconcile_claim_expires_at,next_reconcile_at,revision
		FROM secondbox.sandboxes WHERE id='sandbox-automatic-start'`,
	).Scan(
		&reconcileOwner, &reconcileClaimExpiry, &persistedNext, &revision,
	); err != nil {
		t.Fatal(err)
	}
	// The deferral releases the claim and reschedules the poll, and it changes
	// nothing a caller can observe, so the claimed revision stays where it was.
	if reconcileOwner != "" ||
		reconcileClaimExpiry != nil ||
		!persistedNext.Equal(nextReconcileAt) ||
		revision != 5 {
		t.Fatalf(
			"unavailable home Runner deferral owner=%q expiry=%v next=%s revision=%d",
			reconcileOwner, reconcileClaimExpiry, persistedNext, revision,
		)
	}
}

func TestOrdinaryStopAndSnapshotDeleteSerializeAcrossControlPlaneReplicas(t *testing.T) {
	databaseURL := openLifecyclePostgresTestDatabase(t)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	broker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), databaseURL, unusedAssignmentScheduler{},
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              8,
			SerializationRetryLimit: 3,
			AssetCatalog:            unusedAssetCatalog{},
			SessionCanceller:        noOpSessionCanceller{},
			NewID: func(prefix string) string {
				return prefix + "-unused"
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
			Now: time.Now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	now := time.Date(2026, 7, 29, 22, 0, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	token := []byte("01234567890123456789012345678901")
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			'runner-home','pool','runner-home','ready','["amd64"]',
			'["compute","local-workspace"]','{}','[1]',1,1,'test','connection-home',0,
			'active','{}','[]',0,0,$1,1,$1,$1
		);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-stop-race','tenant','subject','sandbox-stop-race','runner-home',
			'ready',8589934592,3,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,reconcile_claim_expires_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-stop-race','tenant','subject','profile','revision','draining','stopped',
			3,'workspace-stop-race','instance-stop-race','{}','{}','worker-stop-race',
			$2,7,$1,$1
		);
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at
		) VALUES (
			'instance-stop-race','sandbox-stop-race',3,'ready','ready','',$1,$1,$1
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-stop-race','sandbox-stop-race','instance-stop-race','runner-home',
			'revision','firecracker','instance-stop-race',3,$3,'ready','{}','{}','{}','',
			0,8,$2,$2,'',$2,$1,1,$1,$1
		);
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at,retention_ended_at
		) VALUES (
			'snapshot-stop-race','tenant','subject','sandbox-stop-race',
			'workspace-stop-race','runner-home','operation-snapshot','effect-snapshot',
			'{}',3,'before-stop',8589934592,'{}','ready',$2,$1,$1,NULL
		)`,
		pgx.QueryExecModeSimpleProtocol, now, now.Add(time.Hour), token,
	); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	go func() {
		<-start
		results <- broker.ExecuteLifecycleEffect(
			ctx,
			ports.LifecycleReconcileClaim{
				SandboxID: "sandbox-stop-race", WorkerID: "worker-stop-race",
				Revision: 7,
			},
			lifecycle.Decision{
				Action:            lifecycle.ActionStopInstance,
				TerminationReason: contracts.TerminationReasonRequestedStop,
			},
			now, now.Add(time.Second),
		)
	}()
	go func() {
		<-start
		_, err := databaseStore.DeleteSnapshot(ctx, ports.SnapshotDeletionInput{
			TenantRef: "tenant", SubjectRef: "subject",
			SnapshotID: "snapshot-stop-race",
			Operation: contracts.Operation{
				ID: "operation-stop-race-delete", Kind: "snapshot_delete",
				State: contracts.OperationStatePending, RequestID: "request-stop-race-delete",
				CreatedAt: now, UpdatedAt: now,
			},
			EffectID: "effect-stop-race-delete", CommandID: "command-stop-race-delete",
			FencingToken: token, IdempotencyKey: "stop-race-delete",
			RequestHash: "stop-race-delete-hash", IdempotencyEnds: now.Add(time.Hour),
			Now: now,
		})
		results <- err
	}()
	close(start)
	var successes, conflicts int
	for index := 0; index < 2; index++ {
		select {
		case err := <-results:
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ports.ErrWorkspaceMutation),
				errors.Is(err, ports.ErrRevisionConflict):
				conflicts++
			default:
				t.Fatalf("stop/Snapshot-delete race error = %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("stop/Snapshot-delete race did not terminate: %v", ctx.Err())
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf(
			"stop/Snapshot-delete race successes=%d conflicts=%d",
			successes, conflicts,
		)
	}
}

func TestSandboxDeleteQueuesHomeWorkspaceRemovalWhileRunnerIsOffline(t *testing.T) {
	databaseURL := openLifecyclePostgresTestDatabase(t)
	broker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), databaseURL, unusedAssignmentScheduler{},
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              8,
			SerializationRetryLimit: 3,
			AssetCatalog:            unusedAssetCatalog{},
			SessionCanceller:        noOpSessionCanceller{},
			NewID: func(prefix string) string {
				return prefix + "-unused"
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
			Now: time.Now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Date(2026, 7, 29, 22, 30, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-delete-offline','tenant','subject','sandbox-delete-offline',
			'runner-offline','ready',8589934592,3,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			reconcile_owner,reconcile_claim_expires_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-delete-offline','tenant','subject','profile','revision','stopped','deleted',
			3,'workspace-delete-offline','','{}','{}','delete-worker',$2,4,$1,$1
		);
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at,retention_ended_at
		) VALUES (
			'snapshot-delete-offline','tenant','subject','sandbox-delete-offline',
			'workspace-delete-offline','runner-offline','operation-snapshot','effect-snapshot',
			'{}',3,'retained',8589934592,'{}','ready',$2,$1,$1,NULL
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES (
			'operation-delete-offline','tenant','subject','sandbox-delete-offline','',
			'delete','pending','request-delete-offline','{}','','',false,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if err := broker.ExecuteLifecycleEffect(
		t.Context(),
		ports.LifecycleReconcileClaim{
			SandboxID: "sandbox-delete-offline", WorkerID: "delete-worker", Revision: 4,
		},
		lifecycle.Decision{Action: lifecycle.ActionDelete},
		now, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	var (
		sandboxState, workspaceState, mutationKind, mutationState  string
		effectState, commandState, commandRunnerID, operationState string
		payload                                                    []byte
	)
	if err := pool.QueryRow(t.Context(), `
		SELECT sandbox.state,workspace.state,workspace.mutation_kind,
		       workspace.mutation_state,effect.state,command.state,
		       command.runner_id,operation.state,command.payload
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.lifecycle_effects AS effect ON effect.id=workspace.mutation_effect_id
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		JOIN secondbox.operations AS operation ON operation.id=workspace.mutation_operation_id
		WHERE sandbox.id='sandbox-delete-offline'`,
	).Scan(
		&sandboxState, &workspaceState, &mutationKind, &mutationState,
		&effectState, &commandState, &commandRunnerID, &operationState, &payload,
	); err != nil {
		t.Fatal(err)
	}
	var envelope runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	command := envelope.GetLocalWorkspace()
	if sandboxState != "deleting" ||
		workspaceState != "deleting" ||
		mutationKind != "workspace_delete" ||
		mutationState != "deleting" ||
		effectState != "queued" ||
		commandState != "pending" ||
		commandRunnerID != "runner-offline" ||
		operationState != "pending" ||
		command == nil ||
		command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE ||
		command.WorkspaceId != "workspace-delete-offline" ||
		command.OperationId != "operation-delete-offline" ||
		command.ExpectedGeneration != 3 {
		t.Fatalf(
			"delete state Sandbox=%q Workspace=%q mutation=%q/%q effect=%q command=%q/%q operation=%q payload=%#v",
			sandboxState, workspaceState, mutationKind, mutationState,
			effectState, commandState, commandRunnerID, operationState, command,
		)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.lifecycle_effects
		SET state='runner_failed',failure_class='runner_failed',
		    failure_message='runner returned a retryable local failure'
		WHERE id=(
			SELECT mutation_effect_id FROM secondbox.workspaces
			WHERE id='workspace-delete-offline'
		);
		UPDATE secondbox.workspaces
		SET mutation_state='failed'
		WHERE id='workspace-delete-offline';
		UPDATE secondbox.sandboxes
		SET reconcile_owner='delete-worker-retry',revision=revision+1
		WHERE id='sandbox-delete-offline'`,
		pgx.QueryExecModeSimpleProtocol,
	); err != nil {
		t.Fatal(err)
	}
	if err := broker.ExecuteLifecycleEffect(
		t.Context(),
		ports.LifecycleReconcileClaim{
			SandboxID: "sandbox-delete-offline", WorkerID: "delete-worker-retry", Revision: 6,
		},
		lifecycle.Decision{Action: lifecycle.ActionDelete},
		now.Add(2*time.Second), now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	var retryCount, commandCount int64
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.retry_count,workspace.mutation_state,
		       (SELECT count(*) FROM secondbox.runner_commands
		        WHERE assignment_id=effect.id)
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.workspaces AS workspace ON workspace.mutation_effect_id=effect.id
		WHERE workspace.id='workspace-delete-offline'`,
	).Scan(&retryCount, &mutationState, &commandCount); err != nil {
		t.Fatal(err)
	}
	if retryCount != 1 || mutationState != "deleting" || commandCount != 2 {
		t.Fatalf(
			"delete retry count=%d mutation=%q commands=%d",
			retryCount, mutationState, commandCount,
		)
	}
}

type unusedAssignmentScheduler struct{}

func (unusedAssignmentScheduler) Schedule(
	context.Context,
	scheduler.ScheduleRequest,
) (scheduler.DurableAssignment, bool, error) {
	return scheduler.DurableAssignment{}, false, errors.New("unused Assignment scheduler")
}

type recordingFailureScheduler struct {
	request scheduler.ScheduleRequest
	err     error
}

func (recorder *recordingFailureScheduler) Schedule(
	_ context.Context,
	request scheduler.ScheduleRequest,
) (scheduler.DurableAssignment, bool, error) {
	recorder.request = request
	return scheduler.DurableAssignment{}, false, recorder.err
}

type fixedLifecycleAssetCatalog struct {
	assets map[string]lifecycle.Asset
}

func (catalog fixedLifecycleAssetCatalog) Resolve(
	digest string,
) (lifecycle.Asset, error) {
	asset, found := catalog.assets[digest]
	if !found {
		return lifecycle.Asset{}, errors.New("missing fixed lifecycle asset")
	}
	return asset, nil
}

type unusedAssetCatalog struct{}

func (unusedAssetCatalog) Resolve(string) (lifecycle.Asset, error) {
	return lifecycle.Asset{}, errors.New("unused asset catalog")
}

type noOpSessionCanceller struct{}

func (noOpSessionCanceller) CancelSandboxSessions(
	context.Context,
	string,
	int64,
	string,
	time.Time,
) (int64, error) {
	return 0, nil
}

func openLifecyclePostgresTestDatabase(t *testing.T) string {
	t.Helper()
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("SECONDBOX_TEST_DATABASE_URL is required for PostgreSQL lifecycle concurrency tests")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf(
		"secondbox_lifecycle_test_%d_%d",
		os.Getpid(), lifecyclePostgresTestSequence.Add(1),
	)
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := pgx.Connect(t.Context(), adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		admin.Close(t.Context())
		t.Fatal(err)
	}
	parsed.Path = "/" + databaseName
	databaseURL := parsed.String()
	if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
		_, _ = admin.Exec(t.Context(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close(t.Context())
		t.Fatal(err)
	}
	seedLifecycleTestQuotaLedgers(t, databaseURL)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop lifecycle test database: %v", err)
		}
		if err := admin.Close(context.Background()); err != nil {
			t.Errorf("close lifecycle test admin connection: %v", err)
		}
	})
	return databaseURL
}

func seedLifecycleTestQuotaLedgers(t *testing.T, databaseURL string) {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(t.Context())
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.tenant_quotas (
			tenant_ref,max_sandboxes,max_active_instances,max_vcpu_count,max_memory_bytes,
			max_snapshots,max_port_sessions,max_concurrent_operations,max_active_subjects,
			max_application_authorities,updated_at
		) VALUES ('tenant',100,100,100000,1099511627776,100,100,100,100,100,now());
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_vcpu_count,
			max_memory_bytes,max_snapshots,max_port_sessions,max_concurrent_operations,updated_at
		) VALUES ('tenant','subject',100,100,100000,1099511627776,100,100,100,now())`,
		pgx.QueryExecModeSimpleProtocol,
	); err != nil {
		t.Fatal(err)
	}
}
