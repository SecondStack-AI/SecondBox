package runnercontrol

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var runnerControlTestDatabaseSequence atomic.Uint64

func TestOpenConnectionRequeuesEveryUnacknowledgedLocalWorkspaceCommand(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 30, 0, 0, time.UTC)
	databaseURL := store.pool.Config().ConnString()
	for index, kind := range allPostgresLocalWorkspaceCommandKinds() {
		command := &runnerv1.LocalWorkspaceCommand{
			MessageId: "command-" + kind.String(), Sequence: uint64(index + 1),
			CommandVersion: 1, Kind: kind,
			OperationId: "operation-" + kind.String(),
			EffectId:    "effect-" + kind.String(),
			SandboxId:   "sandbox-home", WorkspaceId: "workspace-home",
			SnapshotId: "snapshot-home", ExpectedGeneration: 4, NextGeneration: 5,
			LogicalCapacityBytes: 16 << 20,
			FencingToken:         []byte("01234567890123456789012345678901"),
			Correlation: &runnerv1.Correlation{
				OperationId: "operation-" + kind.String(),
				SandboxId:   "sandbox-home",
				RunnerId:    "runner-home",
			},
		}
		payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{
				LocalWorkspace: command,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		state := "delivering"
		if index%2 == 1 {
			state = "delivered"
		}
		if _, err := store.pool.Exec(t.Context(), `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,'runner-home',$2,'local-workspace',$3,$4,'connection-old',1,$5,$5,$5)`,
			command.MessageId, command.EffectId, payload, state, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-acknowledged','runner-home','effect-acknowledged','local-workspace',
			$1,'acknowledged','connection-old',1,$2,$2,$2
		)`,
		[]byte{}, now,
	); err != nil {
		t.Fatal(err)
	}
	store.Close()
	restarted, err := NewPostgresStateStore(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if err := restarted.OpenConnection(t.Context(), RunnerIdentity{
		RunnerID: "runner-home", CredentialSerial: "credential-new",
	}, "connection-new", 1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rows, err := restarted.pool.Query(t.Context(), `
		SELECT id,state,target_connection_id
		FROM secondbox.runner_commands
		WHERE runner_id='runner-home'
		ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string][2]string)
	for rows.Next() {
		var id, state, connectionID string
		if err := rows.Scan(&id, &state, &connectionID); err != nil {
			t.Fatal(err)
		}
		got[id] = [2]string{state, connectionID}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"command-acknowledged":               {"acknowledged", "connection-old"},
		"workspace-reconcile-connection-new": {"pending", ""},
	}
	for _, kind := range allPostgresLocalWorkspaceCommandKinds() {
		want["command-"+kind.String()] = [2]string{"pending", ""}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recovered runner commands = %#v, want %#v", got, want)
	}
}

func allPostgresLocalWorkspaceCommandKinds() []runnerv1.LocalWorkspaceCommandKind {
	return []runnerv1.LocalWorkspaceCommandKind{
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_INSPECT,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT,
		runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE,
	}
}

func TestReturningRunnerClaimsReconciliationBeforeOlderCommands(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 35, 0, 0, time.UTC)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at
		) VALUES (
			'older-assignment-command','runner-home','assignment-old','assignment',
			$1,'pending','',0,$2,$2
		)`,
		[]byte("deliberately invalid older payload"),
		now.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if err := store.OpenConnection(
		t.Context(),
		RunnerIdentity{
			RunnerID:         "runner-home",
			CredentialSerial: "credential-returning",
		},
		"connection-priority",
		1,
		now,
	); err != nil {
		t.Fatal(err)
	}
	delivery, found, err := store.ClaimCommand(
		t.Context(),
		"runner-home",
		"connection-priority",
		now.Add(time.Second),
	)
	if err != nil || !found {
		t.Fatalf("reconciliation claim found=%t error=%v", found, err)
	}
	command := delivery.Message.GetLocalWorkspace()
	if delivery.ID != "workspace-reconcile-connection-priority" ||
		command == nil ||
		command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE {
		t.Fatalf("first returning-runner command = %q %#v", delivery.ID, delivery.Message)
	}
}

func TestReturningRunnerReplaysDurableWorkspaceCreateReceipt(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 40, 0, 0, time.UTC)
	seedPendingWorkspaceCreation(t, store, now)
	if err := store.OpenConnection(
		t.Context(),
		RunnerIdentity{
			RunnerID:         "runner-home",
			CredentialSerial: "credential-returning",
		},
		"connection-returning",
		1,
		now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	reconcileID := "workspace-reconcile-connection-returning"
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    reconcileID,
		EffectId:       reconcileID,
		Inventory: []*runnerv1.LocalWorkspaceInventoryItem{{
			WorkspaceId:          "workspace-create-reconcile",
			Generation:           1,
			LogicalCapacityBytes: 8 << 30,
			Formatted:            true,
		}},
		Receipts: []*runnerv1.LocalWorkspaceReceiptItem{{
			Kind:                 runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
			OperationId:          "operation-create-reconcile",
			WorkspaceId:          "workspace-create-reconcile",
			Generation:           1,
			LogicalCapacityBytes: 8 << 30,
			ReceiptRecordedAtUnixMs: uint64(
				now.Add(time.Second).UnixMilli(),
			),
		}},
		Correlation: &runnerv1.Correlation{
			RequestId:   reconcileID,
			OperationId: reconcileID,
			RunnerId:    "runner-home",
		},
	}, now.Add(2*time.Second))
	var (
		workspaceState, mutationState, sandboxState     string
		operationState, effectState, createCommandState string
		reconcileCommandState, reconcileOwner           string
		reconcileClaimCleared                           bool
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,workspace.mutation_state,sandbox.state,
		       operation.state,effect.state,create_command.state,reconcile_command.state,
		       sandbox.reconcile_owner,sandbox.reconcile_claim_expires_at IS NULL
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		JOIN secondbox.operations AS operation
		  ON operation.id='operation-create-reconcile'
		JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id='effect-create-reconcile'
		JOIN secondbox.runner_commands AS create_command
		  ON create_command.id='command-create-reconcile'
		JOIN secondbox.runner_commands AS reconcile_command
		  ON reconcile_command.id=$1
		WHERE workspace.id='workspace-create-reconcile'`,
		reconcileID,
	).Scan(
		&workspaceState,
		&mutationState,
		&sandboxState,
		&operationState,
		&effectState,
		&createCommandState,
		&reconcileCommandState,
		&reconcileOwner,
		&reconcileClaimCleared,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "ready" ||
		mutationState != "" ||
		sandboxState != "stopped" ||
		operationState != "succeeded" ||
		effectState != "succeeded" ||
		createCommandState != "acknowledged" ||
		reconcileCommandState != "acknowledged" ||
		reconcileOwner != "" ||
		!reconcileClaimCleared {
		t.Fatalf(
			"reconciled create Workspace=%q/%q Sandbox=%q Operation=%q effect=%q commands=%q/%q claim=%q/%t",
			workspaceState,
			mutationState,
			sandboxState,
			operationState,
			effectState,
			createCommandState,
			reconcileCommandState,
			reconcileOwner,
			reconcileClaimCleared,
		)
	}
}

