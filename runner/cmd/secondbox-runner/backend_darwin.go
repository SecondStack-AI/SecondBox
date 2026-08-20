//go:build darwin

package main

import (
	"context"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtimeconfig"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func newPlatformAssignmentBackend(
	_ context.Context,
	composition runtimeconfig.Composition,
	workspaceStore *workspacestore.Store,
) (runnercontrol.AssignmentBackend, func(context.Context) error, error) {
	if composition.BackendKind != "microsandbox" {
		return nil, nil, fmt.Errorf("SecondBox Darwin runner supports only the Microsandbox backend")
	}
	return newMicrosandboxAssignmentBackend(composition, workspaceStore)
}
