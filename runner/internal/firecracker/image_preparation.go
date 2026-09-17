package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// A Sandbox stages the rootfs of its execution image into the run directory by
// reflink, which cannot cross a filesystem. An operator learns that at Runner
// startup instead of at the first start of a client-selected image, and image
// bytes keep the reflink-only staging that Workspace bytes have.
func validateExecutionImageCacheFilesystem(cacheRoot, runDir string) error {
	cacheDevice, err := filesystemDevice("SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT", cacheRoot)
	if err != nil {
		return err
	}
	runDevice, err := filesystemDevice("SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR", runDir)
	if err != nil {
		return err
	}
	if cacheDevice != runDevice {
		return fmt.Errorf(
			"SecondBox execution image cache root %q and microVM run dir %q are on different filesystems; set SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT and SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR on one reflink-capable filesystem",
			cacheRoot, runDir,
		)
	}
	return nil
}

func filesystemDevice(setting, path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("inspect %s %q: %w", setting, path, err)
	}
	device, _, _, ok := fileStatIdentity(info)
	if !ok {
		return 0, fmt.Errorf("%s %q filesystem identity is unavailable", setting, path)
	}
	return device, nil
}
