package runnercontrol

import (
	"fmt"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

// WorkspaceReceiptRequests translates exact control-plane lookups into local
// receipt identities. The WorkspaceStore keeps every other tombstone on disk.
func WorkspaceReceiptRequests(requests []*runnerprotocol.LocalWorkspaceReceiptRequest) ([]workspacestore.ReceiptIdentity, error) {
	kinds := map[runnerprotocol.LocalWorkspaceCommandKind]string{
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE:                  workspacestore.ReceiptWorkspaceCreate,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT:     workspacestore.ReceiptWorkspaceClone,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE:                  workspacestore.ReceiptWorkspaceDelete,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION:      workspacestore.ReceiptGenerationAdvance,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE:         workspacestore.ReceiptSnapshotCreate,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE:         workspacestore.ReceiptSnapshotDelete,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE:         workspacestore.ReceiptRestorePrepare,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP:            workspacestore.ReceiptRestoreSwap,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE:        workspacestore.ReceiptRestoreFinalize,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:           workspacestore.ReceiptRestoreAbort,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_EXPORT:       workspacestore.ReceiptRelocationExport,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE: workspacestore.ReceiptRelocationAbort,
	}
	identities := make([]workspacestore.ReceiptIdentity, 0, len(requests))
	for _, request := range requests {
		if request == nil {
			return nil, fmt.Errorf("SecondBox reconnect receipt request is absent")
		}
		kind, ok := kinds[request.Kind]
		if !ok {
			return nil, fmt.Errorf("SecondBox reconnect receipt request kind %s is unsupported", request.Kind)
		}
		identities = append(identities, workspacestore.ReceiptIdentity{WorkspaceID: request.WorkspaceId, OperationID: request.OperationId, Kind: kind})
	}
	return identities, nil
}
