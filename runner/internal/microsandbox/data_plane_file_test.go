package microsandbox

import (
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// TestFinalizeFileResultAttachesMetadataOnlyToCompletedReads proves a failed
// read keeps the helper's own report instead of synthesizing an existing file
// with a size and checksum the guest never delivered.
func TestFinalizeFileResultAttachesMetadataOnlyToCompletedReads(t *testing.T) {
	read := &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, ExpectedSize: 16}

	completed := finalizeFileResult(read, runnercontrol.FileOperationResult{
		Content:  []byte("delivered"),
		Terminal: &runnerprotocol.FileTerminal{Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED},
	})
	if completed.Metadata == nil || !completed.Metadata.Exists ||
		completed.Metadata.Size != 9 || completed.Metadata.Checksum == "" {
		t.Fatalf("completed read metadata = %#v", completed.Metadata)
	}

	failed := finalizeFileResult(read, runnercontrol.FileOperationResult{
		Terminal: &runnerprotocol.FileTerminal{Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED, SafeDetail: "no such file"},
	})
	if failed.Metadata != nil {
		t.Fatalf("failed read synthesized metadata: %#v", failed.Metadata)
	}
	if failed.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED {
		t.Fatalf("failed read terminal = %v", failed.Terminal.GetKind())
	}

	oversized := finalizeFileResult(read, runnercontrol.FileOperationResult{
		Content:  make([]byte, 32),
		Terminal: &runnerprotocol.FileTerminal{Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED},
	})
	if oversized.Content != nil ||
		oversized.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED {
		t.Fatalf("oversized read = %#v", oversized)
	}

	write := &runnerprotocol.FileOpen{Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE}
	untouched := finalizeFileResult(write, runnercontrol.FileOperationResult{
		Terminal: &runnerprotocol.FileTerminal{Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED},
	})
	if untouched.Metadata != nil {
		t.Fatalf("write result gained read metadata: %#v", untouched.Metadata)
	}
}
