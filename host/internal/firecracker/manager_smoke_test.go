package microvm

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentcy/internal/config"
	"agentcy/internal/runtimemanager"
)

func TestSmokeBootFirecracker(t *testing.T) {
	if os.Getenv("AG_MICROVM_SMOKE") != "1" {
		t.Skip("set AG_MICROVM_SMOKE=1 to boot a local Firecracker microVM")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:         requiredEnv(t, "AG_FIRECRACKER_PATH"),
		MicroVMKernelPath:       requiredEnv(t, "AG_MICROVM_KERNEL_PATH"),
		MicroVMRootfsPath:       requiredEnv(t, "AG_MICROVM_ROOTFS_PATH"),
		MicroVMWorkspaceDir:     filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:           filepath.Join(workDir, "run"),
		MicroVMLogDir:           filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:       os.Getenv("AG_MICROVM_KERNEL_ARGS"),
		MicroVMMemoryMiB:        256,
		MicroVMVCPUs:            1,
		MicroVMWorkspaceSizeMiB: 64,
		MicroVMAllowUnjailed:    true,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", runtimemanager.StartOpts{Timezone: "UTC", CompartmentID: "cmp_smoke_boot"})
	if err != nil {
		t.Fatalf("start microVM: %v", err)
	}
	defer mgr.Remove(context.Background(), instanceID)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running, err := mgr.IsRunning(ctx, instanceID)
		if err != nil {
			t.Fatalf("is running: %v", err)
		}
		if running {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("microVM did not stay running")
}

func TestSmokeGeneratedImageBootsControlAndRuntime(t *testing.T) {
	if os.Getenv("AG_MICROVM_GENERATED_SMOKE") != "1" {
		t.Skip("set AG_MICROVM_GENERATED_SMOKE=1 to boot generated microVM artifacts")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:         requiredEnv(t, "AG_FIRECRACKER_PATH"),
		MicroVMKernelPath:       requiredEnv(t, "AG_MICROVM_KERNEL_PATH"),
		MicroVMRootfsPath:       requiredEnv(t, "AG_MICROVM_ROOTFS_PATH"),
		MicroVMSharedImagePath:  os.Getenv("AG_MICROVM_SHARED_IMAGE_PATH"),
		MicroVMWorkspaceDir:     filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:           filepath.Join(workDir, "run"),
		MicroVMLogDir:           filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:       envOrDefault("AG_MICROVM_KERNEL_ARGS", "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"),
		MicroVMMemoryMiB:        envIntOrDefault("AG_MICROVM_MEMORY_MIB", 2048),
		MicroVMVCPUs:            1,
		MicroVMWorkspaceSizeMiB: envIntOrDefault("AG_MICROVM_WORKSPACE_SIZE_MIB", 256),
		MicroVMAllowUnjailed:    true,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", runtimemanager.StartOpts{Timezone: "UTC", CompartmentID: "cmp_smoke_generated"})
	if err != nil {
		t.Fatalf("start microVM: %v", err)
	}
	defer mgr.Remove(context.Background(), instanceID)
	logPath := ""
	if inst := mgr.lookup(instanceID); inst != nil {
		logPath = inst.logPath
	}

	var heartbeatErr error
	waitForSmoke(t, 30*time.Second, func() bool {
		hb, err := mgr.Heartbeat(ctx, instanceID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "last heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, logPath)
	})

	if err := mgr.ApplySecrets(ctx, instanceID, SecretBundle{Env: map[string]string{
		"AGENT_ID":                "0123456789abcdef",
		"AGENT_PLATFORM_TOKEN":    "generated-smoke-token",
		"AG_PLATFORM_API_URL":     "http://127.0.0.1:1",
		"AGENT_MODEL":             "openai:gpt-5.4",
		"MOM_BROWSER_HEADLESS":    "true",
		"MOM_BROWSER_MODE":        "managed",
		"AGENTCY_SMOKE_GENERATED": "1",
	}}); err != nil {
		t.Fatalf("apply secrets: %v\n%s", err, smokeLogPath(t, logPath))
	}

	waitForSmoke(t, 60*time.Second, func() bool {
		return strings.Contains(smokeLogPath(t, logPath), "started microVM runtime command")
	}, func() string { return smokeLogPath(t, logPath) })
}

func TestSmokeGeneratedToolExecutorImageReadiness(t *testing.T) {
	if os.Getenv("AG_MICROVM_TOOL_EXECUTOR_SMOKE") != "1" {
		t.Skip("set AG_MICROVM_TOOL_EXECUTOR_SMOKE=1 to boot generated tool-executor artifacts")
	}
	workDir := shortSmokeDir(t)
	rootfsPath := requiredEnv(t, "AG_MICROVM_ROOTFS_PATH")
	sharedImagePath := os.Getenv("AG_MICROVM_SHARED_IMAGE_PATH")
	cfg := &config.Config{
		FirecrackerPath:            requiredEnv(t, "AG_FIRECRACKER_PATH"),
		MicroVMKernelPath:          requiredEnv(t, "AG_MICROVM_KERNEL_PATH"),
		MicroVMRootfsPath:          rootfsPath,
		MicroVMSharedImagePath:     sharedImagePath,
		MicroVMToolRootfsPath:      envOrDefault("AG_MICROVM_TOOL_ROOTFS_PATH", rootfsPath),
		MicroVMToolSharedImagePath: envOrDefault("AG_MICROVM_TOOL_SHARED_IMAGE_PATH", sharedImagePath),
		MicroVMWorkspaceDir:        filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:              filepath.Join(workDir, "run"),
		MicroVMLogDir:              filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:          envOrDefault("AG_MICROVM_KERNEL_ARGS", "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"),
		MicroVMMemoryMiB:           envIntOrDefault("AG_MICROVM_MEMORY_MIB", 2048),
		MicroVMVCPUs:               1,
		MicroVMWorkspaceSizeMiB:    envIntOrDefault("AG_MICROVM_WORKSPACE_SIZE_MIB", 256),
		MicroVMAllowUnjailed:       true,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_tool_executor",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
	})
	if err != nil {
		t.Fatalf("start tool executor microVM: %v", err)
	}
	defer func() {
		if instanceID != "" {
			_ = mgr.Remove(context.Background(), instanceID)
		}
	}()
	logPath := ""
	if inst := mgr.lookup(instanceID); inst != nil {
		logPath = inst.logPath
	}

	var heartbeatErr error
	waitForSmoke(t, 30*time.Second, func() bool {
		hb, err := mgr.Heartbeat(ctx, instanceID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "tool executor heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, logPath)
	})

	writeResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpWriteFile,
		Path:      "tool-ready.txt",
		Content:   "ready",
	})
	if err != nil || writeResp.Error != "" {
		t.Fatalf("write through tool executor: resp=%+v err=%v\n%s", writeResp, err, smokeLogPath(t, logPath))
	}
	readResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "tool-ready.txt",
	})
	if err != nil || readResp.Error != "" || readResp.Content != "ready" {
		t.Fatalf("read through tool executor: resp=%+v err=%v\n%s", readResp, err, smokeLogPath(t, logPath))
	}
	mkdirResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpMkdir,
		Path:      "nested",
		Recursive: true,
	})
	if err != nil || mkdirResp.Error != "" {
		t.Fatalf("mkdir through tool executor: resp=%+v err=%v\n%s", mkdirResp, err, smokeLogPath(t, logPath))
	}
	writeResp, err = mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation:     ToolOpWriteFile,
		Path:          "nested/blob.bin",
		Encoding:      "base64",
		ContentBase64: "AQID",
	})
	if err != nil || writeResp.Error != "" {
		t.Fatalf("write buffer through tool executor: resp=%+v err=%v\n%s", writeResp, err, smokeLogPath(t, logPath))
	}
	bufferResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpReadFileBuffer,
		Path:      "nested/blob.bin",
	})
	if err != nil || bufferResp.Error != "" || bufferResp.ContentBase64 != "AQID" {
		t.Fatalf("read buffer through tool executor: resp=%+v err=%v\n%s", bufferResp, err, smokeLogPath(t, logPath))
	}
	statResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpStat,
		Path:      "nested/blob.bin",
	})
	if err != nil || statResp.Error != "" || statResp.Stat["type"] != "file" {
		t.Fatalf("stat through tool executor: resp=%+v err=%v\n%s", statResp, err, smokeLogPath(t, logPath))
	}
	listResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpReaddir,
		Path:      "nested",
	})
	if err != nil || listResp.Error != "" || len(listResp.Entries) != 1 || listResp.Entries[0].Name != "blob.bin" {
		t.Fatalf("readdir through tool executor: resp=%+v err=%v\n%s", listResp, err, smokeLogPath(t, logPath))
	}
	existsResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExists,
		Path:      "nested/blob.bin",
	})
	if err != nil || existsResp.Error != "" || existsResp.Exists == nil || !*existsResp.Exists {
		t.Fatalf("exists through tool executor: resp=%+v err=%v\n%s", existsResp, err, smokeLogPath(t, logPath))
	}
	rmResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpRm,
		Path:      "nested",
		Recursive: true,
		Force:     true,
	})
	if err != nil || rmResp.Error != "" {
		t.Fatalf("rm through tool executor: resp=%+v err=%v\n%s", rmResp, err, smokeLogPath(t, logPath))
	}
	npmResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec,
		Command:   "sh",
		Args: []string{"-lc", strings.Join([]string{
			"mkdir -p npm-smoke",
			"cd npm-smoke",
			"cat > package.json <<'JSON'\n{\"scripts\":{\"build\":\"node -e \\\"require('fs').mkdirSync('dist',{recursive:true});require('fs').writeFileSync('dist/ok.txt','ok')\\\"\"},\"dependencies\":{}}\nJSON",
			"node -v",
			"npm -v",
			"npm install --ignore-scripts",
			"npm run build",
			"test -f dist/ok.txt",
		}, "\n")},
		TimeoutMillis: 120000,
	})
	if err != nil || npmResp.Error != "" || npmResp.ExitCode != 0 {
		t.Fatalf("npm smoke through tool executor: resp=%+v err=%v\n%s", npmResp, err, smokeLogPath(t, logPath))
	}
	timeoutResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation:     ToolOpExec,
		Command:       "sh",
		Args:          []string{"-c", "sleep 1"},
		TimeoutMillis: 50,
	})
	if err == nil || !timeoutResp.TimedOut || timeoutResp.Error == "" {
		t.Fatalf("timeout through tool executor: resp=%+v err=%v\n%s", timeoutResp, err, smokeLogPath(t, logPath))
	}
	if err := mgr.Remove(ctx, instanceID); err != nil {
		t.Fatalf("remove first tool executor microVM before isolation check: %v\n%s", err, smokeLogPath(t, logPath))
	}
	instanceID = ""

	otherID, err := mgr.createAndStart(ctx, "0123456789abcdef", runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_tool_executor_other",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
	})
	if err != nil {
		t.Fatalf("start second tool executor microVM: %v\n%s", err, smokeLogPath(t, logPath))
	}
	defer mgr.Remove(context.Background(), otherID)
	otherLogPath := ""
	if inst := mgr.lookup(otherID); inst != nil {
		otherLogPath = inst.logPath
	}
	waitForSmoke(t, 30*time.Second, func() bool {
		hb, err := mgr.Heartbeat(ctx, otherID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "second tool executor heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, otherLogPath)
	})
	isolationResp, err := mgr.ExecuteTool(ctx, otherID, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "tool-ready.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "workspace file not found") {
		t.Fatalf("read isolated workspace through second executor: resp=%+v err=%v\n%s", isolationResp, err, smokeLogPath(t, otherLogPath))
	}
	if isolationResp.Error == "" {
		t.Fatalf("second compartment read first compartment workspace file: resp=%+v", isolationResp)
	}
}

