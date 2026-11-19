package runnercontrol

import (
	"context"
	"fmt"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// Reconnect evidence is bounded by current home mutations, never the number of
// historical tiering cycles or deleted Workspaces. Archive phases recover by
// exact command replay and cannot be recovered from ordinary receipt inventory.
func workspaceReconcileReceiptRequests(ctx context.Context, tx pgx.Tx, runnerID string) ([]*runnerv1.LocalWorkspaceReceiptRequest, error) {
	rows, err := tx.Query(ctx, `SELECT command.payload,workspace.id,workspace.mutation_kind,restore.id IS NOT NULL
 FROM secondbox.workspaces workspace JOIN secondbox.runner_commands command
 ON command.runner_id=workspace.home_runner_id AND command.kind='local-workspace' AND command.state IN ('pending','delivering','delivered')
 LEFT JOIN secondbox.workspace_restores restore ON restore.workspace_id=workspace.id AND restore.home_runner_id=$1
 AND restore.state NOT IN ('finalized','failed')
 AND command.id IN (restore.prepare_command_id,restore.swap_command_id,restore.finalize_command_id,restore.abort_command_id)
 WHERE workspace.home_runner_id=$1
 AND ((workspace.mutation_state<>'' AND workspace.mutation_kind NOT IN ('archive','archive_restore','start')
 AND command.assignment_id IN (workspace.mutation_id,workspace.mutation_effect_id,workspace.mutation_operation_id)) OR restore.id IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM secondbox.workspace_archives archive WHERE archive.id=command.assignment_id)
 ORDER BY workspace.id,command.created_at,command.id`, runnerID)
	if err != nil {
		return nil, fmt.Errorf("SecondBox reconnect receipt requests lookup: %w", err)
	}
	defer rows.Close()
	var requested []*runnerv1.LocalWorkspaceReceiptRequest
	type identity struct {
		workspace, operation string
		kind                 runnerv1.LocalWorkspaceCommandKind
	}
	seen := make(map[identity]struct{})
	for rows.Next() {
		var payload []byte
		var workspaceID, mutation string
		var restoring bool
		if err := rows.Scan(&payload, &workspaceID, &mutation, &restoring); err != nil {
			return nil, fmt.Errorf("SecondBox reconnect receipt requests scan: %w", err)
		}
		var envelope runnerv1.ControlPlaneToRunner
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			return nil, fmt.Errorf("SecondBox reconnect receipt command decode: %w", err)
		}
		command := envelope.GetLocalWorkspace()
		if command == nil || command.WorkspaceId != workspaceID || command.OperationId == "" || command.Correlation.GetRunnerId() != runnerID {
			return nil, fmt.Errorf("SecondBox reconnect receipt command authority is invalid for Workspace %s", workspaceID)
		}
		eligible := workspaceMutationMatchesReceipt(mutation, command.Kind)
		switch command.Kind {
		case runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
			eligible = restoring
		}
		if !eligible {
			continue
		}
		key := identity{workspaceID, command.OperationId, command.Kind}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		requested = append(requested, &runnerv1.LocalWorkspaceReceiptRequest{WorkspaceId: workspaceID, OperationId: command.OperationId, Kind: command.Kind})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox reconnect receipt requests iteration: %w", err)
	}
	return requested, nil
}
