package firecracker

import (
	"errors"
	"syscall"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func TestLocalWorkspaceFailureMapsStableTerminalKinds(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want runnerprotocol.LocalWorkspaceTerminalKind
	}{
		{
			name: "absent local data", err: workspacestore.ErrWorkspaceNotFound,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_LOCAL_DATA_ABSENT,
		},
		{
			name: "active writer", err: workspacestore.ErrActiveWriter,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_ACTIVE_WRITER,
		},
		{
			name: "stale generation", err: workspacestore.ErrStaleGeneration,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_GENERATION,
		},
		{
			name: "stale fence", err: workspacestore.ErrStaleFence,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_FENCE,
		},
		{
			name: "unsupported reflink store", err: workspacestore.ErrStorageIncompatible,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STORAGE_INCOMPATIBLE,
		},
		{
			name: "insufficient space", err: syscall.ENOSPC,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_INSUFFICIENT_SPACE,
		},
		{
			name: "corrupt receipt", err: workspacestore.ErrCorruptState,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CORRUPT_RECEIPT,
		},
		{
			name: "conflicting replay", err: workspacestore.ErrConflictingReplay,
			want: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CONFLICTING_REPLAY,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var typed interface {
				LocalWorkspaceTerminal() runnerprotocol.LocalWorkspaceTerminalKind
			}
			if err := localWorkspaceFailure(testCase.err); !errors.As(err, &typed) {
				t.Fatalf("failure %v is not typed", err)
			} else if got := typed.LocalWorkspaceTerminal(); got != testCase.want {
				t.Fatalf("terminal = %v, want %v", got, testCase.want)
			}
		})
	}
}
