package runnercontrol

import (
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/jackc/pgx/v5"
)

// A replayed AssignmentProgress must record identically to its first delivery:
// observed_at is persisted as a timestamptz (microsecond precision), so a
// nanosecond-precision observed time has to compare equal to the truncated row
// instead of reading as different evidence and costing the control connection.
func TestAssignmentProgressReplayWithNanosecondPrecisionIsAccepted(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	fence := seedStartingAssignment(t, store, "progress-replay", "accepted", now)
	observed := time.Date(2026, 8, 6, 10, 0, 1, 123456789, time.UTC)
	message := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentProgress{
			AssignmentProgress: &runnerv1.AssignmentProgress{
				MessageId: "progress-replay-message", Sequence: 1,
				Fence: fence,
				Stage: runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_RUNNER_ADMISSION,
				ObservedAtUnixMs: uint64(observed.UnixMilli()),
				ObservedAtUnixNs: uint64(observed.UnixNano()),
				Correlation: &runnerv1.Correlation{
					RequestId: "request-start", OperationId: "operation-start",
					SandboxId: fence.SandboxId, InstanceId: fence.InstanceId,
					SandboxGeneration: fence.SandboxGeneration,
					AssignmentId:      fence.AssignmentId, RunnerId: "runner-home",
				},
			},
		},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := recordAssignmentTestEvent(t, store, message, now.Add(time.Second)); err != nil {
			t.Fatalf("AssignmentProgress delivery %d = %v", attempt, err)
		}
	}
	var assignmentState string
	var persistedObservedAt time.Time
	if err := store.pool.QueryRow(t.Context(), `
		SELECT assignment.state,timing.observed_at
		FROM secondbox.assignments AS assignment
		JOIN secondbox.assignment_stage_timings AS timing
		  ON timing.assignment_id=assignment.id
		WHERE assignment.id=$1`,
		fence.AssignmentId,
	).Scan(&assignmentState, &persistedObservedAt); err != nil {
		t.Fatal(err)
	}
	if assignmentState != "starting" ||
		!persistedObservedAt.Equal(observed.Truncate(time.Microsecond)) {
		t.Fatalf(
			"assignment=%q observedAt=%s, want starting at %s",
			assignmentState,
			persistedObservedAt.UTC().Format(time.RFC3339Nano),
			observed.Truncate(time.Microsecond).Format(time.RFC3339Nano),
		)
	}
}

// A repeated stage whose evidence genuinely disagrees is a telemetry anomaly:
// the timing row keeps its first-delivery evidence, and the progress message
// still lands instead of dropping the runner session.
func TestAssignmentProgressEvidenceDisagreementDoesNotFailDelivery(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 6, 10, 10, 0, 0, time.UTC)
	fence := seedStartingAssignment(t, store, "progress-disagree", "accepted", now)
	correlation := &runnerv1.Correlation{
		RequestId: "request-start", OperationId: "operation-start",
		SandboxId: fence.SandboxId, InstanceId: fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration,
		AssignmentId:      fence.AssignmentId, RunnerId: "runner-home",
	}
	observed := time.Date(2026, 8, 6, 10, 10, 1, 0, time.UTC)
	progressAt := func(at time.Time) *runnerv1.RunnerToControlPlane {
		return &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_AssignmentProgress{
				AssignmentProgress: &runnerv1.AssignmentProgress{
					MessageId: "progress-disagree-message", Sequence: 1,
					Fence: fence,
					Stage: runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_RUNNER_ADMISSION,
					ObservedAtUnixMs: uint64(at.UnixMilli()),
					Correlation:      correlation,
				},
			},
		}
	}
	if err := recordAssignmentTestEvent(t, store, progressAt(observed), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := recordAssignmentTestEvent(
		t, store, progressAt(observed.Add(time.Second)), now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("disagreeing AssignmentProgress replay = %v, want telemetry-only anomaly", err)
	}
	var persistedObservedAt time.Time
	if err := store.pool.QueryRow(t.Context(), `
		SELECT observed_at FROM secondbox.assignment_stage_timings
		WHERE assignment_id=$1`,
		fence.AssignmentId,
	).Scan(&persistedObservedAt); err != nil {
		t.Fatal(err)
	}
	if !persistedObservedAt.Equal(observed) {
		t.Fatalf(
			"persisted observedAt = %s, want the first delivery preserved at %s",
			persistedObservedAt.UTC().Format(time.RFC3339Nano),
			observed.Format(time.RFC3339Nano),
		)
	}
}

