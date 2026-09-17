package firecracker

import (
	"context"
	"errors"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// Staging capacity is admitted by the fetcher against the cache filesystem it
// writes to, and only when the digest is absent. The Runner cannot admit it
// here: the request carries a tag, so warm and cold are indistinguishable
// until the fetcher has resolved the digest.
func (b *AssignmentBackend) PrepareExecutionImage(ctx context.Context, command *runnerprotocol.PrepareImageCommand, progress func(runnerprotocol.AssignmentProgressStage) error) (*runnerprotocol.PrepareImageResult, error) {
	input := executionimage.FetchRequest{
		OperationID: command.OperationId, TenantRef: command.TenantRef, Reference: command.Reference, Deadline: time.UnixMilli(int64(command.DeadlineUnixMs)),
	}
	result, err := executionimage.FetchExecutionImage(ctx, b.manager.cfg.ExecutionImageFetcherSocket, input, func(result executionimage.FetchResult) error {
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