func TestReturningRunnerDoesNotFailQueuedWorkspaceCreateMissingFromInventory(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 41, 0, 0, time.UTC)
	seedPendingWorkspaceCreation(t, store, now)
	recordReturningRunnerReconciliation(
		t,
		store,
		"connection-before-workspace-create",
		nil,
		now.Add(time.Second),
	)
	var (
		workspaceState, mutationState, sandboxState, operationState string
		effectState, createCommandState, reconcileCommandState      string
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,workspace.mutation_state,sandbox.state,
		       operation.state,effect.state,create_command.state,reconcile_command.state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		JOIN secondbox.operations AS operation
		  ON operation.id='operation-create-reconcile'
		JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id='effect-create-reconcile'
		JOIN secondbox.runner_commands AS create_command
		  ON create_command.id='command-create-reconcile'
		JOIN secondbox.runner_commands AS reconcile_command
		  ON reconcile_command.id='workspace-reconcile-connection-before-workspace-create'
		WHERE workspace.id='workspace-create-reconcile'`,
	).Scan(
		&workspaceState,
		&mutationState,
		&sandboxState,
		&operationState,
		&effectState,
		&createCommandState,
		&reconcileCommandState,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "creating" ||
		mutationState != "queued" ||
		sandboxState != "creating" ||
		operationState != "pending" ||
		effectState != "queued" ||
		createCommandState != "pending" ||
		reconcileCommandState != "acknowledged" {
		t.Fatalf(
			"queued create Workspace=%q/%q Sandbox=%q Operation=%q effect=%q commands=%q/%q",
			workspaceState,
			mutationState,
			sandboxState,
			operationState,
			effectState,
			createCommandState,
			reconcileCommandState,
		)
	}
}

func TestWorkspaceCreateFailureCompletesOperationWithTypedError(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 42, 0, 0, time.UTC)
	seedPendingWorkspaceCreation(t, store, now)
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion:       1,
		Kind:                 runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		Terminal:             runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STORAGE_INCOMPATIBLE,
		OperationId:          "operation-create-reconcile",
		EffectId:             "effect-create-reconcile",
		SandboxId:            "sandbox-create-reconcile",
		WorkspaceId:          "workspace-create-reconcile",
		Generation:           1,
		LogicalCapacityBytes: 8 << 30,
		SafeDetail:           "local workspace storage is incompatible",
	}, now.Add(time.Second))
	var (
		workspaceState, mutationState, sandboxState, failureClass string
		operationState, operationError, effectState, commandState string
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,workspace.mutation_state,sandbox.state,
		       sandbox.lifecycle_failure_class,operation.state,operation.error_code,
		       effect.state,command.state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		JOIN secondbox.operations AS operation
		  ON operation.id='operation-create-reconcile'
		JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id='effect-create-reconcile'
		JOIN secondbox.runner_commands AS command
		  ON command.id='command-create-reconcile'
		WHERE workspace.id='workspace-create-reconcile'`,
	).Scan(
		&workspaceState,
		&mutationState,
		&sandboxState,
		&failureClass,
		&operationState,
		&operationError,
		&effectState,
		&commandState,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "failed" ||
		mutationState != "failed" ||
		sandboxState != "failed" ||
		failureClass != "workspace_storage_incompatible" ||
		operationState != "failed" ||
		operationError != "workspace_storage_incompatible" ||
		effectState != "runner_failed" ||
		commandState != "acknowledged" {
		t.Fatalf(
			"failed create Workspace=%q/%q Sandbox=%q/%q Operation=%q/%q effect=%q command=%q",
			workspaceState,
			mutationState,
			sandboxState,
			failureClass,
			operationState,
			operationError,
			effectState,
			commandState,
		)
	}
}

func TestReturningRunnerMissingWorkspaceFailsAndExactEvidenceRecoversWithoutRelocation(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 45, 0, 0, time.UTC)
	seedReadyReconciledWorkspace(t, store, now)
	recordReturningRunnerReconciliation(
		t,
		store,
		"connection-missing-workspace",
		nil,
		now.Add(time.Second),
	)
	var (
		workspaceState, sandboxState, failureClass, homeRunnerID string
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,sandbox.state,sandbox.lifecycle_failure_class,
		       workspace.home_runner_id
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		WHERE workspace.id='workspace-ready-reconcile'`,
	).Scan(
		&workspaceState,
		&sandboxState,
		&failureClass,
		&homeRunnerID,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "failed" ||
		sandboxState != "failed" ||
		failureClass != "home_workspace_missing" ||
		homeRunnerID != "runner-home" {
		t.Fatalf(
			"missing Workspace state=%q Sandbox=%q failure=%q home=%q",
			workspaceState,
			sandboxState,
			failureClass,
			homeRunnerID,
		)
	}
	recordReturningRunnerReconciliation(
		t,
		store,
		"connection-restored-workspace",
		[]*runnerv1.LocalWorkspaceInventoryItem{{
			WorkspaceId:          "workspace-ready-reconcile",
			Generation:           3,
			LogicalCapacityBytes: 8 << 30,
			Formatted:            true,
		}},
		now.Add(2*time.Second),
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,sandbox.state,sandbox.lifecycle_failure_class,
		       workspace.home_runner_id
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		WHERE workspace.id='workspace-ready-reconcile'`,
	).Scan(
		&workspaceState,
		&sandboxState,
		&failureClass,
		&homeRunnerID,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "ready" ||
		sandboxState != "stopped" ||
		failureClass != "" ||
		homeRunnerID != "runner-home" {
		t.Fatalf(
			"recovered Workspace state=%q Sandbox=%q failure=%q home=%q",
			workspaceState,
			sandboxState,
			failureClass,
			homeRunnerID,
		)
	}
	var failedAudits, recoveredAudits int
	if err := store.pool.QueryRow(t.Context(), `
		SELECT
		  count(*) FILTER (WHERE outcome='failed'),
		  count(*) FILTER (WHERE outcome='succeeded')
		FROM secondbox.audit_events
		WHERE action='runner.workspace_reconciliation'
		  AND resource_id='sandbox-ready-reconcile'`,
	).Scan(&failedAudits, &recoveredAudits); err != nil {
		t.Fatal(err)
	}
	if failedAudits != 1 || recoveredAudits != 1 {
		t.Fatalf(
			"Workspace reconciliation audits failed=%d recovered=%d",
			failedAudits,
			recoveredAudits,
		)
	}
}

func TestReturningRunnerWithoutWriterPreservesUncertainHomeWorkspace(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 50, 0, 0, time.UTC)
	seedReadyReconciledWorkspace(t, store, now)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='ready',desired_state='running',current_instance_id='instance-runner-loss',
		    updated_at=$1
		WHERE id='sandbox-ready-reconcile';
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES (
			'instance-runner-loss','sandbox-ready-reconcile',3,'ready','ready','',
			$1,$1,$1,$1,$2,NULL
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-runner-loss','sandbox-ready-reconcile','instance-runner-loss',
			'runner-home','revision','firecracker','fc-runner-loss',3,$3,'uncertain',
			'{}','{}','{}','transient',0,3,$2,$2,'',$2,$1,2,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		now.Add(time.Hour),
		[]byte("01234567890123456789012345678901"),
	); err != nil {
		t.Fatal(err)
	}
	recordReturningRunnerReconciliation(
		t,
		store,
		"connection-runner-loss",
		[]*runnerv1.LocalWorkspaceInventoryItem{{
			WorkspaceId:          "workspace-ready-reconcile",
			Generation:           3,
			LogicalCapacityBytes: 8 << 30,
			Formatted:            true,
			ActiveWriter:         false,
		}},
		now.Add(time.Second),
	)
	var workspaceState, sandboxState, failureClass, assignmentState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,sandbox.state,COALESCE(sandbox.lifecycle_failure_class,''),
		       assignment.state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		JOIN secondbox.assignments AS assignment
		  ON assignment.id='assignment-runner-loss'
		WHERE workspace.id='workspace-ready-reconcile'`,
	).Scan(
		&workspaceState,
		&sandboxState,
		&failureClass,
		&assignmentState,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "ready" ||
		sandboxState != "ready" ||
		failureClass != "" ||
		assignmentState != "uncertain" {
		t.Fatalf(
			"runner-loss reconciliation Workspace=%q Sandbox=%q failure=%q Assignment=%q",
			workspaceState,
			sandboxState,
			failureClass,
			assignmentState,
		)
	}
}

func TestReturningRunnerWithoutWriterPreservesAssignedWorkspace(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 18, 55, 0, 0, time.UTC)
	fence := seedStartingAssignment(t, store, "assigned-reconcile", "assigned", now)
	recordReturningRunnerReconciliation(
		t,
		store,
		"connection-assigned-reconcile",
		[]*runnerv1.LocalWorkspaceInventoryItem{{
			WorkspaceId:          "workspace-assigned-reconcile",
			Generation:           3,
			LogicalCapacityBytes: 8 << 30,
			Formatted:            true,
			ActiveWriter:         false,
		}},
		now.Add(time.Second),
	)
	var workspaceState, sandboxState, failureClass, assignmentState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,sandbox.state,COALESCE(sandbox.lifecycle_failure_class,''),
		       assignment.state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		JOIN secondbox.assignments AS assignment ON assignment.id=$2
		WHERE workspace.id=$1`,
		"workspace-assigned-reconcile",
		fence.AssignmentId,
	).Scan(
		&workspaceState,
		&sandboxState,
		&failureClass,
		&assignmentState,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "ready" ||
		sandboxState != "starting" ||
		failureClass != "" ||
		assignmentState != "assigned" {
		t.Fatalf(
			"assigned reconciliation Workspace=%q Sandbox=%q failure=%q Assignment=%q",
			workspaceState, sandboxState, failureClass, assignmentState,
		)
	}
}

