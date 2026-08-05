package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
)

func TestJailerUIDAllocatorNeverSharesLiveUID(t *testing.T) {
	m := &Manager{
		cfg:        &config.Config{MicroVMJailerUIDStart: 2000, MicroVMJailerUIDCount: 2},
		jailerUIDs: map[int]string{},
	}
	first, err := m.allocateJailerUID("fc-first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.allocateJailerUID("fc-second")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("live instances share UID %d", first)
	}
	if _, err := m.allocateJailerUID("fc-third"); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("exhausted range error = %v", err)
	}
	if err := m.releaseJailerUID("fc-first", first); err != nil {
		t.Fatal(err)
	}
	reused, err := m.allocateJailerUID("fc-third")
	if err != nil {
		t.Fatal(err)
	}
	if reused != first {
		t.Fatalf("released UID = %d, reused UID = %d", first, reused)
	}
}

func TestJailerUIDLeaseReconcilesRestartReservation(t *testing.T) {
	runDir := t.TempDir()
	m := &Manager{
		cfg:        &config.Config{MicroVMJailerUIDStart: 3000, MicroVMJailerUIDCount: 2},
		jailerUIDs: map[int]string{},
	}
	if err := m.writeJailerUIDLease(runDir, 3001); err != nil {
		t.Fatal(err)
	}
	uid, err := m.readJailerUIDLease(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 3001 {
		t.Fatalf("lease UID = %d", uid)
	}
	if err := m.reserveJailerUID("fc-adopted", uid); err != nil {
		t.Fatal(err)
	}
	if err := m.reserveJailerUID("fc-conflict", uid); err == nil || !strings.Contains(err.Error(), "already reserved") {
		t.Fatalf("duplicate restart reservation error = %v", err)
	}
	if err := m.releaseJailerUID("fc-adopted", uid); err != nil {
		t.Fatal(err)
	}
	allocated, err := m.allocateJailerUID("fc-new")
	if err != nil {
		t.Fatal(err)
	}
	if allocated != 3000 {
		t.Fatalf("allocator did not restart from range start: %d", allocated)
	}
}

func TestRunningOrphanWithoutJailerUIDLeaseFailsClosed(t *testing.T) {
	runRoot := t.TempDir()
	instanceID := "fc-orphan-cmp-00000000"
	if err := os.Mkdir(filepath.Join(runRoot, instanceID), 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		cfg: &config.Config{
			MicroVMRunDir:              runRoot,
			MicroVMJailerChrootBaseDir: filepath.Join(t.TempDir(), "jailer"),
			MicroVMJailerUIDStart:      4000,
			MicroVMJailerUIDCount:      1,
			FirecrackerPath:            "/usr/local/bin/firecracker",
		},
		instances:  map[string]*instance{},
		jailerUIDs: map[int]string{},
	}
	original := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(id string) (bool, error) { return id == instanceID, nil }
	t.Cleanup(func() { firecrackerProcessRunningFunc = original })
	if err := m.sweepStartupOrphans(t.Context()); err == nil || !strings.Contains(err.Error(), jailerUIDLeaseName+" is missing") {
		t.Fatalf("missing orphan UID lease error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(runRoot, instanceID)); err != nil {
		t.Fatalf("unsafe orphan state was removed despite missing UID evidence: %v", err)
	}
}

func TestStartupSweepReservesPersistedUIDUntilAdoptedOrphanExits(t *testing.T) {
	runRoot := t.TempDir()
	instanceID := "fc-adopted-cmp-00000000"
	runDir := filepath.Join(runRoot, instanceID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		cfg: &config.Config{
			MicroVMRunDir:              runRoot,
			MicroVMJailerChrootBaseDir: filepath.Join(t.TempDir(), "jailer"),
			MicroVMJailerUIDStart:      5000,
			MicroVMJailerUIDCount:      1,
			FirecrackerPath:            "/usr/local/bin/firecracker",
		},
		instances:  map[string]*instance{},
		jailerUIDs: map[int]string{},
	}
	if err := m.writeJailerUIDLease(runDir, 5000); err != nil {
		t.Fatal(err)
	}
	var running atomic.Bool
	running.Store(true)
	original := firecrackerProcessRunningFunc
	firecrackerProcessRunningFunc = func(id string) (bool, error) {
		return id == instanceID && running.Load(), nil
	}
	t.Cleanup(func() { firecrackerProcessRunningFunc = original })
	m.signalInstance = func(id string, signal syscall.Signal) error {
		if id != instanceID || signal != syscall.SIGTERM {
			t.Fatalf("signal = %s %v", id, signal)
		}
		m.mu.Lock()
		owner := m.jailerUIDs[5000]
		m.mu.Unlock()
		if owner != instanceID {
			t.Fatalf("UID released before adopted process exit: owner = %q", owner)
		}
		running.Store(false)
		return nil
	}
	if err := m.sweepStartupOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("adopted orphan run directory still exists: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.jailerUIDs) != 0 {
		t.Fatalf("UID reservations after orphan cleanup = %v", m.jailerUIDs)
	}
}
