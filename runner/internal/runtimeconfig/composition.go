// Package runtimeconfig owns the Runner's production environment composition.
package runtimeconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	runnerconfig "github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
)

type Composition struct {
	Protocol      runnercontrol.RunnerProtocolConfig
	Connector     *runnercontrol.GRPCConnector
	Firecracker   *runnerconfig.Config
	RunnerLogPath string
}

// LoadFromEnvironment is shared by PID 1 and deployment conformance tests. A
// healthcheck validates only the authenticated protocol boundary; normal start
// validates the complete Firecracker and container-entrypoint contract.
func LoadFromEnvironment(healthcheck bool) (Composition, error) {
	protocol, connectorConfig, err := runnercontrol.LoadRunnerProtocolConfigFromEnv()
	if err != nil {
		return Composition{}, fmt.Errorf("load SecondBox runner protocol config: %w", err)
	}
	connector, err := runnercontrol.NewGRPCConnector(connectorConfig)
	if err != nil {
		return Composition{}, fmt.Errorf("load SecondBox runner mTLS credentials: %w", err)
	}
	composition := Composition{Protocol: protocol, Connector: connector}
	if healthcheck {
		return composition, nil
	}
	logPath := strings.TrimSpace(os.Getenv("SECONDBOX_RUNNER_LOG_PATH"))
	if logPath == "" || !filepath.IsAbs(logPath) {
		connector.Close()
		return Composition{}, fmt.Errorf("SECONDBOX_RUNNER_LOG_PATH must be an absolute path")
	}
	for _, name := range []string{"SECONDBOX_RUNNER_WORKSPACE_ROOT", "SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR", "SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR", "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT", "SECONDBOX_RUNNER_LOG_DIR"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" || !filepath.IsAbs(value) {
			connector.Close()
			return Composition{}, fmt.Errorf("%s must be an absolute path", name)
		}
	}
	firecrackerConfig, err := firecracker.LoadRunnerFirecrackerConfigFromEnv()
	if err != nil {
		connector.Close()
		return Composition{}, fmt.Errorf("load SecondBox Firecracker config: %w", err)
	}
	composition.Firecracker = firecrackerConfig
	composition.RunnerLogPath = logPath
	return composition, nil
}
