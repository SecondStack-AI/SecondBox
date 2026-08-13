// Package microsandbox implements the experimental local Microsandbox compute backend.
package microsandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

// Config is the complete, explicit immutable backend composition.
type Config struct {
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
	WorkspaceStore        *workspacestore.Store
}

type validatedConfig struct {
	Config
	manifest materialization.Manifest
}

func validateConfig(config Config) (validatedConfig, error) {
	for name, value := range map[string]string{
		"helper executable":        config.HelperExecutable,
		"libkrunfw":                config.LibkrunfwPath,
		"agentd":                   config.AgentdPath,
		"flat root":                config.FlatRootPath,
		"materialization manifest": config.MaterializationPath,
	} {
		if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox %s path must be clean and absolute", name)
		}
	}
	for name, path := range map[string]string{
		"helper executable": config.HelperExecutable,
		"libkrunfw":         config.LibkrunfwPath,
		"agentd":            config.AgentdPath,
	} {
		info, err := os.Stat(path)
		if err != nil {
			return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox inspect %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox %s is not a regular file", name)
		}
	}
	helperInfo, _ := os.Stat(config.HelperExecutable)
	agentInfo, _ := os.Stat(config.AgentdPath)
	if helperInfo.Mode().Perm()&0o111 == 0 || agentInfo.Mode().Perm()&0o111 == 0 {
		return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox helper and agentd must be executable")
	}
	rootInfo, err := os.Stat(config.FlatRootPath)
	if err != nil || !rootInfo.IsDir() {
		return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox flat root must be an existing directory")
	}
	if config.WorkspaceStore == nil {
		return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox WorkspaceStore is required")
	}
	if config.MaximumVCPUs == 0 || config.MaximumMemoryBytes == 0 || config.MaximumDiskBytes == 0 ||
		config.MaximumInstances == 0 || config.MaximumOperations == 0 {
		return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox integer capacity is incomplete")
	}
	manifest, err := materialization.Load(config.MaterializationPath, config.MaterializationDigest)
	if err != nil {
		return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox materialization: %w", err)
	}
	if manifest.Key.BackendKind != materialization.BackendMicrosandbox ||
		manifest.Key.GuestArchitecture != runtime.GOARCH {
		return validatedConfig{}, fmt.Errorf("SecondBox Microsandbox materialization does not match this backend and architecture")
	}
	for id, path := range map[string]string{
		"agentd": config.AgentdPath, "helper": config.HelperExecutable, "libkrunfw": config.LibkrunfwPath,
	} {
		if err := verifyLaunchArtifact(manifest, id, path); err != nil {
			return validatedConfig{}, err
		}
	}
	return validatedConfig{Config: config, manifest: manifest}, nil
}

func verifyLaunchArtifact(manifest materialization.Manifest, id, path string) error {
	expected := ""
	for _, artifact := range manifest.LaunchArtifacts {
		if artifact.ID == id {
			expected = artifact.SHA256
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("SecondBox Microsandbox materialization omits %s launch identity", id)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("SecondBox Microsandbox read %s launch artifact: %w", id, err)
	}
	digest := sha256.Sum256(content)
	if "sha256:"+hex.EncodeToString(digest[:]) != expected {
		return fmt.Errorf("SecondBox Microsandbox %s launch artifact differs from materialization", id)
	}
	return nil
}
