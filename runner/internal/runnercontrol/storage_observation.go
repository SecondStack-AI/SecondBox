package runnercontrol

import (
	"context"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

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