// A replayed result for an effect that already completed must be absorbed as
// a no-op even though the Workspace row has moved on: after a create succeeds
// the mutation slot belongs to the start mutation, so the durable-authority
// comparison no longer matches the replay and must not be reached.
func TestCompletedCreateEffectReplayIsAbsorbedAfterMutationHandoff(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 6, 10, 20, 0, 0, time.UTC)
	token := []byte("01234567890123456789012345678901")
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-create-replay','tenant','subject','sandbox-create-replay',
			'runner-home','creating',8589934592,1,'create','effect-create-replay',
			'effect-create-replay','operation-create-replay',1,1,'queued','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			lifecycle_intent_kind,next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-create-replay','tenant','subject','profile','revision','creating','running',
			1,'workspace-create-replay','','{}','{}','create_workspace',$1,1,$1,$1
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES (
			'operation-create-replay','tenant','subject','sandbox-create-replay','',
			'create','pending','request-create-replay','{}','','',false,$1,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-create-replay','sandbox-create-replay',1,'local_workspace_create','queued',
			'','','runner-home','command-create-replay','',$2,0,8,$3,'',$1,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-create-replay','runner-home','effect-create-replay','local-workspace',
			$4,'delivered','connection-home',1,$1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now, token, now.Add(time.Minute), []byte{},
	); err != nil {
		t.Fatal(err)
	}
	result := &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-create-replay",
		EffectId:       "effect-create-replay",
		SandboxId:      "sandbox-create-replay",
		WorkspaceId:    "workspace-create-replay",
		Generation:     1, LogicalCapacityBytes: 8589934592,
		ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
	}
	recordLocalWorkspaceTestResult(t, store, result, now.Add(time.Second))
	recordLocalWorkspaceTestResult(t, store, result, now.Add(2*time.Second))
	var workspaceState, mutationKind, mutationID, effectState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,workspace.mutation_kind,workspace.mutation_id,effect.state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.lifecycle_effects AS effect ON effect.id='effect-create-replay'
		WHERE workspace.id='workspace-create-replay'`,
	).Scan(&workspaceState, &mutationKind, &mutationID, &effectState); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "ready" ||
		mutationKind != "start" ||
		mutationID != "operation-create-replay" ||
		effectState != "succeeded" {
		t.Fatalf(
			"Workspace=%q mutation=%q/%q effect=%q, want the replay absorbed without changes",
			workspaceState, mutationKind, mutationID, effectState,
		)
	}
}

func TestLocalWorkspaceAuthorityConflictNamesDivergentFields(t *testing.T) {
	workspace := ports.HomeWorkspace{
		Generation:           3,
		LogicalCapacityBytes: 2147483648,
		Mutation: ports.WorkspaceMutation{
			ID:          "effect-authority",
			OperationID: "operation-authority",
			State:       "queued",
		},
	}
	agreeing := &runnerv1.LocalWorkspaceResult{
		EffectId:    "effect-authority",
		OperationId: "operation-authority",
		Generation:  3, LogicalCapacityBytes: 2147483648,
	}
	if conflict := localWorkspaceAuthorityConflict(workspace, agreeing); conflict != "" {
		t.Fatalf("agreeing result reported conflict %q", conflict)
	}
	diverging := &runnerv1.LocalWorkspaceResult{
		EffectId:    "effect-authority",
		OperationId: "operation-other",
		Generation:  4, LogicalCapacityBytes: 2147483648,
	}
	conflict := localWorkspaceAuthorityConflict(workspace, diverging)
	for _, fragment := range []string{
		`mutationOperationId "operation-authority" != reported operationId "operation-other"`,
		"generation 3 != reported generation 4",
	} {
		if !strings.Contains(conflict, fragment) {
			t.Fatalf("conflict %q is missing %q", conflict, fragment)
		}
	}
	if strings.Contains(conflict, "logicalCapacityBytes") ||
		strings.Contains(conflict, "mutationId") ||
		strings.Contains(conflict, "mutationState") {
		t.Fatalf("conflict %q names fields that agree", conflict)
	}
}

func recordAssignmentTestEvent(
	t *testing.T,
	store *PostgresStateStore,
	message *runnerv1.RunnerToControlPlane,
	now time.Time,
) error {
	t.Helper()
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := recordAssignmentEvent(t.Context(), tx, "runner-home", message, now); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}
