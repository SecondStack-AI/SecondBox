package microvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxThinDeviceID uint32 = 1 << 24

type thinDeviceIDStore struct {
	Next uint32            `json:"next"`
	IDs  map[string]uint32 `json:"ids"`
}

type thinDeviceIDAllocator struct {
	mu     sync.Mutex
	path   string
	store  thinDeviceIDStore
	loaded bool
}

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

func (m *Manager) thinDeviceIDAllocatorRef() *thinDeviceIDAllocator {
	m.thinDeviceIDOnce.Do(func() {
		path := ""
		if m.cfg != nil && strings.TrimSpace(m.cfg.DataDir) != "" {
			path = filepath.Join(m.cfg.DataDir, "microvm", "thin-device-ids.json")
		}
		m.thinDeviceIDs = &thinDeviceIDAllocator{path: path}
	})
	return m.thinDeviceIDs
}

func (a *thinDeviceIDAllocator) allocate(seed string) (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.loadLocked(); err != nil {
		return 0, err
	}
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return 0, fmt.Errorf("dm-thin device id seed is required")
	}
	if id, ok := a.store.IDs[seed]; ok {
		return id, nil
	}
	if a.store.Next == 0 {
		a.store.Next = 1
	}
	if a.store.Next >= maxThinDeviceID {
		return 0, fmt.Errorf("dm-thin device id space exhausted at %d", maxThinDeviceID-1)
	}
	id := a.store.Next
	previousNext := a.store.Next
	a.store.IDs[seed] = id
	a.store.Next++
	if err := a.saveLocked(); err != nil {
		delete(a.store.IDs, seed)
		a.store.Next = previousNext
		return 0, err
	}
	return id, nil
}

func (a *thinDeviceIDAllocator) loadLocked() error {
	if a.loaded {
		return nil
	}
	store := thinDeviceIDStore{Next: 1, IDs: map[string]uint32{}}
	if strings.TrimSpace(a.path) != "" {
		data, err := os.ReadFile(a.path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read dm-thin device id store: %w", err)
		}
		if err == nil {
			if err := json.Unmarshal(data, &store); err != nil {
				return fmt.Errorf("decode dm-thin device id store: %w", err)
			}
		}
	}
	if store.IDs == nil {
		store.IDs = map[string]uint32{}
	}
	if store.Next == 0 {
		store.Next = 1
	}
	used := make(map[uint32]string, len(store.IDs))
	var highest uint32
	for seed, id := range store.IDs {
		if strings.TrimSpace(seed) == "" || id == 0 || id >= maxThinDeviceID {
			return fmt.Errorf("invalid persisted dm-thin device id %d for %q", id, seed)
		}
		if other, exists := used[id]; exists {
			return fmt.Errorf("persisted dm-thin device id %d is shared by %q and %q", id, other, seed)
		}
		used[id] = seed
		if id > highest {
			highest = id
		}
	}
	if store.Next <= highest {
		return fmt.Errorf("persisted dm-thin next device id %d does not follow allocated id %d", store.Next, highest)
	}
	if store.Next > maxThinDeviceID {
		return fmt.Errorf("persisted dm-thin next device id %d exceeds 24-bit range", store.Next)
	}
	a.store = store
	a.loaded = true
	return nil
}

func (a *thinDeviceIDAllocator) saveLocked() error {
	if strings.TrimSpace(a.path) == "" {
		return nil
	}
	data, err := json.MarshalIndent(a.store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode dm-thin device id store: %w", err)
	}
	dir := filepath.Dir(a.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dm-thin device id store directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".thin-device-ids-*.tmp")
	if err != nil {
		return fmt.Errorf("create dm-thin device id store temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure dm-thin device id store temp file: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write dm-thin device id store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync dm-thin device id store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close dm-thin device id store: %w", err)
	}
	if err := os.Rename(tmpPath, a.path); err != nil {
		return fmt.Errorf("replace dm-thin device id store: %w", err)
	}
	return nil
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
	devID, err := m.thinDeviceIDAllocatorRef().allocate("workspace-origin:" + name)
	if err != nil {
		return "", fmt.Errorf("allocate dm-thin workspace device id: %w", err)
	}
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
	originID, err := m.thinDeviceIDAllocatorRef().allocate("workspace-origin:" + originName)
	if err != nil {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("allocate dm-thin origin device id: %w", err)
	}
	snapID, err := m.thinDeviceIDAllocatorRef().allocate("workspace-snapshot:" + name)
	if err != nil {
		return ThinWorkspaceSnapshot{}, fmt.Errorf("allocate dm-thin snapshot device id: %w", err)
	}
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
