// Package config contains configuration owned by the privileged SecondBox Runner.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

type Config struct {
	FirecrackerPath                string
	JailerPath                     string
	MicroVMJailerChrootBaseDir     string
	MicroVMJailerUIDStart          int
	MicroVMJailerUIDCount          int
	MicroVMJailerUIDAllowBelow1000 bool
	MicroVMJailerGID               int
	MicroVMJailerCgroupVersion     int
	MicroVMJailerParentCgroup      string
	MicroVMKernelPath              string
	MicroVMRootfsPath              string
	MicroVMToolRootfsPath          string
	MicroVMSharedImagePath         string
	MicroVMToolSharedImagePath     string
	// MicroVMBundleVerification requires the kernel, rootfs and shared image to
	// form one bundle that matches its manifest and checksums.
	MicroVMBundleVerification                  bool
	RunnerWorkspaceRoot                        string
	MicroVMRunDir                              string
	MicroVMLogDir                              string
	MicroVMKernelArgs                          string
	MicroVMGuestControlVsockPort               uint32
	MicroVMGuestProtocolVsockPort              uint32
	MicroVMGuestHeartbeatInterval              time.Duration
	MicroVMMemoryMiB                           int
	MicroVMVCPUs                               int
	MicroVMCPUTemplate                         string
	MicroVMWorkspaceSizeMiB                    int
	MicroVMStoragePressureRecoveryPercent      int
	MicroVMStoragePressureWarningPercent       int
	MicroVMStoragePressureAdmissionDenyPercent int
	MicroVMStorageAdmissionMode                string
	MicroVMAllowUnjailed                       bool
	// MicroVMSnapshotTemplateCacheRoot is the operator-owned runner-local root
	// for immutable snapshot-resume templates. It is required, so an operator
	// always states where resume templates live; the runner advertises resume
	// capacity only when the cache under it already holds a compatible template.
	MicroVMSnapshotTemplateCacheRoot     string
	MicroVMGuestIP                       string
	MicroVMBridgeName                    string
	MicroVMBridgeCIDR                    string
	MicroVMTapPrefix                     string
	MicroVMMaxConcurrentPerSandbox       int
	MicroVMMaxConcurrentGlobal           int
	MicroVMMaxConcurrentOperationsGlobal int
	MicroVMMemoryBudgetMiB               int
	FileTransferMaxBytes                 int64
	NetworkPolicyNFTPath                 string
	NetworkPolicyMaximumDNSPins          int
	NetworkPolicyMaximumDNSTTL           time.Duration
	NetworkPolicyRunnerAddresses         []netip.Addr
	NetworkPolicyManagementCIDRs         []netip.Prefix
	NetworkPolicyEgressContexts          networkpolicy.EgressContextConfig
	NetworkPolicyDNSUpstream             netip.AddrPort
	ExecutionImageCacheRoot              string
	ExecutionImageFetcherSocket          string
	ExecutionImageRegistryAllowlist      []string
	ExecutionImageRegistryCertificates   string
	ExecutionImageMaximumDownloadBytes   int64
	ExecutionImageMaximumExpandedBytes   int64
	ExecutionImageMaximumCacheBytes      int64
}

// VerifyMicroVMArtifactDirectory verifies that one bundle directory matches its own manifest and checksums.
func VerifyMicroVMArtifactDirectory(ctx context.Context, directory string) error {
	verification := &Config{
		MicroVMKernelPath:          filepath.Join(directory, "kernel"),
		MicroVMRootfsPath:          filepath.Join(directory, "rootfs.ext4"),
		MicroVMToolRootfsPath:      filepath.Join(directory, "rootfs.ext4"),
		MicroVMSharedImagePath:     filepath.Join(directory, "shared.img"),
		MicroVMToolSharedImagePath: filepath.Join(directory, "shared.img"),
		MicroVMBundleVerification:  true,
	}
	return verification.ValidateMicroVMBundle(ctx)
}

// ValidateMicroVMBundle proves the configured kernel, rootfs and shared image
// are the files the bundle manifest and checksums name. Bundle integrity rests
// on the digest-pinned OCI image that delivered the directory.
func (c *Config) ValidateMicroVMBundle(ctx context.Context) error {
	if c == nil || !c.MicroVMBundleVerification {
		return nil
	}
	return verifyArtifactSet(ctx, c)
}

