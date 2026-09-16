package runnercontrol

import (
	"context"
	"errors"
	"log/slog"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type ImagePreparationBackend interface {
	PrepareExecutionImage(context.Context, *runnerprotocol.PrepareImageCommand, func(runnerprotocol.AssignmentProgressStage) error) (*runnerprotocol.PrepareImageResult, error)
}

func (s *RunnerProtocolService) handleImagePreparation(ctx context.Context, stream RunnerProtocolStream, command *runnerprotocol.PrepareImageCommand) error {
	deadline := time.UnixMilli(int64(command.DeadlineUnixMs))
	if command.OperationId == "" || command.TenantRef == "" || command.Reference == "" || !deadline.After(time.Now()) || deadline.After(time.Now().Add(time.Hour)) {
		return s.sendImagePreparationResult(stream, &runnerprotocol.PrepareImageResult{OperationId: command.OperationId, Failure: "Image preparation request is invalid or expired"})
	}
	backend, ok := s.backend.(ImagePreparationBackend)
	if !ok {
		return s.sendImagePreparationResult(stream, &runnerprotocol.PrepareImageResult{OperationId: command.OperationId, Failure: "Runner does not support image preparation"})
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	result, err := backend.PrepareExecutionImage(ctx, command, func(stage runnerprotocol.AssignmentProgressStage) error {
		return s.sendImagePreparationResult(stream, &runnerprotocol.PrepareImageResult{OperationId: command.OperationId, Stage: stage})
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		slog.WarnContext(ctx, "SecondBox image preparation failed", "operationId", command.OperationId, "error", err)
		result = &runnerprotocol.PrepareImageResult{Failure: "Image preparation failed; check Runner and image fetcher diagnostics"}
	}
	result.OperationId = command.OperationId
	return s.sendImagePreparationResult(stream, result)
}

func (s *RunnerProtocolService) sendImagePreparationResult(stream RunnerProtocolStream, result *runnerprotocol.PrepareImageResult) error {
	return s.sendSequencedRunnerFrame(stream, func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
		result.MessageId, result.Sequence = s.messageID(sequence), sequence
		return &runnerprotocol.RunnerToControlPlane{Message: &runnerprotocol.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: result}}
	})
}
