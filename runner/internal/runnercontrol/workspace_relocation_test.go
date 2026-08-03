package runnercontrol

import (
	"context"
	"errors"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type relocationRestartBackend struct {
	next  WorkspaceRelocationImport
	calls int
}

func (*relocationRestartBackend) OpenWorkspaceRelocationExport(
	context.Context,
	*runnerprotocol.LocalWorkspaceCommand,
) (WorkspaceRelocationExport, error) {
	return nil, errors.New("unused Workspace relocation export")
}

func (backend *relocationRestartBackend) BeginWorkspaceRelocationImport(
	context.Context,
	*runnerprotocol.WorkspaceTransferFrame,
) (WorkspaceRelocationImport, error) {
	backend.calls++
	return backend.next, nil
}

type relocationRestartImport struct {
	aborted bool
}

func (*relocationRestartImport) WriteChunk(uint64, []byte) error { return nil }

func (*relocationRestartImport) Complete(uint64, string) (LocalWorkspaceEvidence, error) {
	return LocalWorkspaceEvidence{}, nil
}

func (relocation *relocationRestartImport) Abort() error {
	relocation.aborted = true
	return nil
}

func (*relocationRestartImport) CompletedEvidence() (LocalWorkspaceEvidence, bool) {
	return LocalWorkspaceEvidence{}, false
}

func TestWorkspaceRelocationRestartAbortsPartialTargetBeforeReimport(t *testing.T) {
	stale := &relocationRestartImport{}
	next := &relocationRestartImport{}
	backend := &relocationRestartBackend{next: next}
	service := &RunnerProtocolService{
		relocationBackend:          backend,
		workspaceRelocationSources: make(map[string]*workspaceRelocationSource),
		workspaceRelocationTargets: map[string]*workspaceRelocationTarget{
			"operation-relocation": {importer: stale, inboundSequence: 2},
		},
	}
	stream := &recordingProtocolStream{}
	frame := &runnerprotocol.WorkspaceTransferFrame{
		OperationId: "operation-relocation", SandboxId: "sandbox-relocation",
		WorkspaceId: "workspace-relocation", Generation: 3, Sequence: 1,
		Payload: &runnerprotocol.WorkspaceTransferFrame_Open{
			Open: &runnerprotocol.WorkspaceTransferOpen{
				LogicalCapacityBytes: 8 << 30,
				FencingToken:         []byte("01234567890123456789012345678901"),
			},
		},
	}
	if err := service.beginWorkspaceRelocationImport(t.Context(), stream, frame); err != nil {
		t.Fatal(err)
	}
	if !stale.aborted || backend.calls != 1 {
		t.Fatalf("restart stale aborted = %t, import calls = %d", stale.aborted, backend.calls)
	}
	if target := service.workspaceRelocationTargets[frame.OperationId]; target == nil || target.importer != next {
		t.Fatalf("restart target = %#v", target)
	}
	if len(stream.outbound) != 1 || stream.outbound[0].GetWorkspaceTransfer().GetCredit().ByteCount !=
		workspaceRelocationWindowBytes {
		t.Fatalf("restart credit frames = %#v", stream.outbound)
	}
}
