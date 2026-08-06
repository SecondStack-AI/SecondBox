package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// teardownPollInterval is the recovery interval this fixture's lifecycle worker
// runs at. It is held comfortably above the database round trips the assertions
// themselves perform, so a Sandbox that a transition scheduled into the future
// is still unambiguously not due when the next assertion reads it.
const teardownPollInterval = 400 * time.Millisecond

// teardownAttributionAllowanceMilliseconds bounds the Operation wall clock that
// the recorded stages are permitted to leave unattributed. The final teardown
// milestone shares its transaction and timestamp with the Operation
// completion, so the only permitted remainder is clock rounding.
const teardownAttributionAllowanceMilliseconds = 5

// TestTeardownStagesCoverTheDeleteOperationWallClock drives one create, ready,
// and delete cycle against PostgreSQL with an in-process Runner and asserts
// that the persisted orchestration milestones account for the delete
// Operation's entire wall clock. An unattributed remainder means a
// control-plane hop exists that no stage names.
//
// It also records, per hop, whether the durable state the previous transition
// committed left the Sandbox immediately due — the difference between the
// lifecycle worker being woken by a PostgreSQL commit notification and waiting
// out its recovery poll interval.
func TestTeardownStagesCoverTheDeleteOperationWallClock(t *testing.T) {
	fixture := newTeardownFixture(t)

	sandboxID, createOperationID := fixture.createReadySandbox(t)

	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != contracts.SandboxStateReady {
		t.Fatalf("Sandbox state before delete = %q, want ready", current.State)
	}
	deleteOperation, err := fixture.controlPlane.DeleteSandbox(
		t.Context(), fixture.principal, sandboxID,
		"teardown-attribution-delete", current.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Every hop below is driven exactly as the deployed worker loop drives it:
	// claim, decide, commit, and only then consult the durable schedule the
	// commit wrote to decide whether the next claim is notified or polled.
	triggers := map[lifecycle.Action]ports.LifecycleWakeTrigger{}
	triggers[lifecycle.ActionDrain] = fixture.runLifecycle(t, sandboxID, lifecycle.ActionDrain)
	triggers[lifecycle.ActionStopInstance] = fixture.runLifecycle(
		t, sandboxID, lifecycle.ActionStopInstance,
	)
	fixture.completeFence(t, sandboxID)
	fixture.completeGenerationAdvance(t, sandboxID)
	triggers[lifecycle.ActionFinishStop] = fixture.runLifecycle(
		t, sandboxID, lifecycle.ActionFinishStop,
	)
	triggers[lifecycle.ActionDelete] = fixture.runLifecycle(t, sandboxID, lifecycle.ActionDelete)
	fixture.completeWorkspaceDelete(t, sandboxID)

	timing := fixture.readOperationTiming(t, deleteOperation.ID)
	if timing.State != contracts.OperationStateSucceeded || timing.CompletedAt == nil {
		t.Fatalf("delete Operation timing = %#v", timing)
	}

	// The delete Operation's complete hop sequence. The two pickup stages are
	// the wake evidence: the intent commit notifies, and a later hop is
	// reached only by the recovery poll deadline. A pickup stage is recorded
	// once per Operation, so the second poll-bound hop is visible in the stage
	// intervals asserted below rather than as a second row.
	wantStages := []string{
		store.StageDurableAdmission,
		store.StageLifecyclePickupNotify,
		"teardown_drain_committed",
		store.StageLifecyclePickupDeadline,
		"teardown_fence_dispatched",
		"teardown_fence_acknowledged",
		"teardown_generation_advanced",
		"teardown_stop_committed",
		"teardown_workspace_delete_dispatched",
		"teardown_finalized",
	}
	gotStages := make([]string, len(timing.Orchestration))
	cumulative := map[string]float64{}
	elapsed := map[string]float64{}
	for index, stage := range timing.Orchestration {
		gotStages[index] = stage.Stage
		cumulative[stage.Stage] = stage.CumulativeMilliseconds
		elapsed[stage.Stage] = stage.ElapsedMilliseconds
	}
	if len(gotStages) != len(wantStages) {
		t.Fatalf("delete Operation stages = %v, want %v", gotStages, wantStages)
	}
	for index := range wantStages {
		if gotStages[index] != wantStages[index] {
			t.Fatalf("delete Operation stages = %v, want %v", gotStages, wantStages)
		}
	}

	if timing.TotalMilliseconds == nil {
		t.Fatal("delete Operation timing carries no total wall clock")
	}
	total := float64(*timing.TotalMilliseconds)
	attributed := timing.Orchestration[len(timing.Orchestration)-1].CumulativeMilliseconds
	if total-attributed > teardownAttributionAllowanceMilliseconds {
		t.Fatalf(
			"delete Operation left %.1f ms of its %.0f ms wall clock unattributed; stages %v",
			total-attributed, total, gotStages,
		)
	}

	// The Operation's first stage is its own durable admission, so attribution
	// starts at the Operation's own clock rather than partway through it.
	if timing.Orchestration[0].CumulativeMilliseconds > teardownAttributionAllowanceMilliseconds {
		t.Fatalf(
			"delete Operation admission milestone is %.1f ms after its creation",
			timing.Orchestration[0].CumulativeMilliseconds,
		)
	}

	// Both poll-bound hops are named, and neither is runner work. The drain
	// commit and the finish-stop commit each leave their successor decision
	// immediately available and still schedule it one recovery poll interval
	// away, so the delete path pays that interval twice for no external work.
	pollFloor := 0.8 * float64(teardownPollInterval.Milliseconds())
	if elapsed[store.StageLifecyclePickupDeadline] < pollFloor {
		t.Fatalf(
			"drain commit to poll-deadline pickup = %.1f ms, want at least %.1f ms",
			elapsed[store.StageLifecyclePickupDeadline], pollFloor,
		)
	}
	stopToDelete := cumulative["teardown_workspace_delete_dispatched"] -
		cumulative["teardown_stop_committed"]
	if stopToDelete < pollFloor {
		t.Fatalf(
			"finish-stop commit to Workspace delete dispatch = %.1f ms, want at least %.1f ms",
			stopToDelete, pollFloor,
		)
	}
	// The generation-advance acknowledgement does move the Sandbox due, so its
	// successor is notified rather than polled.
	fenceToAdvance := cumulative["teardown_generation_advanced"] -
		cumulative["teardown_fence_acknowledged"]
	if fenceToAdvance >= pollFloor {
		t.Fatalf(
			"Fence acknowledgement to generation advance = %.1f ms, want below %.1f ms",
			fenceToAdvance, pollFloor,
		)
	}

	if createOperationID == "" {
		t.Fatal("start Operation identity was not captured")
	}
	if trigger := triggers[lifecycle.ActionDrain]; trigger != ports.LifecycleWakeTriggerNotify {
		t.Fatalf("delete intent pickup trigger = %q, want notify", trigger)
	}
	if trigger := triggers[lifecycle.ActionStopInstance]; trigger != ports.LifecycleWakeTriggerDeadline {
		t.Fatalf("post-drain pickup trigger = %q, want deadline", trigger)
	}
	if trigger := triggers[lifecycle.ActionFinishStop]; trigger != ports.LifecycleWakeTriggerNotify {
		t.Fatalf("post-generation-advance pickup trigger = %q, want notify", trigger)
	}
	if trigger := triggers[lifecycle.ActionDelete]; trigger != ports.LifecycleWakeTriggerDeadline {
		t.Fatalf("post-finish-stop pickup trigger = %q, want deadline", trigger)
	}
}

// TestTeardownTransitionWakeupCoverage enumerates the delete path's durable
// transitions and asserts, for each, whether the transition leaves its Sandbox
// immediately due. The sandboxes_notify_due trigger only emits a lifecycle
// wakeup when next_reconcile_at is at or before the commit's clock, so a
// transition that schedules itself into the future cannot notify and its
// successor must wait out the recovery poll interval.
//
// This is a characterization of current behaviour, not an endorsement of it.
// Two teardown transitions make their successor decision immediately available
// and still schedule it a full poll interval away. Changing that must change
// this test.
func TestTeardownTransitionWakeupCoverage(t *testing.T) {
	fixture := newTeardownFixture(t)
	sandboxID, _ := fixture.createReadySandbox(t)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlPlane.DeleteSandbox(
		t.Context(), fixture.principal, sandboxID,
		"teardown-wakeup-coverage-delete", current.Revision,
	); err != nil {
		t.Fatal(err)
	}

	type transition struct {
		name          string
		advance       func(t *testing.T)
		wantDueNow    bool
		successorNote string
	}
	transitions := []transition{
		{
			name: "lifecycle intent commit",
			advance: func(*testing.T) {
			},
			wantDueNow:    true,
			successorNote: "the drain decision is available at once",
		},
		{
			name: "drain commit",
			advance: func(t *testing.T) {
				fixture.runLifecycle(t, sandboxID, lifecycle.ActionDrain)
			},
			wantDueNow:    false,
			successorNote: "the stop decision is available at once and still waits a poll interval",
		},
		{
			name: "stop dispatch commit",
			advance: func(t *testing.T) {
				fixture.runLifecycle(t, sandboxID, lifecycle.ActionStopInstance)
			},
			wantDueNow:    false,
			successorNote: "the Runner must acknowledge the Fence first, so waiting is correct",
		},
		{
			name: "Fence acknowledgement",
			advance: func(t *testing.T) {
				fixture.completeFence(t, sandboxID)
			},
			wantDueNow:    false,
			successorNote: "the generation advance is still outstanding, so waiting is correct",
		},
		{
			name: "generation advance acknowledgement",
			advance: func(t *testing.T) {
				fixture.completeGenerationAdvance(t, sandboxID)
			},
			wantDueNow:    true,
			successorNote: "the finish-stop decision is available at once and is notified",
		},
		{
			name: "finish-stop commit",
			advance: func(t *testing.T) {
				fixture.runLifecycle(t, sandboxID, lifecycle.ActionFinishStop)
			},
			wantDueNow:    false,
			successorNote: "the delete decision is available at once and still waits a poll interval",
		},
		{
			name: "Workspace delete dispatch commit",
			advance: func(t *testing.T) {
				fixture.runLifecycle(t, sandboxID, lifecycle.ActionDelete)
			},
			wantDueNow:    false,
			successorNote: "the Runner must acknowledge the delete first, so waiting is correct",
		},
	}
	for _, step := range transitions {
		step.advance(t)
		dueNow := fixture.sandboxIsDue(t, sandboxID)
		if dueNow != step.wantDueNow {
			t.Fatalf(
				"%s left the Sandbox dueNow=%t, want %t (%s)",
				step.name, dueNow, step.wantDueNow, step.successorNote,
			)
		}
	}
	fixture.completeWorkspaceDelete(t, sandboxID)
	if fixture.sandboxIsDue(t, sandboxID) {
		t.Fatal("delete finalization left the Sandbox due for further reconciliation")
	}
}

type teardownFixture struct {
	controlPlane *service.ControlPlaneService
	store        *store.PostgresControlPlaneStore
	pool         *pgxpool.Pool
	stateStore   *runnercontrol.PostgresStateStore
	reconciler   lifecycle.Reconciler
	principal    contracts.Principal
	profileName  string
	runnerID     string
	connectionID string
	server       string
	tenantRef    string
	subjectRef   string
	sequence     uint64
}

func newTeardownFixture(t *testing.T) *teardownFixture {
	t.Helper()
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	// The attribution assertions compare persisted milestones against the
	// Operation's own wall clock, so every component reads the real clock.
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store:                 databaseStore,
		PlatformToken:         testPlatformToken,
		DefaultSubjectQuota:   generousQuota(),
		Now:                   service.SystemClock,
		NewID:                 newFixtureID,
		NewCredentialMaterial: func() string { return fmt.Sprintf("credential-material-%032d", integrationIdentitySequence.Add(1)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "teardown-attribution",
	)
	fixtureSequence := integrationIdentitySequence.Add(1)
	poolName := fmt.Sprintf("teardown-pool-%d", fixtureSequence)
	seededAt := time.Now().UTC()
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: poolName, State: contracts.RunnerPoolStateReady,
		Architectures:    []string{"amd64"},
		Capabilities:     []string{"compute", "local-workspace"},
		CapacityPolicy:   map[string]int64{"maxInstances": 100},
		ReadyRunnerCount: 1,
		Revision:         1,
		CreatedAt:        seededAt,
		UpdatedAt:        seededAt,
	}); err != nil {
		t.Fatal(err)
	}
	runnerID := fmt.Sprintf("runner-teardown-%d", fixtureSequence)
	seedFixtureHomeRunner(t, poolName, runnerID)
	profileName := fmt.Sprintf("teardown-profile-%d", fixtureSequence)
	spec := testProfileSpec(1000)
	spec.Pool = poolName
	profile, err := controlPlane.CreateProfile(
		t.Context(), admin, contracts.CreateProfileRequest{Name: profileName, Spec: spec},
	)
	if err != nil {
		t.Fatal(err)
	}
	grants := append([]string{}, account.ProfileGrants...)
	grants = append(grants, profile.Name)
	if _, err := updateFixtureServiceAccount(
		t, controlPlane, t.Context(), admin, account.TenantRef, account.ID,
		fixtureUpdateServiceAccountRequest{ProfileGrants: &grants},
	); err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, credential)

	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	connectionID := "connection-" + runnerID
	if err := stateStore.OpenConnection(
		t.Context(),
		runnercontrol.RunnerIdentity{
			RunnerID: runnerID, CredentialSerial: "credential-" + runnerID,
		},
		connectionID, 1, seededAt,
	); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	assignmentScheduler, err := scheduler.NewPostgresStore(
		t.Context(), scheduler.PostgresStoreConfig{
			DatabaseURL: integrationDatabaseURL, Now: service.SystemClock,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assignmentScheduler.Close)
	idSequence := 0
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, assignmentScheduler,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              8,
			SerializationRetryLimit: 3,
			AssetCatalog:            teardownAssetCatalog{},
			SessionCanceller:        teardownSessionCanceller{},
			NewID: func(prefix string) string {
				idSequence++
				return fmt.Sprintf("%s-teardown-%d-%d", prefix, fixtureSequence, idSequence)
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
			Now: service.SystemClock,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(effectBroker.Close)

	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	return &teardownFixture{
		controlPlane: controlPlane,
		store:        databaseStore,
		pool:         pool,
		stateStore:   stateStore,
		reconciler: lifecycle.Reconciler{
			Store: databaseStore, Effects: effectBroker,
			WorkerID:      fmt.Sprintf("teardown-worker-%d", fixtureSequence),
			ClaimDuration: time.Minute, PollInterval: teardownPollInterval,
			BatchSize: 1,
		},
		principal:    principal,
		profileName:  profile.Name,
		runnerID:     runnerID,
		connectionID: connectionID,
		server:       server.URL,
		tenantRef:    principal.TenantRef,
		subjectRef:   principal.SubjectRef,
		sequence:     1,
	}
}

func (fixture *teardownFixture) nextSequence() uint64 {
	fixture.sequence++
	return fixture.sequence
}

// createReadySandbox drives one Sandbox from create to ready through the same
// durable transitions the deployed control plane uses.
func (fixture *teardownFixture) createReadySandbox(t *testing.T) (string, string) {
	t.Helper()
	operation, created, err := fixture.controlPlane.CreateSandboxOperation(
		t.Context(), fixture.principal,
		fmt.Sprintf("teardown-create-%d", integrationIdentitySequence.Add(1)),
		contracts.CreateSandboxRequest{
			Profile:  fixture.profileName,
			Metadata: map[string]string{"fixture": "teardown-attribution"},
		},
	)
	if err != nil || !created {
		t.Fatalf("create Sandbox created=%t error=%v", created, err)
	}
	sandboxID := operation.SandboxID
	fixture.completeWorkspaceCreate(t, sandboxID, operation.ID)
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	startOperation, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, sandboxID,
		fmt.Sprintf("teardown-start-%d", integrationIdentitySequence.Add(1)),
		current.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	fixture.completeAssignmentReady(t, sandboxID)
	return sandboxID, startOperation.ID
}

func (fixture *teardownFixture) completeWorkspaceCreate(
	t *testing.T,
	sandboxID string,
	operationID string,
) {
	t.Helper()
	command := fixture.pendingLocalWorkspaceCommand(t, sandboxID)
	if command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE {
		t.Fatalf("Workspace create command kind = %v", command.Kind)
	}
	if command.OperationId != operationID {
		t.Fatalf(
			"Workspace create command Operation = %q, want %q",
			command.OperationId, operationID,
		)
	}
	fixture.recordLocalWorkspaceSuccess(t, command, command.LogicalCapacityBytes)
}

func (fixture *teardownFixture) completeGenerationAdvance(t *testing.T, sandboxID string) {
	t.Helper()
	command := fixture.pendingLocalWorkspaceCommand(t, sandboxID)
	if command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION {
		t.Fatalf("generation-advance command kind = %v", command.Kind)
	}
	fixture.recordLocalWorkspaceSuccess(t, command, command.LogicalCapacityBytes)
}

func (fixture *teardownFixture) completeWorkspaceDelete(t *testing.T, sandboxID string) {
	t.Helper()
	command := fixture.pendingLocalWorkspaceCommand(t, sandboxID)
	if command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE {
		t.Fatalf("Workspace delete command kind = %v", command.Kind)
	}
	fixture.recordLocalWorkspaceSuccess(t, command, command.LogicalCapacityBytes)
}

func (fixture *teardownFixture) recordLocalWorkspaceSuccess(
	t *testing.T,
	command *runnerv1.LocalWorkspaceCommand,
	capacity uint64,
) {
	t.Helper()
	sequence := fixture.nextSequence()
	fixture.recordEvent(t, runnercontrol.EventLocalWorkspace, &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_LocalWorkspaceResult{
			LocalWorkspaceResult: &runnerv1.LocalWorkspaceResult{
				MessageId:               fmt.Sprintf("teardown-local-result-%d", sequence),
				Sequence:                sequence,
				CommandVersion:          command.CommandVersion,
				Kind:                    command.Kind,
				Terminal:                runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
				OperationId:             command.OperationId,
				EffectId:                command.EffectId,
				SandboxId:               command.SandboxId,
				WorkspaceId:             command.WorkspaceId,
				SnapshotId:              command.SnapshotId,
				PreviousGeneration:      command.ExpectedGeneration,
				Generation:              command.NextGeneration,
				LogicalCapacityBytes:    capacity,
				ReceiptRecordedAtUnixMs: uint64(time.Now().UTC().UnixMilli()),
				Correlation:             proto.Clone(command.Correlation).(*runnerv1.Correlation),
			},
		},
	})
}

