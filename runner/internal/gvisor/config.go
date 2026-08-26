//go:build linux

package gvisor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// NetworkProfile separates runners sharing one host network namespace:
	// it selects the DNS proxy address, the link-local /30 slot space, and
	// the veth and namespace name spaces. Single-runner hosts keep 0.
	NetworkProfile uint32
	WorkspaceRoot  string
	SelfExecutable string
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

// maximumRuntimeDirLength keeps every per-Instance Unix socket path -
// "<runtimeDir>/<16-hex instance>/sockets/protocol.sock" - inside sun_path,
// so no assignment can fail on socket-path length after readiness advertised.
const maximumRuntimeDirLength = 107 - len("/0123456789abcdef/sockets/protocol.sock")

// validateRuntimeDir refuses runtime directories whose startup reconciliation
// - which removes every child - could destroy durable or unrelated data: the
// filesystem root, symlinked paths, and any path that contains or is
// contained by the WorkspaceStore root or the immutable flat root.
func validateRuntimeDir(runtimeDir, workspaceRoot, flatRoot string) error {
	if runtimeDir == "/" || filepath.Dir(runtimeDir) == "/" {
		return fmt.Errorf("SecondBox gVisor runtime directory must be at least two levels below the filesystem root")
	}
	if len(runtimeDir) > maximumRuntimeDirLength {
		return fmt.Errorf("SecondBox gVisor runtime directory exceeds %d bytes; per-Instance socket paths would exceed the Unix socket limit", maximumRuntimeDirLength)
	}
	if info, err := os.Lstat(runtimeDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("SecondBox gVisor runtime directory must not be a symlink")
		}
		if !info.IsDir() {
			return fmt.Errorf("SecondBox gVisor runtime directory must be a directory")
		}
		resolved, err := filepath.EvalSymlinks(runtimeDir)
		if err != nil {
			return fmt.Errorf("SecondBox gVisor runtime directory resolution: %w", err)
		}
		if resolved != runtimeDir {
			return fmt.Errorf("SecondBox gVisor runtime directory must not traverse symlinks")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox gVisor runtime directory inspection: %w", err)
	} else {
		// The directory does not exist yet, so MkdirAll will walk whatever
		// ancestors do: the deepest existing ancestor must itself resolve
		// symlink-free, or creation could be redirected beneath a protected
		// root that the lexical overlap checks below cannot see.
		ancestor := filepath.Dir(runtimeDir)
		for {
			if _, statErr := os.Lstat(ancestor); statErr == nil {
				break
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("SecondBox gVisor runtime directory ancestor inspection: %w", statErr)
			}
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				break
			}
			ancestor = parent
		}
		resolved, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr != nil {
			return fmt.Errorf("SecondBox gVisor runtime directory ancestor resolution: %w", resolveErr)
		}
		if resolved != ancestor {
			return fmt.Errorf("SecondBox gVisor runtime directory ancestors must not traverse symlinks")
		}
	}
	for name, protected := range map[string]string{
		"WorkspaceStore root": workspaceRoot,
		"flat root":           flatRoot,
	} {
		// Overlap is checked on the configured spelling and on the fully
		// resolved path: a protected root reachable through a symlink
		// beneath the runtime directory would otherwise evade the lexical
		// check and be destroyed by startup reconciliation.
		if pathsOverlap(runtimeDir, protected) {
			return fmt.Errorf("SecondBox gVisor runtime directory must be disjoint from the %s", name)
		}
		resolved, err := filepath.EvalSymlinks(protected)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("SecondBox gVisor %s resolution: %w", name, err)
		}
		if pathsOverlap(runtimeDir, resolved) {
			return fmt.Errorf("SecondBox gVisor runtime directory must be disjoint from the resolved %s", name)
		}
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	return left == right ||
		strings.HasPrefix(left, right+string(filepath.Separator)) ||
		strings.HasPrefix(right, left+string(filepath.Separator))
}

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
	if err := validateRuntimeDir(config.RuntimeDir, config.WorkspaceRoot, config.FlatRootPath); err != nil {
		return validatedConfig{}, err
	}
	if config.MaximumVCPUs == 0 || config.MaximumMemoryBytes == 0 || config.MaximumDiskBytes == 0 ||
		config.MaximumInstances == 0 || config.MaximumOperations == 0 {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor capacity bounds must be positive")
	}
	if config.WorkspaceStore == nil {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor backend requires the runner WorkspaceStore")
	}
	if config.NetworkProfile >= maximumNetworkProfiles {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor network profile must be below %d", maximumNetworkProfiles)
	}
	if config.MaximumInstances > maximumNetworkslots {
		return validatedConfig{}, fmt.Errorf("SecondBox gVisor supports at most %d concurrent Instances per runner", maximumNetworkslots)
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
