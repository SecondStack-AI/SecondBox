package workspacestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

type WorkspaceStorageObservation struct {
	WorkspaceID     string
	Generation      uint64
	ObservedAt      time.Time
	AllocatedBytes  *int64
	ExclusiveBytes  *int64
	ExclusiveReason string
	Reason          string
}

// ObserveStorage visits at most limit directory entries per call. It reads
// manifest and inode metadata only, including while compute owns the writer.
func (store *Store) ObserveStorage(ctx context.Context, limit int) ([]WorkspaceStorageObservation, error) {
	if limit < 1 || limit > 64 {
		return nil, fmt.Errorf("SecondBox Workspace storage observation limit must be from 1 through 64")
	}
	store.observationMu.Lock()
	defer store.observationMu.Unlock()
	if store.observationDirectory == nil {
		directory, err := os.Open(store.workspacesRoot())
		if err != nil {
			return nil, fmt.Errorf("SecondBox Workspace storage observation directory failed: %w", err)
		}
		store.observationDirectory = directory
	}
	entries, err := store.observationDirectory.ReadDir(limit)
	if err != nil {
		closeErr := store.observationDirectory.Close()
		store.observationDirectory = nil
		if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("SecondBox Workspace storage observation directory read failed: %w", errors.Join(err, closeErr))
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	observations := make([]WorkspaceStorageObservation, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || validateID(entry.Name()) != nil {
			return nil, fmt.Errorf("%w: invalid Workspace observation directory", ErrCorruptState)
		}
		observations = append(observations, store.observeWorkspaceStorage(entry.Name()))
	}
	return observations, nil
}

func (store *Store) observeWorkspaceStorage(workspaceID string) WorkspaceStorageObservation {
	observation := WorkspaceStorageObservation{WorkspaceID: workspaceID, ObservedAt: store.now().UTC()}
	manifest, err := store.readCurrentManifest(workspaceID)
	if err != nil {
		observation.Reason = workspaceObservationFailure(err)
		return observation
	}
	observation.Generation = manifest.Generation
	path, err := store.validatedVersionPath(workspaceID, manifest.Image)
	if err != nil {
		observation.Reason = workspaceObservationFailure(err)
		return observation
	}
	// Pin both probes to the same inode without following a replaced symlink or
	// blocking on a non-regular file. This does not acquire the compute writer.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		observation.Reason = workspaceObservationFailure(err)
		return observation
	}
	info, err := file.Stat()
	if err != nil {
		observation.Reason = workspaceObservationFailure(errors.Join(err, file.Close()))
		return observation
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Blocks < 0 {
		_ = file.Close()
		observation.Reason = "probe_failed"
		return observation
	}
	// st_blocks counts 512-byte allocated blocks, including reflink-shared blocks.
	allocated := stat.Blocks * 512
	observation.AllocatedBytes = &allocated
	observation.ExclusiveBytes, observation.ExclusiveReason = observeExclusiveBytes(file)
	if err := file.Close(); err != nil {
		observation.ExclusiveBytes, observation.ExclusiveReason = nil, "exclusive_probe_failed"
	}
	return observation
}

func workspaceObservationFailure(err error) string {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrWorkspaceNotFound) {
		return "missing"
	}
	return "probe_failed"
}