func (fixture *teardownFixture) completeAssignmentReady(t *testing.T, sandboxID string) {
	t.Helper()
	envelope := fixture.pendingCommand(t, sandboxID, "assignment")
	assignment := envelope.GetAssignment()
	if assignment == nil || assignment.Fence == nil {
		t.Fatalf("assignment command = %#v", envelope)
	}
	sequence := fixture.nextSequence()
	fixture.recordEvent(t, runnercontrol.EventAssignment, &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId:        fmt.Sprintf("teardown-assignment-ready-%d", sequence),
				Sequence:         sequence,
				Fence:            proto.Clone(assignment.Fence).(*runnerv1.AssignmentFence),
				Terminal:         runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind:      "firecracker",
				BackendReference: "compute-teardown-attribution",
				Correlation:      proto.Clone(assignment.Correlation).(*runnerv1.Correlation),
			},
		},
	})
}

func (fixture *teardownFixture) completeFence(t *testing.T, sandboxID string) {
	t.Helper()
	envelope := fixture.pendingCommand(t, sandboxID, "lifecycle_fence")
	fence := envelope.GetFence()
	if fence == nil || fence.Fence == nil {
		t.Fatalf("Fence command = %#v", envelope)
	}
	sequence := fixture.nextSequence()
	fixture.recordEvent(t, runnercontrol.EventFence, &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_FenceResult{
			FenceResult: &runnerv1.FenceResult{
				MessageId: fmt.Sprintf("teardown-fence-stopped-%d", sequence),
				Sequence:  sequence,
				Fence:     proto.Clone(fence.Fence).(*runnerv1.AssignmentFence),
				Result:    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
				TerminationEvidenceDigest: "sha256:" +
					"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Correlation: proto.Clone(fence.Correlation).(*runnerv1.Correlation),
			},
		},
	})
}

