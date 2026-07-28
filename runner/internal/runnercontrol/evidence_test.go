package runnercontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type recordingEvidenceSink struct {
	mu      sync.Mutex
	records []runnerevidence.Record
}

func (s *recordingEvidenceSink) Emit(_ context.Context, record runnerevidence.Record) error {
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
	return nil
}

func (s *recordingEvidenceSink) snapshot() []runnerevidence.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runnerevidence.Record(nil), s.records...)
}

func TestTerminalEvidenceRetainsDistinctOperationCorrelationWithoutPayloads(t *testing.T) {
	sink := &recordingEvidenceSink{}
	fence := relayRunnerFence()
	service := &RunnerProtocolService{
		config:         testRunnerConfig(),
		evidence:       sink,
		correlations:   map[string]*runnerprotocol.Correlation{},
		execOperations: map[string]*runnerExecOperation{},
		fileOperations: map[string]*runnerFileOperation{},
	}
	stream := &recordingProtocolStream{}

	execState := &runnerExecOperation{
		key:          "exec-key",
		fence:        cloneRunnerFence(fence),
		correlation:  relayOperationCorrelation(fence, "exec-operation", "request-exec", "lease-exec"),
		operationID:  "exec-operation",
		streamID:     "exec-stream",
		nextOutgoing: 1,
	}
	service.execOperations[execState.key] = execState
	if err := service.sendExecTerminal(stream, execState, &runnerprotocol.ExecTerminal{
		Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
	}); err != nil {
		t.Fatal(err)
	}

	fileState := &runnerFileOperation{
		key:          "file-key",
		fence:        cloneRunnerFence(fence),
		correlation:  relayOperationCorrelation(fence, "file-operation", "request-file", "lease-file"),
		operationID:  "file-operation",
		streamID:     "file-stream",
		nextOutgoing: 1,
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
	messages := stream.outbound
	if len(messages) != 2 ||
		!proto.Equal(messages[0].GetExec().GetCorrelation(), execState.correlation) ||
		!proto.Equal(messages[1].GetFile().GetCorrelation(), fileState.correlation) {
		t.Fatalf("terminal frame correlations = %+v", messages)
	}
}

func TestFenceEvidenceUsesRetainedAssignmentCorrelation(t *testing.T) {
	sink := &recordingEvidenceSink{}
	fence := relayRunnerFence()
	service := &RunnerProtocolService{
		config:   testRunnerConfig(),
		backend:  &recordingAssignmentBackend{},
		evidence: sink,
		active: map[string]*runnerprotocol.ActiveAssignmentSummary{
			fence.AssignmentId: {
				AssignmentId: fence.AssignmentId,
			},
		},
		correlations: map[string]*runnerprotocol.Correlation{
			fence.AssignmentId: {
				RequestId: "request-1", OperationId: "assignment-operation",
				LeaseId: "lease-1", RunnerId: "runner-1",
			},
		},
	}
	if err := service.handleFence(
		context.Background(),
		&recordingProtocolStream{},
		&runnerprotocol.FenceCommand{
			Fence: cloneRunnerFence(fence),
			Correlation: relayOperationCorrelation(
				fence, "assignment-operation", "request-1", "lease-1",
			),
		},
	); err != nil {
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

func TestCheckpointAndRestoreEvidenceRetainCompleteCorrelation(t *testing.T) {
	sink := &recordingEvidenceSink{}
	backend := &evidenceCheckpointRestoreBackend{}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.SetEvidenceSink(sink)
	service.checkpointBackend = backend
	service.restoreBackend = backend
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	checkpointCorrelation := relayOperationCorrelation(
		fence, "checkpoint-operation", "request-checkpoint", "lease-checkpoint",
	)
	checkpoint := &runnerprotocol.CheckpointCommand{
		MessageId: "checkpoint-message", Sequence: 1, Fence: cloneRunnerFence(fence),
		CheckpointId: "checkpoint-1", StorageObjectId: "storage-1",
		MaximumSizeBytes: 1024, DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Correlation: checkpointCorrelation,
	}
	if err := service.handleCheckpoint(t.Context(), &recordingProtocolStream{}, checkpoint); err != nil {
		t.Fatal(err)
	}

	restoreCorrelation := relayOperationCorrelation(
		fence, "restore-operation", "request-restore", "lease-restore",
	)
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT: true,
	}
	if err := service.handleCommand(
		t.Context(),
		&recordingProtocolStream{},
		&runnerprotocol.ControlPlaneToRunner{
			Message: &runnerprotocol.ControlPlaneToRunner_RestoreBegin{
				RestoreBegin: &runnerprotocol.RestoreBegin{
					Fence: cloneRunnerFence(fence), CheckpointId: "restore-1",
					StorageObjectId: "storage-restore-1", Sha256: "digest",
					SizeBytes: 7, DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
					Correlation: restoreCorrelation,
				},
			},
		},
		enabled,
		make(chan error, 1),
	); err != nil {
		t.Fatal(err)
	}
	if err := service.handleCommand(
		t.Context(),
		&recordingProtocolStream{},
		&runnerprotocol.ControlPlaneToRunner{
			Message: &runnerprotocol.ControlPlaneToRunner_RestoreChunk{
				RestoreChunk: &runnerprotocol.RestoreChunk{
					Fence: cloneRunnerFence(fence), CheckpointId: "restore-1",
					StorageObjectId: "storage-restore-1", Offset: 7, EndOfObject: true,
				},
			},
		},
		enabled,
		make(chan error, 1),
	); err != nil {
		t.Fatal(err)
	}

	records := sink.snapshot()
	if len(records) != 2 {
		t.Fatalf("checkpoint/restore evidence = %+v", records)
	}
	expected := []struct {
		event       runnerevidence.Event
		requestID   string
		operationID string
		leaseID     string
		terminal    string
	}{
		{runnerevidence.Event("checkpoint_terminal"), "request-checkpoint", "checkpoint-operation", "lease-checkpoint", "CHECKPOINT_TERMINAL_KIND_CREATED"},
		{runnerevidence.Event("restore_terminal"), "request-restore", "restore-operation", "lease-restore", "restored"},
	}
	for index, want := range expected {
		record := records[index]
		if record.Event != want.event ||
			record.RequestID != want.requestID ||
			record.OperationID != want.operationID ||
			record.LeaseID != want.leaseID ||
			record.TerminalKind != want.terminal ||
			record.SandboxID != fence.SandboxId ||
			record.InstanceID != fence.InstanceId ||
			record.SandboxGeneration != fence.SandboxGeneration ||
			record.AssignmentID != fence.AssignmentId ||
			record.RunnerID != "runner-1" {
			t.Fatalf("record %d = %+v", index, record)
		}
	}
}

func TestCheckpointAndRestoreRejectMissingOrMismatchedCorrelation(t *testing.T) {
	backend := &evidenceCheckpointRestoreBackend{}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.checkpointBackend = backend
	service.restoreBackend = backend
	fence := relayRunnerFence()
	service.recordActiveAssignment(fence, "fc-instance-1")
	if err := service.handleCheckpoint(t.Context(), &recordingProtocolStream{}, &runnerprotocol.CheckpointCommand{
		Fence: cloneRunnerFence(fence), CheckpointId: "checkpoint-missing-correlation",
		StorageObjectId: "storage-1", MaximumSizeBytes: 1024,
		DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
	}); err == nil {
		t.Fatal("Checkpoint without correlation was accepted")
	}
	mismatched := relayOperationCorrelation(
		fence, "restore-operation", "request-restore", "lease-restore",
	)
	mismatched.AssignmentId = "different-assignment"
	err = service.handleCommand(
		t.Context(),
		&recordingProtocolStream{},
		&runnerprotocol.ControlPlaneToRunner{
			Message: &runnerprotocol.ControlPlaneToRunner_RestoreBegin{
				RestoreBegin: &runnerprotocol.RestoreBegin{
					Fence: cloneRunnerFence(fence), CheckpointId: "restore-mismatched-correlation",
					StorageObjectId: "storage-restore-1", Sha256: "digest",
					SizeBytes: 7, DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
					Correlation: mismatched,
				},
			},
		},
		map[runnerprotocol.RunnerFeature]bool{
			runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT: true,
		},
		make(chan error, 1),
	)
	if err == nil {
		t.Fatal("Restore with mismatched correlation was accepted")
	}
}

func TestRestoreFailureEmitsCorrelatedEvidenceAndFailsHard(t *testing.T) {
	sink := &recordingEvidenceSink{}
	backend := &evidenceCheckpointRestoreBackend{
		restoreChunkErr: errors.New("restore verification failed"),
	}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.SetEvidenceSink(sink)
	service.restoreBackend = backend
	fence := relayRunnerFence()
	correlation := relayOperationCorrelation(
		fence, "restore-failure", "request-restore-failure", "lease-restore-failure",
	)
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT: true,
	}
	begin := &runnerprotocol.RestoreBegin{
		Fence: cloneRunnerFence(fence), CheckpointId: "restore-failure",
		StorageObjectId: "storage-restore-failure", Sha256: "digest",
		SizeBytes: 7, DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Correlation: correlation,
	}
	if err := service.handleCommand(
		t.Context(), &recordingProtocolStream{},
		&runnerprotocol.ControlPlaneToRunner{
			Message: &runnerprotocol.ControlPlaneToRunner_RestoreBegin{RestoreBegin: begin},
		},
		enabled, make(chan error, 1),
	); err != nil {
		t.Fatal(err)
	}
	err = service.handleCommand(
		t.Context(), &recordingProtocolStream{},
		&runnerprotocol.ControlPlaneToRunner{
			Message: &runnerprotocol.ControlPlaneToRunner_RestoreChunk{
				RestoreChunk: &runnerprotocol.RestoreChunk{
					Fence: cloneRunnerFence(fence), CheckpointId: begin.CheckpointId,
					StorageObjectId: begin.StorageObjectId, Offset: 7, EndOfObject: true,
				},
			},
		},
		enabled, make(chan error, 1),
	)
	if !errors.Is(err, backend.restoreChunkErr) {
		t.Fatalf("restore failure = %v", err)
	}
	records := sink.snapshot()
	if len(records) != 1 ||
		records[0].Event != runnerevidence.Event("restore_terminal") ||
		records[0].Outcome != "failed" ||
		records[0].RequestID != correlation.RequestId ||
		records[0].OperationID != correlation.OperationId {
		t.Fatalf("restore failure evidence = %+v", records)
	}
}

type evidenceCheckpointRestoreBackend struct {
	restoreChunkErr error
}

func (*evidenceCheckpointRestoreBackend) CreateCheckpoint(
	_ context.Context,
	_ *runnerprotocol.CheckpointCommand,
	emit func([]byte) error,
) (CheckpointEvidence, error) {
	content := []byte("checkpoint")
	if err := emit(content); err != nil {
		return CheckpointEvidence{}, err
	}
	return CheckpointEvidence{
		SHA256: "digest", SizeBytes: uint64(len(content)),
		Compatibility: map[string]string{"backend": "firecracker"},
	}, nil
}

func (*evidenceCheckpointRestoreBackend) BeginRestore(
	context.Context,
	*runnerprotocol.RestoreBegin,
) error {
	return nil
}

func (backend *evidenceCheckpointRestoreBackend) WriteRestoreChunk(
	context.Context,
	*runnerprotocol.RestoreChunk,
) error {
	return backend.restoreChunkErr
}
