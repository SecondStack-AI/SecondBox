package firecracker

import (
	"context"
	"crypto/sha256"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func TestSmokeBootFirecracker(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_FIRECRACKER") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1 to boot a local Firecracker microVM")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:         requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMWorkspaceDir:     filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:           filepath.Join(workDir, "run"),
		MicroVMLogDir:           filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
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
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{Timezone: "UTC", CompartmentID: "cmp_smoke_boot"}))
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
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_GENERATED_IMAGE") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_GENERATED_IMAGE=1 to boot generated microVM artifacts")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:         requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMWorkspaceDir:     filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:           filepath.Join(workDir, "run"),
		MicroVMLogDir:           filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:        requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:            1,
		MicroVMWorkspaceSizeMiB: requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:    true,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{Timezone: "UTC", CompartmentID: "cmp_smoke_generated"}))
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
		"SECONDBOX_SANDBOX_ID":            "0123456789abcdef",
		"SECONDBOX_INSTANCE_ID":           "cmp_smoke_generated",
		"SECONDBOX_RUNTIME_CREDENTIAL_ID": "generated-smoke-credential",
	}}); err != nil {
		t.Fatalf("apply secrets: %v\n%s", err, smokeLogPath(t, logPath))
	}

	waitForSmoke(t, 60*time.Second, func() bool {
		return strings.Contains(smokeLogPath(t, logPath), "started microVM runtime command")
	}, func() string { return smokeLogPath(t, logPath) })
}

