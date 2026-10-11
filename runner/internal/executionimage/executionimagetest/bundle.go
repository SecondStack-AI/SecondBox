// Package executionimagetest writes execution bundles that pass the real Runner
// verification, so tests exercise admission instead of stubbing it.
package executionimagetest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Bundle is the identity one WriteBundle call produced.
type Bundle struct {
	RuntimeArtifactID       string
	RuntimeManifestDigest   string
	ToolchainArtifactID     string
	ToolchainManifestDigest string
	Manifest                []byte
}

// WriteBundle completes a bundle in directory. A file already present, such as
// a real rootfs.ext4, is recorded as it is; every absent file receives fixture
// content.
func WriteBundle(t testing.TB, directory string, mandatoryGuestFeatures []string) Bundle {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if mandatoryGuestFeatures == nil {
		mandatoryGuestFeatures = []string{}
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
		path := filepath.Join(directory, name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			content := []byte("fixture:" + name)
			if name == "secondbox-rootfs-contract.json" {
				content = []byte(`{"state":"verified"}`)
			}
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		digests[name] = fileSHA256(t, path)
		switch name {
		case "runtime-manifest.json", "toolchain-manifest.json":
		default:
			checksums += digests[name] + "  " + name + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(checksums), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		RuntimeArtifactID: "secondbox-test-runtime", RuntimeManifestDigest: "sha256:" + digests["runtime-manifest.json"],
		ToolchainArtifactID: "secondbox-test-toolchain", ToolchainManifestDigest: "sha256:" + digests["toolchain-manifest.json"],
	}
	manifest, err := json.Marshal(map[string]any{
		"artifactVersion":  "secondbox-test-bundle",
		"architecture":     runtime.GOARCH,
		"guestProtocol":    map[string]uint32{"minimum": 1, "maximum": 1},
		"kernel":           map[string]string{"path": "kernel", "sha256": digests["kernel"]},
		"rootfs":           map[string]string{"path": "rootfs.ext4", "sha256": digests["rootfs.ext4"]},
		"shared":           map[string]string{"path": "shared.img", "sha256": digests["shared.img"]},
		"kernelProvenance": map[string]string{"path": "kernel-provenance.json", "sha256": digests["kernel-provenance.json"]},
		"rootfsSource":     map[string]string{"path": "rootfs-source-manifest.json", "sha256": digests["rootfs-source-manifest.json"]},
		"rootfsContract":   map[string]string{"path": "secondbox-rootfs-contract.json", "sha256": digests["secondbox-rootfs-contract.json"]},
		"runtimeBundle": map[string]any{
			"artifactId": bundle.RuntimeArtifactID, "path": "runtime-manifest.json",
			"manifestDigest": bundle.RuntimeManifestDigest, "mandatoryGuestFeatures": mandatoryGuestFeatures,
		},
		"toolchainBundle": map[string]any{
			"artifactId": bundle.ToolchainArtifactID, "path": "toolchain-manifest.json",
			"manifestDigest": bundle.ToolchainManifestDigest, "mandatoryGuestFeatures": mandatoryGuestFeatures,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle.Manifest = manifest
	return bundle
}

func fileSHA256(t testing.TB, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(digest.Sum(nil))
}