func TestSmokeGoldenSnapshotRestoreGeneratedImage(t *testing.T) {
	if os.Getenv("AG_MICROVM_SNAPSHOT_SMOKE") != "1" {
		t.Skip("set AG_MICROVM_SNAPSHOT_SMOKE=1 to create and restore a local Firecracker snapshot")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:         requiredEnv(t, "AG_FIRECRACKER_PATH"),
		MicroVMKernelPath:       requiredEnv(t, "AG_MICROVM_KERNEL_PATH"),
		MicroVMRootfsPath:       requiredEnv(t, "AG_MICROVM_ROOTFS_PATH"),
		MicroVMSharedImagePath:  os.Getenv("AG_MICROVM_SHARED_IMAGE_PATH"),
		MicroVMWorkspaceDir:     filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:           filepath.Join(workDir, "run"),
		MicroVMLogDir:           filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:       envOrDefault("AG_MICROVM_KERNEL_ARGS", "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"),
		MicroVMMemoryMiB:        envIntOrDefault("AG_MICROVM_MEMORY_MIB", 2048),
		MicroVMVCPUs:            1,
		MicroVMWorkspaceSizeMiB: envIntOrDefault("AG_MICROVM_WORKSPACE_SIZE_MIB", 256),
		MicroVMAllowUnjailed:    true,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	agentID := "0123456789abcdef"
	instanceID, err := mgr.createAndStart(ctx, agentID, runtimemanager.StartOpts{Timezone: "UTC", CompartmentID: "cmp_smoke_snapshot_source"})
	if err != nil {
		t.Fatalf("start source microVM: %v", err)
	}
	sourceLogPath := ""
	if inst := mgr.lookup(instanceID); inst != nil {
		sourceLogPath = inst.logPath
	}
	defer mgr.Remove(context.Background(), instanceID)

	var heartbeatErr error
	waitForSmoke(t, 30*time.Second, func() bool {
		hb, err := mgr.Heartbeat(ctx, instanceID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "source heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, sourceLogPath)
	})

	snapshotDir := filepath.Join(workDir, "snapshots", "golden")
	manifest, err := mgr.CreateGoldenSnapshot(ctx, instanceID, snapshotDir, map[string]string{"smoke": "generated"})
	if err != nil {
		t.Fatalf("create golden snapshot: %v\n%s", err, smokeLogPath(t, sourceLogPath))
	}
	if err := mgr.Stop(ctx, instanceID); err != nil {
		t.Fatalf("stop source microVM: %v", err)
	}

	restoreStarted := time.Now()
	restoredID, err := mgr.RestoreGoldenSnapshot(ctx, manifest, RestoreSnapshotOpts{
		AgentID:           agentID,
		CompartmentID:     "cmp_smoke_snapshot_restore",
		Resume:            true,
		ClockRealtime:     true,
		HardenPostRestore: true,
		Metadata:          map[string]string{"smoke": "restore"},
	})
	restoreReturned := time.Since(restoreStarted)
	if err != nil {
		t.Fatalf("restore golden snapshot: %v\n%s", err, smokeLogPath(t, sourceLogPath))
	}
	t.Logf("golden snapshot restore returned in %s", restoreReturned)
	defer mgr.Remove(context.Background(), restoredID)
	restoredLogPath := ""
	if inst := mgr.lookup(restoredID); inst != nil {
		restoredLogPath = inst.logPath
	}

	heartbeatStarted := time.Now()
	waitForSmoke(t, 30*time.Second, func() bool {
		hb, err := mgr.Heartbeat(ctx, restoredID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "restored heartbeat error: " + errorString(heartbeatErr) + "\nsource log:\n" + smokeLogPath(t, sourceLogPath) + "\nrestored log:\n" + smokeLogPath(t, restoredLogPath)
	})
	t.Logf("golden snapshot restored heartbeat healthy in %s after restore returned; total restore-to-healthy %s", time.Since(heartbeatStarted), time.Since(restoreStarted))
}

func TestSmokeJailedTapAndTransparentRouteGeneratedImage(t *testing.T) {
	if os.Getenv("AG_MICROVM_JAILED_NET_SMOKE") != "1" {
		t.Skip("set AG_MICROVM_JAILED_NET_SMOKE=1 to run jailed Firecracker tap/iptables smoke")
	}
	if os.Geteuid() != 0 {
		t.Skip("jailed tap/iptables smoke requires root")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:                requiredEnv(t, "AG_FIRECRACKER_PATH"),
		JailerPath:                     requiredEnv(t, "AG_FIRECRACKER_JAILER_PATH"),
		MicroVMKernelPath:              requiredEnv(t, "AG_MICROVM_KERNEL_PATH"),
		MicroVMRootfsPath:              requiredEnv(t, "AG_MICROVM_ROOTFS_PATH"),
		MicroVMSharedImagePath:         os.Getenv("AG_MICROVM_SHARED_IMAGE_PATH"),
		MicroVMWorkspaceDir:            filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:                  filepath.Join(workDir, "run"),
		MicroVMLogDir:                  filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:              envOrDefault("AG_MICROVM_KERNEL_ARGS", "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"),
		MicroVMMemoryMiB:               envIntOrDefault("AG_MICROVM_MEMORY_MIB", 2048),
		MicroVMVCPUs:                   1,
		MicroVMWorkspaceSizeMiB:        envIntOrDefault("AG_MICROVM_WORKSPACE_SIZE_MIB", 256),
		MicroVMAllowUnjailed:           false,
		MicroVMJailerChrootBaseDir:     requiredEnv(t, "AG_MICROVM_JAILER_CHROOT_BASE_DIR"),
		MicroVMJailerUID:               envIntOrDefaultAllowZero("AG_MICROVM_JAILER_UID", os.Geteuid()),
		MicroVMJailerGID:               envIntOrDefaultAllowZero("AG_MICROVM_JAILER_GID", os.Getegid()),
		MicroVMJailerCgroupVersion:     envIntOrDefaultAllowZero("AG_MICROVM_JAILER_CGROUP_VERSION", 2),
		MicroVMJailerParentCgroup:      envOrDefault("AG_MICROVM_JAILER_PARENT_CGROUP", "agentcy"),
		MicroVMBridgeName:              requiredEnv(t, "AG_MICROVM_BRIDGE_NAME"),
		MicroVMBridgeCIDR:              requiredEnv(t, "AG_MICROVM_BRIDGE_CIDR"),
		MicroVMGuestIP:                 requiredEnv(t, "AG_MICROVM_GUEST_IP"),
		MicroVMTapPrefix:               envOrDefault("AG_MICROVM_TAP_PREFIX", "agfc"),
		EgressProxyTransparentHTTPPort: envIntOrDefault("AG_EGRESS_PROXY_TRANSPARENT_HTTP_PORT", 18080),
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.SetSourceBindingRegistrar(&fakeSourceBindingRegistrar{})
	mgr.SetHostEgressRouter(NewIPTablesEgressRouter())

	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_jailed",
		ProxyEgress: &runtimemanager.ProxyEgressConfig{
			Enabled:             true,
			TransparentHTTPPort: cfg.EgressProxyTransparentHTTPPort,
		},
	})
	if err != nil {
		t.Fatalf("start jailed networked microVM: %v\n%s\n%s", err, smokeJailerConfig(cfg), latestSmokeLog(t, workDir))
	}
	defer mgr.Remove(context.Background(), instanceID)
	logPath := ""
	if inst := mgr.lookup(instanceID); inst != nil {
		logPath = inst.logPath
		if inst.tapName == "" {
			t.Fatalf("expected tap-backed instance, got empty tap\n%s", smokeLogPath(t, logPath))
		}
		if inst.jailRoot == "" {
			t.Fatalf("expected jailed instance, got empty jail root\n%s", smokeLogPath(t, logPath))
		}
	}
	if err := mgr.ApplySecrets(ctx, instanceID, SecretBundle{Env: map[string]string{
		"AGENT_ID":                "0123456789abcdef",
		"AGENT_PLATFORM_TOKEN":    "generated-jailed-smoke-token",
		"PLATFORM_API_URL":        "http://127.0.0.1:1",
		"AG_FLUE_STORE_URL":       "http://127.0.0.1:1/api/agents/0123456789abcdef/flue-store",
		"MOM_BROWSER_HEADLESS":    "true",
		"MOM_BROWSER_MODE":        "managed",
		"AGENTCY_SMOKE_GENERATED": "1",
	}}); err != nil {
		t.Fatalf("apply jailed smoke secrets: %v\n%s", err, smokeLogPath(t, logPath))
	}

	var heartbeatErr error
	waitForSmoke(t, 45*time.Second, func() bool {
		hb, err := mgr.Heartbeat(ctx, instanceID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, logPath)
	})
}

func requiredEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required for AG_MICROVM_SMOKE=1", key)
	}
	return value
}

