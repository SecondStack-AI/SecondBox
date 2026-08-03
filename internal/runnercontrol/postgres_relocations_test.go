package runnercontrol

import (
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
)

func TestWorkspaceRelocationHomeFlipFollowsDurableTargetImport(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 3, 17, 0, 0, 0, time.UTC)
	seedWorkspaceRelocationAuthority(t, store, "commit", now)
	recordWorkspaceRelocationExport(t, store, "commit", now.Add(time.Second))
	assertWorkspaceRelocationAuthority(t, store, "commit", "runner-home", "source_sealed", "source_sealed")

	frame := &runnerv1.WorkspaceTransferFrame{
		OperationId: "operation-relocation-commit",
		SandboxId:   "sandbox-relocation-commit",
		WorkspaceId: "workspace-relocation-commit",
		Generation:  3,
		Sequence:    1,
		Payload: &runnerv1.WorkspaceTransferFrame_Result{
			Result: &runnerv1.WorkspaceTransferResult{
				Terminal:  runnerv1.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_SUCCEEDED,
				SizeBytes: 8 << 30,
				Sha256:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
		},
	}
	peer, err := store.RouteWorkspaceTransfer(
		t.Context(), "runner-relocation-target", frame, now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if peer != "runner-home" {
		t.Fatalf("target result peer = %q", peer)
	}
	assertWorkspaceRelocationAuthority(t, store, "commit", "runner-relocation-target", "deleting_source", "deleting_source")

	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion:       1,
		Kind:                 runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE,
		Terminal:             runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:          "operation-relocation-commit",
		EffectId:             "relocation-commit-delete-source",
		SandboxId:            "sandbox-relocation-commit",
		WorkspaceId:          "workspace-relocation-commit",
		Generation:           3,
		LogicalCapacityBytes: 8 << 30,
		ReceiptRecordedAtUnixMs: uint64(
			now.Add(3 * time.Second).UnixMilli(),
		),
	}, now.Add(3*time.Second))
	assertWorkspaceRelocationAuthority(t, store, "commit", "runner-relocation-target", "succeeded", "")
	var operationState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.operations WHERE id='operation-relocation-commit'`,
	).Scan(&operationState); err != nil {
		t.Fatal(err)
	}
	if operationState != "succeeded" {
		t.Fatalf("completed relocation Operation state = %q", operationState)
	}
}

func TestWorkspaceRelocationCrashBeforeHomeFlipRestoresOriginalSource(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 3, 17, 30, 0, 0, time.UTC)
	seedWorkspaceRelocationAuthority(t, store, "crash", now)
	recordWorkspaceRelocationExport(t, store, "crash", now.Add(time.Second))
	assertWorkspaceRelocationAuthority(t, store, "crash", "runner-home", "source_sealed", "source_sealed")

	if err := store.FailWorkspaceTransfer(
		t.Context(),
		"operation-relocation-crash",
		"workspace_relocation_interrupted",
		"Workspace relocation control plane connection ended",
		now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceRelocationAuthority(t, store, "crash", "runner-home", "aborting", "aborting_source_sealed")
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion:       1,
		Kind:                 runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE,
		Terminal:             runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:          "operation-relocation-crash",
		EffectId:             "relocation-crash-abort-source",
		SandboxId:            "sandbox-relocation-crash",
		WorkspaceId:          "workspace-relocation-crash",
		Generation:           3,
		LogicalCapacityBytes: 8 << 30,
		ReceiptRecordedAtUnixMs: uint64(
			now.Add(3 * time.Second).UnixMilli(),
		),
	}, now.Add(3*time.Second))
	assertWorkspaceRelocationAuthority(t, store, "crash", "runner-home", "failed", "")
	var sandboxState, desiredState, currentInstanceID, operationState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT sandbox.state,sandbox.desired_state,sandbox.current_instance_id,operation.state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.operations AS operation ON operation.sandbox_id=sandbox.id
		WHERE sandbox.id='sandbox-relocation-crash'`,
	).Scan(&sandboxState, &desiredState, &currentInstanceID, &operationState); err != nil {
		t.Fatal(err)
	}
	if sandboxState != "stopped" || desiredState != "stopped" ||
		currentInstanceID != "" || operationState != "failed" {
		t.Fatalf(
			"restored source Sandbox state=%q desired=%q instance=%q operation=%q",
			sandboxState, desiredState, currentInstanceID, operationState,
		)
	}
}