// BundleArtifactFileNames lists every file a bundle verification reads.
var BundleArtifactFileNames = []string{
	"kernel",
	"rootfs.ext4",
	"shared.img",
	"kernel-provenance.json",
	"rootfs-source-manifest.json",
	"secondbox-rootfs-contract.json",
	"rootfs-debian-packages.lock",
	"rootfs-python.freeze",
	"rootfs-debian-license-inventory.json",
	"rootfs-python-license-inventory.json",
	"runtime-manifest.json",
	"toolchain-manifest.json",
	"manifest.json",
	"SHA256SUMS",
}

// artifactDigestCache hashes each file once per verification pass. The checksum
// list and the manifest cover the same multi-gigabyte kernel, rootfs and
// shared image.
type artifactDigestCache struct {
	digests map[string]string
}

func newArtifactDigestCache() *artifactDigestCache {
	return &artifactDigestCache{digests: map[string]string{}}
}

func (cache *artifactDigestCache) hex(ctx context.Context, path string) (string, error) {
	if digest, recorded := cache.digests[path]; recorded {
		return digest, nil
	}
	digest, err := fileSHA256Hex(ctx, path)
	if err != nil {
		return "", err
	}
	cache.digests[path] = digest
	return digest, nil
}

func verifyArtifactSet(ctx context.Context, cfg *Config) error {
	if cfg.MicroVMToolRootfsPath != "" && cfg.MicroVMToolRootfsPath != cfg.MicroVMRootfsPath {
		return fmt.Errorf("SecondBox Runner tool rootfs must match SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH for bundle verification")
	}
	if cfg.MicroVMToolSharedImagePath != "" && cfg.MicroVMToolSharedImagePath != cfg.MicroVMSharedImagePath {
		return fmt.Errorf("SecondBox Runner tool shared image must match SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH for bundle verification")
	}
	if cfg.MicroVMSharedImagePath == "" {
		return fmt.Errorf("SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH is required for bundle verification")
	}
	artifactDir := filepath.Dir(cfg.MicroVMKernelPath)
	if filepath.Dir(cfg.MicroVMRootfsPath) != artifactDir || filepath.Dir(cfg.MicroVMSharedImagePath) != artifactDir {
		return fmt.Errorf("SecondBox Runner kernel, rootfs, and shared image must be in the same bundle directory")
	}
	for path, name := range map[string]string{
		cfg.MicroVMKernelPath:      "kernel",
		cfg.MicroVMRootfsPath:      "rootfs.ext4",
		cfg.MicroVMSharedImagePath: "shared.img",
	} {
		if filepath.Base(path) != name {
			return fmt.Errorf("SecondBox Runner artifact path %s must name %s", path, name)
		}
	}
	for _, name := range BundleArtifactFileNames {
		if _, err := os.Stat(filepath.Join(artifactDir, name)); err != nil {
			return fmt.Errorf("SecondBox Runner bundle artifact %s: %w", name, err)
		}
	}
	digests := newArtifactDigestCache()
	if err := verifyChecksums(ctx, artifactDir, digests); err != nil {
		return err
	}
	manifest, err := ReadArtifactMetadata(filepath.Join(artifactDir, "manifest.json"), MaximumArtifactManifestBytes)
	if err != nil {
		return fmt.Errorf("read SecondBox Runner manifest: %w", err)
	}
	if err := verifyManifestArtifacts(ctx, artifactDir, manifest, digests); err != nil {
		return err
	}
	return verifySecondBoxRootfsContract(artifactDir)
}

type artifactManifest struct {
	Kernel           artifactManifestEntry          `json:"kernel"`
	Rootfs           artifactManifestEntry          `json:"rootfs"`
	Shared           artifactManifestEntry          `json:"shared"`
	KernelProvenance artifactManifestEntry          `json:"kernelProvenance"`
	RootfsSource     artifactManifestEntry          `json:"rootfsSource"`
	RootfsContract   artifactManifestEntry          `json:"rootfsContract"`
	RuntimeBundle    artifactComponentManifestEntry `json:"runtimeBundle"`
	ToolchainBundle  artifactComponentManifestEntry `json:"toolchainBundle"`
}

type artifactComponentManifestEntry struct {
	Path           string `json:"path"`
	ManifestDigest string `json:"manifestDigest"`
}

type artifactManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func verifyManifestArtifacts(ctx context.Context, artifactDir string, manifestData []byte, digests *artifactDigestCache) error {
	var manifest artifactManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse SecondBox Runner manifest: %w", err)
	}
	for label, named := range map[string]struct {
		entry artifactManifestEntry
		path  string
	}{
		"kernel":                 {manifest.Kernel, "kernel"},
		"rootfs":                 {manifest.Rootfs, "rootfs.ext4"},
		"shared":                 {manifest.Shared, "shared.img"},
		"kernel provenance":      {manifest.KernelProvenance, "kernel-provenance.json"},
		"rootfs source manifest": {manifest.RootfsSource, "rootfs-source-manifest.json"},
		"rootfs contract":        {manifest.RootfsContract, "secondbox-rootfs-contract.json"},
	} {
		if !safeManifestPath(named.entry.Path) || named.entry.SHA256 == "" {
			return fmt.Errorf("SecondBox Runner manifest missing %s path or sha256", label)
		}
		if named.entry.Path != named.path {
			return fmt.Errorf("SecondBox Runner manifest %s path must be %s, got %s", label, named.path, named.entry.Path)
		}
		actual, err := digests.hex(ctx, filepath.Join(artifactDir, named.entry.Path))
		if err != nil {
			return err
		}
		if actual != named.entry.SHA256 {
			return fmt.Errorf("SecondBox Runner manifest hash mismatch for %s: expected %s, got %s", named.entry.Path, named.entry.SHA256, actual)
		}
	}
	for label, component := range map[string]struct {
		entry artifactComponentManifestEntry
		path  string
	}{
		"runtime component":   {manifest.RuntimeBundle, "runtime-manifest.json"},
		"toolchain component": {manifest.ToolchainBundle, "toolchain-manifest.json"},
	} {
		if component.entry.Path != component.path ||
			!strings.HasPrefix(component.entry.ManifestDigest, "sha256:") {
			return fmt.Errorf("SecondBox Runner manifest missing %s path or digest", label)
		}
		actual, err := digests.hex(ctx, filepath.Join(artifactDir, component.path))
		if err != nil {
			return err
		}
		if "sha256:"+actual != component.entry.ManifestDigest {
			return fmt.Errorf("SecondBox Runner manifest %s digest mismatch", label)
		}
	}
	return nil
}

func verifySecondBoxRootfsContract(artifactDir string) error {
	data, err := ReadArtifactMetadata(filepath.Join(artifactDir, "secondbox-rootfs-contract.json"), 64<<10)
	if err != nil {
		return fmt.Errorf("read SecondBox rootfs contract: %w", err)
	}
	var toolset struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &toolset); err != nil {
		return fmt.Errorf("parse SecondBox rootfs contract: %w", err)
	}
	if toolset.State != "verified" {
		return fmt.Errorf("SecondBox rootfs contract state must be verified")
	}
	return nil
}

func safeManifestPath(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, ".."+string(os.PathSeparator)) && path != ".."
}

func verifyChecksums(ctx context.Context, artifactDir string, digests *artifactDigestCache) error {
	data, err := ReadArtifactMetadata(filepath.Join(artifactDir, "SHA256SUMS"), 64<<10)
	if err != nil {
		return fmt.Errorf("read SecondBox Runner checksums: %w", err)
	}
	want := map[string]string{}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 128 {
		return fmt.Errorf("SecondBox artifact checksums exceed 128 entries")
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			want[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	for _, name := range []string{
		"kernel",
		"rootfs.ext4",
		"shared.img",
		"kernel-provenance.json",
		"rootfs-source-manifest.json",
		"secondbox-rootfs-contract.json",
		"rootfs-debian-packages.lock",
		"rootfs-python.freeze",
		"rootfs-debian-license-inventory.json",
		"rootfs-python-license-inventory.json",
	} {
		expected := want[name]
		if expected == "" {
			return fmt.Errorf("SHA256SUMS missing %s", name)
		}
		actual, err := digests.hex(ctx, filepath.Join(artifactDir, name))
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("SecondBox Runner artifact checksum mismatch for %s: expected %s, got %s", name, expected, actual)
		}
	}
	return nil
}

func fileSHA256Hex(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, reader: file}); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
