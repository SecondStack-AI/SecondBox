package runnercontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const (
	workspaceRelocationChunkBytes  = 64 << 10
	workspaceRelocationWindowBytes = 1 << 20
)

var errWorkspaceRelocationTargetCompleted = errors.New("SecondBox runner Workspace relocation target completed")

type workspaceRelocationSource struct {
	mu      sync.Mutex
	credit  uint64
	notify  chan struct{}
	failure chan error
}

func newWorkspaceRelocationSource() *workspaceRelocationSource {
	return &workspaceRelocationSource{
		notify: make(chan struct{}, 1), failure: make(chan error, 1),
	}
}

func (source *workspaceRelocationSource) addCredit(credit uint64) {
	source.mu.Lock()
	source.credit += credit
	source.mu.Unlock()
	select {
	case source.notify <- struct{}{}:
	default:
	}
}

func (source *workspaceRelocationSource) takeCredit(
	ctx context.Context,
	maximum uint64,
) (uint64, error) {
	for {
		source.mu.Lock()
		if source.credit > 0 {
			granted := min(source.credit, maximum)
			source.credit -= granted
			source.mu.Unlock()
			return granted, nil
		}
		source.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case err := <-source.failure:
			return 0, err
		case <-source.notify:
		}
	}
}

type workspaceRelocationTarget struct {
	importer         WorkspaceRelocationImport
	inboundSequence  uint64
	outboundSequence uint64
}

func (s *RunnerProtocolService) handleWorkspaceRelocationExport(
	ctx context.Context,
	stream RunnerProtocolStream,
	command *runnerprotocol.LocalWorkspaceCommand,
) error {
	if s.relocationBackend == nil || command.Correlation == nil ||
		command.Correlation.RunnerId != s.config.RunnerID ||
		command.ExpectedGeneration == 0 || command.LogicalCapacityBytes == 0 ||
		len(command.FencingToken) == 0 {
		return errors.New("SecondBox runner Workspace relocation export command is incomplete")
	}
	export, executionErr := s.relocationBackend.OpenWorkspaceRelocationExport(ctx, command)
	evidence := LocalWorkspaceEvidence{}
	if executionErr == nil {
		evidence = export.Evidence()
	}
	terminal := localWorkspaceTerminal(executionErr)
	result := &runnerprotocol.LocalWorkspaceResult{
		CommandVersion:       command.CommandVersion,
		Kind:                 command.Kind,
		Terminal:             terminal,
		OperationId:          command.OperationId,
		EffectId:             command.EffectId,
		SandboxId:            command.SandboxId,
		WorkspaceId:          command.WorkspaceId,
		Generation:           evidence.Generation,
		LogicalCapacityBytes: evidence.LogicalCapacity,
		Correlation:          cloneRunnerCorrelation(command.Correlation),
	}
	if !evidence.ReceiptRecordedAt.IsZero() {
		result.ReceiptRecordedAtUnixMs = uint64(evidence.ReceiptRecordedAt.UTC().UnixMilli())
	}
	if executionErr != nil {
		result.SafeDetail = localWorkspaceSafeDetail(terminal)
	}
	if err := s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			result.MessageId = s.messageID(sequence)
			result.Sequence = sequence
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_LocalWorkspaceResult{
					LocalWorkspaceResult: result,
				},
			}
		},
	); err != nil {
		if export != nil {
			return errors.Join(err, export.Close())
		}
		return err
	}
	if executionErr != nil {
		return nil
	}
	defer export.Close()
	if export.SizeBytes() != int64(command.LogicalCapacityBytes) {
		return s.sendWorkspaceRelocationFailure(
			stream,
			command,
			1,
			"source Workspace size changed after sealing",
		)
	}
	source := newWorkspaceRelocationSource()
	s.workspaceRelocationMu.Lock()
	if _, exists := s.workspaceRelocationSources[command.OperationId]; exists {
		s.workspaceRelocationMu.Unlock()
		return errors.New("SecondBox runner Workspace relocation source operation is duplicated")
	}
	s.workspaceRelocationSources[command.OperationId] = source
	s.workspaceRelocationMu.Unlock()
	defer func() {
		s.workspaceRelocationMu.Lock()
		delete(s.workspaceRelocationSources, command.OperationId)
		s.workspaceRelocationMu.Unlock()
	}()
	sequence := uint64(1)
	if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_WorkspaceTransfer{
			WorkspaceTransfer: &runnerprotocol.WorkspaceTransferFrame{
				OperationId: command.OperationId,
				SandboxId:   command.SandboxId,
				WorkspaceId: command.WorkspaceId,
				Generation:  command.ExpectedGeneration,
				Sequence:    sequence,
				Payload: &runnerprotocol.WorkspaceTransferFrame_Open{
					Open: &runnerprotocol.WorkspaceTransferOpen{
						LogicalCapacityBytes: command.LogicalCapacityBytes,
						FencingToken:         append([]byte(nil), command.FencingToken...),
					},
				},
			},
		},
	}); err != nil {
		return err
	}
	hash := sha256.New()
	offset := uint64(0)
	buffer := make([]byte, workspaceRelocationChunkBytes)
	for offset < uint64(export.SizeBytes()) {
		credit, err := source.takeCredit(ctx, uint64(len(buffer)))
		if err != nil {
			return nil
		}
		remaining := uint64(export.SizeBytes()) - offset
		limit := min(credit, remaining)
		read, readErr := io.ReadFull(export, buffer[:limit])
		if read > 0 {
			chunk := append([]byte(nil), buffer[:read]...)
			if _, err := hash.Write(chunk); err != nil {
				return err
			}
			sequence++
			if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_WorkspaceTransfer{
					WorkspaceTransfer: &runnerprotocol.WorkspaceTransferFrame{
						OperationId: command.OperationId,
						SandboxId:   command.SandboxId,
						WorkspaceId: command.WorkspaceId,
						Generation:  command.ExpectedGeneration,
						Sequence:    sequence,
						Payload: &runnerprotocol.WorkspaceTransferFrame_Chunk{
							Chunk: &runnerprotocol.WorkspaceTransferChunk{Offset: offset, Data: chunk},
						},
					},
				},
			}); err != nil {
				return err
			}
			offset += uint64(read)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return s.sendWorkspaceRelocationFailure(
				stream, command, sequence+1, "source Workspace read failed",
			)
		}
		if read == 0 {
			return s.sendWorkspaceRelocationFailure(
				stream, command, sequence+1, "source Workspace ended before its logical size",
			)
		}
	}
	sequence++
	checksum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_WorkspaceTransfer{
			WorkspaceTransfer: &runnerprotocol.WorkspaceTransferFrame{
				OperationId: command.OperationId,
				SandboxId:   command.SandboxId,
				WorkspaceId: command.WorkspaceId,
				Generation:  command.ExpectedGeneration,
				Sequence:    sequence,
				Payload: &runnerprotocol.WorkspaceTransferFrame_Commit{
					Commit: &runnerprotocol.WorkspaceTransferCommit{
						SizeBytes: offset, Sha256: checksum,
					},
				},
			},
		},
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case <-source.failure:
		return nil
	}
}