func TestLocalRestoreResultsDrivePrepareSwapCommitAndFinalize(t *testing.T) {
	runnerStore := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	seedRunnerLocalRestore(t, runnerStore, now)
	recordLocalWorkspaceTestResult(t, runnerStore, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-prepare",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
		LogicalCapacityBytes: 8 << 30, ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
	}, now.Add(time.Second))
	assertRestorePhase(t, runnerStore, "prepared", "effect-swap", "command-swap", 3, "pending")

	recordLocalWorkspaceTestResult(t, runnerStore, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-swap",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
		LogicalCapacityBytes: 8 << 30, ReceiptRecordedAtUnixMs: uint64(now.Add(2 * time.Second).UnixMilli()),
	}, now.Add(2*time.Second))
	assertRestorePhase(t, runnerStore, "database_committed", "", "command-finalize", 4, "succeeded")
	var auditAction, auditOutcome string
	if err := runnerStore.pool.QueryRow(t.Context(), `
		SELECT action,outcome FROM secondbox.audit_events
		WHERE id='audit_snapshot_restore_commit_restore-one'`,
	).Scan(&auditAction, &auditOutcome); err != nil {
		t.Fatal(err)
	}
	if auditAction != "snapshot.restore_committed" || auditOutcome != "succeeded" {
		t.Fatalf("restore commit audit = %q/%q", auditAction, auditOutcome)
	}

	recordLocalWorkspaceTestResult(t, runnerStore, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-finalize",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
		LogicalCapacityBytes: 8 << 30, ReceiptRecordedAtUnixMs: uint64(now.Add(3 * time.Second).UnixMilli()),
	}, now.Add(3*time.Second))
	assertRestorePhase(t, runnerStore, "finalized", "", "command-finalize", 4, "succeeded")
}

func TestLocalRestoreReconcilesAcrossControlPlaneRestartsAndDuplicateReceipts(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 19, 30, 0, 0, time.UTC)
	seedRunnerLocalRestore(t, store, now)
	phases := []struct {
		commandID  string
		kind       runnerv1.LocalWorkspaceCommandKind
		effectID   string
		state      string
		mutation   string
		generation int64
		operation  string
	}{
		{
			commandID: "command-prepare",
			kind:      runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
			effectID:  "effect-prepare", state: "prepared", mutation: "effect-swap",
			generation: 3, operation: "pending",
		},
		{
			commandID: "command-swap",
			kind:      runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
			effectID:  "effect-swap", state: "database_committed", mutation: "",
			generation: 4, operation: "succeeded",
		},
		{
			commandID: "command-finalize",
			kind:      runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
			effectID:  "effect-finalize", state: "finalized", mutation: "",
			generation: 4, operation: "succeeded",
		},
	}
	current := store
	for index, phase := range phases {
		phaseNow := now.Add(time.Duration(index+1) * time.Second)
		if _, err := current.pool.Exec(t.Context(), `
			UPDATE secondbox.runner_commands
			SET state='delivered',target_connection_id=$2,delivered_at=$3,updated_at=$3
			WHERE id=$1`,
			phase.commandID,
			fmt.Sprintf("connection-before-restart-%d", index),
			phaseNow,
		); err != nil {
			t.Fatal(err)
		}
		restarted, err := NewPostgresStateStore(
			t.Context(),
			current.pool.Config().ConnString(),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(restarted.Close)
		current = restarted
		if err := current.OpenConnection(
			t.Context(),
			RunnerIdentity{
				RunnerID:         "runner-home",
				CredentialSerial: fmt.Sprintf("credential-restart-%d", index),
			},
			fmt.Sprintf("connection-after-restart-%d", index),
			1,
			phaseNow,
		); err != nil {
			t.Fatal(err)
		}
		var commandState string
		if err := current.pool.QueryRow(t.Context(), `
			SELECT state FROM secondbox.runner_commands WHERE id=$1`,
			phase.commandID,
		).Scan(&commandState); err != nil {
			t.Fatal(err)
		}
		if commandState != "pending" {
			t.Fatalf(
				"restore command %q after restart = %q",
				phase.commandID,
				commandState,
			)
		}
		result := &runnerv1.LocalWorkspaceResult{
			CommandVersion: 1,
			Kind:           phase.kind,
			Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
			OperationId:    "operation-restore", EffectId: phase.effectID,
			SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
			SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
			LogicalCapacityBytes:    8 << 30,
			ReceiptRecordedAtUnixMs: uint64(phaseNow.UnixMilli()),
		}
		recordLocalWorkspaceTestResult(t, current, result, phaseNow)
		recordLocalWorkspaceTestResult(t, current, result, phaseNow)
		assertRestorePhase(
			t,
			current,
			phase.state,
			phase.mutation,
			map[runnerv1.LocalWorkspaceCommandKind]string{
				runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE:  "command-swap",
				runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP:     "command-finalize",
				runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE: "command-finalize",
			}[phase.kind],
			phase.generation,
			phase.operation,
		)
	}
}

func TestLocalRestoreFailureRetainsMutationUntilAbortReceipt(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 19, 45, 0, 0, time.UTC)
	seedRunnerLocalRestore(t, store, now)
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_INSUFFICIENT_SPACE,
		OperationId:    "operation-restore", EffectId: "effect-prepare",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore",
		SafeDetail: "local workspace storage has insufficient space",
	}, now.Add(time.Second))
	assertRestoreFailureState(
		t,
		store,
		"aborting",
		"effect-abort",
		"queued",
		0,
		"pending",
	)
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED,
		OperationId:    "operation-restore", EffectId: "effect-abort",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore",
		SafeDetail: "local workspace operation failed",
	}, now.Add(2*time.Second))
	assertRestoreFailureState(
		t,
		store,
		"aborting",
		"effect-abort",
		"queued",
		1,
		"pending",
	)
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-abort",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId:              "snapshot-restore",
		ReceiptRecordedAtUnixMs: uint64(now.Add(3 * time.Second).UnixMilli()),
	}, now.Add(3*time.Second))
	assertRestoreFailureState(
		t,
		store,
		"failed",
		"",
		"succeeded",
		1,
		"failed",
	)
}