func (fixture *teardownFixture) recordEvent(
	t *testing.T,
	kind runnercontrol.EventKind,
	message *runnerv1.RunnerToControlPlane,
) {
	t.Helper()
	duplicate, err := fixture.stateStore.RecordEvent(
		t.Context(),
		runnercontrol.Event{
			Kind: kind, RunnerID: fixture.runnerID,
			ConnectionID: fixture.connectionID, Message: message,
		},
		time.Now().UTC(),
	)
	if err != nil || duplicate {
		t.Fatalf("record %s event duplicate=%t error=%v", kind, duplicate, err)
	}
}

func (fixture *teardownFixture) pendingLocalWorkspaceCommand(
	t *testing.T,
	sandboxID string,
) *runnerv1.LocalWorkspaceCommand {
	t.Helper()
	command := fixture.pendingCommand(t, sandboxID, "local-workspace").GetLocalWorkspace()
	if command == nil {
		t.Fatalf("Sandbox %s has no pending local-workspace command", sandboxID)
	}
	return command
}

func (fixture *teardownFixture) pendingCommand(
	t *testing.T,
	sandboxID string,
	kind string,
) *runnerv1.ControlPlaneToRunner {
	t.Helper()
	var payload []byte
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT command.payload
		FROM secondbox.runner_commands AS command
		LEFT JOIN secondbox.assignments AS assignment
		  ON assignment.id=command.assignment_id
		LEFT JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id=command.assignment_id
		LEFT JOIN secondbox.workspaces AS workspace
		  ON workspace.mutation_effect_id=command.assignment_id
		WHERE COALESCE(assignment.sandbox_id,effect.sandbox_id,workspace.sandbox_id)=$1
		  AND command.kind=$2
		  AND command.state IN ('pending','delivering','delivered')
		ORDER BY command.created_at DESC,command.id DESC
		LIMIT 1`,
		sandboxID, kind,
	).Scan(&payload); err != nil {
		t.Fatalf("pending %s command for %s: %v", kind, sandboxID, err)
	}
	var envelope runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	return &envelope
}

// runLifecycle waits exactly as the deployed worker waits — immediately when
// the previous commit left the Sandbox due, otherwise until the durable
// schedule says so — then claims and commits one transition. It returns the
// wake trigger that the wait actually earned.
func (fixture *teardownFixture) runLifecycle(
	t *testing.T,
	sandboxID string,
	want lifecycle.Action,
) ports.LifecycleWakeTrigger {
	t.Helper()
	fixture.deferOtherSandboxes(t, sandboxID)
	trigger := ports.LifecycleWakeTriggerNotify
	if dueAt, scheduled := fixture.sandboxDueAt(t, sandboxID); scheduled {
		if wait := time.Until(dueAt); wait > 0 {
			trigger = ports.LifecycleWakeTriggerDeadline
			time.Sleep(wait)
		}
	} else {
		t.Fatalf("Sandbox %s is not scheduled for reconciliation", sandboxID)
	}
	decision, found, err := fixture.reconciler.RunOnce(
		t.Context(), time.Now().UTC(), trigger,
	)
	if err != nil || !found || decision.Action != want {
		t.Fatalf(
			"lifecycle action for %s = %#v found=%t error=%v, want %s",
			sandboxID, decision, found, err, want,
		)
	}
	return trigger
}

// deferOtherSandboxes keeps residue from other tests in the shared database out
// of this fixture's claim. It never touches the Sandbox under test, because
// that Sandbox's own durable schedule is the thing being measured.
func (fixture *teardownFixture) deferOtherSandboxes(t *testing.T, sandboxID string) {
	t.Helper()
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at=$2::timestamptz + interval '1 day'
		WHERE id<>$1 AND next_reconcile_at IS NOT NULL
		  AND next_reconcile_at<=$2::timestamptz + interval '1 hour'`,
		sandboxID, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
}