func (s *RunnerProtocolService) sendWorkspaceRelocationFailure(
	stream RunnerProtocolStream,
	command *runnerprotocol.LocalWorkspaceCommand,
	sequence uint64,
	detail string,
) error {
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_WorkspaceTransfer{
			WorkspaceTransfer: &runnerprotocol.WorkspaceTransferFrame{
				OperationId: command.OperationId,
				SandboxId:   command.SandboxId,
				WorkspaceId: command.WorkspaceId,
				Generation:  command.ExpectedGeneration,
				Sequence:    sequence,
				Payload: &runnerprotocol.WorkspaceTransferFrame_Result{
					Result: &runnerprotocol.WorkspaceTransferResult{
						Terminal:   runnerprotocol.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_RUNNER_FAILED,
						SafeDetail: detail,
					},
				},
			},
		},
	})
}

func (s *RunnerProtocolService) handleWorkspaceTransferFrame(
	ctx context.Context,
	stream RunnerProtocolStream,
	frame *runnerprotocol.WorkspaceTransferFrame,
) error {
	if err := validateWorkspaceTransferFrame(frame); err != nil {
		return err
	}
	if frame.GetOpen() != nil {
		return s.beginWorkspaceRelocationImport(ctx, stream, frame)
	}
	s.workspaceRelocationMu.Lock()
	source := s.workspaceRelocationSources[frame.OperationId]
	target := s.workspaceRelocationTargets[frame.OperationId]
	s.workspaceRelocationMu.Unlock()
	if source != nil {
		switch {
		case frame.GetCredit() != nil:
			source.addCredit(frame.GetCredit().ByteCount)
			return nil
		case frame.GetResult() != nil:
			terminal := errors.New("SecondBox runner Workspace relocation target failed")
			if frame.GetResult().Terminal == runnerprotocol.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_SUCCEEDED {
				terminal = errWorkspaceRelocationTargetCompleted
			}
			select {
			case source.failure <- terminal:
			default:
			}
			return nil
		case frame.GetCancel() != nil:
			select {
			case source.failure <- errors.New("SecondBox runner Workspace relocation was cancelled"):
			default:
			}
			return nil
		}
	}
	if target == nil || frame.Sequence != target.inboundSequence+1 {
		return errors.New("SecondBox runner Workspace relocation frame is reordered or unknown")
	}
	target.inboundSequence = frame.Sequence
	switch {
	case frame.GetChunk() != nil:
		chunk := frame.GetChunk()
		if len(chunk.Data) == 0 || len(chunk.Data) > workspaceRelocationChunkBytes {
			return errors.New("SecondBox runner Workspace relocation chunk exceeds its bound")
		}
		if err := target.importer.WriteChunk(chunk.Offset, chunk.Data); err != nil {
			return s.failWorkspaceRelocationTarget(stream, frame, target, "target Workspace write failed")
		}
		return s.sendWorkspaceRelocationCredit(stream, frame, target, uint64(len(chunk.Data)))
	case frame.GetCommit() != nil:
		commit := frame.GetCommit()
		if !immutableManifestDigest.MatchString(commit.Sha256) {
			return errors.New("SecondBox runner Workspace relocation commit checksum is invalid")
		}
		evidence, err := target.importer.Complete(commit.SizeBytes, commit.Sha256)
		if err != nil {
			if _, completed := target.importer.CompletedEvidence(); completed {
				return fmt.Errorf("SecondBox runner Workspace relocation completed receipt cleanup: %w", err)
			}
			return s.failWorkspaceRelocationTarget(stream, frame, target, "target Workspace import failed")
		}
		s.removeWorkspaceRelocationTarget(frame.OperationId)
		return s.sendWorkspaceRelocationTargetResult(stream, frame, target, evidence)
	case frame.GetCancel() != nil, frame.GetResult() != nil:
		err := target.importer.Abort()
		s.removeWorkspaceRelocationTarget(frame.OperationId)
		return err
	default:
		return errors.New("SecondBox runner Workspace relocation target payload is invalid")
	}
}

