package microvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentcy/internal/runtimecontext"
	"agentcy/internal/runtimemanager"
)

const (
	snapshotStateName  = "vmstate.snap"
	snapshotMemoryName = "memory.snap"
)

type GoldenSnapshotManifest struct {
	InstanceID         string            `json:"instanceId"`
	AgentID            string            `json:"agentId"`
	CompartmentID      string            `json:"compartmentId,omitempty"`
	CreatedAt          string            `json:"createdAt"`
	SnapshotPath       string            `json:"snapshotPath"`
	MemFilePath        string            `json:"memFilePath"`
	KernelPath         string            `json:"kernelPath"`
	KernelArgs         string            `json:"kernelArgs,omitempty"`
	KernelSHA256       string            `json:"kernelSha256,omitempty"`
	KernelIdentity     *ArtifactIdentity `json:"kernelIdentity,omitempty"`
	RootfsPath         string            `json:"rootfsPath"`
	RootfsSHA256       string            `json:"rootfsSha256,omitempty"`
	RootfsIdentity     *ArtifactIdentity `json:"rootfsIdentity,omitempty"`
	WorkspacePath      string            `json:"workspacePath,omitempty"`
	SharedImagePath    string            `json:"sharedImagePath,omitempty"`
	SharedSHA256       string            `json:"sharedSha256,omitempty"`
	SharedIdentity     *ArtifactIdentity `json:"sharedIdentity,omitempty"`
	FirecrackerPath    string            `json:"firecrackerPath"`
	FirecrackerVersion string            `json:"firecrackerVersion"`
	Machine            machineConfig     `json:"machine"`
	VsockUDSPath       string            `json:"vsockUDSPath,omitempty"`
	TapName            string            `json:"tapName,omitempty"`
	GuestIP            string            `json:"guestIp,omitempty"`
	OriginalRunDir     string            `json:"originalRunDir,omitempty"`
	Jailed             bool              `json:"jailed,omitempty"`
	StartupFingerprint string            `json:"startupFingerprint,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type ArtifactIdentity struct {
	Size            int64 `json:"size"`
	ModTimeUnixNano int64 `json:"modTimeUnixNano"`
}

type RestoreSnapshotOpts struct {
	AgentID                  string
	CompartmentID            string
	ShapeFingerprint         string
	Timezone                 string
	RuntimeActorContext      runtimecontext.VerifiedActorContext
	ProxyEgress              *runtimemanager.ProxyEgressConfig
	RuntimeContextProjection runtimecontext.Projection
	Resume                   bool
	TrackDirtyPages          bool
	ClockRealtime            bool
	HardenPostRestore        bool
	MemoryBackendType        string
	MemoryBackendPath        string
	Metadata                 map[string]string
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
		InstanceID:         inst.id,
		AgentID:            inst.agentID,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
		SnapshotPath:       snapshotPath,
		MemFilePath:        memPath,
		KernelPath:         m.cfg.MicroVMKernelPath,
		KernelArgs:         effectiveKernelArgs(m.cfg, firstNonEmpty(inst.guestIP, m.guestIP(inst.id))),
		RootfsPath:         firstNonEmpty(inst.rootfsPath, m.cfg.MicroVMRootfsPath),
		WorkspacePath:      inst.workspacePath,
		SharedImagePath:    firstNonEmpty(inst.sharedImagePath, m.cfg.MicroVMSharedImagePath),
		FirecrackerPath:    m.cfg.FirecrackerPath,
		FirecrackerVersion: expectedFirecrackerVersionString(),
		Machine:            machineConfig{VCPUCount: m.cfg.MicroVMVCPUs, MemSizeMiB: m.cfg.MicroVMMemoryMiB, SMT: false, CPUTemplate: m.cfg.MicroVMCPUTemplate},
		VsockUDSPath:       inst.vsockUDS,
		TapName:            inst.tapName,
		GuestIP:            firstNonEmpty(inst.guestIP, m.guestIP(inst.id)),
		OriginalRunDir:     inst.dir,
		Jailed:             inst.jailRoot != "",
		StartupFingerprint: inst.startupFingerprint,
		Metadata:           copyStringMap(metadata),
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

// RestoreGoldenSnapshot starts a fresh Firecracker process and loads a golden
// snapshot into it. Firecracker requires the snapshotted host resources (block
// devices, tap devices, and vsock UDS path) to remain available at the same
// paths used by the source VM.
func (m *Manager) RestoreGoldenSnapshot(ctx context.Context, manifest GoldenSnapshotManifest, opts RestoreSnapshotOpts) (string, error) {
	if strings.TrimSpace(manifest.SnapshotPath) == "" || strings.TrimSpace(manifest.MemFilePath) == "" {
		return "", fmt.Errorf("snapshot manifest must include snapshot and memory paths")
	}
	if err := verifySnapshotArtifacts(manifest); err != nil {
		return "", err
	}
	agentID := strings.TrimSpace(opts.AgentID)
	if agentID == "" {
		agentID = strings.TrimSpace(manifest.AgentID)
	}
	if agentID == "" {
		return "", fmt.Errorf("agent id is required to restore golden snapshot")
	}
	compartmentID := normalizeRuntimeCompartmentID(opts.CompartmentID)
	if err := validateRuntimeCompartmentID(compartmentID); err != nil {
		return "", err
	}
	id, err := newInstanceID(agentID, compartmentID)
	if err != nil {
		return "", err
	}
	startOpts := runtimemanager.StartOpts{
		Timezone:                 opts.Timezone,
		CompartmentID:            compartmentID,
		RuntimeActorContext:      opts.RuntimeActorContext,
		ShapeFingerprint:         opts.ShapeFingerprint,
		ProxyEgress:              opts.ProxyEgress,
		RuntimeContextProjection: opts.RuntimeContextProjection,
	}
	guestIP, err := m.reserveGuestIP(id)
	if err != nil {
		return "", fmt.Errorf("reserve restore guest IP: %w", err)
	}
	releaseIP := true
	defer func() {
		if releaseIP {
			m.releaseGuestIP(id)
		}
	}()
	tapName := ""
	if m.networkRequired(startOpts) {
		tapName = tapNameForInstance(m.cfg.MicroVMTapPrefix, id)
		if m.network == nil {
			return "", fmt.Errorf("microVM tap networking is required but no host network configurer is available")
		}
		if err := m.network.ConfigureTap(ctx, TapConfig{
			AgentID:    agentID,
			InstanceID: id,
			TapName:    tapName,
			BridgeName: m.cfg.MicroVMBridgeName,
			BridgeCIDR: m.cfg.MicroVMBridgeCIDR,
			OwnerUID:   m.tapOwnerUID(),
		}); err != nil {
			m.releaseGuestIP(id)
			return "", fmt.Errorf("configure restore microVM tap: %w", err)
		}
	}
	dir := filepath.Join(m.cfg.MicroVMRunDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.cleanupTap(ctx, tapName)
		return "", fmt.Errorf("create restore instance dir: %w", err)
	}
	if err := m.writeIdentityFile(dir, id, agentID, startOpts); err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", err
	}
	logPath := filepath.Join(m.cfg.MicroVMLogDir, id+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create restore log directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("open restore microVM log: %w", err)
	}
	defer logFile.Close()

	socket := filepath.Join(dir, firecrackerSockName)
	rootfsPath := filepath.Join(dir, rootfsName)
	if err := copyFile(rootfsPath, manifest.RootfsPath, 0o600); err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("stage golden rootfs: %w", err)
	}
	workspacePath, err := m.prepareWorkspace(ctx, agentID, compartmentID)
	if err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("stage golden workspace: %w", err)
	}
	cloneSnapshotPath := filepath.Join(dir, snapshotStateName)
	if err := copyFile(cloneSnapshotPath, manifest.SnapshotPath, 0o600); err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("stage golden snapshot: %w", err)
	}
	cloneMemPath := filepath.Join(dir, snapshotMemoryName)
	if err := copyFile(cloneMemPath, manifest.MemFilePath, 0o600); err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("stage golden memory: %w", err)
	}
	cmd := exec.CommandContext(ctx, m.cfg.FirecrackerPath, "--api-sock", socket)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("start firecracker for snapshot restore: %w", err)
	}
	startupFingerprint, err := m.startupFingerprint(agentID, compartmentID, startOpts)
	if err != nil {
		m.cleanupTap(ctx, tapName)
		_ = os.RemoveAll(dir)
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("build restore startup fingerprint: %w", err)
	}
	inst := &instance{
		id:                 id,
		agentID:            agentID,
		compartmentID:      compartmentID,
		dir:                dir,
		logPath:            logPath,
		socket:             socket,
		vsockUDS:           filepath.Join(dir, vsockUDSName),
		tapName:            tapName,
		rootfsPath:         rootfsPath,
		workspacePath:      workspacePath,
		guestIP:            guestIP,
		startupFingerprint: startupFingerprint,
		cmd:                cmd,
		startedAt:          time.Now().UTC(),
		done:               make(chan struct{}),
	}
	if inst.vsockUDS == "" {
		inst.vsockUDS = filepath.Join(dir, vsockUDSName)
	}
	m.mu.Lock()
	m.addInstanceLocked(inst)
	m.mu.Unlock()
	// Start the reaper before any stopInstance call so failures below cannot
	// deadlock waiting on inst.done.
	go m.reap(inst)

	if err := waitForAPISocket(ctx, socket, 3*time.Second); err != nil {
		if data, readErr := os.ReadFile(logPath); readErr == nil && len(data) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(data)))
		}
		_ = m.stopInstance(context.Background(), inst, true)
		return "", err
	}
	contextToken, err := m.registerSourceBinding(ctx, agentID, id, startOpts)
	if err != nil {
		_ = m.stopInstance(context.Background(), inst, true)
		return "", err
	}
	if startOpts.ProxyEgress != nil {
		startOpts.ProxyEgress.ContextToken = contextToken
	}
	backendType := strings.TrimSpace(opts.MemoryBackendType)
	if backendType == "" {
		backendType = "File"
	}
	backendPath := strings.TrimSpace(opts.MemoryBackendPath)
	if backendPath == "" {
		backendPath = cloneMemPath
	}
	loadReq := snapshotLoadRequest{
		SnapshotPath:    cloneSnapshotPath,
		MemBackend:      &memoryBackend{BackendPath: backendPath, BackendType: backendType},
		TrackDirtyPages: opts.TrackDirtyPages,
		ResumeVM:        opts.Resume,
		ClockRealtime:   opts.ClockRealtime,
	}
	if tapName != "" {
		loadReq.NetworkOverrides = []networkOverride{{IfaceID: "eth0", HostDevName: tapName}}
	}
	if inst.vsockUDS != "" {
		loadReq.VsockOverride = &vsockOverride{UDSPath: inst.vsockUDS}
	}
	if err := inst.apiClient(30*time.Second).LoadSnapshotWithOptions(ctx, loadReq); err != nil {
		_ = m.stopInstance(context.Background(), inst, true)
		return "", fmt.Errorf("load golden snapshot: %w", err)
	}
	if opts.Resume && strings.TrimSpace(contextToken) != "" {
		timer := newColdStartStageTimer("agent", agentID, "compartment", compartmentID, "instance", id, "restore", true)
		if err := m.deliverStartupSecrets(ctx, inst, agentID, startOpts, timer); err != nil {
			_ = m.stopInstance(context.Background(), inst, true)
			return "", fmt.Errorf("refresh runtime startup secrets after snapshot restore: %w", err)
		}
	}
	// Re-individuation (reseed entropy + reset clock) can only run once the guest
	// is resumed because a paused load has no responsive control plane. Direct
	// restores harden only when explicitly requested.
	if opts.Resume && opts.HardenPostRestore {
		if err := m.HardenPostRestore(ctx, id); err != nil {
			_ = m.stopInstance(context.Background(), inst, true)
			return "", fmt.Errorf("harden restored VM: %w", err)
		}
	}
	releaseIP = false
	slog.Info("restored firecracker microVM from golden snapshot", "agent", agentID, "instance", id, "snapshot", manifest.SnapshotPath, "log", logPath)
	return id, nil
}

func waitForAPISocket(ctx context.Context, socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(socket); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for Firecracker API socket %s", socket)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

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
		if check.identity != nil {
			if err := verifyArtifactIdentity(label, check.path, check.identity); err != nil {
				return err
			}
			continue
		}
		if strings.TrimSpace(check.sum) == "" {
			continue
		}
		got, err := fileSHA256(check.path)
		if err != nil {
			return fmt.Errorf("hash %s artifact: %w", label, err)
		}
		if got != check.sum {
			return fmt.Errorf("%s artifact hash mismatch: got %s want %s", label, got, check.sum)
		}
	}
	return nil
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
