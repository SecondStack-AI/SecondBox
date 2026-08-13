package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type GoldenSnapshotManifest struct {
	InstanceID            string            `json:"instanceId"`
	SandboxID             string            `json:"sandboxId"`
	CompartmentID         string            `json:"compartmentId,omitempty"`
	CreatedAt             string            `json:"createdAt"`
	SnapshotPath          string            `json:"snapshotPath"`
	MemFilePath           string            `json:"memFilePath"`
	KernelPath            string            `json:"kernelPath"`
	KernelArgs            string            `json:"kernelArgs,omitempty"`
	KernelSHA256          string            `json:"kernelSha256,omitempty"`
	KernelIdentity        *ArtifactIdentity `json:"kernelIdentity,omitempty"`
	RootfsPath            string            `json:"rootfsPath"`
	RootfsSHA256          string            `json:"rootfsSha256,omitempty"`
	RootfsIdentity        *ArtifactIdentity `json:"rootfsIdentity,omitempty"`
	WorkspacePath         string            `json:"workspacePath,omitempty"`
	SharedImagePath       string            `json:"sharedImagePath,omitempty"`
	SharedSHA256          string            `json:"sharedSha256,omitempty"`
	SharedIdentity        *ArtifactIdentity `json:"sharedIdentity,omitempty"`
	FirecrackerPath       string            `json:"firecrackerPath"`
	ComputeBackendVersion string            `json:"firecrackerVersion"`
	Machine               machineConfig     `json:"machine"`
	VsockUDSPath          string            `json:"vsockUDSPath,omitempty"`
	TapName               string            `json:"tapName,omitempty"`
	GuestIP               string            `json:"guestIp,omitempty"`
	OriginalRunDir        string            `json:"originalRunDir,omitempty"`
	Jailed                bool              `json:"jailed,omitempty"`
	StartupFingerprint    string            `json:"startupFingerprint,omitempty"`
	Metadata              map[string]string `json:"metadata,omitempty"`
}

type ArtifactIdentity struct {
	Size            int64 `json:"size"`
	ModTimeUnixNano int64 `json:"modTimeUnixNano"`
}

// CreateGoldenSnapshot pauses a warmed VM, writes a full Firecracker snapshot,
// resumes the VM, and records the artifact metadata needed to decide whether
// that snapshot is still compatible with the current image set.
func (m *Manager) CreateGoldenSnapshot(ctx context.Context, instanceID, outDir string, metadata map[string]string) (GoldenSnapshotManifest, error) {
	inst := m.lookup(instanceID)
	if inst == nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	outDir = strings.TrimSpace(outDir)
	if outDir == "" {
		return GoldenSnapshotManifest{}, fmt.Errorf("snapshot output directory is required")
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("create snapshot output directory: %w", err)
	}
	snapshotPath := filepath.Join(outDir, "vmstate.snap")
	memPath := filepath.Join(outDir, "memory.snap")
	client := inst.apiClient(30 * time.Second)
	if err := client.Pause(ctx); err != nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("pause VM for golden snapshot: %w", err)
	}
	resumed := false
	defer func() {
		if !resumed {
			_ = client.Resume(context.Background())
		}
	}()
	if err := client.CreateFullSnapshot(ctx, snapshotPath, memPath); err != nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("create golden snapshot: %w", err)
	}
	if err := client.Resume(ctx); err != nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("resume VM after golden snapshot: %w", err)
	}
	resumed = true

	manifest := GoldenSnapshotManifest{
		InstanceID:            inst.id,
		SandboxID:             inst.sandboxID,
		CreatedAt:             time.Now().UTC().Format(time.RFC3339),
		SnapshotPath:          snapshotPath,
		MemFilePath:           memPath,
		KernelPath:            m.cfg.MicroVMKernelPath,
		KernelArgs:            effectiveKernelArgs(m.cfg, firstNonEmpty(inst.guestIP, m.guestIP(inst.id))),
		RootfsPath:            firstNonEmpty(inst.rootfsPath, m.cfg.MicroVMRootfsPath),
		WorkspacePath:         inst.workspacePath,
		SharedImagePath:       firstNonEmpty(inst.sharedImagePath, m.cfg.MicroVMSharedImagePath),
		FirecrackerPath:       m.cfg.FirecrackerPath,
		ComputeBackendVersion: expectedComputeBackendVersionString(),
		Machine:               machineConfig{VCPUCount: m.cfg.MicroVMVCPUs, MemSizeMiB: m.cfg.MicroVMMemoryMiB, SMT: false, CPUTemplate: m.cfg.MicroVMCPUTemplate},
		VsockUDSPath:          inst.vsockUDS,
		TapName:               inst.tapName,
		GuestIP:               firstNonEmpty(inst.guestIP, m.guestIP(inst.id)),
		OriginalRunDir:        inst.dir,
		Jailed:                inst.jailRoot != "",
		StartupFingerprint:    inst.startupFingerprint,
		Metadata:              copyStringMap(metadata),
	}
	manifest.KernelSHA256, _ = fileSHA256(manifest.KernelPath)
	manifest.RootfsSHA256, _ = fileSHA256(manifest.RootfsPath)
	manifest.KernelIdentity, _ = fileArtifactIdentity(manifest.KernelPath)
	manifest.RootfsIdentity, _ = fileArtifactIdentity(manifest.RootfsPath)
	if manifest.SharedImagePath != "" {
		manifest.SharedSHA256, _ = fileSHA256(manifest.SharedImagePath)
		manifest.SharedIdentity, _ = fileArtifactIdentity(manifest.SharedImagePath)
	}
	if err := writeSnapshotManifest(filepath.Join(outDir, "manifest.json"), manifest); err != nil {
		return GoldenSnapshotManifest{}, err
	}
	return manifest, nil
}

func fileArtifactIdentity(path string) (*ArtifactIdentity, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &ArtifactIdentity{Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()}, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync target is not a directory: %s", path)
	}
	return dir.Sync()
}

func verifyArtifactIdentity(label, path string, want *ArtifactIdentity) error {
	if want == nil {
		return nil
	}
	got, err := fileArtifactIdentity(path)
	if err != nil {
		return fmt.Errorf("stat current %s artifact: %w", label, err)
	}
	if got.Size != want.Size || got.ModTimeUnixNano != want.ModTimeUnixNano {
		return fmt.Errorf("%s artifact changed since snapshot", label)
	}
	return nil
}

func writeSnapshotManifest(path string, manifest GoldenSnapshotManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal golden snapshot manifest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write golden snapshot manifest: %w", err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k != "" {
			out[k] = v
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