func TestLocalRestoreCommitFencesEveryPreviousGenerationAuthority(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 19, 50, 0, 0, time.UTC)
	seedRunnerLocalRestore(t, store, now)
	seedStaleRestoreGenerationAuthority(t, store, now)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,
			created_at,updated_at
		) VALUES (
			'workspace-restore-neighbor','tenant','subject',
			'sandbox-restore-neighbor','runner-neighbor','ready',
			8589934592,9,'','','','',NULL,NULL,'','{"neighbor":true}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,
			desired_state,generation,workspace_id,current_instance_id,
			metadata_json,compatibility_summary_json,revision,created_at,updated_at
		) VALUES (
			'sandbox-restore-neighbor','tenant','subject','profile','revision',
			'stopped','stopped',9,'workspace-restore-neighbor','','{}','{}',7,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
	); err != nil {
		t.Fatal(err)
	}
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-prepare",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
		LogicalCapacityBytes:    8 << 30,
		ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
	}, now.Add(time.Second))
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-swap",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
		LogicalCapacityBytes:    8 << 30,
		ReceiptRecordedAtUnixMs: uint64(now.Add(2 * time.Second).UnixMilli()),
	}, now.Add(2*time.Second))
	var (
		assignmentState, instanceState, guestLiveness string
		commandState, effectState, operationState     string
		operationError, leaseState, activityState     string
		sessionState, infrastructureFailure           string
		portState, restoreOperationState              string
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT assignment.state,instance.state,instance.guest_liveness,
		       command.state,effect.state,operation.state,operation.error_code,
		       lease.state,activity.state,session.state,
		       session.infrastructure_failure_reason,port.state,restore_operation.state
		FROM secondbox.assignments AS assignment
		JOIN secondbox.instances AS instance ON instance.id=assignment.instance_id
		JOIN secondbox.runner_commands AS command
		  ON command.id='command-stale-generation'
		JOIN secondbox.lifecycle_effects AS effect
		  ON effect.id='effect-stale-generation'
		JOIN secondbox.operations AS operation
		  ON operation.id='operation-stale-generation'
		JOIN secondbox.leases AS lease ON lease.id='lease-stale-generation'
		JOIN secondbox.activity_sessions AS activity
		  ON activity.id='activity-stale-generation'
		JOIN secondbox.data_plane_sessions AS session
		  ON session.id='session-stale-generation'
		JOIN secondbox.port_sessions AS port ON port.id='port-stale-generation'
		JOIN secondbox.operations AS restore_operation
		  ON restore_operation.id='operation-restore'
		WHERE assignment.id='assignment-stale-generation'`,
	).Scan(
		&assignmentState,
		&instanceState,
		&guestLiveness,
		&commandState,
		&effectState,
		&operationState,
		&operationError,
		&leaseState,
		&activityState,
		&sessionState,
		&infrastructureFailure,
		&portState,
		&restoreOperationState,
	); err != nil {
		t.Fatal(err)
	}
	if assignmentState != "fenced" ||
		instanceState != "stopped" ||
		guestLiveness != "lost" ||
		commandState != "expired" ||
		effectState != "runner_failed" ||
		operationState != "failed" ||
		operationError != "generation_fenced" ||
		leaseState != "fenced" ||
		activityState != "closed" ||
		sessionState != "failed" ||
		infrastructureFailure != "INFRASTRUCTURE_FAILURE_REASON_GENERATION_FENCED" ||
		portState != "fenced" ||
		restoreOperationState != "succeeded" {
		t.Fatalf(
			"fenced authority assignment=%q instance=%q/%q command=%q effect=%q operation=%q/%q lease=%q activity=%q session=%q/%q port=%q restore=%q",
			assignmentState,
			instanceState,
			guestLiveness,
			commandState,
			effectState,
			operationState,
			operationError,
			leaseState,
			activityState,
			sessionState,
			infrastructureFailure,
			portState,
			restoreOperationState,
		)
	}
	var (
		neighborWorkspaceState, neighborSandboxState, neighborHome string
		neighborWorkspaceGeneration, neighborSandboxGeneration     int64
		restoreSnapshotState                                       string
		restoreSnapshotCount                                       int
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,sandbox.state,workspace.home_runner_id,
		       workspace.generation,sandbox.generation,
		       (SELECT state FROM secondbox.snapshots WHERE id='snapshot-restore'),
		       (SELECT count(*) FROM secondbox.snapshots
		        WHERE sandbox_id='sandbox-restore')
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox
		  ON sandbox.workspace_id=workspace.id
		WHERE workspace.id='workspace-restore-neighbor'`,
	).Scan(
		&neighborWorkspaceState,
		&neighborSandboxState,
		&neighborHome,
		&neighborWorkspaceGeneration,
		&neighborSandboxGeneration,
		&restoreSnapshotState,
		&restoreSnapshotCount,
	); err != nil {
		t.Fatal(err)
	}
	if neighborWorkspaceState != "ready" ||
		neighborSandboxState != "stopped" ||
		neighborHome != "runner-neighbor" ||
		neighborWorkspaceGeneration != 9 ||
		neighborSandboxGeneration != 9 ||
		restoreSnapshotState != "ready" ||
		restoreSnapshotCount != 1 {
		t.Fatalf(
			"restore isolation neighbor=%q/%q/%q/%d/%d chosen Snapshot=%q count=%d",
			neighborWorkspaceState,
			neighborSandboxState,
			neighborHome,
			neighborWorkspaceGeneration,
			neighborSandboxGeneration,
			restoreSnapshotState,
			restoreSnapshotCount,
		)
	}
}

func TestLocalRestoreSwapDatabaseRollbackPreservesOldAuthorityForRetry(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 19, 55, 0, 0, time.UTC)
	seedRunnerLocalRestore(t, store, now)
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-prepare",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
		LogicalCapacityBytes:    8 << 30,
		ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
	}, now.Add(time.Second))
	swapResult := &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-restore", EffectId: "effect-swap",
		SandboxId: "sandbox-restore", WorkspaceId: "workspace-restore",
		SnapshotId: "snapshot-restore", PreviousGeneration: 3, Generation: 4,
		LogicalCapacityBytes:    8 << 30,
		ReceiptRecordedAtUnixMs: uint64(now.Add(2 * time.Second).UnixMilli()),
	}
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := recordLocalWorkspaceResult(
		t.Context(),
		tx,
		"runner-home",
		swapResult,
		now.Add(2*time.Second),
	); err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var (
		restoreState, operationState, mutationEffect, commandState string
		sandboxGeneration, workspaceGeneration                     int64
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT restore.state,operation.state,workspace.mutation_effect_id,
		       command.state,sandbox.generation,workspace.generation
		FROM secondbox.workspace_restores AS restore
		JOIN secondbox.operations AS operation ON operation.id=restore.operation_id
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=restore.sandbox_id
		JOIN secondbox.workspaces AS workspace ON workspace.id=restore.workspace_id
		JOIN secondbox.runner_commands AS command ON command.id=restore.swap_command_id
		WHERE restore.id='restore-one'`,
	).Scan(
		&restoreState,
		&operationState,
		&mutationEffect,
		&commandState,
		&sandboxGeneration,
		&workspaceGeneration,
	); err != nil {
		t.Fatal(err)
	}
	if restoreState != "prepared" ||
		operationState != "pending" ||
		mutationEffect != "effect-swap" ||
		commandState != "pending" ||
		sandboxGeneration != 3 ||
		workspaceGeneration != 3 {
		t.Fatalf(
			"rollback restore=%q operation=%q mutation=%q command=%q generation=%d/%d",
			restoreState,
			operationState,
			mutationEffect,
			commandState,
			sandboxGeneration,
			workspaceGeneration,
		)
	}
	recordLocalWorkspaceTestResult(
		t,
		store,
		swapResult,
		now.Add(3*time.Second),
	)
	assertRestorePhase(
		t,
		store,
		"database_committed",
		"",
		"command-finalize",
		4,
		"succeeded",
	)
}

func TestGenerationAdvanceReceiptRetainsStopMutationUntilDatabaseCommit(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			'runner-home','pool','runner-home','ready','["amd64"]',
			'["compute","local-workspace"]','{}','[1]',1,1,'test','connection',0,
			'active','{}','[]',0,0,$1,1,$1,$1
		);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-stop','tenant','subject','sandbox-stop','runner-home','ready',
			8589934592,3,'stop','effect-stop','effect-stop','effect-stop',
			3,4,'advancing','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-stop','tenant','subject','profile','revision','stopping','stopped',
			3,'workspace-stop','','{}','{}',$1,1,$1,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-stop','sandbox-stop',3,'stop','queued','','','runner-home',
			'command-advance','',$2,0,8,$3,'',$1,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-advance','runner-home','effect-stop','local-workspace',$4,
			'delivered','connection',1,$1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now,
		[]byte("01234567890123456789012345678901"), now.Add(time.Minute), []byte{},
	); err != nil {
		t.Fatal(err)
	}
	result := &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "effect-stop", EffectId: "effect-stop",
		SandboxId: "sandbox-stop", WorkspaceId: "workspace-stop",
		PreviousGeneration: 3, Generation: 4, LogicalCapacityBytes: 8 << 30,
		ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
	}
	recordLocalWorkspaceTestResult(t, store, result, now.Add(time.Second))
	var workspaceGeneration int64
	var mutationState, effectState, commandState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.generation,workspace.mutation_state,effect.state,command.state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.lifecycle_effects AS effect ON effect.id=workspace.mutation_effect_id
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE workspace.id='workspace-stop'`,
	).Scan(&workspaceGeneration, &mutationState, &effectState, &commandState); err != nil {
		t.Fatal(err)
	}
	if workspaceGeneration != 3 ||
		mutationState != "runner_succeeded" ||
		effectState != "runner_succeeded" ||
		commandState != "acknowledged" {
		t.Fatalf(
			"generation=%d mutation=%q effect=%q command=%q",
			workspaceGeneration, mutationState, effectState, commandState,
		)
	}
	if err := store.OpenConnection(
		t.Context(),
		RunnerIdentity{
			RunnerID:         "runner-home",
			CredentialSerial: "credential-generation-reconcile",
		},
		"connection-generation-reconcile",
		1,
		now.Add(1500*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	reconcileID := "workspace-reconcile-connection-generation-reconcile"
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    reconcileID,
		EffectId:       reconcileID,
		Inventory: []*runnerv1.LocalWorkspaceInventoryItem{{
			WorkspaceId:          "workspace-stop",
			Generation:           4,
			LogicalCapacityBytes: 8 << 30,
			Formatted:            true,
		}},
		Receipts: []*runnerv1.LocalWorkspaceReceiptItem{{
			Kind:                    runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
			OperationId:             "effect-stop",
			WorkspaceId:             "workspace-stop",
			PreviousGeneration:      3,
			Generation:              4,
			LogicalCapacityBytes:    8 << 30,
			ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
		}},
		Correlation: &runnerv1.Correlation{
			RequestId:   reconcileID,
			OperationId: reconcileID,
			RunnerId:    "runner-home",
		},
	}, now.Add(2*time.Second))
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.workspaces SET mutation_state='advancing'
		WHERE id='workspace-stop';
		UPDATE secondbox.lifecycle_effects
		SET state='runner_failed',failure_class='stop_retry_exhausted',
		    failure_message='delivery deadline expired'
		WHERE id='effect-stop';
		UPDATE secondbox.runner_commands SET state='delivered'
		WHERE id='command-advance';
		UPDATE secondbox.sandboxes
		SET state='failed',lifecycle_failure_class='internal',
		    lifecycle_failure_message='stop effect failed',next_reconcile_at=NULL
		WHERE id='sandbox-stop'`,
		pgx.QueryExecModeSimpleProtocol,
	); err != nil {
		t.Fatal(err)
	}
	recordLocalWorkspaceTestResult(t, store, result, now.Add(3*time.Second))
	var sandboxState, failureClass, failureMessage string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.mutation_state,effect.state,sandbox.state,
		       sandbox.lifecycle_failure_class,sandbox.lifecycle_failure_message
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.lifecycle_effects AS effect ON effect.id=workspace.mutation_effect_id
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		WHERE workspace.id='workspace-stop'`,
	).Scan(
		&mutationState, &effectState, &sandboxState, &failureClass, &failureMessage,
	); err != nil {
		t.Fatal(err)
	}
	if mutationState != "runner_succeeded" ||
		effectState != "runner_succeeded" ||
		sandboxState != "stopping" ||
		failureClass != "" ||
		failureMessage != "" {
		t.Fatalf(
			"late generation receipt mutation=%q effect=%q Sandbox=%q failure=%q/%q",
			mutationState, effectState, sandboxState, failureClass, failureMessage,
		)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.workspaces
		SET state='failed',mutation_state='failed'
		WHERE id='workspace-stop';
		UPDATE secondbox.sandboxes
		SET state='failed',lifecycle_failure_class='home_workspace_conflict',
		    lifecycle_failure_message='home runner local Workspace evidence conflicts with durable authority',
		    next_reconcile_at=NULL
		WHERE id='sandbox-stop'`,
		pgx.QueryExecModeSimpleProtocol,
	); err != nil {
		t.Fatal(err)
	}
	recordLocalWorkspaceTestResult(t, store, result, now.Add(4*time.Second))
	var recoveredWorkspaceState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,workspace.mutation_state,sandbox.state,
		       sandbox.lifecycle_failure_class,sandbox.lifecycle_failure_message
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		WHERE workspace.id='workspace-stop'`,
	).Scan(
		&recoveredWorkspaceState,
		&mutationState,
		&sandboxState,
		&failureClass,
		&failureMessage,
	); err != nil {
		t.Fatal(err)
	}
	if recoveredWorkspaceState != "ready" ||
		mutationState != "runner_succeeded" ||
		sandboxState != "stopping" ||
		failureClass != "" ||
		failureMessage != "" {
		t.Fatalf(
			"recovered generation receipt Workspace=%q/%q Sandbox=%q failure=%q/%q",
			recoveredWorkspaceState,
			mutationState,
			sandboxState,
			failureClass,
			failureMessage,
		)
	}
}