func TestSmokeGeneratedToolExecutorImageReadiness(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_TOOL_EXECUTOR") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_TOOL_EXECUTOR=1 to boot generated tool-executor artifacts")
	}
	for name, value := range map[string]string{
		"SECONDBOX_TEST_SECRET":      "host-secret-must-not-cross-vm-boundary",
		"ANTHROPIC_API_KEY":          "host-anthropic-secret-must-not-cross-vm-boundary",
		"OPENAI_API_KEY":             "host-openai-secret-must-not-cross-vm-boundary",
		"AWS_ACCESS_KEY_ID":          "host-aws-access-key-must-not-cross-vm-boundary",
		"AWS_SECRET_ACCESS_KEY":      "host-aws-secret-must-not-cross-vm-boundary",
		"SECONDBOX_TEST_SECRET_PATH": "/host/secret-must-not-cross-vm-boundary",
	} {
		t.Setenv(name, value)
	}
	workDir := shortSmokeDir(t)
	rootfsPath := requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH")
	sharedImagePath := requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH")
	cfg := &config.Config{
		FirecrackerPath:            requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:          rootfsPath,
		MicroVMSharedImagePath:     sharedImagePath,
		MicroVMToolRootfsPath:      rootfsPath,
		MicroVMToolSharedImagePath: sharedImagePath,
		MicroVMPublicKeyPath:       requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:     requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMWorkspaceDir:        filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:              filepath.Join(workDir, "run"),
		MicroVMLogDir:              filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:           requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:               1,
		MicroVMWorkspaceSizeMiB:    requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:       true,
	}
	rejectedCfg := *cfg
	rejectedCfg.MicroVMPublicKeySHA256 = strings.Repeat("0", sha256.Size*2)
	if _, err := New(&rejectedCfg); err == nil || !strings.Contains(err.Error(), "PUBLIC_KEY_SHA256 mismatch") {
		t.Fatalf("mismatched signed-artifact trust anchor was not rejected: %v", err)
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_tool_executor",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
	}))
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

	envResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec,
		Command:   "env",
	})
	if err != nil || envResp.Error != "" || envResp.ExitCode != 0 {
		t.Fatalf("inspect tool executor environment: resp=%+v err=%v\n%s", envResp, err, smokeLogPath(t, logPath))
	}
	for _, name := range []string{
		"SECONDBOX_TEST_SECRET",
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
	} {
		for _, line := range strings.Split(envResp.Stdout, "\n") {
			if strings.HasPrefix(line, name+"=") {
				t.Fatalf("credential variable %s reached tool executor environment\n%s", name, smokeLogPath(t, logPath))
			}
		}
	}
	if strings.Contains(envResp.Stdout, "model-auth") {
		t.Fatalf("model-auth credential path reached tool executor environment\n%s", smokeLogPath(t, logPath))
	}

	egressResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpExec,
		Command:   "sh",
		Args: []string{"-c", strings.Join([]string{
			"if command -v curl >/dev/null 2>&1; then",
			"  curl --fail --silent --show-error --connect-timeout 3 --max-time 5 http://1.1.1.1/ >/dev/null",
			"elif command -v wget >/dev/null 2>&1; then",
			"  wget -q -T 5 -O /dev/null http://1.1.1.1/",
			"else",
			"  exit 125",
			"fi",
		}, "\n")},
		TimeoutMillis: 10000,
	})
	if egressResp.ExitCode == 125 {
		t.Fatalf("tool executor image has neither curl nor wget; egress assertion did not run\n%s", smokeLogPath(t, logPath))
	}
	if err == nil && egressResp.Error == "" && egressResp.ExitCode == 0 {
		t.Fatalf("tool executor reached an external IP despite network-none policy: resp=%+v\n%s", egressResp, smokeLogPath(t, logPath))
	}

	escapeResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "../../../../etc/passwd",
	})
	if err == nil || escapeResp.Error == "" {
		t.Fatalf("workspace traversal was not rejected: resp=%+v err=%v\n%s", escapeResp, err, smokeLogPath(t, logPath))
	}

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
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_CONTINUITY") != "1" {
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
	}
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatalf("shut down first tool executor manager before continuity check: %v\n%s", err, smokeLogPath(t, logPath))
	}
	instanceID = ""

	restartedManager, err := New(cfg)
	if err != nil {
		t.Fatalf("restart tool executor manager: %v", err)
	}
	if err := restartedManager.Start(ctx); err != nil {
		t.Fatalf("start restarted tool executor manager: %v", err)
	}
	defer restartedManager.Shutdown(context.Background())
	continuityID, err := restartedManager.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_tool_executor",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
	}))
	if err != nil {
		t.Fatalf("remount tool executor workspace after manager restart: %v\n%s", err, latestSmokeLog(t, workDir))
	}
	continuityLogPath := ""
	if inst := restartedManager.lookup(continuityID); inst != nil {
		continuityLogPath = inst.logPath
	}
	waitForSmoke(t, 30*time.Second, func() bool {
		hb, err := restartedManager.Heartbeat(ctx, continuityID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "restarted tool executor heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, continuityLogPath)
	})
	continuityResp, err := restartedManager.ExecuteTool(ctx, continuityID, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "tool-ready.txt",
	})
	if err != nil || continuityResp.Error != "" || continuityResp.Content != "ready" {
		t.Fatalf("read remounted workspace after manager restart: resp=%+v err=%v\n%s", continuityResp, err, smokeLogPath(t, continuityLogPath))
	}
	if err := restartedManager.Remove(ctx, continuityID); err != nil {
		t.Fatalf("remove continuity tool executor before second-compartment check: %v\n%s", err, smokeLogPath(t, continuityLogPath))
	}
	continuityID = ""
	otherID, err := restartedManager.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_tool_executor_other",
		RuntimeClass:  runtimemanager.RuntimeClassToolExecutor,
	}))
	if err != nil {
		t.Fatalf("start second tool executor microVM: %v\n%s", err, smokeLogPath(t, logPath))
	}
	defer restartedManager.Remove(context.Background(), otherID)
	otherLogPath := ""
	if inst := restartedManager.lookup(otherID); inst != nil {
		otherLogPath = inst.logPath
	}
	waitForSmoke(t, 30*time.Second, func() bool {
		hb, err := restartedManager.Heartbeat(ctx, otherID)
		heartbeatErr = err
		return err == nil && hb.Healthy
	}, func() string {
		return "second tool executor heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, otherLogPath)
	})
	isolationResp, err := restartedManager.ExecuteTool(ctx, otherID, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "tool-ready.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "workspace file not found") {
		t.Fatalf("read isolated workspace through second executor: resp=%+v err=%v\n%s", isolationResp, err, smokeLogPath(t, otherLogPath))
	}
	if isolationResp.Error == "" {
		t.Fatalf("second compartment read first compartment workspace file: resp=%+v", isolationResp)
	}
	if err := restartedManager.Remove(ctx, otherID); err != nil {
		t.Fatalf("remove isolated second-compartment executor before warm-reuse check: %v\n%s", err, smokeLogPath(t, otherLogPath))
	}
	restartedManager.cfg.MicroVMToolVMIdleTTL = 100 * time.Millisecond
	warmOpts := runtimemanager.StartOpts{Timezone: "UTC"}
	warmID, warmWrite, err := restartedManager.ExecuteToolLeased(ctx, "0123456789abcdef", "cmp_smoke_warm_reuse", warmOpts, ToolExecRequest{
		Operation: ToolOpWriteFile,
		Path:      "warm-ready.txt",
		Content:   "warm-ready",
	})
	if err != nil || warmWrite.Error != "" {
		t.Fatalf("write through warm tool executor: resp=%+v err=%v\n%s", warmWrite, err, latestSmokeLog(t, workDir))
	}
	reusedID, warmRead, err := restartedManager.ExecuteToolLeased(ctx, "0123456789abcdef", "cmp_smoke_warm_reuse", warmOpts, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "warm-ready.txt",
	})
	if err != nil || warmRead.Error != "" || warmRead.Content != "warm-ready" || reusedID != warmID {
		t.Fatalf("warm executor was not reused: first=%s second=%s resp=%+v err=%v\n%s", warmID, reusedID, warmRead, err, latestSmokeLog(t, workDir))
	}
	if reaped := restartedManager.sweepIdleToolVMs(time.Now().Add(time.Second)); reaped != 1 {
		t.Fatalf("idle warm executor reap count = %d, want 1", reaped)
	}
	waitForSmoke(t, 30*time.Second, func() bool {
		return restartedManager.lookup(warmID) == nil
	}, func() string {
		return "idle warm executor remained active\n" + latestSmokeLog(t, workDir)
	})
	remountedID, remountedRead, err := restartedManager.ExecuteToolLeased(ctx, "0123456789abcdef", "cmp_smoke_warm_reuse", warmOpts, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "warm-ready.txt",
	})
	if err != nil || remountedRead.Error != "" || remountedRead.Content != "warm-ready" || remountedID == warmID {
		t.Fatalf("idle-stop workspace remount failed: first=%s remounted=%s resp=%+v err=%v\n%s", warmID, remountedID, remountedRead, err, latestSmokeLog(t, workDir))
	}
}

