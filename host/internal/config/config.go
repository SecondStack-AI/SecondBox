// Package config contains configuration owned by the privileged Sandbox Host.
package config

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	DataDir                      string
	PlatformAPIURL               string
	FirecrackerPath              string
	MicroVMLauncherSocket        string
	JailerPath                   string
	MicroVMJailerChrootBaseDir   string
	MicroVMJailerUID             int
	MicroVMJailerGID             int
	MicroVMJailerCgroupVersion   int
	MicroVMJailerParentCgroup    string
	MicroVMKernelPath            string
	MicroVMRootfsPath            string
	MicroVMToolRootfsPath        string
	MicroVMSharedImagePath       string
	MicroVMToolSharedImagePath   string
	MicroVMPublicKeyPath         string
	MicroVMPublicKeySHA256       string
	MicroVMWorkspaceDir          string
	MicroVMRunDir                string
	MicroVMLogDir                string
	MicroVMKernelArgs            string
	MicroVMMemoryMiB             int
	MicroVMVCPUs                 int
	MicroVMCPUTemplate           string
	MicroVMWorkspaceSizeMiB      int
	MicroVMWorkspaceBackend      string
	MicroVMThinPoolDevice        string
	MicroVMAllowUnjailed         bool
	MicroVMGuestIP               string
	MicroVMBridgeName            string
	MicroVMBridgeCIDR            string
	MicroVMTapPrefix             string
	MicroVMMaxConcurrentPerAgent int
	MicroVMMaxConcurrentGlobal   int
	MicroVMMemoryBudgetMiB       int
	MicroVMToolVMReuseEnabled    bool
	MicroVMToolVMIdleTTL         time.Duration
	FileTransferMaxBytes         int64
}

func (c *Config) ToolVMReuseEffective() bool {
	return c != nil && c.MicroVMToolVMReuseEnabled && strings.TrimSpace(c.MicroVMBridgeCIDR) != ""
}

func (c *Config) AgentsDir() string {
	return filepath.Join(c.DataDir, "agents")
}

func (c *Config) ValidateMicroVMTrustAnchor() error {
	if c == nil {
		return nil
	}
	if c.MicroVMPublicKeySHA256 != "" {
		if _, err := hex.DecodeString(c.MicroVMPublicKeySHA256); err != nil || len(c.MicroVMPublicKeySHA256) != sha256.Size*2 {
			return fmt.Errorf("SANDBOX_HOST_PUBLIC_KEY_SHA256 must be 64 lowercase hex characters")
		}
	}
	if c.MicroVMPublicKeySHA256 != "" && c.MicroVMPublicKeyPath == "" {
		return fmt.Errorf("SANDBOX_HOST_PUBLIC_KEY_SHA256 requires SANDBOX_HOST_PUBLIC_KEY")
	}
	if c.MicroVMPublicKeyPath == "" {
		return nil
	}
	publicKey, publicKeyDER, err := readPublicKey(c.MicroVMPublicKeyPath)
	if err != nil {
		return fmt.Errorf("SANDBOX_HOST_PUBLIC_KEY %q: %w", c.MicroVMPublicKeyPath, err)
	}
	actualFingerprint := sha256.Sum256(publicKeyDER)
	actualFingerprintHex := hex.EncodeToString(actualFingerprint[:])
	if c.MicroVMPublicKeySHA256 != "" && actualFingerprintHex != c.MicroVMPublicKeySHA256 {
		return fmt.Errorf("SANDBOX_HOST_PUBLIC_KEY_SHA256 mismatch: expected %s, got %s", c.MicroVMPublicKeySHA256, actualFingerprintHex)
	}
	return verifyArtifactSet(c, publicKey)
}

func readPublicKey(path string) (*rsa.PublicKey, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	der := data
	if block, _ := pem.Decode(data); block != nil {
		der = block.Bytes
	}
	publicKey, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse public key: %w", err)
	}
	rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("public key must be RSA")
	}
	return rsaPublicKey, der, nil
}

