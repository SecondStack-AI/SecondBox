package firecracker

import (
	"context"
	"fmt"
	"os"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func (b *AssignmentBackend) PrepareExecutionImage(ctx context.Context, command *runnerprotocol.PrepareImageCommand, progress func(runnerprotocol.AssignmentProgressStage) error) (*runnerprotocol.PrepareImageResult, error) {
	return executionimage.PrepareThroughFetcher(ctx, b.manager.cfg.ExecutionImageFetcherSocket, command, progress)
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
