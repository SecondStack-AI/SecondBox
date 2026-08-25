//go:build linux

package main

import (
	"context"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtimeconfig"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func newPlatformAssignmentBackend(
	ctx context.Context,
	composition runtimeconfig.Composition,
	workspaceStore *workspacestore.Store,
) (runnercontrol.AssignmentBackend, func(context.Context) error, error) {
	switch composition.BackendKind {
	case "microsandbox":
		return newMicrosandboxAssignmentBackend(composition, workspaceStore)
	case "gvisor":
		return nil, nil, fmt.Errorf("SecondBox gVisor backend selection is valid but its assignment backend is not yet composed")
	case "firecracker":
		manager, err := firecracker.New(composition.Firecracker)
		if err != nil {
			return nil, nil, fmt.Errorf("create SecondBox Firecracker backend: %w", err)
		}
		if err = manager.SetWorkspaceStore(workspaceStore); err != nil {
			return nil, nil, fmt.Errorf("bind SecondBox runner WorkspaceStore: %w", err)
		}
		if err = manager.Start(ctx); err != nil {
			return nil, nil, fmt.Errorf("start SecondBox Firecracker backend: %w", err)
		}
		backend, err := firecracker.NewAssignmentBackend(manager)
		if err != nil {
			return nil, nil, fmt.Errorf("create SecondBox Firecracker assignment backend: %w", err)
		}
		return backend, manager.Shutdown, nil
	default:
		return nil, nil, fmt.Errorf("SecondBox runner compute backend selection is invalid")
	}
}
