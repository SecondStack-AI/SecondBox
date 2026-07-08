package microvm

import (
	"context"
	"errors"
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
}

func stubHostCommands(t *testing.T, fn hostCommandRunner) func() {
	t.Helper()
	old := runHostCommand
	runHostCommand = fn
	return func() { runHostCommand = old }
}