func (s *RunnerProtocolService) beginWorkspaceRelocationImport(
	ctx context.Context,
	stream RunnerProtocolStream,
	frame *runnerprotocol.WorkspaceTransferFrame,
) error {
	if frame.Sequence != 1 || frame.GetOpen().LogicalCapacityBytes == 0 ||
		len(frame.GetOpen().FencingToken) == 0 || s.relocationBackend == nil {
		return errors.New("SecondBox runner Workspace relocation open is incomplete")
	}
	s.workspaceRelocationMu.Lock()
	stale := s.workspaceRelocationTargets[frame.OperationId]
	delete(s.workspaceRelocationTargets, frame.OperationId)
	s.workspaceRelocationMu.Unlock()
	if stale != nil {
		if err := stale.importer.Abort(); err != nil {
			return err
		}
	}
	importer, err := s.relocationBackend.BeginWorkspaceRelocationImport(ctx, frame)
	if err != nil {
		target := &workspaceRelocationTarget{inboundSequence: frame.Sequence}
		return s.failWorkspaceRelocationTarget(stream, frame, target, "target Workspace import could not start")
	}
	if evidence, completed := importer.CompletedEvidence(); completed {
		target := &workspaceRelocationTarget{importer: importer, inboundSequence: frame.Sequence}
		return s.sendWorkspaceRelocationTargetResult(stream, frame, target, evidence)
	}
	target := &workspaceRelocationTarget{importer: importer, inboundSequence: frame.Sequence}
	s.workspaceRelocationMu.Lock()
	if _, exists := s.workspaceRelocationTargets[frame.OperationId]; exists {
		s.workspaceRelocationMu.Unlock()
		return errors.Join(
			errors.New("SecondBox runner Workspace relocation target operation is duplicated"),
			importer.Abort(),
		)
	}
	s.workspaceRelocationTargets[frame.OperationId] = target
	s.workspaceRelocationMu.Unlock()
	return s.sendWorkspaceRelocationCredit(
		stream,
		frame,
		target,
		workspaceRelocationWindowBytes,
	)
}

