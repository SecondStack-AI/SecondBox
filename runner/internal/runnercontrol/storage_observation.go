package runnercontrol

import (
	"context"
	"errors"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

type workspaceStorageResult struct {
	storage  []*runnerprotocol.WorkspaceStorageObservation
	pressure *runnerprotocol.StoragePressureObservation
	err      error
}

// pollWorkspaceStorage never waits for a filesystem probe. One in-flight scan
// and one completed batch are bounded across protocol reconnects. A blocked
// kernel ioctl cannot be cancelled, so cancellation must not wait or spawn a
// replacement worker until that scan returns.
func (s *RunnerProtocolService) pollWorkspaceStorage(ctx context.Context) workspaceStorageResult {
	backend, ok := s.backend.(workspaceStorageBackend)
	if !ok {
		return workspaceStorageResult{}
	}
	s.storageObservationMu.Lock()
	defer s.storageObservationMu.Unlock()
	var result workspaceStorageResult
	if s.storageObservationResult != nil {
		result = *s.storageObservationResult
		s.storageObservationResult = nil
	}
	if !s.storageObservationRunning && ctx.Err() == nil && result.err == nil {
		s.storageObservationRunning = true
		go func() {
			storage, pressure, err := backend.ObserveWorkspaceStorage(ctx)
			s.storageObservationMu.Lock()
			defer s.storageObservationMu.Unlock()
			s.storageObservationRunning = false
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return
			}
			s.storageObservationResult = &workspaceStorageResult{storage: storage, pressure: pressure, err: err}
		}()
	}
	return result
}

type workspaceStorageBackend interface {
	ObserveWorkspaceStorage(context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error)
}

func ObserveWorkspaceStorage(ctx context.Context, store workspacestore.WorkspaceStore) ([]*runnerprotocol.WorkspaceStorageObservation, error) {
	observations, err := store.ObserveStorage(ctx, 64)
	if err != nil {
		return nil, err
	}
	result := make([]*runnerprotocol.WorkspaceStorageObservation, 0, len(observations))
	for _, observation := range observations {
		item := &runnerprotocol.WorkspaceStorageObservation{WorkspaceId: observation.WorkspaceID, Generation: observation.Generation, ObservedAtUnixMs: uint64(observation.ObservedAt.UnixMilli()), UnavailableReason: observation.Reason}
		if observation.AllocatedBytes != nil {
			value := uint64(*observation.AllocatedBytes)
			item.AllocatedBytes = &value
		}
		if observation.ExclusiveBytes != nil {
			value := uint64(*observation.ExclusiveBytes)
			item.ExclusiveBytes = &value
		}
		item.ExclusiveReason = observation.ExclusiveReason
		result = append(result, item)
	}
	return result, nil
}