func TestWorkspaceCreateReceiptHandsRunningSandboxDirectlyToStartMutation(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 20, 15, 0, 0, time.UTC)
	token := []byte("01234567890123456789012345678901")
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-create-running','tenant','subject','sandbox-create-running',
			'runner-home','creating',8589934592,1,'create','effect-create-running',
			'effect-create-running','operation-create-running',1,1,'queued','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			lifecycle_intent_kind,next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-create-running','tenant','subject','profile','revision','creating','running',
			1,'workspace-create-running','','{}','{}','create_workspace',$1,1,$1,$1
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES (
			'operation-create-running','tenant','subject','sandbox-create-running','',
			'create','pending','request-create-running','{}','','',false,$1,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-create-running','sandbox-create-running',1,'local_workspace_create','queued',
			'','','runner-home','command-create-running','',$2,0,8,$3,'',$1,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-create-running','runner-home','effect-create-running','local-workspace',
			$4,'delivered','connection-home',1,$1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now, token, now.Add(time.Minute), []byte{},
	); err != nil {
		t.Fatal(err)
	}
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-create-running",
		EffectId:       "effect-create-running",
		SandboxId:      "sandbox-create-running",
		WorkspaceId:    "workspace-create-running",
		Generation:     1, LogicalCapacityBytes: 8589934592,
		ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
	}, now.Add(time.Second))
	var (
		workspaceState, mutationKind, mutationID, mutationOperationID string
		mutationState, sandboxState, desiredState, operationState     string
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.state,workspace.mutation_kind,workspace.mutation_id,
		       workspace.mutation_operation_id,workspace.mutation_state,
		       sandbox.state,sandbox.desired_state,operation.state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.sandboxes AS sandbox ON sandbox.workspace_id=workspace.id
		JOIN secondbox.operations AS operation ON operation.id='operation-create-running'
		WHERE workspace.id='workspace-create-running'`,
	).Scan(
		&workspaceState, &mutationKind, &mutationID, &mutationOperationID,
		&mutationState, &sandboxState, &desiredState, &operationState,
	); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "ready" ||
		mutationKind != "start" ||
		mutationID != "operation-create-running" ||
		mutationOperationID != "operation-create-running" ||
		mutationState != "queued" ||
		sandboxState != "stopped" ||
		desiredState != "running" ||
		operationState != "pending" {
		t.Fatalf(
			"Workspace=%q mutation=%q/%q/%q/%q Sandbox=%q/%q Operation=%q",
			workspaceState, mutationKind, mutationID, mutationOperationID,
			mutationState, sandboxState, desiredState, operationState,
		)
	}
}

func TestWorkspaceDeleteReceiptFinalizesSandboxAndAllLocalSnapshotMetadata(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 20, 20, 0, 0, time.UTC)
	token := []byte("01234567890123456789012345678901")
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-delete-result','tenant','subject','sandbox-delete-result',
			'runner-home','deleting',8589934592,3,'workspace_delete','effect-delete-result',
			'effect-delete-result','operation-delete-result',3,3,'deleting','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-delete-result','tenant','subject','profile','revision','deleting','deleted',
			3,'workspace-delete-result','','{}','{}',$1,5,$1,$1
		);
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at,retention_ended_at
		) VALUES (
			'snapshot-delete-result','tenant','subject','sandbox-delete-result',
			'workspace-delete-result','runner-home','operation-snapshot','effect-snapshot',
			'{}',3,'retained',8589934592,'{}','ready',$2,$1,$1,NULL
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES (
			'operation-delete-result','tenant','subject','sandbox-delete-result','',
			'delete','pending','request-delete-result','{}','','',false,$1,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-delete-result','sandbox-delete-result',3,'local_workspace_delete','queued',
			'','','runner-home','command-delete-result','',$3,0,8,$2,'',$1,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-delete-result','runner-home','effect-delete-result','local-workspace',
			$4,'delivered','connection-home',1,$1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now, now.Add(time.Hour), token, []byte{},
	); err != nil {
		t.Fatal(err)
	}
	result := &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    "operation-delete-result",
		EffectId:       "effect-delete-result",
		SandboxId:      "sandbox-delete-result",
		WorkspaceId:    "workspace-delete-result",
		Generation:     3, LogicalCapacityBytes: 8589934592,
		ReceiptRecordedAtUnixMs: uint64(now.Add(time.Second).UnixMilli()),
	}
	recordLocalWorkspaceTestResult(t, store, result, now.Add(time.Second))
	var (
		sandboxState, workspaceState, snapshotState, operationState string
		effectState, commandState, mutationState                    string
		deletedAt                                                   *time.Time
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT sandbox.state,workspace.state,snapshot.state,operation.state,
		       effect.state,command.state,workspace.mutation_state,sandbox.deleted_at
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.snapshots AS snapshot ON snapshot.workspace_id=workspace.id
		JOIN secondbox.operations AS operation ON operation.id='operation-delete-result'
		JOIN secondbox.lifecycle_effects AS effect ON effect.id='effect-delete-result'
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE sandbox.id='sandbox-delete-result'`,
	).Scan(
		&sandboxState, &workspaceState, &snapshotState, &operationState,
		&effectState, &commandState, &mutationState, &deletedAt,
	); err != nil {
		t.Fatal(err)
	}
	if sandboxState != "deleted" ||
		workspaceState != "deleted" ||
		snapshotState != "deleted" ||
		operationState != "succeeded" ||
		effectState != "succeeded" ||
		commandState != "acknowledged" ||
		mutationState != "" ||
		deletedAt == nil {
		t.Fatalf(
			"delete completion Sandbox=%q Workspace=%q Snapshot=%q Operation=%q effect=%q command=%q mutation=%q deletedAt=%v",
			sandboxState, workspaceState, snapshotState, operationState,
			effectState, commandState, mutationState, deletedAt,
		)
	}
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := recordLocalWorkspaceResult(
		t.Context(), tx, "runner-home", result, now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("delete completion replay: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestFailedStartEvidenceRetainsDurableWorkspaceMutationForReconciliation(t *testing.T) {
	testCases := []struct {
		name            string
		assignmentState string
		message         func(*runnerv1.AssignmentFence) *runnerv1.RunnerToControlPlane
	}{
		{
			name: "rejected acknowledgement", assignmentState: "assigned",
			message: func(fence *runnerv1.AssignmentFence) *runnerv1.RunnerToControlPlane {
				return &runnerv1.RunnerToControlPlane{
					Message: &runnerv1.RunnerToControlPlane_AssignmentAck{
						AssignmentAck: &runnerv1.AssignmentAck{
							Fence:      fence,
							Decision:   runnerv1.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_CAPACITY,
							SafeDetail: "capacity changed after scheduling",
						},
					},
				}
			},
		},
		{
			name: "failed assignment result", assignmentState: "starting",
			message: func(fence *runnerv1.AssignmentFence) *runnerv1.RunnerToControlPlane {
				return &runnerv1.RunnerToControlPlane{
					Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
						AssignmentResult: &runnerv1.AssignmentResult{
							Fence:      fence,
							Terminal:   runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_INFRASTRUCTURE_FAILED,
							SafeDetail: "launch failed",
							Correlation: &runnerv1.Correlation{
								RequestId: "request-start", OperationId: "operation-start",
								SandboxId: fence.SandboxId, InstanceId: fence.InstanceId,
								SandboxGeneration: fence.SandboxGeneration,
								AssignmentId:      fence.AssignmentId, RunnerId: "runner-home",
							},
						},
					},
				}
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			store := openRunnerControlDatabase(t)
			now := time.Date(2026, 7, 29, 20, 30, 0, 0, time.UTC)
			fence := seedStartingAssignment(
				t, store, strings.ReplaceAll(testCase.name, " ", "-"),
				testCase.assignmentState, now,
			)
			insertDeliveredAssignmentCommand(
				t,
				store,
				fence,
				"assignment-command-"+strings.ReplaceAll(testCase.name, " ", "-"),
				now,
			)
			tx, err := store.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := recordAssignmentEvent(
				t.Context(), tx, "runner-home", testCase.message(fence), now.Add(time.Second),
			); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			var assignmentState, mutationKind, mutationState, commandState string
			if err := store.pool.QueryRow(t.Context(), `
				SELECT assignment.state,workspace.mutation_kind,workspace.mutation_state,
				       command.state
				FROM secondbox.assignments AS assignment
				JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
				JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
				JOIN secondbox.runner_commands AS command
				  ON command.assignment_id=assignment.id AND command.kind='assignment'
				WHERE assignment.id=$1`,
				fence.AssignmentId,
			).Scan(
				&assignmentState,
				&mutationKind,
				&mutationState,
				&commandState,
			); err != nil {
				t.Fatal(err)
			}
			if assignmentState != "failed" ||
				mutationKind != "start" ||
				mutationState != "assigned" ||
				commandState != "acknowledged" {
				t.Fatalf(
					"failed start assignment=%q mutation=%q/%q command=%q",
					assignmentState, mutationKind, mutationState, commandState,
				)
			}
		})
	}
}

