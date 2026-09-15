package firecracker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func (b *AssignmentBackend) PrepareExecutionImage(ctx context.Context, command *runnerprotocol.PrepareImageCommand, progress func(runnerprotocol.AssignmentProgressStage) error) (_ *runnerprotocol.PrepareImageResult, resultErr error) {
	cfg := b.manager.cfg
	reservationID := command.OperationId + "-image-preparation"
	bytes := uint64(cfg.ExecutionImageMaximumDownloadBytes)*2 + uint64(cfg.ExecutionImageMaximumExpandedBytes)
	input := executionimage.FetchRequest{
		OperationID: command.OperationId, TenantRef: command.TenantRef, Reference: command.Reference, Deadline: time.UnixMilli(int64(command.DeadlineUnixMs)),
	}
	admissionErr := b.storagePressure.Reserve(ctx, reservationID, bytes)
	if errors.Is(admissionErr, ErrStoragePressureAdmissionDenied) {
		input.ReclaimOnly = true
		if _, err := executionimage.FetchExecutionImage(ctx, cfg.ExecutionImageFetcherSocket, input, func(executionimage.FetchResult) error { return nil }); err != nil {
			return nil, err
		}
		input.ReclaimOnly = false
		admissionErr = b.storagePressure.Reserve(ctx, reservationID, bytes)
	}
	if err := admissionErr; err != nil {
		return nil, fmt.Errorf("SecondBox image preparation storage reservation: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, b.storagePressure.Release(context.Background(), reservationID))
	}()
	result, err := executionimage.FetchExecutionImage(ctx, cfg.ExecutionImageFetcherSocket, input, func(result executionimage.FetchResult) error {
		stage, ok := runnerprotocol.AssignmentProgressStage_value[result.Stage]
		if !ok {
			return errors.New("SecondBox image fetcher reported an invalid stage")
		}
		return progress(runnerprotocol.AssignmentProgressStage(stage))
	})
	if err != nil {
		return nil, err
	}
	return &runnerprotocol.PrepareImageResult{ResolvedDigest: result.Digest, Manifest: result.Manifest, Signature: result.Signature}, nil
}
