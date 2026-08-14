package runnercontrol

import (
	"context"
	"sync"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type recordingEvidenceSink struct {
	mu      sync.Mutex
	records []runnerevidence.Record
}

func (sink *recordingEvidenceSink) Emit(_ context.Context, record runnerevidence.Record) error {
	sink.mu.Lock()
	sink.records = append(sink.records, record)
	sink.mu.Unlock()
	return nil
}

func (sink *recordingEvidenceSink) snapshot() []runnerevidence.Record {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]runnerevidence.Record(nil), sink.records...)
}

func TestTerminalEvidenceRetainsDistinctOperationCorrelationWithoutPayloads(t *testing.T) {
	sink := &recordingEvidenceSink{}
	fence := relayRunnerFence()
	service := &RunnerProtocolService{
		config: testRunnerConfig(), evidence: sink,
		correlations:   map[string]*runnerprotocol.Correlation{},
		execOperations: map[string]*runnerExecOperation{},
		fileOperations: map[string]*runnerFileOperation{},
		dataPlane:      newDataPlaneListener(),
		directPorts:    newDirectPortRegistry(),
	}
	stream := &recordingProtocolStream{}
	execState := &runnerExecOperation{
		key: "exec-key", fence: cloneRunnerFence(fence),
		correlation: relayOperationCorrelation(fence, "exec-operation", "request-exec", "lease-exec"),
		operationID: "exec-operation", streamID: "exec-stream", nextOutgoing: 1,
		done: make(chan struct{}),
	}
	service.execOperations[execState.key] = execState
	if err := service.sendExecTerminal(stream, execState, &runnerprotocol.ExecTerminal{
		Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
	}); err != nil {
		t.Fatal(err)
	}
	fileState := &runnerFileOperation{
		key: "file-key", fence: cloneRunnerFence(fence),
		correlation: relayOperationCorrelation(fence, "file-operation", "request-file", "lease-file"),
		operationID: "file-operation", streamID: "file-stream", nextOutgoing: 1,
	}
	service.fileOperations[fileState.key] = fileState
	if err := service.sendFileTerminal(stream, fileState, &runnerprotocol.FileTerminal{
		Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
	}); err != nil {
		t.Fatal(err)
	}
	records := sink.snapshot()
	if len(records) != 2 {
		t.Fatalf("terminal evidence records = %d, want 2", len(records))
	}
	for index, record := range records {
		if record.SandboxID != fence.SandboxId ||
			record.InstanceID != fence.InstanceId ||
			record.SandboxGeneration != fence.SandboxGeneration ||
			record.AssignmentID != fence.AssignmentId ||
			record.RunnerID != "runner-1" {
			t.Fatalf("record %d correlation = %+v", index, record)
		}
	}
	if records[0].Event != runnerevidence.EventExecTerminal ||
		records[0].RequestID != "request-exec" ||
		records[0].OperationID != "exec-operation" ||
		records[0].LeaseID != "lease-exec" ||
		records[1].Event != runnerevidence.EventFileTerminal ||
		records[1].RequestID != "request-file" ||
		records[1].OperationID != "file-operation" ||
		records[1].LeaseID != "lease-file" {
		t.Fatalf("terminal evidence = %+v", records)
	}
	if len(stream.outbound) != 2 ||
		!proto.Equal(stream.outbound[0].GetExec().GetCorrelation(), execState.correlation) ||
		!proto.Equal(stream.outbound[1].GetFile().GetCorrelation(), fileState.correlation) {
		t.Fatalf("terminal frame correlations = %+v", stream.outbound)
	}
}

func TestFenceEvidenceUsesRetainedAssignmentCorrelation(t *testing.T) {
	sink := &recordingEvidenceSink{}
	fence := relayRunnerFence()
	service := &RunnerProtocolService{
		config: testRunnerConfig(), backend: &recordingAssignmentBackend{}, evidence: sink,
		active: map[string]*runnerprotocol.ActiveAssignmentSummary{
			fence.AssignmentId: {AssignmentId: fence.AssignmentId},
		},
		correlations: map[string]*runnerprotocol.Correlation{
			fence.AssignmentId: {
				RequestId: "request-1", OperationId: "assignment-operation",
				LeaseId: "lease-1", RunnerId: "runner-1",
			},
		},
		dataPlane:   newDataPlaneListener(),
		directPorts: newDirectPortRegistry(),
	}
	if err := service.handleFence(context.Background(), &recordingProtocolStream{}, &runnerprotocol.FenceCommand{
		Fence: cloneRunnerFence(fence),
		Correlation: relayOperationCorrelation(
			fence, "assignment-operation", "request-1", "lease-1",
		),
	}); err != nil {
		t.Fatal(err)
	}
	records := sink.snapshot()
	if len(records) != 1 ||
		records[0].Event != runnerevidence.EventFenceTerminal ||
		records[0].RequestID != "request-1" ||
		records[0].OperationID != "assignment-operation" ||
		records[0].LeaseID != "lease-1" ||
		records[0].AssignmentID != fence.AssignmentId {
		t.Fatalf("fence evidence = %+v", records)
	}
}
