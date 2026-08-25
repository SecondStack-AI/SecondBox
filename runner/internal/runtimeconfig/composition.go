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
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
)

type Composition struct {
	Protocol                       runnercontrol.RunnerProtocolConfig
	Connector                      *runnercontrol.GRPCConnector
	BackendKind                    string
	Firecracker                    *runnerconfig.Config
	Microsandbox                   *MicrosandboxComposition
	GVisor                         *GVisorComposition
	WorkspaceRoot                  string
	WorkspaceTemplateCapacityBytes int64
	RunnerLogPath                  string
}

// GVisorComposition requires no KVM, jailer, TAP, bridge, signature-key,
// trust-anchor, or nested-virtualization configuration: only the pinned runsc
// launch artifact, the injected guest agent, the pre-materialized flat root,
// the pinned materialization manifest, and integer capacity bounds.
type GVisorComposition struct {
	RunscPath             string
	RuntimeDir            string
	AgentPath             string
	FlatRootPath          string
	MaterializationPath   string
	MaterializationDigest string
	MaximumVCPUs          uint32
	MaximumMemoryBytes    uint64
	MaximumDiskBytes      uint64
	MaximumInstances      uint32
	MaximumOperations     uint32
	NetworkProfile        uint32
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
	if backendKind != "firecracker" && backendKind != "microsandbox" && backendKind != "gvisor" {
		return Composition{}, errors.Join(
			fmt.Errorf("SECONDBOX_COMPUTE_BACKEND must be firecracker, microsandbox, or gvisor"), connector.Close(),
		)
	}
	if err := validatePlatformBackendKind(backendKind); err != nil {
		return Composition{}, errors.Join(err, connector.Close())
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
	if backendKind == "gvisor" {
		gvisor, templateBytes, err := loadGVisorComposition()
		if err != nil {
			return Composition{}, errors.Join(err, connector.Close())
		}
		composition.GVisor = &gvisor
		composition.WorkspaceTemplateCapacityBytes = templateBytes
		return composition, nil
	}
	return loadPlatformBackendComposition(composition, connector)
}

func loadGVisorComposition() (GVisorComposition, int64, error) {
	values := map[string]string{}
	for _, name := range []string{
		"SECONDBOX_GVISOR_RUNSC_PATH",
		"SECONDBOX_GVISOR_AGENT_PATH",
		"SECONDBOX_GVISOR_FLAT_ROOT_PATH",
		"SECONDBOX_GVISOR_MATERIALIZATION_PATH",
		"SECONDBOX_GVISOR_RUNTIME_DIR",
	} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return GVisorComposition{}, 0, fmt.Errorf("%s must be a clean absolute path", name)
		}
		values[name] = value
	}
	digest := strings.TrimSpace(os.Getenv("SECONDBOX_GVISOR_MATERIALIZATION_DIGEST"))
	if digest == "" {
		return GVisorComposition{}, 0, fmt.Errorf("SECONDBOX_GVISOR_MATERIALIZATION_DIGEST is required")
	}
	readUint := func(name string, bits int) (uint64, error) {
		value, err := strconv.ParseUint(strings.TrimSpace(os.Getenv(name)), 10, bits)
		if err != nil || value == 0 {
			return 0, fmt.Errorf("%s must be a positive base-10 integer", name)
		}
		return value, nil
	}
	vcpus, err := readUint("SECONDBOX_GVISOR_MAXIMUM_VCPUS", 32)
	if err != nil {
		return GVisorComposition{}, 0, err
	}
	memory, err := readUint("SECONDBOX_GVISOR_MAXIMUM_MEMORY_BYTES", 64)
	if err != nil {
		return GVisorComposition{}, 0, err
	}
	disk, err := readUint("SECONDBOX_GVISOR_MAXIMUM_DISK_BYTES", 64)
	if err != nil {
		return GVisorComposition{}, 0, err
	}
	instances, err := readUint("SECONDBOX_GVISOR_MAXIMUM_INSTANCES", 32)
	if err != nil {
		return GVisorComposition{}, 0, err
	}
	operations, err := readUint("SECONDBOX_GVISOR_MAXIMUM_OPERATIONS", 32)
	if err != nil {
		return GVisorComposition{}, 0, err
	}
	template, err := readUint("SECONDBOX_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES", 63)
	if err != nil {
		return GVisorComposition{}, 0, err
	}
	// The network profile keeps runners sharing one host network namespace
	// apart; a single runner per host keeps the default 0.
	networkProfile := uint64(0)
	if raw := strings.TrimSpace(os.Getenv("SECONDBOX_GVISOR_NETWORK_PROFILE")); raw != "" {
		networkProfile, err = strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return GVisorComposition{}, 0, fmt.Errorf("SECONDBOX_GVISOR_NETWORK_PROFILE must be a base-10 integer")
		}
	}
	return GVisorComposition{
		RunscPath:             values["SECONDBOX_GVISOR_RUNSC_PATH"],
		RuntimeDir:            values["SECONDBOX_GVISOR_RUNTIME_DIR"],
		AgentPath:             values["SECONDBOX_GVISOR_AGENT_PATH"],
		FlatRootPath:          values["SECONDBOX_GVISOR_FLAT_ROOT_PATH"],
		MaterializationPath:   values["SECONDBOX_GVISOR_MATERIALIZATION_PATH"],
		MaterializationDigest: digest,
		MaximumVCPUs:          uint32(vcpus), MaximumMemoryBytes: memory, MaximumDiskBytes: disk,
		MaximumInstances: uint32(instances), MaximumOperations: uint32(operations),
		NetworkProfile: uint32(networkProfile),
	}, int64(template), nil
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
