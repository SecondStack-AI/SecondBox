package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var checksumArtifactNames = []string{
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
}

func TestValidateMicroVMBundleVerifiesContent(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg, _ := bundleArtifactFixture(t)
		if err := cfg.ValidateMicroVMBundle(t.Context()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		cfg := &Config{MicroVMKernelPath: filepath.Join(t.TempDir(), "absent", "kernel")}
		if err := cfg.ValidateMicroVMBundle(t.Context()); err != nil {
			t.Fatalf("disabled bundle verification error = %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		cfg, _ := bundleArtifactFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := cfg.ValidateMicroVMBundle(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled verification error = %v", err)
		}
	})

	t.Run("missing manifest", func(t *testing.T) {
		cfg, artifactDir := bundleArtifactFixture(t)
		if err := os.Remove(filepath.Join(artifactDir, "manifest.json")); err != nil {
			t.Fatal(err)
		}
		if err := cfg.ValidateMicroVMBundle(t.Context()); err == nil || !strings.Contains(err.Error(), "SecondBox Runner bundle artifact manifest.json") {
			t.Fatalf("missing-manifest verification error = %v", err)
		}
	})

	t.Run("manifest hash mismatch", func(t *testing.T) {
		cfg, artifactDir := bundleArtifactFixture(t)
		manifestPath := filepath.Join(artifactDir, "manifest.json")
		manifest, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var decoded artifactManifest
		if err := json.Unmarshal(manifest, &decoded); err != nil {
			t.Fatal(err)
		}
		decoded.Kernel.SHA256 = strings.Repeat("0", sha256.Size*2)
		manifest, err = json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cfg.ValidateMicroVMBundle(t.Context()); err == nil || !strings.Contains(err.Error(), "manifest hash mismatch for kernel") {
			t.Fatalf("manifest-hash-mismatch verification error = %v", err)
		}
	})

	t.Run("tampered component manifest", func(t *testing.T) {
		cfg, artifactDir := bundleArtifactFixture(t)
		if err := os.WriteFile(filepath.Join(artifactDir, "runtime-manifest.json"), []byte(`{"component":"other"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cfg.ValidateMicroVMBundle(t.Context()); err == nil || !strings.Contains(err.Error(), "runtime component digest mismatch") {
			t.Fatalf("tampered-component verification error = %v", err)
		}
	})
}

func TestValidateMicroVMBundleNamesRealEnvironmentVariables(t *testing.T) {
	t.Run("separate tool rootfs", func(t *testing.T) {
		cfg, _ := bundleArtifactFixture(t)
		cfg.MicroVMToolRootfsPath = "/images/other-rootfs.ext4"
		if err := cfg.ValidateMicroVMBundle(t.Context()); err == nil ||
			!strings.Contains(err.Error(), "must match SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH for bundle verification") {
			t.Fatalf("separate-tool-rootfs error = %v", err)
		}
	})

	t.Run("separate tool shared image", func(t *testing.T) {
		cfg, _ := bundleArtifactFixture(t)
		cfg.MicroVMToolSharedImagePath = "/images/other-shared.img"
		if err := cfg.ValidateMicroVMBundle(t.Context()); err == nil ||
			!strings.Contains(err.Error(), "must match SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH for bundle verification") {
			t.Fatalf("separate-tool-shared-image error = %v", err)
		}
	})

	t.Run("missing shared image", func(t *testing.T) {
		cfg, _ := bundleArtifactFixture(t)
		cfg.MicroVMSharedImagePath = ""
		if err := cfg.ValidateMicroVMBundle(t.Context()); err == nil ||
			!strings.Contains(err.Error(), "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH is required for bundle verification") {
			t.Fatalf("missing-shared-image error = %v", err)
		}
	})
}

func TestVerifyChecksumsWalksRequiredArtifacts(t *testing.T) {
	t.Run("bounded metadata", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("a", 65537)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyChecksums(t.Context(), dir, newArtifactDigestCache()); err == nil || !strings.Contains(err.Error(), "at most") {
			t.Fatalf("unbounded checksum metadata: %v", err)
		}
	})
	t.Run("bounded entries", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("a b\n", 129)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyChecksums(t.Context(), dir, newArtifactDigestCache()); err == nil || !strings.Contains(err.Error(), "128 entries") {
			t.Fatalf("unbounded checksum entries: %v", err)
		}
	})
	t.Run("good", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := verifyChecksums(t.Context(), dir, newArtifactDigestCache()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := os.Remove(filepath.Join(dir, "rootfs.ext4")); err != nil {
			t.Fatal(err)
		}
		if err := verifyChecksums(t.Context(), dir, newArtifactDigestCache()); err == nil || !strings.Contains(err.Error(), "rootfs.ext4") {
			t.Fatalf("missing-file checksum error = %v", err)
		}
	})

	t.Run("altered content", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := os.WriteFile(filepath.Join(dir, "kernel"), []byte("altered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyChecksums(t.Context(), dir, newArtifactDigestCache()); err == nil || !strings.Contains(err.Error(), "checksum mismatch for kernel") {
			t.Fatalf("altered-content checksum error = %v", err)
		}
	})
}

func TestSafeManifestPathRejectsTraversal(t *testing.T) {
	for _, path := range []string{"", ".", "..", "../kernel", "assets/../../kernel", "/kernel", "assets/../kernel"} {
		if safeManifestPath(path) {
			t.Errorf("safeManifestPath(%q) = true", path)
		}
	}
	for _, path := range []string{"kernel", "assets/kernel"} {
		if !safeManifestPath(path) {
			t.Errorf("safeManifestPath(%q) = false", path)
		}
	}
}

func bundleArtifactFixture(t *testing.T) (*Config, string) {
	t.Helper()
	dir := checksumFixture(t)
	for name, data := range map[string][]byte{
		"runtime-manifest.json":   []byte(`{"component":"runtime"}`),
		"toolchain-manifest.json": []byte(`{"component":"toolchain"}`),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) artifactManifestEntry {
		digest, err := fileSHA256Hex(t.Context(), filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return artifactManifestEntry{Path: name, SHA256: digest}
	}
	component := func(name string) artifactComponentManifestEntry {
		digest, err := fileSHA256Hex(t.Context(), filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return artifactComponentManifestEntry{Path: name, ManifestDigest: "sha256:" + digest}
	}
	manifest, err := json.Marshal(artifactManifest{
		Kernel:           entry("kernel"),
		Rootfs:           entry("rootfs.ext4"),
		Shared:           entry("shared.img"),
		KernelProvenance: entry("kernel-provenance.json"),
		RootfsSource:     entry("rootfs-source-manifest.json"),
		RootfsContract:   entry("secondbox-rootfs-contract.json"),
		RuntimeBundle:    component("runtime-manifest.json"),
		ToolchainBundle:  component("toolchain-manifest.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		MicroVMKernelPath:         filepath.Join(dir, "kernel"),
		MicroVMRootfsPath:         filepath.Join(dir, "rootfs.ext4"),
		MicroVMSharedImagePath:    filepath.Join(dir, "shared.img"),
		MicroVMBundleVerification: true,
	}
	return cfg, dir
}

func checksumFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var sums strings.Builder
	for _, name := range checksumArtifactNames {
		data := []byte("fixture:" + name)
		if name == "secondbox-rootfs-contract.json" {
			data = []byte(`{"state":"verified"}`)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		sums.WriteString(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// One verification pass hashes kernel, rootfs and shared image once, although
// both the checksum list and the bundle manifest cover them.
func TestArtifactDigestCacheHashesEachFileOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(path, []byte("verified bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := newArtifactDigestCache()
	first, err := cache.hex(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("substituted!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := cache.hex(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(cache.digests) != 1 {
		t.Fatalf("digest cache re-read the file: %s then %s across %d entries", first, second, len(cache.digests))
	}
}
