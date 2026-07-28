package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type artifactEvidence struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

type evidenceDocument struct {
	SchemaVersion int                         `json:"schemaVersion"`
	Artifacts     map[string]artifactEvidence `json:"artifacts"`
}

var artifactEvidenceFiles = map[string]string{
	"kernel":                 "kernel",
	"rootfs":                 "rootfs.ext4",
	"shared":                 "shared.img",
	"kernelProvenance":       "kernel-provenance.json",
	"rootfsSource":           "rootfs-source-manifest.json",
	"rootfsContract":         "secondbox-rootfs-contract.json",
	"debianPackages":         "rootfs-debian-packages.lock",
	"pythonPackages":         "rootfs-python.freeze",
	"debianLicenseInventory": "rootfs-debian-license-inventory.json",
	"pythonLicenseInventory": "rootfs-python-license-inventory.json",
	"manifest":               "manifest.json",
	"signature":              "manifest.sig",
	"checksums":              "SHA256SUMS",
	"bundledVerificationKey": "signing.pub",
}

func main() {
	var artifactsDir string
	var outputPath string
	flag.StringVar(&artifactsDir, "artifacts", "", "signed Firecracker artifact directory")
	flag.StringVar(&outputPath, "out", "", "new owner-only evidence JSON path")
	flag.Parse()
	if err := collect(artifactsDir, outputPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func collect(artifactsDir, outputPath string) error {
	if strings.TrimSpace(artifactsDir) == "" {
		return errors.New("--artifacts is required")
	}
	if strings.TrimSpace(outputPath) == "" {
		return errors.New("--out is required")
	}
	if _, err := os.Lstat(outputPath); err == nil {
		return fmt.Errorf("evidence output already exists: %s", outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect evidence output: %w", err)
	}
	artifacts, err := collectArtifacts(artifactsDir)
	if err != nil {
		return err
	}
	parent := filepath.Dir(outputPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create evidence parent: %w", err)
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create evidence output: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(evidenceDocument{SchemaVersion: 1, Artifacts: artifacts})
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write evidence output: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close evidence output: %w", closeErr)
	}
	return nil
}

func collectArtifacts(root string) (map[string]artifactEvidence, error) {
	result := make(map[string]artifactEvidence, len(artifactEvidenceFiles))
	for logical, name := range artifactEvidenceFiles {
		content, err := readRegular(filepath.Join(root, name))
		if err != nil {
			return nil, fmt.Errorf("read Firecracker %s: %w", logical, err)
		}
		digest := sha256.Sum256(content)
		result[logical] = artifactEvidence{
			SHA256:    hex.EncodeToString(digest[:]),
			SizeBytes: int64(len(content)),
		}
	}
	return result, nil
}

func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("input must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}
