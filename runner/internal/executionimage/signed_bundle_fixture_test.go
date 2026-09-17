package executionimage

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
	"testing"
)

// writeSignedBundleFixture publishes a bundle that passes the real signature
// verification, so a test can exercise cache admission instead of stubbing it.
func writeSignedBundleFixture(t *testing.T, directory string) (publicKeyPath string, publicKeySHA256 string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	checksums := ""
	for _, name := range []string{
		"kernel", "rootfs.ext4", "shared.img", "kernel-provenance.json",
		"rootfs-source-manifest.json", "secondbox-rootfs-contract.json",
		"rootfs-debian-packages.lock", "rootfs-python.freeze",
		"rootfs-debian-license-inventory.json", "rootfs-python-license-inventory.json",
		"runtime-manifest.json", "toolchain-manifest.json",
	} {
		content := []byte("fixture:" + name)
		if name == "secondbox-rootfs-contract.json" {
			content = []byte(`{"state":"verified"}`)
		}
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		digests[name] = hex.EncodeToString(digest[:])
		switch name {
		case "runtime-manifest.json", "toolchain-manifest.json":
		default:
			checksums += digests[name] + "  " + name + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(checksums), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"kernel":           map[string]string{"path": "kernel", "sha256": digests["kernel"]},
		"rootfs":           map[string]string{"path": "rootfs.ext4", "sha256": digests["rootfs.ext4"]},
		"shared":           map[string]string{"path": "shared.img", "sha256": digests["shared.img"]},
		"kernelProvenance": map[string]string{"path": "kernel-provenance.json", "sha256": digests["kernel-provenance.json"]},
		"rootfsSource":     map[string]string{"path": "rootfs-source-manifest.json", "sha256": digests["rootfs-source-manifest.json"]},
		"rootfsContract":   map[string]string{"path": "secondbox-rootfs-contract.json", "sha256": digests["secondbox-rootfs-contract.json"]},
		"runtimeBundle":    map[string]string{"path": "runtime-manifest.json", "manifestDigest": "sha256:" + digests["runtime-manifest.json"]},
		"toolchainBundle":  map[string]string{"path": "toolchain-manifest.json", "manifestDigest": "sha256:" + digests["toolchain-manifest.json"]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifest)
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, manifestDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.sig"), signature, 0o600); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyPath = filepath.Join(t.TempDir(), "execution-image-public.pem")
	if err := os.WriteFile(publicKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(der)
	return publicKeyPath, hex.EncodeToString(fingerprint[:])
}
