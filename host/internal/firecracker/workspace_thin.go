package microvm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type ThinWorkspaceSnapshot struct {
	Name         string `json:"name"`
	DevicePath   string `json:"devicePath"`
	OriginDevice string `json:"originDevice"`
	CreatedAt    string `json:"createdAt"`
}

type hostCommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var runHostCommand hostCommandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func (m *Manager) ensureThinWorkspace(ctx context.Context, agentID, compartmentID string) (string, error) {
	pool := strings.TrimSpace(m.cfg.MicroVMThinPoolDevice)
	if pool == "" {
		return "", fmt.Errorf("dm-thin pool device is required")
	}
	name := thinWorkspaceName(agentID, compartmentID)
	if _, err := runHostCommand(ctx, "dmsetup", "info", name); err == nil {
		return thinDevicePath(name), nil
	}
	devID := thinDeviceID("workspace-origin:" + name)
	if out, err := runHostCommand(ctx, "dmsetup", "message", pool, "0", fmt.Sprintf("create_thin %d", devID)); err != nil && !strings.Contains(string(out), "File exists") {
		return "", fmt.Errorf("create dm-thin workspace device: %w: %s", err, strings.TrimSpace(string(out)))
	}
	sectors := sectorsForMiB(m.cfg.MicroVMWorkspaceSizeMiB)
	table := fmt.Sprintf("0 %d thin %s %d", sectors, pool, devID)
	if out, err := runHostCommand(ctx, "dmsetup", "create", name, "--table", table); err != nil {
		return "", fmt.Errorf("activate dm-thin workspace device: %w: %s", err, strings.TrimSpace(string(out)))
	}
	path := thinDevicePath(name)
	if out, err := runHostCommand(ctx, "mkfs.ext4", "-F", "-q", "-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard", path); err != nil {
		return "", fmt.Errorf("format dm-thin workspace: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}

func (m *Manager) CreateWorkspaceThinSnapshot(ctx context.Context, instanceID, snapshotName string) (ThinWorkspaceSnapshot, error) {
	if !strings.EqualFold(m.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("workspace backend is %q, not dm-thin", m.cfg.MicroVMWorkspaceBackend)
	}
	inst := m.lookup(instanceID)
	if inst == nil {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	if _, err := m.FreezeWorkspace(ctx, instanceID); err != nil {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("freeze workspace before dm-thin snapshot: %w", err)
	}
	defer func() { _, _ = m.ThawWorkspace(context.Background(), instanceID) }()
	origin := thinWorkspaceName(inst.agentID, inst.compartmentID)
	snap, err := m.createThinSnapshot(ctx, origin, snapshotName)
	if err != nil {
		return ThinWorkspaceSnapshot{}, err
	}
	return snap, nil
}

func (m *Manager) createThinSnapshot(ctx context.Context, originName, snapshotName string) (ThinWorkspaceSnapshot, error) {
	pool := strings.TrimSpace(m.cfg.MicroVMThinPoolDevice)
	if pool == "" {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("dm-thin pool device is required")
	}
	name := thinSnapshotName(originName, snapshotName)
	originID := thinDeviceID("workspace-origin:" + originName)
	snapID := thinDeviceID("workspace-snapshot:" + name)
	if out, err := runHostCommand(ctx, "dmsetup", "message", pool, "0", fmt.Sprintf("create_snap %d %d", snapID, originID)); err != nil && !strings.Contains(string(out), "File exists") {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("create dm-thin snapshot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	sectors := sectorsForMiB(m.cfg.MicroVMWorkspaceSizeMiB)
	table := fmt.Sprintf("0 %d thin %s %d", sectors, pool, snapID)
	if out, err := runHostCommand(ctx, "dmsetup", "create", name, "--table", table); err != nil {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("activate dm-thin snapshot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return ThinWorkspaceSnapshot{
		Name:         name,
		DevicePath:   thinDevicePath(name),
		OriginDevice: thinDevicePath(originName),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func sectorsForMiB(sizeMiB int) int64 {
	return int64(sizeMiB) * 1024 * 1024 / 512
}

func thinDeviceID(seed string) uint64 {
	sum := sha256.Sum256([]byte(seed))
	id := binary.BigEndian.Uint64(sum[:8])
	if id == 0 {
		return 1
	}
	return id
}

var thinNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func thinWorkspaceName(agentID, compartmentID string) string {
	name := thinNameUnsafe.ReplaceAllString(strings.TrimSpace(agentID), "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "agent"
	}
	compartmentID = normalizeRuntimeCompartmentID(compartmentID)
	if compartmentID != "" {
		compartment := thinNameUnsafe.ReplaceAllString(compartmentID, "-")
		compartment = strings.Trim(compartment, "-_.")
		if compartment == "" {
			compartment = "compartment"
		}
		name += "-" + compartment
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return "agentcy-ws-" + name
}

func thinSnapshotName(originName, snapshotName string) string {
	name := thinNameUnsafe.ReplaceAllString(strings.TrimSpace(snapshotName), "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = time.Now().UTC().Format("20060102150405")
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return originName + "-snap-" + name
}

func thinDevicePath(name string) string {
	return filepath.Join("/dev/mapper", name)
}
