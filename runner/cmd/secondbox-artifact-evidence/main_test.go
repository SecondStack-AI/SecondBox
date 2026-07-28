package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectWritesOnlyArtifactDigests(t *testing.T) {
	root := t.TempDir()
	artifacts := filepath.Join(root, "artifacts")
	if err := os.Mkdir(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"kernel":                               "kernel bytes",
		"rootfs.ext4":                          "rootfs bytes",
		"shared.img":                           "shared bytes",
		"kernel-provenance.json":               "kernel provenance",
		"rootfs-source-manifest.json":          "rootfs source",
		"secondbox-rootfs-contract.json":       "rootfs contract",
		"rootfs-debian-packages.lock":          "debian packages",
		"rootfs-python.freeze":                 "python packages",
		"rootfs-debian-license-inventory.json": "debian licenses",
		"rootfs-python-license-inventory.json": "python licenses",
		"manifest.json":                        `{"schemaVersion":1,"secret":"must not be copied"}`,
		"manifest.sig":                         "signature bytes",
		"SHA256SUMS":                           "checksums",
		"signing.pub":                          "public key bytes",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(artifacts, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(root, "evidence", "firecracker-artifacts.json")
	if err := collect(artifacts, output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"kernel bytes", "rootfs bytes", "must not be copied"} {
		if strings.Contains(string(content), forbidden) {
			t.Fatalf("evidence copied raw artifact content %q", forbidden)
		}
	}
	var document evidenceDocument
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	kernelDigest := sha256.Sum256([]byte(files["kernel"]))
	if got := document.Artifacts["kernel"]; got.SHA256 != hex.EncodeToString(kernelDigest[:]) || got.SizeBytes != int64(len(files["kernel"])) {
		t.Fatalf("kernel evidence = %+v", got)
	}
	if len(document.Artifacts) != len(files) {
		t.Fatalf("artifact evidence count = %d, want %d", len(document.Artifacts), len(files))
	}
	if info, err := os.Stat(output); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode: info=%v err=%v", info, err)
	}
}

func TestCollectRejectsSymlinkArtifactAndExistingOutput(t *testing.T) {
	root := t.TempDir()
	artifacts := filepath.Join(root, "artifacts")
	if err := os.Mkdir(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rootfs.ext4", "shared.img", "manifest.json", "manifest.sig", "signing.pub"} {
		if err := os.WriteFile(filepath.Join(artifacts, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "kernel")
	if err := os.WriteFile(target, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(artifacts, "kernel")); err != nil {
		t.Fatal(err)
	}
	if err := collect(artifacts, filepath.Join(root, "out.json")); err == nil {
		t.Fatal("expected symlink artifact rejection")
	}
	output := filepath.Join(root, "existing.json")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collect(artifacts, output); err == nil {
		t.Fatal("expected existing output rejection")
	}
}
