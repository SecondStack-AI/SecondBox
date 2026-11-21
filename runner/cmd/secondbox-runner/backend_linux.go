//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"

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
		return newGVisorAssignmentBackend(ctx, composition, workspaceStore)
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
		go prewarmExecutionImages(ctx, manager.PrewarmExecutionImages)
		return backend, manager.Shutdown, nil
	default:
		return nil, nil, fmt.Errorf("SecondBox runner compute backend selection is invalid")
	}
}

// prewarmExecutionImages runs for the Runner's lifetime; Sandbox starts do not
// wait for it.
func prewarmExecutionImages(ctx context.Context, prewarm func(context.Context) error) {
	if err := prewarm(ctx); err != nil {
		slog.Error("SecondBox execution image prewarm stopped", "error", err)
	}
}