func TestStartupTimeoutFenceResultDoesNotRequireStopEffect(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 20, 40, 0, 0, time.UTC)
	fence := seedStartingAssignment(t, store, "startup-timeout", "fencing", now)
	correlation := &runnerv1.Correlation{
		RequestId: "request-startup-timeout", OperationId: "operation-start",
		SandboxId: fence.SandboxId, InstanceId: fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration,
		AssignmentId:      fence.AssignmentId, RunnerId: "runner-home",
	}
	commandID := "fence-command-startup-timeout"
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Fence{
			Fence: &runnerv1.FenceCommand{
				MessageId: commandID, Sequence: 1, Fence: fence,
				Reason:      runnerv1.FenceReason_FENCE_REASON_ASSIGNMENT_REPLACED,
				Correlation: correlation,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.assignments
		SET failure_class='startup_timeout' WHERE id=$1;
		UPDATE secondbox.workspaces
		SET state='failed',mutation_state='failed' WHERE sandbox_id=$2;
		UPDATE secondbox.sandboxes
		SET state='failed' WHERE id=$2;
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($3,'runner-home',$1,'fence',$4,'delivered',
		          'connection-old',1,$5,$5,$5)`,
		pgx.QueryExecModeSimpleProtocol,
		fence.AssignmentId,
		fence.SandboxId,
		commandID,
		payload,
		now,
	); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	result := &runnerv1.FenceResult{
		Fence:                     fence,
		Result:                    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
		TerminationEvidenceDigest: "sha256:startup-timeout",
		Correlation:               correlation,
	}
	if err := recordFenceEvent(
		t.Context(), tx, "runner-home", result, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var assignmentState, instanceState, terminationReason, commandState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT assignment.state,instance.state,instance.termination_reason,command.state
		FROM secondbox.assignments AS assignment
		JOIN secondbox.instances AS instance ON instance.id=assignment.instance_id
		JOIN secondbox.runner_commands AS command ON command.id=$2
		WHERE assignment.id=$1`,
		fence.AssignmentId,
		commandID,
	).Scan(
		&assignmentState, &instanceState, &terminationReason, &commandState,
	); err != nil {
		t.Fatal(err)
	}
	if assignmentState != "fenced" ||
		instanceState != "stopped" ||
		terminationReason != "startup_failed" ||
		commandState != "acknowledged" {
		t.Fatalf(
			"startup-timeout fence assignment=%q instance=%q reason=%q command=%q",
			assignmentState, instanceState, terminationReason, commandState,
		)
	}
}

func TestReadyAssignmentRecordsInitialGuestHeartbeatEvidence(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 7, 29, 20, 45, 0, 0, time.UTC)
	fence := seedStartingAssignment(t, store, "ready-evidence", "starting", now)
	insertDeliveredAssignmentCommand(
		t,
		store,
		fence,
		"assignment-command-ready-evidence",
		now,
	)
	resultAt := now.Add(time.Second)
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := recordAssignmentEvent(
		t.Context(),
		tx,
		"runner-home",
		&runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
				AssignmentResult: &runnerv1.AssignmentResult{
					Fence:            fence,
					Terminal:         runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
					BackendKind:      "firecracker",
					BackendReference: "fc-ready-evidence",
					Correlation: &runnerv1.Correlation{
						RequestId: "request-start", OperationId: "operation-start",
						SandboxId: fence.SandboxId, InstanceId: fence.InstanceId,
						SandboxGeneration: fence.SandboxGeneration,
						AssignmentId:      fence.AssignmentId, RunnerId: "runner-home",
					},
				},
			},
		},
		resultAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var (
		state, liveness, mutationKind, commandState string
		readyAt, heartbeatAt                        time.Time
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT instance.state,instance.guest_liveness,instance.ready_at,
		       instance.guest_heartbeat_at,workspace.mutation_kind,command.state
		FROM secondbox.instances AS instance
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=instance.sandbox_id
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.runner_commands AS command
		  ON command.assignment_id=$2 AND command.kind='assignment'
		WHERE instance.id=$1`,
		fence.InstanceId,
		fence.AssignmentId,
	).Scan(
		&state,
		&liveness,
		&readyAt,
		&heartbeatAt,
		&mutationKind,
		&commandState,
	); err != nil {
		t.Fatal(err)
	}
	if state != "ready" ||
		liveness != "ready" ||
		!readyAt.Equal(resultAt) ||
		!heartbeatAt.Equal(resultAt) ||
		mutationKind != "" ||
		commandState != "acknowledged" {
		t.Fatalf(
			"ready evidence state=%q liveness=%q readyAt=%s heartbeatAt=%s mutation=%q command=%q",
			state,
			liveness,
			readyAt,
			heartbeatAt,
			mutationKind,
			commandState,
		)
	}
}

func insertDeliveredAssignmentCommand(
	t *testing.T,
	store *PostgresStateStore,
	fence *runnerv1.AssignmentFence,
	commandID string,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,'runner-home',$2,'assignment',$3,'delivered',
		          'connection-old',1,$4,$4,$4)`,
		commandID,
		fence.AssignmentId,
		[]byte{},
		now,
	); err != nil {
		t.Fatal(err)
	}
}

