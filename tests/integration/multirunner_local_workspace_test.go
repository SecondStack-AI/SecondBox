package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestTwoFakeRunnersPinHomesAndNeverRelocateAutomatically(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t,
		controlPlane,
		admin,
		"multirunner-local-workspace",
	)
	now := time.Date(2026, 7, 29, 23, 0, 0, 0, time.UTC)
	fixtureSequence := integrationIdentitySequence.Add(1)
	poolName := fmt.Sprintf("multirunner-pool-%d", fixtureSequence)
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: poolName, State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"},
		Capabilities:  []string{"compute", "local-workspace"},
		CapacityPolicy: map[string]int64{
			"maxInstances": 100,
		},
		ReadyRunnerCount: 2,
		Revision:         1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}); err != nil {
		t.Fatal(err)
	}
	runnerA := fmt.Sprintf("runner-multirunner-a-%d", fixtureSequence)
	runnerB := fmt.Sprintf("runner-multirunner-b-%d", fixtureSequence)
	seedFixtureHomeRunner(t, poolName, runnerA)
	seedFixtureHomeRunner(t, poolName, runnerB)
	profileName := fmt.Sprintf("multirunner-profile-%d", fixtureSequence)
	spec := testProfileSpec(1)
	spec.Pool = poolName
	profile, err := controlPlane.CreateProfile(
		t.Context(),
		admin,
		contracts.CreateProfileRequest{Name: profileName, Spec: spec},
	)
	if err != nil {
		t.Fatal(err)
	}
	grants := append([]string{}, account.ProfileGrants...)
	grants = append(grants, profile.Name)
	if _, err := updateFixtureServiceAccount(
		t,
		controlPlane,
		t.Context(),
		admin,
		account.TenantRef,
		account.ID,
		fixtureUpdateServiceAccountRequest{ProfileGrants: &grants},
	); err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, credential)
	stateStore, err := runnercontrol.NewPostgresStateStore(
		t.Context(),
		integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	connections := map[string]string{
		runnerA: "connection-" + runnerA,
		runnerB: "connection-" + runnerB,
	}
	for runnerID, connectionID := range connections {
		if err := stateStore.OpenConnection(
			t.Context(),
			runnercontrol.RunnerIdentity{
				RunnerID:         runnerID,
				CredentialSerial: "credential-" + runnerID,
			},
			connectionID,
			1,
			now,
		); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	operationA, created, err := controlPlane.CreateSandboxOperation(
		t.Context(),
		principal,
		"multirunner-create-a",
		contracts.CreateSandboxRequest{
			Profile:  profile.Name,
			Metadata: map[string]string{"fixture": "runner-a"},
		},
	)
	if err != nil || !created {
		t.Fatalf("create Sandbox A created=%t error=%v", created, err)
	}
	sandboxA, err := controlPlane.GetSandbox(
		t.Context(),
		principal,
		operationA.SandboxID,
	)
	if err != nil {
		t.Fatal(err)
	}
	homeA, workspaceA := multirunnerWorkspaceAuthority(
		t,
		pool,
		sandboxA.ID,
	)
	if homeA != runnerA {
		t.Fatalf("deterministic initial home = %q, want %q", homeA, runnerA)
	}
	multirunnerCompleteWorkspaceCreate(
		t,
		stateStore,
		pool,
		runnerA,
		connections[runnerA],
		sandboxA.ID,
		workspaceA,
		operationA.ID,
		1,
		now.Add(time.Second),
	)

	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET reserved_capacity_json=$2,updated_at=$3
		WHERE id=$1`,
		runnerA,
		`{"VCPUCount":0,"MemoryBytes":0,"DiskBytes":10995116277760,"Instances":0,"Operations":0}`,
		now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	operationB, created, err := controlPlane.CreateSandboxOperation(
		t.Context(),
		principal,
		"multirunner-create-b",
		contracts.CreateSandboxRequest{
			Profile:  profile.Name,
			Metadata: map[string]string{"fixture": "runner-b"},
		},
	)
	if err != nil || !created {
		t.Fatalf("create Sandbox B created=%t error=%v", created, err)
	}
	sandboxB, err := controlPlane.GetSandbox(
		t.Context(),
		principal,
		operationB.SandboxID,
	)
	if err != nil {
		t.Fatal(err)
	}
	homeB, workspaceB := multirunnerWorkspaceAuthority(
		t,
		pool,
		sandboxB.ID,
	)
	if homeB != runnerB {
		t.Fatalf("capacity-aware second home = %q, want %q", homeB, runnerB)
	}
	multirunnerCompleteWorkspaceCreate(
		t,
		stateStore,
		pool,
		runnerB,
		connections[runnerB],
		sandboxB.ID,
		workspaceB,
		operationB.ID,
		1,
		now.Add(3*time.Second),
	)

	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET state='draining',reserved_capacity_json=$2,
		    drain_phase='draining',updated_at=$3
		WHERE id=$1`,
		runnerA,
		`{"VCPUCount":0,"MemoryBytes":0,"DiskBytes":0,"Instances":0,"Operations":0}`,
		now.Add(4*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	currentA, err := controlPlane.GetSandbox(t.Context(), principal, sandboxA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StartSandbox(
		t.Context(),
		principal,
		sandboxA.ID,
		"multirunner-start-draining-home",
		currentA.Revision,
	); !errors.Is(err, ports.ErrHomeRunnerUnavailable) {
		t.Fatalf("start on draining home error = %v, want ErrHomeRunnerUnavailable", err)
	}
	if actual, _ := multirunnerWorkspaceAuthority(t, pool, sandboxA.ID); actual != runnerA {
		t.Fatalf("drain relocated Sandbox A to %q", actual)
	}

	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET state='lost',active_connection_id='',drain_phase='active',updated_at=$2
		WHERE id=$1`,
		runnerA,
		now.Add(5*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	currentA, err = controlPlane.GetSandbox(t.Context(), principal, sandboxA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StartSandbox(
		t.Context(),
		principal,
		sandboxA.ID,
		"multirunner-start-offline-home",
		currentA.Revision,
	); !errors.Is(err, ports.ErrHomeRunnerUnavailable) {
		t.Fatalf("start on offline home error = %v, want ErrHomeRunnerUnavailable", err)
	}
	if actual, _ := multirunnerWorkspaceAuthority(t, pool, sandboxA.ID); actual != runnerA {
		t.Fatalf("offline recovery relocated Sandbox A to %q", actual)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET state='ready',active_connection_id=$2,drain_phase='active',
		    reserved_capacity_json='{"VCPUCount":0,"MemoryBytes":0,"DiskBytes":0,
		      "Instances":0,"Operations":0}',updated_at=$3
		WHERE id=$1`,
		runnerA,
		connections[runnerA],
		now.Add(6*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	currentA, err = controlPlane.GetSandbox(t.Context(), principal, sandboxA.ID)
	if err != nil {
		t.Fatal(err)
	}
	startOperation, err := controlPlane.StartSandbox(
		t.Context(),
		principal,
		sandboxA.ID,
		"multirunner-start-returned-home",
		currentA.Revision,
	)
	if err != nil {
		t.Fatalf("start after same home runner returned: %v", err)
	}
	if startOperation.State != contracts.OperationStatePending {
		t.Fatalf("same-home recovery start Operation = %#v", startOperation)
	}
	if actual, _ := multirunnerWorkspaceAuthority(t, pool, sandboxA.ID); actual != runnerA {
		t.Fatalf("same-home recovery relocated Sandbox A to %q", actual)
	}

	assignmentScheduler, err := scheduler.NewPostgresStore(
		t.Context(), scheduler.PostgresStoreConfig{
			DatabaseURL: integrationDatabaseURL,
			Now:         func() time.Time { return now },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assignmentScheduler.Close)
	idSequence := 0
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(),
		integrationDatabaseURL,
		assignmentScheduler,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              8,
			SerializationRetryLimit: 3,
			AssetCatalog:            multirunnerAssetCatalog{},
			SessionCanceller:        multirunnerSessionCanceller{},
			NewID: func(prefix string) string {
				idSequence++
				return fmt.Sprintf("%s-multirunner-%d", prefix, idSequence)
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
	t.Cleanup(effectBroker.Close)
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker,
		WorkerID:      "multirunner-routing-worker",
		ClaimDuration: time.Minute, PollInterval: time.Second,
	}
	routeNow := now.Add(10 * time.Second)
	multirunnerRunLifecycle(
		t,
		pool,
		reconciler,
		sandboxA.ID,
		routeNow,
		lifecycle.ActionStartInstance,
	)
	assignmentCommand := multirunnerAssertHomeCommand(
		t,
		pool,
		sandboxA.ID,
		runnerA,
		runnerB,
		"assignment",
	)
	assignment := assignmentCommand.GetAssignment()
	if assignment == nil || assignment.Fence == nil {
		t.Fatalf("start command = %#v", assignmentCommand)
	}
	multirunnerRecordEvent(
		t,
		stateStore,
		runnerA,
		connections[runnerA],
		runnercontrol.EventAssignment,
		&runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
				AssignmentResult: &runnerv1.AssignmentResult{
					MessageId:        "multirunner-assignment-ready",
					Sequence:         2,
					Fence:            proto.Clone(assignment.Fence).(*runnerv1.AssignmentFence),
					Terminal:         runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
					BackendKind:      "firecracker",
					BackendReference: "fc-multirunner-routing",
					Correlation: proto.Clone(
						assignment.Correlation,
					).(*runnerv1.Correlation),
				},
			},
		},
		routeNow.Add(time.Second),
	)
	multirunnerRunLifecycle(
		t,
		pool,
		reconciler,
		sandboxA.ID,
		routeNow.Add(2*time.Second),
		lifecycle.ActionWait,
	)
	currentA, err = controlPlane.GetSandbox(t.Context(), principal, sandboxA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StopSandbox(
		t.Context(),
		principal,
		sandboxA.ID,
		"multirunner-stop-home",
		currentA.Revision,
	); err != nil {
		t.Fatal(err)
	}
	multirunnerRunLifecycle(
		t,
		pool,
		reconciler,
		sandboxA.ID,
		routeNow.Add(3*time.Second),
		lifecycle.ActionDrain,
	)
	multirunnerRunLifecycle(
		t,
		pool,
		reconciler,
		sandboxA.ID,
		routeNow.Add(4*time.Second),
		lifecycle.ActionStopInstance,
	)
	fenceEnvelope := multirunnerAssertHomeCommand(
		t,
		pool,
		sandboxA.ID,
		runnerA,
		runnerB,
		"lifecycle_fence",
	)
	fence := fenceEnvelope.GetFence()
	if fence == nil || fence.Fence == nil {
		t.Fatalf("stop Fence command = %#v", fenceEnvelope)
	}
	multirunnerRecordEvent(
		t,
		stateStore,
		runnerA,
		connections[runnerA],
		runnercontrol.EventFence,
		&runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_FenceResult{
				FenceResult: &runnerv1.FenceResult{
					MessageId: "multirunner-fence-stopped",
					Sequence:  3,
					Fence:     proto.Clone(fence.Fence).(*runnerv1.AssignmentFence),
					Result:    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
					TerminationEvidenceDigest: "sha256:" +
						"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
					Correlation: proto.Clone(
						fence.Correlation,
					).(*runnerv1.Correlation),
				},
			},
		},
		routeNow.Add(5*time.Second),
	)
	advanceEnvelope := multirunnerAssertHomeCommand(
		t,
		pool,
		sandboxA.ID,
		runnerA,
		runnerB,
		"local-workspace",
	)
	advance := advanceEnvelope.GetLocalWorkspace()
	if advance == nil ||
		advance.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION {
		t.Fatalf("stop generation-advance command = %#v", advanceEnvelope)
	}
	multirunnerRecordLocalWorkspaceSuccess(
		t,
		stateStore,
		runnerA,
		connections[runnerA],
		advance,
		4,
		routeNow.Add(6*time.Second),
	)
	multirunnerRunLifecycle(
		t,
		pool,
		reconciler,
		sandboxA.ID,
		routeNow.Add(7*time.Second),
		lifecycle.ActionFinishStop,
	)

	currentA, err = controlPlane.GetSandbox(t.Context(), principal, sandboxA.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshotOperation, replayed, err := controlPlane.CreateSandboxSnapshot(
		t.Context(),
		principal,
		sandboxA.ID,
		"multirunner-snapshot-home",
		currentA.Revision,
		contracts.CreateSnapshotRequest{
			Name: "multirunner-routing",
			Metadata: map[string]string{
				"qualification": "home-routing",
			},
		},
	)
	if err != nil || replayed || snapshotOperation.Snapshot == nil {
		t.Fatalf("create Snapshot replayed=%t operation=%#v error=%v", replayed, snapshotOperation, err)
	}
	snapshotCreate := multirunnerAssertHomeCommand(
		t,
		pool,
		sandboxA.ID,
		runnerA,
		runnerB,
		"local-workspace",
	).GetLocalWorkspace()
	if snapshotCreate == nil ||
		snapshotCreate.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE {
		t.Fatalf("Snapshot create command = %#v", snapshotCreate)
	}
	multirunnerRecordLocalWorkspaceSuccess(
		t,
		stateStore,
		runnerA,
		connections[runnerA],
		snapshotCreate,
		5,
		routeNow.Add(8*time.Second),
	)
	currentA, err = controlPlane.GetSandbox(t.Context(), principal, sandboxA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := controlPlane.RestoreSandboxSnapshot(
		t.Context(),
		principal,
		sandboxA.ID,
		"multirunner-restore-home",
		currentA.Revision,
		contracts.RestoreSnapshotRequest{
			SnapshotID: snapshotOperation.Snapshot.ID,
		},
	); err != nil || replayed {
		t.Fatalf("restore Snapshot replayed=%t error=%v", replayed, err)
	}
	for index, wantedKind := range []runnerv1.LocalWorkspaceCommandKind{
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
	} {
		command := multirunnerAssertHomeCommand(
			t,
			pool,
			sandboxA.ID,
			runnerA,
			runnerB,
			"local-workspace",
		).GetLocalWorkspace()
		if command == nil || command.Kind != wantedKind {
			t.Fatalf("restore phase %d command = %#v, want %s", index, command, wantedKind)
		}
		multirunnerRecordLocalWorkspaceSuccess(
			t,
			stateStore,
			runnerA,
			connections[runnerA],
			command,
			uint64(6+index),
			routeNow.Add(time.Duration(9+index)*time.Second),
		)
	}
	if _, replayed, err := controlPlane.DeleteSnapshot(
		t.Context(),
		principal,
		snapshotOperation.Snapshot.ID,
		"multirunner-snapshot-delete-home",
	); err != nil || replayed {
		t.Fatalf("delete Snapshot replayed=%t error=%v", replayed, err)
	}
	snapshotDelete := multirunnerAssertHomeCommand(
		t,
		pool,
		sandboxA.ID,
		runnerA,
		runnerB,
		"local-workspace",
	).GetLocalWorkspace()
	if snapshotDelete == nil ||
		snapshotDelete.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE {
		t.Fatalf("Snapshot delete command = %#v", snapshotDelete)
	}
	multirunnerRecordLocalWorkspaceSuccess(
		t,
		stateStore,
		runnerA,
		connections[runnerA],
		snapshotDelete,
		9,
		routeNow.Add(12*time.Second),
	)
	currentA, err = controlPlane.GetSandbox(t.Context(), principal, sandboxA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.DeleteSandbox(
		t.Context(),
		principal,
		sandboxA.ID,
		"multirunner-delete-home",
		currentA.Revision,
	); err != nil {
		t.Fatal(err)
	}
	multirunnerRunLifecycle(
		t,
		pool,
		reconciler,
		sandboxA.ID,
		routeNow.Add(13*time.Second),
		lifecycle.ActionDelete,
	)
	workspaceDelete := multirunnerAssertHomeCommand(
		t,
		pool,
		sandboxA.ID,
		runnerA,
		runnerB,
		"local-workspace",
	).GetLocalWorkspace()
	if workspaceDelete == nil ||
		workspaceDelete.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE {
		t.Fatalf("Workspace delete command = %#v", workspaceDelete)
	}
	multirunnerRecordLocalWorkspaceSuccess(
		t,
		stateStore,
		runnerA,
		connections[runnerA],
		workspaceDelete,
		10,
		routeNow.Add(14*time.Second),
	)
}

func multirunnerWorkspaceAuthority(
	t *testing.T,
	pool *pgxpool.Pool,
	sandboxID string,
) (string, string) {
	t.Helper()
	var homeRunnerID, workspaceID string
	if err := pool.QueryRow(t.Context(), `
		SELECT home_runner_id,id
		FROM secondbox.workspaces
		WHERE sandbox_id=$1`,
		sandboxID,
	).Scan(&homeRunnerID, &workspaceID); err != nil {
		t.Fatal(err)
	}
	return homeRunnerID, workspaceID
}

func multirunnerCompleteWorkspaceCreate(
	t *testing.T,
	stateStore *runnercontrol.PostgresStateStore,
	pool *pgxpool.Pool,
	runnerID string,
	connectionID string,
	sandboxID string,
	workspaceID string,
	operationID string,
	sequence uint64,
	now time.Time,
) {
	t.Helper()
	var effectID string
	var capacity, generation int64
	var payload []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT workspace.mutation_effect_id,workspace.logical_capacity_bytes,
		       workspace.generation,command.payload
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id=workspace.mutation_effect_id
		JOIN secondbox.runner_commands AS command
		  ON command.id=effect.command_id
		WHERE workspace.id=$1`,
		workspaceID,
	).Scan(&effectID, &capacity, &generation, &payload); err != nil {
		t.Fatal(err)
	}
	var envelope runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	command := envelope.GetLocalWorkspace()
	if command == nil ||
		command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE ||
		command.Correlation == nil ||
		command.Correlation.RunnerId != runnerID {
		t.Fatalf("Workspace create command = %#v", command)
	}
	duplicate, err := stateStore.RecordEvent(
		t.Context(),
		runnercontrol.Event{
			Kind:         runnercontrol.EventLocalWorkspace,
			RunnerID:     runnerID,
			ConnectionID: connectionID,
			Message: &runnerv1.RunnerToControlPlane{
				Message: &runnerv1.RunnerToControlPlane_LocalWorkspaceResult{
					LocalWorkspaceResult: &runnerv1.LocalWorkspaceResult{
						MessageId:      fmt.Sprintf("workspace-create-result-%d", sequence),
						Sequence:       sequence,
						CommandVersion: 1,
						Kind:           command.Kind,
						Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
						OperationId:    operationID,
						EffectId:       effectID,
						SandboxId:      sandboxID,
						WorkspaceId:    workspaceID,
						Generation:     uint64(generation),
						LogicalCapacityBytes: uint64(
							capacity,
						),
						ReceiptRecordedAtUnixMs: uint64(now.UnixMilli()),
						Correlation:             proto.Clone(command.Correlation).(*runnerv1.Correlation),
					},
				},
			},
		},
		now,
	)
	if err != nil || duplicate {
		t.Fatalf("Workspace create result duplicate=%t error=%v", duplicate, err)
	}
}

func multirunnerRunLifecycle(
	t *testing.T,
	pool *pgxpool.Pool,
	reconciler lifecycle.Reconciler,
	sandboxID string,
	now time.Time,
	want lifecycle.Action,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=CASE
		      WHEN id=$1 THEN $2::timestamptz
		      ELSE $2::timestamptz + interval '1 day'
		    END,
		    reconcile_owner=CASE WHEN id=$1 THEN '' ELSE reconcile_owner END,
		    reconcile_claim_expires_at=CASE
		      WHEN id=$1 THEN NULL
		      ELSE reconcile_claim_expires_at
		    END`,
		sandboxID,
		now.UTC(),
	); err != nil {
		t.Fatal(err)
	}
	decision, found, err := reconciler.RunOnce(
		t.Context(), now.UTC(), ports.LifecycleWakeTriggerNotify,
	)
	if err != nil || !found || decision.Action != want {
		t.Fatalf(
			"lifecycle action for %s = %#v found=%t error=%v, want %s",
			sandboxID,
			decision,
			found,
			err,
			want,
		)
	}
}

func multirunnerAssertHomeCommand(
	t *testing.T,
	pool *pgxpool.Pool,
	sandboxID string,
	homeRunnerID string,
	otherRunnerID string,
	kind string,
) *runnerv1.ControlPlaneToRunner {
	t.Helper()
	var runnerID string
	var payload []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT command.runner_id,command.payload
		FROM secondbox.runner_commands AS command
		LEFT JOIN secondbox.assignments AS assignment
		  ON assignment.id=command.assignment_id
		LEFT JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id=command.assignment_id
		WHERE COALESCE(assignment.sandbox_id,effect.sandbox_id)=$1
		  AND command.kind=$2
		  AND command.state IN ('pending','delivering','delivered')
		ORDER BY command.created_at DESC,command.id DESC
		LIMIT 1`,
		sandboxID,
		kind,
	).Scan(&runnerID, &payload); err != nil {
		t.Fatal(err)
	}
	if runnerID != homeRunnerID {
		t.Fatalf("%s command Runner = %q, want current home %q", kind, runnerID, homeRunnerID)
	}
	var wrongRunnerCommands int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM secondbox.runner_commands AS command
		LEFT JOIN secondbox.assignments AS assignment
		  ON assignment.id=command.assignment_id
		LEFT JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id=command.assignment_id
		WHERE COALESCE(assignment.sandbox_id,effect.sandbox_id)=$1
		  AND command.runner_id=$2`,
		sandboxID,
		otherRunnerID,
	).Scan(&wrongRunnerCommands); err != nil {
		t.Fatal(err)
	}
	if wrongRunnerCommands != 0 {
		t.Fatalf(
			"Sandbox %s emitted %d commands to non-home Runner %s",
			sandboxID,
			wrongRunnerCommands,
			otherRunnerID,
		)
	}
	var envelope runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	var correlation *runnerv1.Correlation
	switch {
	case envelope.GetAssignment() != nil:
		correlation = envelope.GetAssignment().Correlation
	case envelope.GetFence() != nil:
		correlation = envelope.GetFence().Correlation
	case envelope.GetLocalWorkspace() != nil:
		correlation = envelope.GetLocalWorkspace().Correlation
	default:
		t.Fatalf("unexpected %s command payload = %#v", kind, &envelope)
	}
	if correlation == nil ||
		correlation.SandboxId != sandboxID ||
		correlation.RunnerId != homeRunnerID {
		t.Fatalf("%s command correlation = %#v", kind, correlation)
	}
	return &envelope
}

func multirunnerRecordLocalWorkspaceSuccess(
	t *testing.T,
	stateStore *runnercontrol.PostgresStateStore,
	runnerID string,
	connectionID string,
	command *runnerv1.LocalWorkspaceCommand,
	sequence uint64,
	now time.Time,
) {
	t.Helper()
	multirunnerRecordEvent(
		t,
		stateStore,
		runnerID,
		connectionID,
		runnercontrol.EventLocalWorkspace,
		&runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_LocalWorkspaceResult{
				LocalWorkspaceResult: &runnerv1.LocalWorkspaceResult{
					MessageId:            fmt.Sprintf("multirunner-local-result-%d", sequence),
					Sequence:             sequence,
					CommandVersion:       command.CommandVersion,
					Kind:                 command.Kind,
					Terminal:             runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
					OperationId:          command.OperationId,
					EffectId:             command.EffectId,
					SandboxId:            command.SandboxId,
					WorkspaceId:          command.WorkspaceId,
					SnapshotId:           command.SnapshotId,
					PreviousGeneration:   command.ExpectedGeneration,
					Generation:           command.NextGeneration,
					LogicalCapacityBytes: command.LogicalCapacityBytes,
					ReceiptRecordedAtUnixMs: uint64(
						now.UTC().UnixMilli(),
					),
					Correlation: proto.Clone(
						command.Correlation,
					).(*runnerv1.Correlation),
				},
			},
		},
		now,
	)
}

func multirunnerRecordEvent(
	t *testing.T,
	stateStore *runnercontrol.PostgresStateStore,
	runnerID string,
	connectionID string,
	kind runnercontrol.EventKind,
	message *runnerv1.RunnerToControlPlane,
	now time.Time,
) {
	t.Helper()
	duplicate, err := stateStore.RecordEvent(
		t.Context(),
		runnercontrol.Event{
			Kind: kind, RunnerID: runnerID, ConnectionID: connectionID,
			Message: message,
		},
		now.UTC(),
	)
	if err != nil || duplicate {
		t.Fatalf("record %s event duplicate=%t error=%v", kind, duplicate, err)
	}
}

type multirunnerAssetCatalog struct{}

func (multirunnerAssetCatalog) Resolve(digest string) (lifecycle.Asset, error) {
	if digest == "" {
		return lifecycle.Asset{}, errors.New("empty fixture asset digest")
	}
	return lifecycle.Asset{
		ArtifactID:     "asset-" + digest[len(digest)-8:],
		ManifestDigest: digest,

		Architecture:            "amd64",
		GuestProtocolGeneration: 1,
		MandatoryGuestFeatures:  []string{},
	}, nil
}

type multirunnerSessionCanceller struct{}

func (multirunnerSessionCanceller) CancelSandboxSessions(
	context.Context,
	string,
	int64,
	string,
	time.Time,
) (int64, error) {
	return 0, nil
}
