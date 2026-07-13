package microvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"agentcy/internal/config"
)

func TestEnsureThinWorkspaceCreatesAndFormatsDevice(t *testing.T) {
	var calls []string
	restore := stubHostCommands(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "dmsetup info ") {
			return nil, errors.New("not found")
		}
		return nil, nil
	})
	defer restore()

	m := &Manager{cfg: &config.Config{
		MicroVMWorkspaceBackend: "dm-thin",
		MicroVMThinPoolDevice:   "/dev/mapper/agentcy-pool",
		MicroVMWorkspaceSizeMiB: 1024,
	}}
	path, err := m.ensureThinWorkspace(context.Background(), "agent-1", "cmp_a")
	if err != nil {
		t.Fatalf("ensure thin workspace: %v", err)
	}
	if path != "/dev/mapper/agentcy-ws-agent-1-cmp_a" {
		t.Fatalf("path = %q", path)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"dmsetup info agentcy-ws-agent-1-cmp_a",
		"dmsetup message /dev/mapper/agentcy-pool 0 create_thin ",
		"dmsetup create agentcy-ws-agent-1-cmp_a --table 0 2097152 thin /dev/mapper/agentcy-pool ",
		"mkfs.ext4 -F -q -E lazy_itable_init=1,lazy_journal_init=1,nodiscard /dev/mapper/agentcy-ws-agent-1-cmp_a",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls missing %q:\n%s", want, joined)
		}
	}
	assertThinIDsInRange(t, joined)
}

func TestCreateThinSnapshotCommands(t *testing.T) {
	var calls []string
	restore := stubHostCommands(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	})
	defer restore()

	m := &Manager{cfg: &config.Config{
		MicroVMThinPoolDevice:   "/dev/mapper/agentcy-pool",
		MicroVMWorkspaceSizeMiB: 512,
	}}
	snap, err := m.createThinSnapshot(context.Background(), "agentcy-ws-agent-1", "backup:daily")
	if err != nil {
		t.Fatalf("create thin snapshot: %v", err)
	}
	if snap.DevicePath != "/dev/mapper/agentcy-ws-agent-1-snap-backup-daily" {
		t.Fatalf("snapshot = %#v", snap)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"dmsetup message /dev/mapper/agentcy-pool 0 create_snap ",
		"dmsetup create agentcy-ws-agent-1-snap-backup-daily --table 0 1048576 thin /dev/mapper/agentcy-pool ",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls missing %q:\n%s", want, joined)
		}
	}
	assertThinIDsInRange(t, joined)
	match := regexp.MustCompile(`create_snap (\d+) (\d+)`).FindStringSubmatch(joined)
	if len(match) != 3 || match[1] != "2" || match[2] != "1" {
		t.Fatalf("create_snap ids = %v, want snapshot=2 origin=1\n%s", match, joined)
	}
}

func TestThinDeviceIDAllocatorIsStableMonotonicAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "microvm", "thin-device-ids.json")
	first := &thinDeviceIDAllocator{path: path}
	for index, seed := range []string{"origin:a", "snapshot:a", "origin:b"} {
		id, err := first.allocate(seed)
		if err != nil {
			t.Fatalf("allocate %s: %v", seed, err)
		}
		if want := uint32(index + 1); id != want {
			t.Fatalf("allocate %s = %d, want %d", seed, id, want)
		}
	}
	stable, err := first.allocate("origin:a")
	if err != nil || stable != 1 {
		t.Fatalf("stable allocation = %d, %v", stable, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("allocator store mode = %o, want 600", info.Mode().Perm())
	}

	reloaded := &thinDeviceIDAllocator{path: path}
	stable, err = reloaded.allocate("origin:a")
	if err != nil || stable != 1 {
		t.Fatalf("reloaded stable allocation = %d, %v", stable, err)
	}
	next, err := reloaded.allocate("snapshot:b")
	if err != nil || next != 4 {
		t.Fatalf("reloaded next allocation = %d, %v", next, err)
	}
}

func TestThinDeviceIDAllocatorExhaustionAndSaveRollback(t *testing.T) {
	exhausted := &thinDeviceIDAllocator{
		loaded: true,
		store:  thinDeviceIDStore{Next: maxThinDeviceID, IDs: map[string]uint32{}},
	}
	if _, err := exhausted.allocate("no-space"); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("exhaustion error = %v", err)
	}

	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollback := &thinDeviceIDAllocator{
		path:   filepath.Join(parentFile, "ids.json"),
		loaded: true,
		store:  thinDeviceIDStore{Next: 1, IDs: map[string]uint32{}},
	}
	if _, err := rollback.allocate("first"); err == nil {
		t.Fatal("allocation with unwritable store path succeeded")
	}
	if rollback.store.Next != 1 || len(rollback.store.IDs) != 0 {
		t.Fatalf("failed save did not roll back: %+v", rollback.store)
	}
}

func assertThinIDsInRange(t *testing.T, commands string) {
	t.Helper()
	matches := regexp.MustCompile(`(?:create_thin|create_snap|thin /dev/mapper/agentcy-pool) (\d+)`).FindAllStringSubmatch(commands, -1)
	if len(matches) == 0 {
		t.Fatalf("no dm-thin ids found in commands:\n%s", commands)
	}
	for _, match := range matches {
		id, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil || id == 0 || id >= uint64(maxThinDeviceID) {
			t.Fatalf("dm-thin id %q is outside [1,%d]", match[1], maxThinDeviceID-1)
		}
	}
}

func stubHostCommands(t *testing.T, fn hostCommandRunner) func() {
	t.Helper()
	old := runHostCommand
	runHostCommand = fn
	return func() { runHostCommand = old }
}