func seedStartingAssignment(
	t *testing.T,
	store *PostgresStateStore,
	suffix string,
	assignmentState string,
	now time.Time,
) *runnerv1.AssignmentFence {
	t.Helper()
	fence := &runnerv1.AssignmentFence{
		AssignmentId: "assignment-" + suffix, SandboxId: "sandbox-" + suffix,
		InstanceId: "instance-" + suffix, SandboxGeneration: 3,
		FencingToken: []byte("01234567890123456789012345678901"),
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			$1,'tenant','subject',$2,'runner-home','ready',8589934592,
			3,'start','operation-start',$3,'operation-start',3,3,'assigned','{}',$4,$4
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			$2,'tenant','subject','profile','revision','starting','running',
			3,$1,$5,'{}','{}',2,$4,$4
		);
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,created_at,updated_at
		) VALUES ($5,$2,3,'starting','starting','',$4,$4);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			$6,$2,$5,'runner-home','revision','firecracker','',3,$7,$8,
			'{}','{}','{}','',0,8,$9,$9,'',$9,$4,1,$4,$4
		)`,
		pgx.QueryExecModeSimpleProtocol,
		"workspace-"+suffix, fence.SandboxId, "assignment-command-"+suffix,
		now, fence.InstanceId, fence.AssignmentId, fence.FencingToken,
		assignmentState, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	return fence
}

func recordLocalWorkspaceTestResult(
	t *testing.T,
	store *PostgresStateStore,
	result *runnerv1.LocalWorkspaceResult,
	now time.Time,
) {
	t.Helper()
	tx, err := store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := recordLocalWorkspaceResult(
		t.Context(), tx, "runner-home", result, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func assertRestorePhase(
	t *testing.T,
	store *PostgresStateStore,
	wantRestoreState string,
	wantMutationEffect string,
	wantQueuedCommand string,
	wantGeneration int64,
	wantOperationState string,
) {
	t.Helper()
	var restoreState, mutationEffect, mutationState, commandState, operationState string
	var generation int64
	var payload []byte
	if err := store.pool.QueryRow(t.Context(), `
		SELECT restore.state,workspace.mutation_effect_id,workspace.mutation_state,
		       workspace.generation,command.state,operation.state,command.payload
		FROM secondbox.workspace_restores AS restore
		JOIN secondbox.workspaces AS workspace ON workspace.id=restore.workspace_id
		JOIN secondbox.runner_commands AS command ON command.id=$1
		JOIN secondbox.operations AS operation ON operation.id=restore.operation_id
		WHERE restore.id='restore-one'`,
		wantQueuedCommand,
	).Scan(
		&restoreState, &mutationEffect, &mutationState,
		&generation, &commandState, &operationState, &payload,
	); err != nil {
		t.Fatal(err)
	}
	if restoreState != wantRestoreState ||
		mutationEffect != wantMutationEffect ||
		generation != wantGeneration ||
		commandState != "pending" && commandState != "acknowledged" ||
		operationState != wantOperationState {
		t.Fatalf(
			"restore=%q mutation=%q/%q generation=%d command=%q operation=%q",
			restoreState, mutationEffect, mutationState, generation, commandState, operationState,
		)
	}
	envelope := &runnerv1.ControlPlaneToRunner{}
	if err := proto.Unmarshal(payload, envelope); err != nil {
		t.Fatal(err)
	}
	command := envelope.GetLocalWorkspace()
	if command == nil ||
		command.Correlation == nil ||
		command.Correlation.SandboxGeneration != uint64(wantGeneration) {
		t.Fatalf(
			"restore command correlation = %#v, want generation %d",
			command,
			wantGeneration,
		)
	}
}

func assertRestoreFailureState(
	t *testing.T,
	store *PostgresStateStore,
	wantRestoreState string,
	wantMutationEffect string,
	wantEffectState string,
	wantRetryCount int64,
	wantOperationState string,
) {
	t.Helper()
	var (
		restoreState, mutationEffect, mutationState string
		effectState, operationState                 string
		retryCount                                  int64
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT restore.state,workspace.mutation_effect_id,workspace.mutation_state,
		       effect.state,effect.retry_count,operation.state
		FROM secondbox.workspace_restores AS restore
		JOIN secondbox.workspaces AS workspace ON workspace.id=restore.workspace_id
		JOIN secondbox.lifecycle_effects AS effect ON effect.id=restore.abort_effect_id
		JOIN secondbox.operations AS operation ON operation.id=restore.operation_id
		WHERE restore.id='restore-one'`,
	).Scan(
		&restoreState,
		&mutationEffect,
		&mutationState,
		&effectState,
		&retryCount,
		&operationState,
	); err != nil {
		t.Fatal(err)
	}
	wantMutationState := "queued"
	if wantMutationEffect == "" {
		wantMutationState = ""
	}
	if restoreState != wantRestoreState ||
		mutationEffect != wantMutationEffect ||
		mutationState != wantMutationState ||
		effectState != wantEffectState ||
		retryCount != wantRetryCount ||
		operationState != wantOperationState {
		t.Fatalf(
			"restore failure state=%q mutation=%q/%q effect=%q retry=%d operation=%q",
			restoreState,
			mutationEffect,
			mutationState,
			effectState,
			retryCount,
			operationState,
		)
	}
}

