//go:build linux

package gvisor

import (
	"context"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func (backend *AssignmentBackend) ObserveWorkspaceStorage(ctx context.Context) ([]*runnerprotocol.WorkspaceStorageObservation, *runnerprotocol.StoragePressureObservation, error) {
	observations, err := runnercontrol.ObserveWorkspaceStorage(ctx, backend.config.WorkspaceStore)
	return observations, nil, err
}
