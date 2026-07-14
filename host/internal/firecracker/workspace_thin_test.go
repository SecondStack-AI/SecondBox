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

type fakeHostExitError struct{ code int }

func (e fakeHostExitError) Error() string { return "host command failed" }
func (e fakeHostExitError) ExitCode() int { return e.code }

func TestEnsureThinWorkspaceCreatesAndFormatsDevice(t *testing.T) {
	var calls []string
	restore := stubHostCommands(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "dmsetup info ") {
			return nil, errors.New("not found")
		}
		if strings.HasPrefix(call, "blkid ") {
			return nil, fakeHostExitError{code: 2}
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
		"blkid -o value -s TYPE /dev/mapper/agentcy-ws-agent-1-cmp_a",
		"mkfs.ext4 -F -q -E lazy_itable_init=1,lazy_journal_init=1,nodiscard /dev/mapper/agentcy-ws-agent-1-cmp_a",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls missing %q:\n%s", want, joined)
		}
	}
	assertThinIDsInRange(t, joined)
}

func TestEnsureThinWorkspaceReactivationDoesNotReformatExistingDevice(t *testing.T) {
	var createAttempts, formatAttempts int
	restore := stubHostCommands(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(call, "dmsetup info "):
			return nil, errors.New("inactive")
		case strings.Contains(call, " create_thin "):
			createAttempts++
			if createAttempts > 1 {
				return []byte("File exists"), errors.New("device exists")
			}
		case strings.HasPrefix(call, "blkid "):
			if formatAttempts > 0 {
				return []byte("ext4"), nil
			}
			return nil, fakeHostExitError{code: 2}
		case strings.HasPrefix(call, "mkfs.ext4 "):
			formatAttempts++
		}
		return nil, nil
	})
	defer restore()

	m := &Manager{cfg: &config.Config{
		MicroVMWorkspaceBackend: "dm-thin",
		MicroVMThinPoolDevice:   "/dev/mapper/agentcy-pool",
		MicroVMWorkspaceSizeMiB: 1024,
	}}
	for wake := 0; wake < 2; wake++ {
		if _, err := m.ensureThinWorkspace(context.Background(), "agent-reboot", "cmp_a"); err != nil {
			t.Fatalf("ensure thin workspace wake %d: %v", wake+1, err)
		}
	}
	if formatAttempts != 1 {
		t.Fatalf("mkfs attempts = %d, want exactly one before reactivation", formatAttempts)
	}
}

func TestEnsureThinWorkspaceFormatsActiveDeviceLeftUnformattedByCrash(t *testing.T) {
	var calls []string
	restore := stubHostCommands(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "dmsetup info ") {
			return []byte("active"), nil
		}
		if strings.HasPrefix(call, "blkid ") {
			return nil, fakeHostExitError{code: 2}
		}
		return nil, nil
	})
	defer restore()

	m := &Manager{cfg: &config.Config{MicroVMThinPoolDevice: "/dev/mapper/agentcy-pool", MicroVMWorkspaceSizeMiB: 1024}}
	if _, err := m.ensureThinWorkspace(context.Background(), "agent-crash", "cmp_a"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "blkid -o value -s TYPE /dev/mapper/agentcy-ws-agent-crash-cmp_a") ||
		!strings.Contains(joined, "mkfs.ext4 -F -q") {
		t.Fatalf("active unformatted device was not recovered:\n%s", joined)
	}
	if strings.Contains(joined, "create_thin") {
		t.Fatalf("active device was recreated:\n%s", joined)
	}
}

func TestEnsureThinWorkspaceSeedsNewDeviceFromCompartmentWorkspace(t *testing.T) {
	dataDir := t.TempDir()
	defaultSeedDir := filepath.Join(dataDir, "agents", "agent-seeded", "workspace")
	if err := os.MkdirAll(defaultSeedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedDir := filepath.Join(dataDir, "agents", "agent-seeded", "compartments", "cmp_a", "workspace")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var calls []string
	restore := stubHostCommands(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "dmsetup info ") {
			return nil, errors.New("not found")
		}
		if strings.HasPrefix(call, "blkid ") {
			return nil, fakeHostExitError{code: 2}
		}
		return nil, nil
	})
	defer restore()
	m := &Manager{cfg: &config.Config{
		DataDir:                 dataDir,
		MicroVMThinPoolDevice:   "/dev/mapper/agentcy-pool",
		MicroVMWorkspaceSizeMiB: 1024,
		MicroVMWorkspaceBackend: "dm-thin",
	}}
	if _, err := m.ensureThinWorkspace(context.Background(), "agent-seeded", "cmp_a"); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(calls, "\n"); !strings.Contains(joined, "mkfs.ext4 -F -q -E lazy_itable_init=1,lazy_journal_init=1,nodiscard -d "+seedDir) {
		t.Fatalf("mkfs did not seed from %s:\n%s", seedDir, joined)
	}
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
