//go:build linux

package gvisor

import (
	"context"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func (backend *AssignmentBackend) ObserveWorkspaceStorage(ctx context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error) {
	observations, err := runnercontrol.ObserveWorkspaceStorage(ctx, backend.config.WorkspaceStore)
	if err != nil || backend.storagePressure == nil {
		return observations, nil, err
	}
	pressure := &runnerprotocol.StoragePressureObservation{Status: "unavailable", ObservedAtUnixMs: uint64(time.Now().UnixMilli())}
	state, probeErr := backend.storagePressure.observe(ctx)
	if probeErr == nil {
		pressure.Status = state
	}
	return observations, pressure, nil
}