func shortSmokeDir(t *testing.T) string {
	t.Helper()
	parent := os.Getenv("AG_MICROVM_SMOKE_PARENT_DIR")
	if parent == "" {
		parent = "/tmp"
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("create smoke parent dir: %v", err)
	}
	dir, err := os.MkdirTemp(parent, "agfc-smoke-")
	if err != nil {
		t.Fatalf("create short smoke dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func smokeLog(t *testing.T, mgr *Manager, instanceID string) string {
	t.Helper()
	inst := mgr.lookup(instanceID)
	if inst == nil || inst.logPath == "" {
		return ""
	}
	return smokeLogPath(t, inst.logPath)
}

func smokeLogPath(t *testing.T, logPath string) string {
	t.Helper()
	if logPath == "" {
		return ""
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return err.Error()
	}
	if len(data) > 16*1024 {
		data = data[len(data)-16*1024:]
	}
	return string(data)
}

func latestSmokeLog(t *testing.T, workDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(workDir, "logs", "*.log"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool {
		left, leftErr := os.Stat(matches[i])
		right, rightErr := os.Stat(matches[j])
		if leftErr != nil || rightErr != nil {
			return matches[i] < matches[j]
		}
		return left.ModTime().After(right.ModTime())
	})
	return smokeLogPath(t, matches[0])
}

func smokeJailerConfig(cfg *config.Config) string {
	return "jailer config: uid=" + strconv.Itoa(cfg.MicroVMJailerUID) +
		" gid=" + strconv.Itoa(cfg.MicroVMJailerGID) +
		" cgroup_version=" + strconv.Itoa(cfg.MicroVMJailerCgroupVersion) +
		" parent_cgroup=" + cfg.MicroVMJailerParentCgroup +
		" chroot_base=" + cfg.MicroVMJailerChrootBaseDir
}

func waitForSmoke(t *testing.T, timeout time.Duration, check func() bool, detail func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for generated microVM smoke condition\n%s", detail())
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envIntOrDefaultAllowZero(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func errorString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
