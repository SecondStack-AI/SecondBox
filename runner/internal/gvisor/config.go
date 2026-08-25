//go:build linux

package gvisor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

// Config is the complete, explicit immutable backend composition. A gVisor
// runner requires no KVM, jailer, TAP, bridge, signature-key, or trust-anchor
// configuration.
type Config struct {
	RunscPath             string
	AgentPath             string
	FlatRootPath          string
	MaterializationPath   string
	MaterializationDigest string
	RuntimeDir            string
	WorkspaceRoot         string
	SelfExecutable        string
	// DNSUpstream optionally overrides host-resolver discovery for the
	// runner DNS proxy, as host:port. Qualification environments use it to
	// pin a test-owned resolver.
	DNSUpstream        string
	MaximumVCPUs       uint32
	MaximumMemoryBytes uint64
	MaximumDiskBytes   uint64
	MaximumInstances   uint32
	MaximumOperations  uint32
	WorkspaceStore     *workspacestore.Store
}

type validatedConfig struct {
	Config
	manifest materialization.Manifest
}

const (
	runscArtifactID = "runsc"
	agentArtifactID = "guest-agent"
)

func validateConfig(config Config) (validatedConfig, error) {
	for name, value := range map[string]string{
		"runsc":                    config.RunscPath,
		"guest agent":              config.AgentPath,
		"flat root":                config.FlatRootPath,
		"materialization manifest": config.MaterializationPath,
		"runtime directory":        config.RuntimeDir,
		"workspace root":           config.WorkspaceRoot,
		"runner executable":        config.SelfExecutable,
	} {
		if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return validatedConfig{}, fmt.Errorf("SecondBox gVisor %s path must be clean and absolute", name)
		}
	}
	if config.MaximumVCPUs == 0 || config.MaximumMemoryBytes == 0 || config.MaximumDiskBytes == 0 ||
		config.MaximumInstances == 0 || config.MaximumOperations == 0 {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor capacity bounds must be positive")
	}
	if config.WorkspaceStore == nil {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor backend requires the runner WorkspaceStore")
	}
	manifest, err := materialization.Load(config.MaterializationPath, config.MaterializationDigest)
	if err != nil {
		return validatedConfig{}, err
	}
	if manifest.Key.BackendKind != materialization.BackendGVisor {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor materialization names backend %q", manifest.Key.BackendKind)
	}
	if manifest.Key.GuestArchitecture != runtime.GOARCH {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor materialization architecture %q differs from host %q",
			manifest.Key.GuestArchitecture, runtime.GOARCH)
	}
	if err := verifyLaunchArtifact(manifest, runscArtifactID, config.RunscPath, true); err != nil {
		return validatedConfig{}, err
	}
	if err := verifyLaunchArtifact(manifest, agentArtifactID, config.AgentPath, true); err != nil {
		return validatedConfig{}, err
	}
	flatRootDigest, err := materialization.DigestFlatRoot(config.FlatRootPath)
	if err != nil {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor flat root digest: %w", err)
	}
	if flatRootDigest != manifest.FlatRootDigest {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor flat root differs from the pinned materialization")
	}
	return validatedConfig{Config: config, manifest: manifest}, nil
}

func verifyLaunchArtifact(
	manifest materialization.Manifest,
	artifactID, path string,
	executable bool,
) error {
	var expected string
	for _, artifact := range manifest.LaunchArtifacts {
		if artifact.ID == artifactID {
			expected = artifact.SHA256
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("SecondBox gVisor materialization omits %s launch identity", artifactID)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("SecondBox gVisor inspect %s: %w", artifactID, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("SecondBox gVisor %s is not a regular file", artifactID)
	}
	if executable && info.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("SecondBox gVisor %s is not executable", artifactID)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox gVisor open %s: %w", artifactID, err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return fmt.Errorf("SecondBox gVisor digest %s: %w", artifactID, err)
	}
	actual := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SecondBox gVisor %s digest differs from the pinned materialization", artifactID)
	}
	return nil
}
