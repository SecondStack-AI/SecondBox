package microvm

import (
	"fmt"
	"os"
	"strings"
)

func verifySnapshotArtifacts(manifest GoldenSnapshotManifest) error {
	wantFirecrackerVersion := expectedFirecrackerVersionString()
	if strings.TrimPrefix(strings.TrimSpace(manifest.FirecrackerVersion), "v") != wantFirecrackerVersion {
		return fmt.Errorf("snapshot firecracker version %q does not match pinned version %s; discard and recreate the golden snapshot", manifest.FirecrackerVersion, wantFirecrackerVersion)
	}
	for label, path := range map[string]string{
		"snapshot": manifest.SnapshotPath,
		"memory":   manifest.MemFilePath,
	} {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("%s path is required", label)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s path %q: %w", label, path, err)
		}
	}
	for label, check := range map[string]struct {
		path     string
		sum      string
		identity *ArtifactIdentity
	}{
		"kernel": {path: manifest.KernelPath, sum: manifest.KernelSHA256, identity: manifest.KernelIdentity},
		"rootfs": {path: manifest.RootfsPath, sum: manifest.RootfsSHA256, identity: manifest.RootfsIdentity},
		"shared": {path: manifest.SharedImagePath, sum: manifest.SharedSHA256, identity: manifest.SharedIdentity},
	} {
		if strings.TrimSpace(check.path) == "" {
			continue
		}
		if strings.TrimSpace(check.sum) != "" {
			got, err := fileSHA256(check.path)
			if err != nil {
				return fmt.Errorf("hash %s artifact: %w", label, err)
			}
			if got != check.sum {
				return fmt.Errorf("%s artifact hash mismatch: got %s want %s", label, got, check.sum)
			}
			continue
		}
		if check.identity != nil {
			if err := verifyArtifactIdentity(label, check.path, check.identity); err != nil {
				return err
			}
		}
	}
	return nil
}
