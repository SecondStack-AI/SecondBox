// Package runtimeconfig owns the Runner's production environment composition.
package runtimeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	runnerconfig "github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
)

type Composition struct {
	Protocol                       runnercontrol.RunnerProtocolConfig
	Connector                      *runnercontrol.GRPCConnector
	BackendKind                    string
	Firecracker                    *runnerconfig.Config
	Microsandbox                   *MicrosandboxComposition
	WorkspaceRoot                  string
	WorkspaceTemplateCapacityBytes int64
	RunnerLogPath                  string
}

type MicrosandboxComposition struct {
	HelperExecutable      string
	LibkrunfwPath         string
	AgentdPath            string
	FlatRootPath          string
	MaterializationPath   string
	MaterializationDigest string
	MaximumVCPUs          uint32
	MaximumMemoryBytes    uint64
	MaximumDiskBytes      uint64
	MaximumInstances      uint32
	MaximumOperations     uint32
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
	protocol.DataPlaneCertificate = connector.RunnerCertificate()
	backendKind := strings.TrimSpace(os.Getenv("SECONDBOX_COMPUTE_BACKEND"))
	if backendKind != "firecracker" && backendKind != "microsandbox" {
		return Composition{}, errors.Join(
			fmt.Errorf("SECONDBOX_COMPUTE_BACKEND must be firecracker or microsandbox"), connector.Close(),
		)
	}
	composition := Composition{Protocol: protocol, Connector: connector, BackendKind: backendKind}
	if healthcheck {
		return composition, nil
	}
	logPath := strings.TrimSpace(os.Getenv("SECONDBOX_RUNNER_LOG_PATH"))
	if logPath == "" || !filepath.IsAbs(logPath) {
		return Composition{}, errors.Join(
			fmt.Errorf("SECONDBOX_RUNNER_LOG_PATH must be an absolute path"),
			connector.Close(),
		)
	}
	for _, name := range []string{"SECONDBOX_RUNNER_WORKSPACE_ROOT", "SECONDBOX_RUNNER_LOG_DIR"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" || !filepath.IsAbs(value) {
			return Composition{}, errors.Join(
				fmt.Errorf("%s must be an absolute path", name), connector.Close(),
			)
		}
	}
	composition.WorkspaceRoot = filepath.Clean(strings.TrimSpace(os.Getenv("SECONDBOX_RUNNER_WORKSPACE_ROOT")))
	composition.RunnerLogPath = logPath
	if backendKind == "microsandbox" {
		microsandbox, templateBytes, err := loadMicrosandboxComposition()
		if err != nil {
			return Composition{}, errors.Join(err, connector.Close())
		}
		composition.Microsandbox = &microsandbox
		composition.WorkspaceTemplateCapacityBytes = templateBytes
		return composition, nil
	}
	for _, name := range []string{"SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR", "SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR", "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT", "SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" || !filepath.IsAbs(value) {
			return Composition{}, errors.Join(fmt.Errorf("%s must be an absolute path", name), connector.Close())
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

func loadMicrosandboxComposition() (MicrosandboxComposition, int64, error) {
	values := map[string]string{}
	for _, name := range []string{
		"SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE",
		"SECONDBOX_MICROSANDBOX_LIBKRUNFW_PATH",
		"SECONDBOX_MICROSANDBOX_AGENTD_PATH",
		"SECONDBOX_MICROSANDBOX_FLAT_ROOT_PATH",
		"SECONDBOX_MICROSANDBOX_MATERIALIZATION_PATH",
	} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return MicrosandboxComposition{}, 0, fmt.Errorf("%s must be a clean absolute path", name)
		}
		values[name] = value
	}
	digest := strings.TrimSpace(os.Getenv("SECONDBOX_MICROSANDBOX_MATERIALIZATION_DIGEST"))
	if digest == "" {
		return MicrosandboxComposition{}, 0, fmt.Errorf("SECONDBOX_MICROSANDBOX_MATERIALIZATION_DIGEST is required")
	}
	readUint := func(name string, bits int) (uint64, error) {
		value, err := strconv.ParseUint(strings.TrimSpace(os.Getenv(name)), 10, bits)
		if err != nil || value == 0 {
			return 0, fmt.Errorf("%s must be a positive base-10 integer", name)
		}
		return value, nil
	}
	vcpus, err := readUint("SECONDBOX_MICROSANDBOX_MAXIMUM_VCPUS", 32)
	if err != nil {
		return MicrosandboxComposition{}, 0, err
	}
	memory, err := readUint("SECONDBOX_MICROSANDBOX_MAXIMUM_MEMORY_BYTES", 64)
	if err != nil {
		return MicrosandboxComposition{}, 0, err
	}
	disk, err := readUint("SECONDBOX_MICROSANDBOX_MAXIMUM_DISK_BYTES", 64)
	if err != nil {
		return MicrosandboxComposition{}, 0, err
	}
	instances, err := readUint("SECONDBOX_MICROSANDBOX_MAXIMUM_INSTANCES", 32)
	if err != nil {
		return MicrosandboxComposition{}, 0, err
	}
	operations, err := readUint("SECONDBOX_MICROSANDBOX_MAXIMUM_OPERATIONS", 32)
	if err != nil {
		return MicrosandboxComposition{}, 0, err
	}
	template, err := readUint("SECONDBOX_MICROSANDBOX_WORKSPACE_TEMPLATE_CAPACITY_BYTES", 63)
	if err != nil {
		return MicrosandboxComposition{}, 0, err
	}
	return MicrosandboxComposition{
		HelperExecutable:      values["SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE"],
		LibkrunfwPath:         values["SECONDBOX_MICROSANDBOX_LIBKRUNFW_PATH"],
		AgentdPath:            values["SECONDBOX_MICROSANDBOX_AGENTD_PATH"],
		FlatRootPath:          values["SECONDBOX_MICROSANDBOX_FLAT_ROOT_PATH"],
		MaterializationPath:   values["SECONDBOX_MICROSANDBOX_MATERIALIZATION_PATH"],
		MaterializationDigest: digest,
		MaximumVCPUs:          uint32(vcpus), MaximumMemoryBytes: memory, MaximumDiskBytes: disk,
		MaximumInstances: uint32(instances), MaximumOperations: uint32(operations),
	}, int64(template), nil
}
