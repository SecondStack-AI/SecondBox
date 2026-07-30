package firecracker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func TestMicroVMInitRemovesForbiddenDeviceNodesBeforeEntrypoint(t *testing.T) {
	initScript, err := os.ReadFile("../../scripts/microvm-image/init")
	if err != nil {
		t.Fatalf("read microVM init: %v", err)
	}
	content := string(initScript)
	devtmpfs := strings.Index(content, "mount -t devtmpfs devtmpfs /dev")
	devpts := strings.Index(content, "mount -t devpts")
	ptmx := strings.Index(content, "ln -s pts/ptmx /dev/ptmx")
	restrictDevices := strings.Index(content, "rm -f /dev/mem /dev/kmem /dev/port /dev/kvm /dev/net/tun")
	guestEntrypoint := strings.Index(content, "exec /usr/local/bin/secondbox-runner-guest-entrypoint")
	if devtmpfs < 0 || devpts <= devtmpfs || ptmx <= devpts ||
		restrictDevices <= ptmx || guestEntrypoint <= restrictDevices {
		t.Fatalf("microVM init must establish PTY devices, then remove forbidden devices before the guest entrypoint")
	}
}

func TestThreatModelJailedGuestEscapeAndResourceExhaustion(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_THREAT") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_THREAT=1 to run hostile workloads in a jailed KVM guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("host threat qualification requires root for jailer and cgroup enforcement")
	}
	workDir := shortSmokeDir(t)
	memoryMiB := threatRequiredPositiveInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB")
	workspaceMiB := threatRequiredPositiveInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB")
	cgroupVersion := threatRequiredPositiveInt(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION")
	jailerUID := threatRequiredNonNegativeInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID")
	jailerGID := threatRequiredNonNegativeInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID")
	cfg := &config.Config{
		FirecrackerPath:            requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		JailerPath:                 requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH"),
		MicroVMKernelPath:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:     requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMToolRootfsPath:      requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMToolSharedImagePath: requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:       requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:     requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMRunDir:              filepath.Join(workDir, "run"),
		MicroVMLogDir:              filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:           memoryMiB,
		MicroVMVCPUs:               1,
		MicroVMWorkspaceSizeMiB:    workspaceMiB,
		MicroVMAllowUnjailed:       false,
		MicroVMJailerChrootBaseDir: requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"),
		MicroVMJailerUID:           jailerUID,
		MicroVMJailerGID:           jailerGID,
		MicroVMJailerCgroupVersion: cgroupVersion,
		MicroVMJailerParentCgroup:  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT"),
	}
	hostSentinel := filepath.Join(workDir, "host-only-sentinel")
	hostContent := []byte("host boundary must remain outside the guest")
	if err := os.WriteFile(hostSentinel, hostContent, 0o600); err != nil {
		t.Fatal(err)
	}
	hostDigest := sha256.Sum256(hostContent)

	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_threat_hostile",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
	}))
	if err != nil {
		t.Fatalf("start jailed hostile guest: %v\n%s", err, latestSmokeLog(t, workDir))
	}
	defer mgr.Remove(context.Background(), instanceID)
	instance := mgr.lookup(instanceID)
	if instance == nil || instance.jailRoot == "" {
		t.Fatalf("hostile guest did not start through jailer: %+v", instance)
	}
	waitForThreatGuest(t, mgr, instanceID, instance.logPath)

	deviceProbe, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec,
		Command:   "sh",
		Args: []string{"-ceu", `
for forbidden in /dev/kvm /dev/mem /dev/net/tun "$1"; do
	if test -e "$forbidden"; then
		printf 'unexpected-device-or-host-path:%s' "$forbidden"
		exit 42
	fi
done
ln -s /etc/passwd escape-link
if mknod fake-device c 1 1 2>/dev/null; then printf created; else printf denied; fi
`, "threat-probe", hostSentinel},
		TimeoutMillis: 5000,
	})
	if err != nil || deviceProbe.Error != "" || deviceProbe.ExitCode != 0 || (deviceProbe.Stdout != "created" && deviceProbe.Stdout != "denied") {
		t.Fatalf("guest device/host-path probe failed closed incorrectly: response=%+v err=%v\n%s", deviceProbe, err, smokeLogPath(t, instance.logPath))
	}
	escape, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{Operation: ToolOpReadFile, Path: "escape-link"})
	if err == nil || escape.Error == "" {
		t.Fatalf("guest workspace symlink escaped to guest system path: response=%+v err=%v", escape, err)
	}
	if deviceProbe.Stdout == "created" {
		device, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{Operation: ToolOpReadFile, Path: "fake-device"})
		if err == nil || device.Error == "" {
			t.Fatalf("guest device node was readable through workspace API: response=%+v err=%v", device, err)
		}
	}

	cpu, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec, Command: "sh", Args: []string{"-c", "while :; do :; done"}, TimeoutMillis: 150,
	})
	if err == nil || !cpu.TimedOut || cpu.Error == "" {
		t.Fatalf("CPU exhaustion was not terminated by the operation deadline: response=%+v err=%v", cpu, err)
	}

	output, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec, Command: "sh", Args: []string{"-c", "yes threat-output | head -c 2097152"}, TimeoutMillis: 5000,
	})
	if err != nil || output.Error != "" || output.ExitCode != 0 || len(output.Stdout) > 256<<10 {
		t.Fatalf("guest output exhaustion was not bounded: response-bytes=%d response=%+v err=%v", len(output.Stdout), output, err)
	}

	diskCommand := fmt.Sprintf("dd if=/dev/zero of=pressure.bin bs=1048576 count=%d conv=fsync 2>/dev/null", workspaceMiB*2)
	disk, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec, Command: "sh", Args: []string{"-c", diskCommand}, TimeoutMillis: 30000,
	})
	if err == nil && disk.Error == "" && disk.ExitCode == 0 {
		t.Fatalf("guest disk exhaustion unexpectedly succeeded: response=%+v", disk)
	}
	cleanup, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec, Command: "sh", Args: []string{"-c", "rm -f pressure.bin escape-link fake-device; sync"}, TimeoutMillis: 10000,
	})
	if err != nil || cleanup.Error != "" || cleanup.ExitCode != 0 {
		t.Fatalf("guest did not recover cleanup capacity after pressure: response=%+v err=%v", cleanup, err)
	}
	waitForThreatGuest(t, mgr, instanceID, instance.logPath)

	retained, err := os.ReadFile(hostSentinel)
	if err != nil || sha256.Sum256(retained) != hostDigest {
		t.Fatalf("host sentinel changed after hostile guest workload: err=%v", err)
	}
}

func waitForThreatGuest(t *testing.T, mgr *Manager, instanceID, logPath string) {
	t.Helper()
	var heartbeatErr error
	waitForSmoke(t, 30*time.Second, func() bool {
		heartbeat, err := mgr.Heartbeat(context.Background(), instanceID)
		heartbeatErr = err
		return err == nil && heartbeat.Healthy
	}, func() string {
		return "hostile guest did not remain healthy: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, logPath)
	})
}

func threatRequiredPositiveInt(t *testing.T, name string) int {
	t.Helper()
	value := threatRequiredNonNegativeInt(t, name)
	if value == 0 {
		t.Fatalf("%s must be positive", name)
	}
	return value
}

func threatRequiredNonNegativeInt(t *testing.T, name string) int {
	t.Helper()
	raw := strings.TrimSpace(requiredEnv(t, name))
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		t.Fatalf("%s must be a non-negative integer", name)
	}
	return value
}
