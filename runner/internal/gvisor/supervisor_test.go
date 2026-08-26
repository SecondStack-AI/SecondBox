//go:build linux

package gvisor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestMountSupervisorPlanRoundTrip(t *testing.T) {
	plan := MountSupervisorPlan{
		Mountpoint:    "/run/secondbox/gvisor/instance-1/workspace",
		ExpectedUUID:  "31e40cd4-5f5a-4b54-a06e-0123456789ab",
		CapacityBytes: 256 << 20,
		RunscPath:     "/opt/secondbox/bin/runsc",
		StateRoot:     "/run/secondbox/gvisor/instance-1/state",
		BundleDir:     "/run/secondbox/gvisor/instance-1/bundle",
		ContainerID:   "sbx-instance-1",
		RunscGlobal:   []string{"--network=sandbox", "--host-uds=all"},
	}
	arguments := plan.arguments()
	if arguments[0] != MountSupervisorInvocation {
		t.Fatalf("arguments[0] = %q", arguments[0])
	}
	decoded, err := parseMountSupervisorPlan(arguments[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, plan) {
		t.Fatalf("decoded plan = %#v, want %#v", decoded, plan)
	}

	holdPlan := MountSupervisorPlan{
		Mountpoint:    plan.Mountpoint,
		ExpectedUUID:  plan.ExpectedUUID,
		CapacityBytes: plan.CapacityBytes,
		Hold:          true,
	}
	decodedHold, err := parseMountSupervisorPlan(holdPlan.arguments()[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !decodedHold.Hold || decodedHold.RunscPath != "" {
		t.Fatalf("decoded hold plan = %#v", decodedHold)
	}
}

func TestMountSupervisorPlanValidation(t *testing.T) {
	if _, err := parseMountSupervisorPlan(nil); err == nil {
		t.Fatal("empty plan was accepted")
	}
	incomplete := MountSupervisorPlan{
		Mountpoint:    "/run/mnt",
		ExpectedUUID:  "31e40cd45f5a4b54a06e0123456789ab",
		CapacityBytes: 1 << 20,
		// Not hold, and no runsc identity.
	}
	if err := incomplete.validate(); err == nil ||
		!strings.Contains(err.Error(), "complete runsc launch identity") {
		t.Fatalf("incomplete compute plan error = %v", err)
	}
}

func TestParseSupervisorStatus(t *testing.T) {
	status, err := parseSupervisorStatus("ready loop_device=/dev/loop3 pid=42")
	if err != nil {
		t.Fatal(err)
	}
	if status.Kind != "ready" || status.Fields["loop_device"] != "/dev/loop3" || status.Fields["pid"] != "42" {
		t.Fatalf("status = %#v", status)
	}
	if _, err := parseSupervisorStatus(""); err == nil {
		t.Fatal("empty status line was accepted")
	}
	if _, err := parseSupervisorStatus("terminal =broken"); err == nil {
		t.Fatal("malformed status field was accepted")
	}
}

func TestVerifyImageIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image")
	content := make([]byte, 1<<20)
	content[ext4SuperblockOffset+ext4MagicWithinSB] = 0x53
	content[ext4SuperblockOffset+ext4MagicWithinSB+1] = 0xef
	uuid := []byte{
		0x31, 0xe4, 0x0c, 0xd4, 0x5f, 0x5a, 0x4b, 0x54,
		0xa0, 0x6e, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab,
	}
	copy(content[ext4UUIDReadOffset:], uuid)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()

	if err := verifyImageIdentity(image, "31e40cd4-5f5a-4b54-a06e-0123456789ab", 1<<20); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if err := verifyImageIdentity(image, "00000000-0000-0000-0000-000000000000", 1<<20); err == nil {
		t.Fatal("wrong UUID was accepted")
	}
	if err := verifyImageIdentity(image, "31e40cd4-5f5a-4b54-a06e-0123456789ab", 2<<20); err == nil {
		t.Fatal("wrong capacity was accepted")
	}
}

func TestVerifyImageIdentityRejectsNonExt4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	if err := verifyImageIdentity(image, "31e40cd45f5a4b54a06e0123456789ab", 1<<20); err == nil ||
		!strings.Contains(err.Error(), "not an ext4 filesystem") {
		t.Fatalf("non-ext4 image error = %v", err)
	}
}

func TestScanStaleLoopsSelectsWorkspaceBackedDevices(t *testing.T) {
	sysfs := t.TempDir()
	writeBacking := func(device, backing string) {
		t.Helper()
		directory := filepath.Join(sysfs, device, "loop")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "backing_file"), []byte(backing+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBacking("loop0", "/var/lib/secondbox/workspaces/ws-1/workspace.img")
	writeBacking("loop1", "/var/lib/other/data.img")
	writeBacking("loop7", "/var/lib/secondbox/workspaces/ws-2/workspace.img (deleted)")
	if err := os.MkdirAll(filepath.Join(sysfs, "sda"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysfs, "loop9"), 0o755); err != nil {
		t.Fatal(err) // Bound to nothing: no loop/backing_file.
	}

	candidates, err := scanStaleLoops(sysfs, "/dev", "/var/lib/secondbox/workspaces")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	devices := map[string]string{}
	for _, candidate := range candidates {
		devices[candidate.Device] = candidate.BackingFile
	}
	if devices["/dev/loop0"] != "/var/lib/secondbox/workspaces/ws-1/workspace.img" ||
		devices["/dev/loop7"] != "/var/lib/secondbox/workspaces/ws-2/workspace.img" {
		t.Fatalf("selected devices = %#v", devices)
	}
}

func TestScanStaleLoopsRequiresAbsoluteRoot(t *testing.T) {
	if _, err := scanStaleLoops(t.TempDir(), "/dev", "relative/root"); err == nil {
		t.Fatal("relative workspace root was accepted")
	}
}

// TestClassifyRunscExitSeparatesExitCodesFromSignals proves ordinary nonzero
// exits keep their code, delivered signals classify as signals, and only a
// non-ExitError wait failure reports wait-failure: lifecycle and terminal
// evidence depend on this distinction.
func TestClassifyRunscExitSeparatesExitCodesFromSignals(t *testing.T) {
	clean := exec.Command("/bin/sh", "-c", "exit 0")
	if err := clean.Run(); err != nil {
		t.Fatal(err)
	}
	if outcome, code := classifyRunscExit(clean, nil); outcome != "exit" || code != 0 {
		t.Fatalf("clean exit = %s/%d", outcome, code)
	}

	nonzero := exec.Command("/bin/sh", "-c", "exit 3")
	nonzeroErr := nonzero.Run()
	if outcome, code := classifyRunscExit(nonzero, nonzeroErr); outcome != "exit" || code != 3 {
		t.Fatalf("nonzero exit = %s/%d, want exit/3", outcome, code)
	}

	signaled := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	signaledErr := signaled.Run()
	if outcome, code := classifyRunscExit(signaled, signaledErr); outcome != "signal" || code != int(syscall.SIGTERM) {
		t.Fatalf("signaled exit = %s/%d, want signal/%d", outcome, code, int(syscall.SIGTERM))
	}

	if outcome, code := classifyRunscExit(clean, errors.New("wait interrupted")); outcome != "wait-failure" || code != -1 {
		t.Fatalf("wait failure = %s/%d", outcome, code)
	}
}
