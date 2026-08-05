package config

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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

func TestValidateMicroVMTrustAnchorVerifiesRSASignature(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg, _ := signedArtifactFixture(t)
		if err := cfg.ValidateMicroVMTrustAnchor(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		cfg, _ := signedArtifactFixture(t)
		wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		writePublicKeyFixture(t, cfg, &wrongKey.PublicKey)
		if err := cfg.ValidateMicroVMTrustAnchor(); err == nil || !strings.Contains(err.Error(), "verify SecondBox Runner manifest signature") {
			t.Fatalf("wrong-key verification error = %v", err)
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		cfg, artifactDir := signedArtifactFixture(t)
		manifestPath := filepath.Join(artifactDir, "manifest.json")
		manifest, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		manifest = append(manifest, '\n')
		if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cfg.ValidateMicroVMTrustAnchor(); err == nil || !strings.Contains(err.Error(), "verify SecondBox Runner manifest signature") {
			t.Fatalf("tampered-payload verification error = %v", err)
		}
	})
}

func TestVerifyChecksumsWalksRequiredArtifacts(t *testing.T) {
	t.Run("good", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := verifyChecksums(dir); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := os.Remove(filepath.Join(dir, "rootfs.ext4")); err != nil {
			t.Fatal(err)
		}
		if err := verifyChecksums(dir); err == nil || !strings.Contains(err.Error(), "rootfs.ext4") {
			t.Fatalf("missing-file checksum error = %v", err)
		}
	})

	t.Run("altered content", func(t *testing.T) {
		dir := checksumFixture(t)
		if err := os.WriteFile(filepath.Join(dir, "kernel"), []byte("altered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyChecksums(dir); err == nil || !strings.Contains(err.Error(), "checksum mismatch for kernel") {
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

func signedArtifactFixture(t *testing.T) (*Config, string) {
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
		digest, err := fileSHA256Hex(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return artifactManifestEntry{Path: name, SHA256: digest}
	}
	component := func(name string) artifactComponentManifestEntry {
		digest, err := fileSHA256Hex(filepath.Join(dir, name))
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
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.sig"), signature, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		MicroVMKernelPath:      filepath.Join(dir, "kernel"),
		MicroVMRootfsPath:      filepath.Join(dir, "rootfs.ext4"),
		MicroVMSharedImagePath: filepath.Join(dir, "shared.img"),
		MicroVMPublicKeyPath:   filepath.Join(t.TempDir(), "manifest-public.pem"),
	}
	writePublicKeyFixture(t, cfg, &privateKey.PublicKey)
	return cfg, dir
}

func writePublicKeyFixture(t *testing.T, cfg *Config, key *rsa.PublicKey) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := os.WriteFile(cfg.MicroVMPublicKeyPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(der)
	cfg.MicroVMPublicKeySHA256 = hex.EncodeToString(fingerprint[:])
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
