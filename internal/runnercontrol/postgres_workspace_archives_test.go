package runnercontrol

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
)

func seedArchiveReceiptAuthority(t *testing.T, storage *PostgresStateStore, suffix string, now time.Time) *runnerv1.LocalWorkspaceResult {
	t.Helper()
	seedWorkspaceRelocationAuthority(t, storage, suffix, now)
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET mutation_kind='archive',mutation_id='archive-' || $1 WHERE id='workspace-relocation-' || $1;
DELETE FROM secondbox.workspace_relocations WHERE id='relocation-' || $1;
UPDATE secondbox.runner_commands SET assignment_id='archive-' || $1 WHERE id='command-relocation-export-' || $1;
INSERT INTO secondbox.workspace_archives(id,operation_id,sandbox_id,workspace_id,source_runner_id,generation,logical_capacity_bytes,archive_key,state,command_id,fencing_token,created_at,updated_at) VALUES('archive-' || $1,'operation-relocation-' || $1,'sandbox-relocation-' || $1,'workspace-relocation-' || $1,'runner-home',3,8589934592,'workspaces/' || $1,'queued_sent','command-relocation-export-' || $1,$3,$2,$2)`, pgx.QueryExecModeSimpleProtocol, suffix, now, []byte("01234567890123456789012345678901")); err != nil {
		t.Fatal(err)
	}
	return &runnerv1.LocalWorkspaceResult{CommandVersion: 1, Kind: runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT, Terminal: runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, OperationId: "operation-relocation-" + suffix, EffectId: "command-relocation-export-" + suffix, SandboxId: "sandbox-relocation-" + suffix, WorkspaceId: "workspace-relocation-" + suffix, Generation: 3, LogicalCapacityBytes: 8 << 30, ReceiptRecordedAtUnixMs: uint64(now.UnixMilli()), ArchiveDescriptor: &runnerv1.WorkspaceArchiveDescriptor{ArchiveBytes: 4096, ArchiveChecksum: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ImageChecksum: "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}}
}

func TestWorkspaceArchiveReceiptCommitsBeforeIndependentSourceCleanup(t *testing.T) {
	storage := openRunnerControlDatabase(t)
	now := time.Now().UTC()
	result := seedArchiveReceiptAuthority(t, storage, "archive-commit", now)
	// A wrong command kind must not release the source's writer fence.
	result.Kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT
	tx, err := storage.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := recordLocalWorkspaceResult(t.Context(), tx, "runner-home", result, now); err == nil {
		t.Fatal("wrong archive receipt kind accepted")
	}
	tx.Rollback(t.Context())
	result.Kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT
	recordLocalWorkspaceTestResult(t, storage, result, now)
	// The acknowledged upload may replay after its phase has advanced.
	recordLocalWorkspaceTestResult(t, storage, result, now)
	var home, state, operation string
	if err := storage.pool.QueryRow(t.Context(), `SELECT workspace.home_runner_id,workspace.state,operation.state FROM secondbox.workspaces workspace JOIN secondbox.operations operation ON operation.sandbox_id=workspace.sandbox_id WHERE workspace.id=$1`, result.WorkspaceId).Scan(&home, &state, &operation); err != nil {
		t.Fatal(err)
	}
	if home != "" || state != "archived" || operation != "succeeded" {
		t.Fatalf("archive commit home=%q state=%q operation=%q", home, state, operation)
	}
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='ready',home_runner_id='new-runner',generation=4 WHERE id=$1;
UPDATE secondbox.workspace_archives SET state='cleanup_queued_sent' WHERE workspace_id=$1;
INSERT INTO secondbox.runner_commands(id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at) VALUES('archive-archive-commit-cleanup','runner-home','archive-archive-commit','local-workspace','','delivered','connection-source',1,$2,$2)`, pgx.QueryExecModeSimpleProtocol, result.WorkspaceId, now); err != nil {
		t.Fatal(err)
	}
	result.EffectId = "archive-archive-commit-cleanup"
	result.Kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE
	recordLocalWorkspaceTestResult(t, storage, result, now)
	if err := storage.pool.QueryRow(t.Context(), `SELECT home_runner_id,state FROM secondbox.workspaces WHERE id=$1`, result.WorkspaceId).Scan(&home, &state); err != nil {
		t.Fatal(err)
	}
	if home != "new-runner" || state != "ready" {
		t.Fatalf("source cleanup modified new home=%q state=%q", home, state)
	}
}

