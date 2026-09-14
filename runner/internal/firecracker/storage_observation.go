package firecracker

import (
	"context"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func (backend *AssignmentBackend) ObserveWorkspaceStorage(ctx context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error) {
	observations, err := runnercontrol.ObserveWorkspaceStorage(ctx, backend.manager.workspaceStore)
	if err != nil {
		return nil, nil, err
	}
	pressure := &runnerprotocol.StoragePressureObservation{Status: "unavailable", ObservedAtUnixMs: uint64(time.Now().UnixMilli())}
	state, err := backend.storagePressure.Observe(ctx)
	if err != nil {
		// Probe failures are surfaced as unavailable evidence, never healthy.
		return observations, pressure, nil
	}
	pressure.Status = string(state)
	return observations, pressure, nil
}
