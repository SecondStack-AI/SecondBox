package main

import (
	"context"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/runner/internal/microsandbox"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtimeconfig"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func newMicrosandboxAssignmentBackend(
	composition runtimeconfig.Composition,
	workspaceStore *workspacestore.Store,
) (runnercontrol.AssignmentBackend, func(context.Context) error, error) {
	settings := composition.Microsandbox
	if settings == nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox composition is missing")
	}
	backend, err := microsandbox.NewAssignmentBackend(microsandbox.Config{
		HelperExecutable:      settings.HelperExecutable,
		LibkrunfwPath:         settings.LibkrunfwPath,
		AgentdPath:            settings.AgentdPath,
		FlatRootPath:          settings.FlatRootPath,
		MaterializationPath:   settings.MaterializationPath,
		MaterializationDigest: settings.MaterializationDigest,
		MaximumVCPUs:          settings.MaximumVCPUs,
		MaximumMemoryBytes:    settings.MaximumMemoryBytes,
		MaximumDiskBytes:      settings.MaximumDiskBytes,
		MaximumInstances:      settings.MaximumInstances,
		MaximumOperations:     settings.MaximumOperations,
		NetworkPolicy:         settings.NetworkPolicy,
		WorkspaceStore:        workspaceStore,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create SecondBox Microsandbox assignment backend: %w", err)
	}
	return backend, backend.Shutdown, nil
}