func verifyArtifactSet(cfg *Config, publicKey *rsa.PublicKey) error {
	if cfg.MicroVMToolRootfsPath != "" && cfg.MicroVMToolRootfsPath != cfg.MicroVMRootfsPath {
		return fmt.Errorf("SANDBOX_HOST_TOOL_ROOTFS_PATH cannot be a separate image when SANDBOX_HOST_PUBLIC_KEY is set")
	}
	if cfg.MicroVMToolSharedImagePath != "" && cfg.MicroVMToolSharedImagePath != cfg.MicroVMSharedImagePath {
		return fmt.Errorf("SANDBOX_HOST_TOOL_SHARED_IMAGE_PATH cannot be a separate image when SANDBOX_HOST_PUBLIC_KEY is set")
	}
	if cfg.MicroVMSharedImagePath == "" {
		return fmt.Errorf("SANDBOX_HOST_SHARED_IMAGE_PATH is required when SANDBOX_HOST_PUBLIC_KEY is set")
	}
	artifactDir := filepath.Dir(cfg.MicroVMKernelPath)
	if filepath.Dir(cfg.MicroVMRootfsPath) != artifactDir || filepath.Dir(cfg.MicroVMSharedImagePath) != artifactDir {
		return fmt.Errorf("Sandbox Host kernel, rootfs, and shared image must be in the same signed artifact directory")
	}
	for path, name := range map[string]string{
		cfg.MicroVMKernelPath:      "kernel",
		cfg.MicroVMRootfsPath:      "rootfs.ext4",
		cfg.MicroVMSharedImagePath: "shared.img",
	} {
		if filepath.Base(path) != name {
			return fmt.Errorf("Sandbox Host artifact path %s must name %s", path, name)
		}
	}
	required := []string{"kernel", "rootfs.ext4", "shared.img", "kernel-provenance.json", "rootfs-source-manifest.json", "standard-toolset.json", "manifest.json", "SHA256SUMS", "manifest.sig"}
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(artifactDir, name)); err != nil {
			return fmt.Errorf("signed Sandbox Host artifact %s: %w", name, err)
		}
	}
	if err := verifyChecksums(artifactDir); err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(artifactDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("read Sandbox Host manifest: %w", err)
	}
	signature, err := os.ReadFile(filepath.Join(artifactDir, "manifest.sig"))
	if err != nil {
		return fmt.Errorf("read Sandbox Host manifest signature: %w", err)
	}
	digest := sha256.Sum256(manifest)
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return fmt.Errorf("verify Sandbox Host manifest signature: %w", err)
	}
	if err := verifySignedManifestArtifacts(artifactDir, manifest); err != nil {
		return err
	}
	return verifyStandardToolset(artifactDir)
}

type artifactManifest struct {
	Kernel           artifactManifestEntry `json:"kernel"`
	Rootfs           artifactManifestEntry `json:"rootfs"`
	Shared           artifactManifestEntry `json:"shared"`
	KernelProvenance artifactManifestEntry `json:"kernelProvenance"`
	RootfsSource     artifactManifestEntry `json:"rootfsSource"`
	StandardToolset  artifactManifestEntry `json:"standardToolset"`
}

type artifactManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func verifySignedManifestArtifacts(artifactDir string, manifestData []byte) error {
	var manifest artifactManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse Sandbox Host manifest: %w", err)
	}
	for label, signed := range map[string]struct {
		entry artifactManifestEntry
		path  string
	}{
		"kernel":                 {manifest.Kernel, "kernel"},
		"rootfs":                 {manifest.Rootfs, "rootfs.ext4"},
		"shared":                 {manifest.Shared, "shared.img"},
		"kernel provenance":      {manifest.KernelProvenance, "kernel-provenance.json"},
		"rootfs source manifest": {manifest.RootfsSource, "rootfs-source-manifest.json"},
		"standard toolset":       {manifest.StandardToolset, "standard-toolset.json"},
	} {
		if !safeManifestPath(signed.entry.Path) || signed.entry.SHA256 == "" {
			return fmt.Errorf("Sandbox Host manifest missing %s path or sha256", label)
		}
		if signed.entry.Path != signed.path {
			return fmt.Errorf("Sandbox Host manifest %s path must be %s, got %s", label, signed.path, signed.entry.Path)
		}
		actual, err := fileSHA256Hex(filepath.Join(artifactDir, signed.entry.Path))
		if err != nil {
			return err
		}
		if actual != signed.entry.SHA256 {
			return fmt.Errorf("signed Sandbox Host manifest hash mismatch for %s: expected %s, got %s", signed.entry.Path, signed.entry.SHA256, actual)
		}
	}
	return nil
}

func verifyStandardToolset(artifactDir string) error {
	data, err := os.ReadFile(filepath.Join(artifactDir, "standard-toolset.json"))
	if err != nil {
		return fmt.Errorf("read Sandbox Host standard toolset: %w", err)
	}
	var toolset struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &toolset); err != nil {
		return fmt.Errorf("parse Sandbox Host standard toolset: %w", err)
	}
	if toolset.State != "verified" {
		return fmt.Errorf("Sandbox Host standard toolset state must be verified")
	}
	return nil
}

func safeManifestPath(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, ".."+string(os.PathSeparator)) && path != ".."
}

func verifyChecksums(artifactDir string) error {
	data, err := os.ReadFile(filepath.Join(artifactDir, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("read Sandbox Host checksums: %w", err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			want[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	for _, name := range []string{"kernel", "rootfs.ext4", "shared.img", "kernel-provenance.json", "rootfs-source-manifest.json", "standard-toolset.json"} {
		expected := want[name]
		if expected == "" {
			return fmt.Errorf("SHA256SUMS missing %s", name)
		}
		actual, err := fileSHA256Hex(filepath.Join(artifactDir, name))
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("Sandbox Host artifact checksum mismatch for %s: expected %s, got %s", name, expected, actual)
		}
	}
	return nil
}

func fileSHA256Hex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
