//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/SecondStack-AI/SecondBox/runner/internal/gvisor"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtimeconfig"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func newGVisorAssignmentBackend(
	composition runtimeconfig.Composition,
	workspaceStore *workspacestore.Store,
) (runnercontrol.AssignmentBackend, func(context.Context) error, error) {
	settings := composition.GVisor
	if settings == nil {
		return nil, nil, fmt.Errorf("SecondBox gVisor composition is missing")
	}
	selfExecutable, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve SecondBox runner executable: %w", err)
	}
	backend, err := gvisor.NewAssignmentBackend(gvisor.Config{
		RunscPath:             settings.RunscPath,
		AgentPath:             settings.AgentPath,
		FlatRootPath:          settings.FlatRootPath,
		MaterializationPath:   settings.MaterializationPath,
		MaterializationDigest: settings.MaterializationDigest,
		RuntimeDir:            settings.RuntimeDir,
		WorkspaceRoot:         composition.WorkspaceRoot,
		SelfExecutable:        selfExecutable,
		MaximumVCPUs:          settings.MaximumVCPUs,
		MaximumMemoryBytes:    settings.MaximumMemoryBytes,
		MaximumDiskBytes:      settings.MaximumDiskBytes,
		MaximumInstances:      settings.MaximumInstances,
		MaximumOperations:     settings.MaximumOperations,
		NetworkProfile:        settings.NetworkProfile,
		DNSUpstream:           settings.DNSUpstream,
		WorkspaceStore:        workspaceStore,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create SecondBox gVisor assignment backend: %w", err)
	}
	return backend, backend.Shutdown, nil
}
