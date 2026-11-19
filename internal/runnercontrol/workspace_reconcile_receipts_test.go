package runnercontrol

import (
	"fmt"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func workspaceReceiptRecoveryPayload(t *testing.T, kind runnerv1.LocalWorkspaceCommandKind, operation, effect, workspace, snapshot string) []byte {
	t.Helper()
	generation := uint64(3)
	if kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE || kind == runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT {
		generation = 1
	}
	sandbox := strings.Replace(workspace, "workspace-", "sandbox-", 1)
	encoded, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{LocalWorkspace: &runnerv1.LocalWorkspaceCommand{CommandVersion: 1, Kind: kind, OperationId: operation, EffectId: effect, SandboxId: sandbox, WorkspaceId: workspace, SnapshotId: snapshot, ExpectedGeneration: generation, NextGeneration: generation, LogicalCapacityBytes: 8 << 30, FencingToken: []byte("01234567890123456789012345678901"), Correlation: &runnerv1.Correlation{OperationId: operation, SandboxId: sandbox, RunnerId: "runner-home"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func requestedReconnectReceipts(t *testing.T, store *PostgresStateStore, connection string) []*runnerv1.LocalWorkspaceReceiptRequest {
	t.Helper()
	var payload []byte
	if err := store.pool.QueryRow(t.Context(), `SELECT payload FROM secondbox.runner_commands WHERE id=$1`, "workspace-reconcile-"+connection).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var envelope runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.GetLocalWorkspace().ReceiptRequests
}

func TestReconnectRequestsOnlyCurrentOrdinaryReceiptsDespiteHistoricalCommands(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Now().UTC()
	seedPendingWorkspaceCreation(t, store, now)
	for i := range 24 {
		id := fmt.Sprintf("historical-receipt-command-%03d", i)
		payload := workspaceReceiptRecoveryPayload(t, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE, id, id, "workspace-create-reconcile", "")
		if _, err := store.pool.Exec(t.Context(), `INSERT INTO secondbox.runner_commands(id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at) VALUES($1,'runner-home',$1,'local-workspace',$2,'pending','',0,$3,$3)`, id, payload, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.OpenConnection(t.Context(), RunnerIdentity{RunnerID: "runner-home", CredentialSerial: "current"}, "bounded-current", 8, now); err != nil {
		t.Fatal(err)
	}
	requests := requestedReconnectReceipts(t, store, "bounded-current")
	if len(requests) != 1 || requests[0].WorkspaceId != "workspace-create-reconcile" || requests[0].OperationId != "operation-create-reconcile" || requests[0].Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE {
		t.Fatalf("history entered current receipt evidence: %#v", requests)
	}
	// Releasing the mutation slot makes all prior receipts irrelevant, even if
	// an obsolete command row still awaits exact replay/expiry independently.
	if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET mutation_state='',mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='' WHERE id='workspace-create-reconcile'`); err != nil {
		t.Fatal(err)
	}
	if err := store.OpenConnection(t.Context(), RunnerIdentity{RunnerID: "runner-home", CredentialSerial: "current"}, "bounded-empty", 8, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := requestedReconnectReceipts(t, store, "bounded-empty"); len(got) != 0 {
		t.Fatalf("released mutation still requested history: %#v", got)
	}
}

func TestReconnectRequestsEveryOrdinaryMutationKindAndRetainsRestoreFinalization(t *testing.T) {
	kinds := []struct {
		mutation string
		kind     runnerv1.LocalWorkspaceCommandKind
	}{
		{"create", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE},
		{"clone", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT},
		{"workspace_delete", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE},
		{"stop", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION},
		{"snapshot_create", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE},
		{"snapshot_delete", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE},
		{"relocate", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT},
		{"relocate", runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE},
	}
	for _, test := range kinds {
		t.Run(test.kind.String(), func(t *testing.T) {
			store := openRunnerControlDatabase(t)
			now := time.Now().UTC()
			seedPendingWorkspaceCreation(t, store, now)
			payload := workspaceReceiptRecoveryPayload(t, test.kind, "operation-create-reconcile", "effect-create-reconcile", "workspace-create-reconcile", "")
			if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET mutation_kind=$1;UPDATE secondbox.runner_commands SET payload=$2 WHERE id='command-create-reconcile'`, pgx.QueryExecModeSimpleProtocol, test.mutation, payload); err != nil {
				t.Fatal(err)
			}
			if err := store.OpenConnection(t.Context(), RunnerIdentity{RunnerID: "runner-home", CredentialSerial: "current"}, "ordinary", 8, now); err != nil {
				t.Fatal(err)
			}
			requests := requestedReconnectReceipts(t, store, "ordinary")
			if len(requests) != 1 || requests[0].Kind != test.kind {
				t.Fatalf("ordinary mutation lost recovery evidence: %#v", requests)
			}
		})
	}
	t.Run("restore-finalization-after-slot-release", func(t *testing.T) {
		store := openRunnerControlDatabase(t)
		now := time.Now().UTC()
		seedRunnerLocalRestore(t, store, now)
		if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET mutation_kind='',mutation_state='',mutation_id='',mutation_effect_id='',mutation_operation_id='';UPDATE secondbox.workspace_restores SET state='database_committed';UPDATE secondbox.runner_commands SET id='command-finalize',assignment_id='effect-finalize',payload=$1 WHERE id='command-prepare'`, pgx.QueryExecModeSimpleProtocol, workspaceReceiptRecoveryPayload(t, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE, "operation-restore", "effect-finalize", "workspace-restore", "snapshot-restore")); err != nil {
			t.Fatal(err)
		}
		if err := store.OpenConnection(t.Context(), RunnerIdentity{RunnerID: "runner-home", CredentialSerial: "current"}, "restore-finalization", 8, now); err != nil {
			t.Fatal(err)
		}
		requests := requestedReconnectReceipts(t, store, "restore-finalization")
		if len(requests) != 1 || requests[0].Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE {
			t.Fatalf("released restore slot lost pending finalization: %#v", requests)
		}
	})
}