func TestSmokeGoldenSnapshotCreateGeneratedImage(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_SNAPSHOT") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_SNAPSHOT=1 to create a local Firecracker snapshot")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:         requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:    requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:  requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMWorkspaceDir:     filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:           filepath.Join(workDir, "run"),
		MicroVMLogDir:           filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:        requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:            1,
		MicroVMWorkspaceSizeMiB: requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:    true,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	sandboxID := "0123456789abcdef"
	instanceID, err := mgr.createAndStart(ctx, sandboxID, smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{Timezone: "UTC", CompartmentID: "cmp_smoke_snapshot_source"}))
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
	if err := verifySnapshotArtifacts(manifest); err != nil {
		t.Fatalf("verify created golden snapshot artifacts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotDir, "manifest.json")); err != nil {
		t.Fatalf("stat golden snapshot manifest: %v", err)
	}
}

func TestSmokeJailedTapGeneratedImage(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_JAILED_NETWORK") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_JAILED_NETWORK=1 to run jailed Firecracker tap/iptables smoke")
	}
	if os.Geteuid() != 0 {
		t.Skip("jailed tap/iptables smoke requires root")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:            requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		JailerPath:                 requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH"),
		MicroVMKernelPath:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:     requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:       requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:     requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMWorkspaceDir:        filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:              filepath.Join(workDir, "run"),
		MicroVMLogDir:              filepath.Join(workDir, "logs"),
		MicroVMKernelArgs:          requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:           requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:               1,
		MicroVMWorkspaceSizeMiB:    requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:       false,
		MicroVMJailerChrootBaseDir: requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"),
		MicroVMJailerUID:           requiredNonNegativeEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID"),
		MicroVMJailerGID:           requiredNonNegativeEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID"),
		MicroVMJailerCgroupVersion: requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION"),
		MicroVMJailerParentCgroup:  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT"),
		MicroVMBridgeName:          requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME"),
		MicroVMBridgeCIDR:          requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR"),
		MicroVMGuestIP:             requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_GUEST_IP"),
		MicroVMTapPrefix:           requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX"),
	}
	configureSmokeNetworkPolicy(t, cfg)
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	instanceID, err := mgr.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_jailed",
	}))
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
		"SECONDBOX_SANDBOX_ID":            "0123456789abcdef",
		"SECONDBOX_INSTANCE_ID":           "cmp_smoke_jailed",
		"SECONDBOX_RUNTIME_CREDENTIAL_ID": "generated-jailed-smoke-credential",
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

	secondID, err := mgr.createAndStart(ctx, "0123456789abcdef", smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_smoke_jailed_concurrent",
	}))
	if err != nil {
		t.Fatalf("start concurrent jailed compartment: %v\n%s\n%s", err, smokeJailerConfig(cfg), latestSmokeLog(t, workDir))
	}
	defer mgr.Remove(context.Background(), secondID)
	first := mgr.lookup(instanceID)
	second := mgr.lookup(secondID)
	if first == nil || second == nil || first.id == second.id || first.tapName == second.tapName || first.guestIP == second.guestIP || first.workspacePath == second.workspacePath {
		t.Fatalf("concurrent jailed compartments are not isolated: first=%+v second=%+v", first, second)
	}
	waitForSmoke(t, 45*time.Second, func() bool {
		firstHeartbeat, firstErr := mgr.Heartbeat(ctx, instanceID)
		secondHeartbeat, secondErr := mgr.Heartbeat(ctx, secondID)
		return firstErr == nil && secondErr == nil && firstHeartbeat.Healthy && secondHeartbeat.Healthy
	}, func() string {
		return "concurrent jailed compartments did not remain healthy\n" + smokeLogPath(t, logPath) + "\n" + smokeLogPath(t, second.logPath)
	})
}

func requiredEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required for SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1", key)
	}
	return value
}

func configureSmokeNetworkPolicy(t *testing.T, cfg *config.Config) {
	t.Helper()
	cfg.NetworkPolicyNFTPath = requiredEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH")
	cfg.NetworkPolicyMaximumDNSPins = requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS")
	ttl, err := time.ParseDuration(requiredEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL"))
	if err != nil || ttl <= 0 {
		t.Fatalf("SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL must be a positive duration")
	}
	cfg.NetworkPolicyMaximumDNSTTL = ttl
	for _, raw := range strings.Split(requiredEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES"), ",") {
		address, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			t.Fatalf("SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES contains %q: %v", raw, err)
		}
		cfg.NetworkPolicyRunnerAddresses = append(cfg.NetworkPolicyRunnerAddresses, address)
	}
	for _, raw := range strings.Split(requiredEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS"), ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			t.Fatalf("SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS contains %q: %v", raw, err)
		}
		cfg.NetworkPolicyManagementCIDRs = append(cfg.NetworkPolicyManagementCIDRs, prefix)
	}
	upstream, err := netip.ParseAddrPort(requiredEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM"))
	if err != nil || upstream.Port() == 0 {
		t.Fatalf("SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM must be an IP:port")
	}
	cfg.NetworkPolicyDNSUpstream = upstream
}

func smokeGuestProtocolOpts(t *testing.T, cfg *config.Config, opts runtimemanager.StartOpts) runtimemanager.StartOpts {
	t.Helper()
	cfg.MicroVMGuestControlVsockPort = uint32(requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT"))
	cfg.MicroVMGuestProtocolVsockPort = uint32(requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT"))
	if cfg.MicroVMGuestControlVsockPort == cfg.MicroVMGuestProtocolVsockPort {
		t.Fatal("guest control and protocol vsock ports must be distinct")
	}
	heartbeat, err := time.ParseDuration(requiredEnv(t, "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL"))
	if err != nil || heartbeat < time.Millisecond || heartbeat > maxGuestHeartbeatInterval {
		t.Fatalf("SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL must be from 1ms through %s", maxGuestHeartbeatInterval)
	}
	cfg.MicroVMGuestHeartbeatInterval = heartbeat
	manifestPath := filepath.Join(filepath.Dir(cfg.MicroVMKernelPath), "manifest.json")
	manifest, err := loadSignedArtifactManifest(manifestPath)
	if err != nil {
		t.Fatalf("load smoke signed artifact manifest: %v", err)
	}
	digest, err := fileSHA256(manifestPath)
	if err != nil {
		t.Fatalf("hash smoke signed artifact manifest: %v", err)
	}
	opts.SandboxGeneration = 1
	opts.GuestBuildID = manifest.ArtifactVersion
	opts.ImageManifestDigest = "sha256:" + digest
	opts.ToolchainManifestDigest = opts.ImageManifestDigest
	opts.MandatoryGuestFeatures = []string{"streaming_exec", "descriptor_pinned_filesystem"}
	return opts
}

func shortSmokeDir(t *testing.T) string {
	t.Helper()
	parent := "/tmp"
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

func requiredPositiveEnvInt(t *testing.T, key string) int {
	t.Helper()
	raw := requiredEnv(t, key)
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive integer", key)
	}
	return value
}

func requiredNonNegativeEnvInt(t *testing.T, key string) int {
	t.Helper()
	raw := requiredEnv(t, key)
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		t.Fatalf("%s must be a non-negative integer", key)
	}
	return value
}

func errorString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