func seedStaleRestoreGenerationAuthority(
	t *testing.T,
	store *PostgresStateStore,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.instances (
			id,sandbox_id,generation,state,guest_liveness,termination_reason,
			created_at,updated_at,ready_at,guest_heartbeat_at,maximum_duration_at,stopped_at
		) VALUES (
			'instance-stale-generation','sandbox-restore',3,'ready','ready','',
			$1,$1,$1,$1,$2,NULL
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-stale-generation','sandbox-restore','instance-stale-generation',
			'runner-home','revision','firecracker','vm-stale',3,$3,'ready','{}','[]',
			'{}','',0,8,$2,$2,'worker-stale',$2,$1,1,$1,$1
		);
		INSERT INTO secondbox.leases (
			id,tenant_ref,subject_ref,sandbox_id,generation,state,expires_at,
			revision,created_at,updated_at
		) VALUES (
			'lease-stale-generation','tenant','subject','sandbox-restore',3,
			'active',$2,1,$1,$1
		);
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,
			last_activity_at,created_at,updated_at,closed_at
		) VALUES (
			'activity-stale-generation','tenant','subject','sandbox-restore',3,
			'exec','active','lease-stale-generation',$1,$1,$1,NULL
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,
			created_at,started_at,completed_at,updated_at
		) VALUES (
			'operation-stale-generation','tenant','subject','sandbox-restore','',
			'exec','running','request-stale-generation','{}','','',true,
			$1,$1,NULL,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-stale-generation','sandbox-restore',3,'start','queued',
			'assignment-stale-generation','instance-stale-generation','runner-home',
			'command-stale-generation','',$3,0,8,$2,'worker-stale',$2,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-stale-generation','runner-home','assignment-stale-generation',
			'assignment',$4,'delivered','connection-stale',1,$1,$1,$1
		);
		INSERT INTO secondbox.data_plane_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,
			instance_id,runner_id,generation,fencing_token,request_id,lease_id,kind,
			operation,stream_id,state,priority,idempotency_key,request_hash,deadline_at,
			maximum_response_bytes,maximum_request_bytes,stream_window_bytes,
			response_credit_bytes,request_stream_bytes,request_stream_closed,detachable,
			terminal_detach_seconds,attachment_id,attached_at,detached_at,detach_expires_at,
			outbound_bytes,inbound_bytes,next_inbound_sequence,terminal_kind,terminal_detail,
			exit_code,signal,spawn_failure_reason,elapsed_milliseconds,limit_bytes,
			infrastructure_failure_reason,retryable,terminal_message,stdout_bytes,
			stderr_bytes,content_bytes,metadata_json,request_json,created_at,updated_at,
			completed_at,retain_until
		) VALUES (
			'session-stale-generation','tenant','subject','sandbox-restore','revision',
			'assignment-stale-generation','instance-stale-generation','runner-home',3,$3,
			'request-session-stale','lease-stale-generation','exec','exec',
			'stream-stale','running',0,'idempotency-session-stale','hash-session-stale',$2,
			1024,1024,1024,0,0,false,false,30,'',NULL,NULL,NULL,0,0,1,'','',0,0,'',
			0,0,'',false,'',$4,$4,$4,'{}','{}',$1,$1,NULL,$2
		);
		INSERT INTO secondbox.port_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,
			data_plane_session_id,lease_id,generation,name,guest_port,protocol,
			stream_window_bytes,client_credit_bytes,client_bytes,runner_bytes,state,
			idempotency_key,request_hash,expires_at,created_at,updated_at,
			connected_at,closed_at
		) VALUES (
			'port-stale-generation','tenant','subject','sandbox-restore','revision',
			'session-stale-generation','lease-stale-generation',3,'web',8080,'tcp',
			1024,1024,0,0,'open','idempotency-port-stale','hash-port-stale',
			$2,$1,$1,$1,NULL
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		now.Add(time.Hour),
		[]byte("01234567890123456789012345678901"),
		[]byte{},
	); err != nil {
		t.Fatal(err)
	}
}

func seedPendingWorkspaceCreation(
	t *testing.T,
	store *PostgresStateStore,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-create-reconcile','tenant','subject','sandbox-create-reconcile',
			'runner-home','creating',8589934592,1,'create','effect-create-reconcile',
			'effect-create-reconcile','operation-create-reconcile',1,1,'queued','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			lifecycle_intent_kind,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-create-reconcile','tenant','subject','profile','revision',
			'creating','stopped',1,'workspace-create-reconcile','','{}','{}',
			'create_workspace','lifecycle-worker',$3,$1,1,$1,$1
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES (
			'operation-create-reconcile','tenant','subject','sandbox-create-reconcile','',
			'create','pending','request-create-reconcile','{}','','',false,$1,$1
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-create-reconcile','sandbox-create-reconcile',1,
			'local_workspace_create','queued','','','runner-home',
			'command-create-reconcile','',$2,0,8,$3,'',$1,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-create-reconcile','runner-home','effect-create-reconcile',
			'local-workspace',$4,'delivered','connection-before-return',1,$1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
		[]byte("01234567890123456789012345678901"),
		now.Add(time.Hour),
		[]byte{},
	); err != nil {
		t.Fatal(err)
	}
}

func seedReadyReconciledWorkspace(
	t *testing.T,
	store *PostgresStateStore,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-ready-reconcile','tenant','subject','sandbox-ready-reconcile',
			'runner-home','ready',8589934592,3,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'sandbox-ready-reconcile','tenant','subject','profile','revision',
			'stopped','stopped',3,'workspace-ready-reconcile','','{}','{}',$1,1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol,
		now,
	); err != nil {
		t.Fatal(err)
	}
}

func recordReturningRunnerReconciliation(
	t *testing.T,
	store *PostgresStateStore,
	connectionID string,
	inventory []*runnerv1.LocalWorkspaceInventoryItem,
	now time.Time,
) {
	t.Helper()
	if err := store.OpenConnection(
		t.Context(),
		RunnerIdentity{
			RunnerID:         "runner-home",
			CredentialSerial: "credential-" + connectionID,
		},
		connectionID,
		1,
		now,
	); err != nil {
		t.Fatal(err)
	}
	reconcileID := "workspace-reconcile-" + connectionID
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion: 1,
		Kind:           runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE,
		Terminal:       runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:    reconcileID,
		EffectId:       reconcileID,
		Inventory:      inventory,
		Correlation: &runnerv1.Correlation{
			RequestId:   reconcileID,
			OperationId: reconcileID,
			RunnerId:    "runner-home",
		},
	}, now)
}

func seedRunnerLocalRestore(
	t *testing.T,
	store *PostgresStateStore,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			'runner-home','pool','runner-home','ready','["amd64"]',
			'["compute","local-workspace"]','{}','[1]',1,1,'test','connection',0,
			'active','{}','[]',0,0,$1,1,$1,$1
		);
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-restore','tenant','subject','sandbox-restore','runner-home','ready',
			8589934592,3,'','','','',NULL,NULL,'','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sandbox-restore','tenant','subject','profile','revision','stopped','stopped',
			3,'workspace-restore','','{}','{}',1,$1,$1
		);
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at,retention_ended_at
		) VALUES (
			'snapshot-restore','tenant','subject','sandbox-restore','workspace-restore',
			'runner-home','operation-snapshot','effect-snapshot','{}',3,'before',
			8589934592,'{}','ready',$2,$1,$1,NULL
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,
			created_at,started_at,completed_at,updated_at
		) VALUES (
			'operation-restore','tenant','subject','sandbox-restore','snapshot-restore',
			'snapshot_restore','pending','request-restore','{}','','',false,$1,NULL,NULL,$1
		);
		INSERT INTO secondbox.workspace_restores (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,snapshot_id,home_runner_id,
			operation_id,prepare_effect_id,swap_effect_id,finalize_effect_id,abort_effect_id,
			prepare_command_id,swap_command_id,finalize_command_id,abort_command_id,
			expected_generation,target_generation,state,prepare_receipt_json,swap_receipt_json,
			finalize_receipt_json,abort_receipt_json,failure_class,failure_message,
			created_at,updated_at,database_committed_at,finalized_at,failed_at
		) VALUES (
			'restore-one','tenant','subject','sandbox-restore','workspace-restore',
			'snapshot-restore','runner-home','operation-restore',
			'effect-prepare','effect-swap','effect-finalize','effect-abort',
			'command-prepare','command-swap','command-finalize','command-abort',
			3,4,'requested','{}','{}','{}','{}','','',$1,$1,NULL,NULL,NULL
		);
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-prepare','sandbox-restore',3,'local_snapshot_restore_prepare','queued',
			'','','runner-home','command-prepare','snapshot-restore',$3,0,8,$2,'',$1,
			'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-prepare','runner-home','effect-prepare','local-workspace',$4,
			'pending','',0,$1,$1,NULL
		);
		UPDATE secondbox.workspaces
		SET mutation_kind='snapshot_restore',mutation_id='restore-one',
		    mutation_effect_id='effect-prepare',mutation_operation_id='operation-restore',
		    mutation_expected_generation=3,mutation_target_generation=4,
		    mutation_state='queued'
		WHERE id='workspace-restore'`,
		pgx.QueryExecModeSimpleProtocol, now, now.Add(time.Hour),
		[]byte("01234567890123456789012345678901"), []byte{},
	); err != nil {
		t.Fatal(err)
	}
}

func openRunnerControlDatabase(
	t *testing.T,
) *PostgresStateStore {
	t.Helper()
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("SECONDBOX_TEST_DATABASE_URL is required for PostgreSQL runner-control tests")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf(
		"secondbox_runnercontrol_test_%d_%d",
		os.Getpid(), runnerControlTestDatabaseSequence.Add(1),
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
	testURL := parsed.String()
	if err := postgresmigrations.Apply(t.Context(), testURL); err != nil {
		admin.Exec(t.Context(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close(t.Context())
		t.Fatal(err)
	}
	runnerStore, err := NewPostgresStateStore(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runnerStore.Close()
		cleanupContext := context.Background()
		if _, err := admin.Exec(
			cleanupContext, "DROP DATABASE "+identifier+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop runner-control test database: %v", err)
		}
		if err := admin.Close(cleanupContext); err != nil {
			t.Errorf("close runner-control test admin connection: %v", err)
		}
	})
	return runnerStore
}