func TestWorkspaceRelocationSourceReconnectRestartsFromSealedSource(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 3, 18, 0, 0, 0, time.UTC)
	seedWorkspaceRelocationAuthority(t, store, "restart", now)
	recordWorkspaceRelocationExport(t, store, "restart", now.Add(time.Second))
	if err := store.OpenConnection(t.Context(), RunnerIdentity{
		RunnerID: "runner-home", CredentialSerial: "credential-relocation-restart",
	}, "connection-relocation-restart", 2, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceRelocationAuthority(t, store, "restart", "runner-home", "source_sealed", "source_sealed")
	var commandState, exportCommandID, mutationEffectID string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT command.state,relocation.export_command_id,workspace.mutation_effect_id
		FROM secondbox.workspace_relocations AS relocation
		JOIN secondbox.workspaces AS workspace ON workspace.id=relocation.workspace_id
		JOIN secondbox.runner_commands AS command ON command.id=relocation.export_command_id
		WHERE relocation.id='relocation-restart'`,
	).Scan(&commandState, &exportCommandID, &mutationEffectID); err != nil {
		t.Fatal(err)
	}
	wantCommandID := "relocation-restart-export-restart-connection-relocation-restart"
	if commandState != "pending" || exportCommandID != wantCommandID ||
		mutationEffectID != wantCommandID {
		t.Fatalf(
			"restarted export state=%q command=%q mutation=%q",
			commandState, exportCommandID, mutationEffectID,
		)
	}
}

func TestWorkspaceRelocationTargetReconnectRestartsFromSealedSource(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Date(2026, 8, 3, 18, 15, 0, 0, time.UTC)
	seedWorkspaceRelocationAuthority(t, store, "target-restart", now)
	recordWorkspaceRelocationExport(t, store, "target-restart", now.Add(time.Second))
	if err := store.OpenConnection(t.Context(), RunnerIdentity{
		RunnerID: "runner-relocation-target", CredentialSerial: "credential-target-restart",
	}, "connection-relocation-target-restart", 2, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var commandState, commandRunnerID, exportCommandID, mutationEffectID string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT command.state,command.runner_id,relocation.export_command_id,
		       workspace.mutation_effect_id
		FROM secondbox.workspace_relocations AS relocation
		JOIN secondbox.workspaces AS workspace ON workspace.id=relocation.workspace_id
		JOIN secondbox.runner_commands AS command ON command.id=relocation.export_command_id
		WHERE relocation.id='relocation-target-restart'`,
	).Scan(&commandState, &commandRunnerID, &exportCommandID, &mutationEffectID); err != nil {
		t.Fatal(err)
	}
	wantCommandID := "relocation-target-restart-export-restart-connection-relocation-target-restart"
	if commandState != "pending" || commandRunnerID != "runner-home" ||
		exportCommandID != wantCommandID || mutationEffectID != wantCommandID {
		t.Fatalf(
			"target reconnect command state=%q runner=%q id=%q mutation=%q",
			commandState, commandRunnerID, exportCommandID, mutationEffectID,
		)
	}
}

func seedWorkspaceRelocationAuthority(
	t *testing.T,
	store *PostgresStateStore,
	suffix string,
	now time.Time,
) {
	t.Helper()
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,
			logical_capacity_bytes,generation,mutation_kind,mutation_id,
			mutation_effect_id,mutation_operation_id,mutation_expected_generation,
			mutation_target_generation,mutation_state,local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-relocation-' || $1,'tenant','subject','sandbox-relocation-' || $1,
			'runner-home','ready',8589934592,3,'relocate','relocation-' || $1,
			'command-relocation-export-' || $1,'operation-relocation-' || $1,3,3,
			'queued','{}',$2,$2
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,
			compatibility_summary_json,revision,created_at,updated_at
		) VALUES (
			'sandbox-relocation-' || $1,'tenant','subject','profile','revision',
			'stopped','stopped',3,'workspace-relocation-' || $1,'','{}','{}',1,$2,$2
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,created_at,updated_at
		) VALUES (
			'operation-relocation-' || $1,'tenant','subject','sandbox-relocation-' || $1,
			'','relocate','pending','request-relocation-' || $1,'{}','','',false,$2,$2
		);
		INSERT INTO secondbox.workspace_relocations (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,operation_id,
			source_runner_id,target_runner_id,generation,logical_capacity_bytes,state,
			export_command_id,cleanup_command_id,fencing_token,checksum,
			failure_code,failure_message,retry_count,created_at,updated_at,completed_at
		) VALUES (
			'relocation-' || $1,'tenant','subject','sandbox-relocation-' || $1,
			'workspace-relocation-' || $1,'operation-relocation-' || $1,
			'runner-home','runner-relocation-target',3,8589934592,'queued',
			'command-relocation-export-' || $1,'',$3,'','','',0,$2,$2,NULL
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES (
			'command-relocation-export-' || $1,'runner-home','relocation-' || $1,
			'local-workspace','', 'delivered','connection-source',1,$2,$2,$2
		)`, pgx.QueryExecModeSimpleProtocol, suffix, now,
		[]byte("01234567890123456789012345678901"),
	); err != nil {
		t.Fatal(err)
	}
}

func recordWorkspaceRelocationExport(
	t *testing.T,
	store *PostgresStateStore,
	suffix string,
	now time.Time,
) {
	t.Helper()
	recordLocalWorkspaceTestResult(t, store, &runnerv1.LocalWorkspaceResult{
		CommandVersion:       1,
		Kind:                 runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT,
		Terminal:             runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED,
		OperationId:          "operation-relocation-" + suffix,
		EffectId:             "command-relocation-export-" + suffix,
		SandboxId:            "sandbox-relocation-" + suffix,
		WorkspaceId:          "workspace-relocation-" + suffix,
		Generation:           3,
		LogicalCapacityBytes: 8 << 30,
		ReceiptRecordedAtUnixMs: uint64(
			now.UnixMilli(),
		),
	}, now)
}

func assertWorkspaceRelocationAuthority(
	t *testing.T,
	store *PostgresStateStore,
	suffix string,
	wantHome string,
	wantRelocationState string,
	wantMutationState string,
) {
	t.Helper()
	var home, relocationState, mutationState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT workspace.home_runner_id,relocation.state,workspace.mutation_state
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.workspace_relocations AS relocation ON relocation.workspace_id=workspace.id
		WHERE workspace.id='workspace-relocation-' || $1`, suffix,
	).Scan(&home, &relocationState, &mutationState); err != nil {
		t.Fatal(err)
	}
	if home != wantHome || relocationState != wantRelocationState || mutationState != wantMutationState {
		t.Fatalf(
			"relocation authority home=%q state=%q mutation=%q, want %q/%q/%q",
			home, relocationState, mutationState,
			wantHome, wantRelocationState, wantMutationState,
		)
	}
}