func (s *RunnerProtocolService) sendWorkspaceRelocationCredit(
	stream RunnerProtocolStream,
	frame *runnerprotocol.WorkspaceTransferFrame,
	target *workspaceRelocationTarget,
	credit uint64,
) error {
	target.outboundSequence++
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_WorkspaceTransfer{
			WorkspaceTransfer: &runnerprotocol.WorkspaceTransferFrame{
				OperationId: frame.OperationId,
				SandboxId:   frame.SandboxId,
				WorkspaceId: frame.WorkspaceId,
				Generation:  frame.Generation,
				Sequence:    target.outboundSequence,
				Payload: &runnerprotocol.WorkspaceTransferFrame_Credit{
					Credit: &runnerprotocol.StreamCredit{ByteCount: credit},
				},
			},
		},
	})
}

func (s *RunnerProtocolService) sendWorkspaceRelocationTargetResult(
	stream RunnerProtocolStream,
	frame *runnerprotocol.WorkspaceTransferFrame,
	target *workspaceRelocationTarget,
	evidence LocalWorkspaceEvidence,
) error {
	target.outboundSequence++
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_WorkspaceTransfer{
			WorkspaceTransfer: &runnerprotocol.WorkspaceTransferFrame{
				OperationId: frame.OperationId,
				SandboxId:   frame.SandboxId,
				WorkspaceId: frame.WorkspaceId,
				Generation:  frame.Generation,
				Sequence:    target.outboundSequence,
				Payload: &runnerprotocol.WorkspaceTransferFrame_Result{
					Result: &runnerprotocol.WorkspaceTransferResult{
						Terminal:  runnerprotocol.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_SUCCEEDED,
						SizeBytes: evidence.LogicalCapacity,
						Sha256:    evidence.Checksum,
					},
				},
			},
		},
	})
}

func (s *RunnerProtocolService) failWorkspaceRelocationTarget(
	stream RunnerProtocolStream,
	frame *runnerprotocol.WorkspaceTransferFrame,
	target *workspaceRelocationTarget,
	detail string,
) error {
	if target.importer != nil {
		if err := target.importer.Abort(); err != nil {
			return err
		}
	}
	s.removeWorkspaceRelocationTarget(frame.OperationId)
	target.outboundSequence++
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_WorkspaceTransfer{
			WorkspaceTransfer: &runnerprotocol.WorkspaceTransferFrame{
				OperationId: frame.OperationId,
				SandboxId:   frame.SandboxId,
				WorkspaceId: frame.WorkspaceId,
				Generation:  frame.Generation,
				Sequence:    target.outboundSequence,
				Payload: &runnerprotocol.WorkspaceTransferFrame_Result{
					Result: &runnerprotocol.WorkspaceTransferResult{
						Terminal:   runnerprotocol.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_RUNNER_FAILED,
						SafeDetail: detail,
					},
				},
			},
		},
	})
}

func (s *RunnerProtocolService) removeWorkspaceRelocationTarget(operationID string) {
	s.workspaceRelocationMu.Lock()
	delete(s.workspaceRelocationTargets, operationID)
	s.workspaceRelocationMu.Unlock()
}

func validateWorkspaceTransferFrame(frame *runnerprotocol.WorkspaceTransferFrame) error {
	if frame == nil || strings.TrimSpace(frame.OperationId) == "" ||
		strings.TrimSpace(frame.SandboxId) == "" || strings.TrimSpace(frame.WorkspaceId) == "" ||
		frame.Generation == 0 || frame.Sequence == 0 || frame.Payload == nil {
		return errors.New("SecondBox runner Workspace relocation frame is incomplete")
	}
	switch {
	case frame.GetOpen() != nil,
		frame.GetChunk() != nil,
		frame.GetCredit() != nil && frame.GetCredit().ByteCount > 0,
		frame.GetCommit() != nil,
		frame.GetResult() != nil &&
			frame.GetResult().Terminal != runnerprotocol.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_UNSPECIFIED,
		frame.GetCancel() != nil:
		return nil
	default:
		return fmt.Errorf("SecondBox runner Workspace relocation payload is invalid")
	}
}

func (s *RunnerProtocolService) abortWorkspaceRelocations() error {
	s.workspaceRelocationMu.Lock()
	sources := s.workspaceRelocationSources
	targets := s.workspaceRelocationTargets
	s.workspaceRelocationSources = make(map[string]*workspaceRelocationSource)
	s.workspaceRelocationTargets = make(map[string]*workspaceRelocationTarget)
	s.workspaceRelocationMu.Unlock()
	for _, source := range sources {
		select {
		case source.failure <- errors.New("SecondBox runner Workspace relocation connection ended"):
		default:
		}
	}
	var result error
	for _, target := range targets {
		result = errors.Join(result, target.importer.Abort())
	}
	return result
}