func (fixture *teardownFixture) sandboxDueAt(
	t *testing.T,
	sandboxID string,
) (time.Time, bool) {
	t.Helper()
	var dueAt *time.Time
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT next_reconcile_at FROM secondbox.sandboxes WHERE id=$1`,
		sandboxID,
	).Scan(&dueAt); err != nil {
		t.Fatal(err)
	}
	if dueAt == nil {
		return time.Time{}, false
	}
	return dueAt.UTC(), true
}

// sandboxIsDue reports whether the Sandbox's durable schedule would let the
// sandboxes_notify_due trigger emit a lifecycle wakeup, which is the same
// predicate the trigger applies.
func (fixture *teardownFixture) sandboxIsDue(t *testing.T, sandboxID string) bool {
	t.Helper()
	dueAt, scheduled := fixture.sandboxDueAt(t, sandboxID)
	return scheduled && !dueAt.After(time.Now().UTC())
}

func (fixture *teardownFixture) readOperationTiming(
	t *testing.T,
	operationID string,
) contracts.OperationTiming {
	t.Helper()
	response := timingHTTPRequest(
		t, fixture.server+"/v1/operations/"+operationID+"/timings",
		fixture.tenantRef, fixture.subjectRef,
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf(
			"Operation timing status=%d body=%s",
			response.StatusCode, readResponse(t, response),
		)
	}
	var timing contracts.OperationTiming
	decodeResponseJSON(t, response, &timing)
	return timing
}

type teardownAssetCatalog struct{}

func (teardownAssetCatalog) Resolve(digest string) (lifecycle.SignedAsset, error) {
	if digest == "" {
		return lifecycle.SignedAsset{}, errors.New("empty teardown fixture asset digest")
	}
	return lifecycle.SignedAsset{
		ArtifactID:              "asset-" + digest[len(digest)-8:],
		ManifestDigest:          digest,
		SignatureKeyID:          "teardown-signing-key",
		Architecture:            "amd64",
		GuestProtocolGeneration: 1,
		MandatoryGuestFeatures:  []string{},
	}, nil
}

type teardownSessionCanceller struct{}

func (teardownSessionCanceller) CancelSandboxSessions(
	context.Context,
	string,
	int64,
	string,
	time.Time,
) (int64, error) {
	return 0, nil
}
