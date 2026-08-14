//go:build linux

package runtimeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
)

func validatePlatformBackendKind(string) error { return nil }

func loadPlatformBackendComposition(
	composition Composition,
	connector *runnercontrol.GRPCConnector,
) (Composition, error) {
	for _, name := range []string{
		"SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR",
		"SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR",
		"SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT",
		"SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT",
	} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" || !filepath.IsAbs(value) {
			return Composition{}, errors.Join(
				fmt.Errorf("%s must be an absolute path", name), connector.Close(),
			)
		}
	}
	firecrackerConfig, err := firecracker.LoadRunnerFirecrackerConfigFromEnv()
	if err != nil {
		return Composition{}, errors.Join(
			fmt.Errorf("load SecondBox Firecracker config: %w", err), connector.Close(),
		)
	}
	composition.Firecracker = firecrackerConfig
	composition.WorkspaceTemplateCapacityBytes = int64(firecrackerConfig.MicroVMWorkspaceSizeMiB) << 20
	return composition, nil
}