func TestWorkspaceArchiveRestoreRequiresChecksumsAndRespectsStop(t *testing.T) {
	storage := openRunnerControlDatabase(t)
	now := time.Now().UTC()
	result := seedArchiveReceiptAuthority(t, storage, "archive-restore", now)
	descriptor, err := json.Marshal(result.ArchiveDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='restoring',home_runner_id='',mutation_kind='archive_restore',archive_key='workspaces/restore',archive_descriptor_json=$2 WHERE id=$1;
UPDATE secondbox.workspace_archives SET target_runner_id='runner-home',source_runner_id='',state='restore_queued_sent',descriptor_json=$2 WHERE workspace_id=$1`, pgx.QueryExecModeSimpleProtocol, result.WorkspaceId, string(descriptor)); err != nil {
		t.Fatal(err)
	}
	result.Kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT
	result.ArchiveDescriptor.ArchiveBytes++
	tx, err := storage.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := recordLocalWorkspaceResult(t.Context(), tx, "runner-home", result, now); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	tx.Rollback(t.Context())
	result.ArchiveDescriptor.ArchiveBytes--
	recordLocalWorkspaceTestResult(t, storage, result, now)
	var home, state, mutation, operation, desired string
	if err := storage.pool.QueryRow(t.Context(), `SELECT workspace.home_runner_id,workspace.state,workspace.mutation_kind,operation.state,sandbox.desired_state FROM secondbox.workspaces workspace JOIN secondbox.sandboxes sandbox ON sandbox.id=workspace.sandbox_id JOIN secondbox.operations operation ON operation.sandbox_id=sandbox.id WHERE workspace.id=$1`, result.WorkspaceId).Scan(&home, &state, &mutation, &operation, &desired); err != nil {
		t.Fatal(err)
	}
	if home != "runner-home" || state != "ready" || mutation != "" || operation != "cancelled" || desired != "stopped" {
		t.Fatalf("stopped restore home=%q state=%q mutation=%q operation=%q desired=%q", home, state, mutation, operation, desired)
	}
}

func TestWorkspaceArchiveRestoreReceiptPreservesConcurrentLifecycleIntent(t *testing.T) {
	storage := openRunnerControlDatabase(t)
	for _, succeeded := range []bool{false, true} {
		for _, desired := range []string{"stopped", "deleted"} {
			name := fmt.Sprintf("restore-intent-%s-%t", desired, succeeded)
			t.Run(name, func(t *testing.T) {
				now := time.Now().UTC()
				result := seedArchiveReceiptAuthority(t, storage, name, now)
				descriptor, err := json.Marshal(result.ArchiveDescriptor)
				if err != nil {
					t.Fatal(err)
				}
				kind := "stop"
				if desired == "deleted" {
					kind = "delete"
				}
				if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state='restoring',home_runner_id='',mutation_kind='archive_restore',archive_key='workspaces/restore',archive_descriptor_json=$2 WHERE id=$1;
UPDATE secondbox.workspace_archives SET target_runner_id='runner-home',source_runner_id='',state='restore_queued_sent',descriptor_json=$2 WHERE workspace_id=$1;
UPDATE secondbox.sandboxes SET state='starting',desired_state=$3,reconcile_owner='old-claim',reconcile_claim_expires_at=$4,next_reconcile_at=$4 WHERE id=$5;
UPDATE secondbox.operations SET kind='start' WHERE id=$6;
INSERT INTO secondbox.operations(id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,request_metadata_json,error_code,error_message,retryable,created_at,updated_at) VALUES($7,'tenant','subject',$5,'',$8,'pending',$7,'{}','','',false,$4,$4)`, pgx.QueryExecModeSimpleProtocol, result.WorkspaceId, string(descriptor), desired, now, result.SandboxId, result.OperationId, "intent-"+name, kind); err != nil {
					t.Fatal(err)
				}
				result.Kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT
				if !succeeded {
					result.Terminal = runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED
					result.SafeDetail = expectedLocalWorkspaceSafeDetail(result.Terminal)
				}
				recordLocalWorkspaceTestResult(t, storage, result, now)
				var actualDesired, state, owner, workspaceState, mutation, intentOperation, startOperation string
				var scheduled bool
				if err := storage.pool.QueryRow(t.Context(), `SELECT s.desired_state,s.state,s.reconcile_owner,s.next_reconcile_at IS NOT NULL,w.state,w.mutation_kind,(SELECT state FROM secondbox.operations WHERE id=$2),(SELECT state FROM secondbox.operations WHERE id=$3) FROM secondbox.sandboxes s JOIN secondbox.workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, result.SandboxId, "intent-"+name, result.OperationId).Scan(&actualDesired, &state, &owner, &scheduled, &workspaceState, &mutation, &intentOperation, &startOperation); err != nil {
					t.Fatal(err)
				}
				if actualDesired != desired || state != "stopped" || owner != "" || mutation != "" {
					t.Fatalf("desired=%q state=%q owner=%q mutation=%q", actualDesired, state, owner, mutation)
				}
				if desired == "deleted" {
					if !scheduled || intentOperation != "pending" {
						t.Fatalf("delete lost: scheduled=%v operation=%q", scheduled, intentOperation)
					}
				} else if scheduled || intentOperation != "succeeded" {
					t.Fatalf("stop left unfinished: scheduled=%v operation=%q", scheduled, intentOperation)
				}
				if succeeded {
					if workspaceState != "ready" || startOperation != "cancelled" {
						t.Fatalf("workspace=%q start=%q", workspaceState, startOperation)
					}
				} else if workspaceState != "archived" || startOperation != "failed" {
					t.Fatalf("workspace=%q start=%q", workspaceState, startOperation)
				}
			})
		}
	}
}
