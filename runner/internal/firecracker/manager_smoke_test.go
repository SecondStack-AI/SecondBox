package firecracker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func TestSmokeBootFirecracker(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_FIRECRACKER") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1 to boot a local Firecracker microVM")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:                  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		RunnerWorkspaceRoot:              filepath.Join(workDir, "durable-workspaces"),
		MicroVMRunDir:                    filepath.Join(workDir, "run"),
		MicroVMLogDir:                    filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "snapshot-templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 256,
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          64,
		MicroVMAllowUnjailed:             true,
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx := context.Background()
	workspaceStore, err := workspacestore.New(
		ctx,
		workspacestore.Config{
			Root:                  cfg.RunnerWorkspaceRoot,
			TemplateCapacityBytes: int64(cfg.MicroVMWorkspaceSizeMiB) << 20,
		},
	)
	if err != nil {
		t.Fatalf("new smoke WorkspaceStore: %v", err)
	}
	if err := mgr.SetWorkspaceStore(workspaceStore); err != nil {
		t.Fatalf("bind smoke WorkspaceStore: %v", err)
	}
	const (
		sandboxID   = "0123456789abcdef"
		workspaceID = "workspace-smoke-boot"
	)
	if _, err := workspaceStore.Create(ctx, workspacestore.CreateWorkspaceRequest{
		Mutation: workspacestore.Mutation{
			OperationID: "create-smoke-boot",
			WorkspaceID: workspaceID,
			FencingToken: []byte(
				"01234567890123456789012345678901",
			),
		},
		CapacityBytes: 64 << 20,
	}); err != nil {
		t.Fatalf("create smoke Workspace: %v", err)
	}
	attachment, err := workspaceStore.Open(ctx, workspaceID, 1)
	if err != nil {
		t.Fatalf("open smoke Workspace attachment: %v", err)
	}
	instanceID, err := mgr.createAndStart(ctx, sandboxID, smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:            "UTC",
		CompartmentID:       "cmp_smoke_boot",
		WorkspaceAttachment: attachment,
	}))
	if err != nil {
		_ = attachment.Close()
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
		FirecrackerPath:                  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:           requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMRunDir:                    filepath.Join(workDir, "run"),
		MicroVMLogDir:                    filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "snapshot-templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:             true,
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
		FirecrackerPath:                  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:                rootfsPath,
		MicroVMSharedImagePath:           sharedImagePath,
		MicroVMToolRootfsPath:            rootfsPath,
		MicroVMToolSharedImagePath:       sharedImagePath,
		MicroVMPublicKeyPath:             requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:           requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMRunDir:                    filepath.Join(workDir, "run"),
		MicroVMLogDir:                    filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "snapshot-templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:             true,
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

func TestSmokeRunnerLocalSnapshotRestore(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_FIRECRACKER") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1 to qualify runner-local Snapshot restore")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:                  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:           requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:             requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:           requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMRunDir:                    filepath.Join(workDir, "run"),
		MicroVMLogDir:                    filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "snapshot-templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          64,
		MicroVMAllowUnjailed:             true,
		MicroVMToolRootfsPath:            requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMToolSharedImagePath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
	}
	ctx := context.Background()
	workspaceStore, err := workspacestore.New(
		ctx,
		workspacestore.Config{
			Root:                  filepath.Join(workDir, "durable-workspaces"),
			TemplateCapacityBytes: int64(cfg.MicroVMWorkspaceSizeMiB) << 20,
		},
	)
	if err != nil {
		t.Fatalf("new qualified WorkspaceStore: %v", err)
	}
	manager, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	manager.SetRunnerEvidenceSink(
		runnerevidence.SlogSink{},
		"runner-qualified-local-restore",
	)
	if err := manager.SetWorkspaceStore(workspaceStore); err != nil {
		t.Fatalf("bind WorkspaceStore: %v", err)
	}
	const (
		sandboxID   = "0123456789abcdef"
		workspaceID = "workspace-qualified-restore"
		snapshotID  = "snapshot-qualified-restore"
	)
	mutation := func(operationID string) workspacestore.Mutation {
		return workspacestore.Mutation{
			OperationID: operationID,
			WorkspaceID: workspaceID,
			FencingToken: []byte(
				"01234567890123456789012345678901",
			),
		}
	}
	if _, err := workspaceStore.Create(ctx, workspacestore.CreateWorkspaceRequest{
		Mutation:      mutation("create-qualified-restore"),
		CapacityBytes: 64 << 20,
	}); err != nil {
		t.Fatalf("create qualified Workspace: %v", err)
	}
	boot := func(generation uint64) (string, string) {
		t.Helper()
		attachment, err := workspaceStore.Open(ctx, workspaceID, generation)
		if err != nil {
			t.Fatalf("open generation %d attachment: %v", generation, err)
		}
		generationText := strconv.FormatUint(generation, 10)
		opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
			Timezone:            "UTC",
			CompartmentID:       "cmp_qualified_local_restore",
			RuntimeClass:        runtimemanager.RuntimeClassToolExecutor,
			WorkspaceAttachment: attachment,
			RequestID:           "request-qualified-local-restore-g" + generationText,
			OperationID:         "operation-qualified-local-restore-g" + generationText,
			AssignmentID:        "assignment-qualified-local-restore-g" + generationText,
			LeaseID:             "lease-qualified-local-restore-g" + generationText,
		})
		opts.SandboxGeneration = generation
		instanceID, err := manager.createAndStart(ctx, sandboxID, opts)
		if err != nil {
			_ = attachment.Close()
			t.Fatalf(
				"start generation %d microVM: %v\n%s",
				generation,
				err,
				latestSmokeLog(t, workDir),
			)
		}
		logPath := ""
		if instance := manager.lookup(instanceID); instance != nil {
			logPath = instance.logPath
		}
		var heartbeatErr error
		waitForSmoke(t, 30*time.Second, func() bool {
			heartbeat, err := manager.Heartbeat(ctx, instanceID)
			heartbeatErr = err
			return err == nil && heartbeat.Healthy
		}, func() string {
			return "generation " + strconv.FormatUint(generation, 10) +
				" heartbeat error: " + errorString(heartbeatErr) + "\n" +
				smokeLogPath(t, logPath)
		})
		return instanceID, logPath
	}
	writeState := func(instanceID string, logPath string, state string) {
		t.Helper()
		response, err := manager.ExecuteTool(ctx, instanceID, ToolExecRequest{
			Operation: ToolOpWriteFile,
			Path:      "restore-state.txt",
			Content:   state,
		})
		if err != nil || response.Error != "" {
			t.Fatalf(
				"write state %q: response=%+v error=%v\n%s",
				state,
				response,
				err,
				smokeLogPath(t, logPath),
			)
		}
	}
	stop := func(instanceID string, logPath string) {
		t.Helper()
		if err := manager.Remove(ctx, instanceID); err != nil {
			t.Fatalf("stop microVM: %v\n%s", err, smokeLogPath(t, logPath))
		}
	}

	instanceID, logPath := boot(1)
	writeState(instanceID, logPath, "A")
	stop(instanceID, logPath)
	if _, err := workspaceStore.AdvanceGeneration(
		ctx,
		workspacestore.AdvanceGenerationRequest{
			Mutation:           mutation("stop-after-state-a"),
			ExpectedGeneration: 1,
			NextGeneration:     2,
		},
	); err != nil {
		t.Fatalf("advance after state A: %v", err)
	}
	if _, err := workspaceStore.CreateSnapshot(
		ctx,
		workspacestore.CreateSnapshotRequest{
			Mutation:           mutation("snapshot-state-a"),
			SnapshotID:         snapshotID,
			ExpectedGeneration: 2,
		},
	); err != nil {
		t.Fatalf("create Snapshot A: %v", err)
	}
	instanceID, logPath = boot(2)
	writeState(instanceID, logPath, "B")
	stop(instanceID, logPath)
	if _, err := workspaceStore.AdvanceGeneration(
		ctx,
		workspacestore.AdvanceGenerationRequest{
			Mutation:           mutation("stop-after-state-b"),
			ExpectedGeneration: 2,
			NextGeneration:     3,
		},
	); err != nil {
		t.Fatalf("advance after state B: %v", err)
	}
	restoreMutation := mutation("restore-state-a")
	if _, err := workspaceStore.PrepareRestore(
		ctx,
		workspacestore.PrepareRestoreRequest{
			Mutation:           restoreMutation,
			SnapshotID:         snapshotID,
			ExpectedGeneration: 3,
			NextGeneration:     4,
		},
	); err != nil {
		t.Fatalf("prepare restore A: %v", err)
	}
	if _, err := workspaceStore.SwapRestore(
		ctx,
		workspacestore.SwapRestoreRequest{
			Mutation:           restoreMutation,
			SnapshotID:         snapshotID,
			ExpectedGeneration: 3,
			NextGeneration:     4,
		},
	); err != nil {
		t.Fatalf("swap restore A: %v", err)
	}
	if _, err := workspaceStore.FinalizeRestore(
		ctx,
		workspacestore.RestoreMutation{Mutation: restoreMutation},
	); err != nil {
		t.Fatalf("finalize restore A: %v", err)
	}
	if _, err := workspaceStore.Open(
		ctx,
		workspaceID,
		3,
	); !errors.Is(err, workspacestore.ErrStaleGeneration) {
		t.Fatalf("stale generation attachment error = %v", err)
	}
	instanceID, logPath = boot(4)
	defer func() {
		_ = manager.Remove(context.Background(), instanceID)
	}()
	response, err := manager.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation: ToolOpReadFile,
		Path:      "restore-state.txt",
	})
	if err != nil || response.Error != "" || response.Content != "A" {
		t.Fatalf(
			"restored state response=%+v error=%v\n%s",
			response,
			err,
			smokeLogPath(t, logPath),
		)
	}
}

func TestSmokeRunnerLocalLifecycleStopPaths(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_FIRECRACKER") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1 to qualify runner-local lifecycle stops")
	}
	var objectStoreRequests atomic.Int64
	objectStoreTrap := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		objectStoreRequests.Add(1)
		http.Error(response, "unexpected Workspace object-store request", http.StatusTeapot)
	}))
	defer objectStoreTrap.Close()
	for name, value := range map[string]string{
		"SECONDBOX_OBJECT_STORE_ENDPOINT":                  objectStoreTrap.URL,
		"SECONDBOX_OBJECT_STORE_REGION":                    "qualification",
		"SECONDBOX_OBJECT_STORE_BUCKET":                    "must-not-be-used",
		"SECONDBOX_OBJECT_STORE_ROOT_USER":                 "must-not-be-used",
		"SECONDBOX_OBJECT_STORE_ROOT_PASSWORD":             "must-not-be-used-credential",
		"SECONDBOX_OBJECT_STORE_USE_PATH_STYLE":            "true",
		"SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS":        "1",
		"SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS": "1000",
		"SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY":            t.TempDir(),
		"SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES":          "10737418240",
	} {
		t.Setenv(name, value)
	}
	t.Cleanup(func() {
		if got := objectStoreRequests.Load(); got != 0 {
			t.Errorf("lifecycle stops issued %d object-store requests, want zero", got)
		}
	})
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:                  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:           requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:             requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:           requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMRunDir:                    filepath.Join(workDir, "run"),
		MicroVMLogDir:                    filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "snapshot-templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          64,
		MicroVMAllowUnjailed:             true,
		MicroVMToolRootfsPath:            requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMToolSharedImagePath:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
	}
	ctx := context.Background()
	workspaceStore, err := workspacestore.New(
		ctx,
		workspacestore.Config{
			Root:                  filepath.Join(workDir, "durable-workspaces"),
			TemplateCapacityBytes: int64(cfg.MicroVMWorkspaceSizeMiB) << 20,
		},
	)
	if err != nil {
		t.Fatalf("new lifecycle qualification WorkspaceStore: %v", err)
	}
	manager, err := New(cfg)
	if err != nil {
		t.Fatalf("new lifecycle qualification manager: %v", err)
	}
	defer manager.Shutdown(context.Background())
	manager.SetRunnerEvidenceSink(
		runnerevidence.SlogSink{},
		"runner-qualified-local-stop-paths",
	)
	if err := manager.SetWorkspaceStore(workspaceStore); err != nil {
		t.Fatalf("bind lifecycle qualification WorkspaceStore: %v", err)
	}
	const (
		sandboxID   = "fedcba9876543210"
		workspaceID = "workspace-qualified-stop-paths"
	)
	mutation := func(operationID string) workspacestore.Mutation {
		return workspacestore.Mutation{
			OperationID: operationID,
			WorkspaceID: workspaceID,
			FencingToken: []byte(
				"01234567890123456789012345678901",
			),
		}
	}
	if _, err := workspaceStore.Create(ctx, workspacestore.CreateWorkspaceRequest{
		Mutation:      mutation("create-qualified-stop-paths"),
		CapacityBytes: 64 << 20,
	}); err != nil {
		t.Fatalf("create lifecycle qualification Workspace: %v", err)
	}
	boot := func(generation uint64, cause string) (string, string) {
		t.Helper()
		attachment, err := workspaceStore.Open(ctx, workspaceID, generation)
		if err != nil {
			t.Fatalf("open %s generation %d attachment: %v", cause, generation, err)
		}
		generationText := strconv.FormatUint(generation, 10)
		opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
			Timezone:            "UTC",
			CompartmentID:       "cmp_qualified_stop_" + strings.ReplaceAll(cause, "-", "_"),
			RuntimeClass:        runtimemanager.RuntimeClassToolExecutor,
			WorkspaceAttachment: attachment,
			RequestID:           "request-qualified-stop-" + cause + "-g" + generationText,
			OperationID:         "operation-qualified-stop-" + cause + "-g" + generationText,
			AssignmentID:        "assignment-qualified-stop-" + cause + "-g" + generationText,
			LeaseID:             "lease-qualified-stop-" + cause + "-g" + generationText,
		})
		opts.SandboxGeneration = generation
		instanceID, err := manager.createAndStart(ctx, sandboxID, opts)
		if err != nil {
			_ = attachment.Close()
			t.Fatalf(
				"start %s generation %d microVM: %v\n%s",
				cause,
				generation,
				err,
				latestSmokeLog(t, workDir),
			)
		}
		logPath := ""
		if instance := manager.lookup(instanceID); instance != nil {
			logPath = instance.logPath
		}
		var heartbeatErr error
		waitForSmoke(t, 30*time.Second, func() bool {
			heartbeat, err := manager.Heartbeat(ctx, instanceID)
			heartbeatErr = err
			return err == nil && heartbeat.Healthy
		}, func() string {
			return cause + " generation " + generationText +
				" heartbeat error: " + errorString(heartbeatErr) + "\n" +
				smokeLogPath(t, logPath)
		})
		return instanceID, logPath
	}
	readMarker := func(instanceID string, logPath string, cause string) {
		t.Helper()
		response, err := manager.ExecuteTool(ctx, instanceID, ToolExecRequest{
			Operation: ToolOpReadFile,
			Path:      "lifecycle-" + cause + ".txt",
		})
		if err != nil || response.Error != "" || response.Content != cause {
			t.Fatalf(
				"read preserved %s marker: response=%+v error=%v\n%s",
				cause,
				response,
				err,
				smokeLogPath(t, logPath),
			)
		}
	}
	causes := []string{
		"explicit-stop",
		"drain",
		"idle-timeout",
		"maximum-duration",
		"guest-shutdown",
		"liveness-loss",
	}
	for index, cause := range causes {
		generation := uint64(index + 1)
		instanceID, logPath := boot(generation, cause)
		for _, prior := range causes[:index] {
			readMarker(instanceID, logPath, prior)
		}
		response, err := manager.ExecuteTool(ctx, instanceID, ToolExecRequest{
			Operation: ToolOpWriteFile,
			Path:      "lifecycle-" + cause + ".txt",
			Content:   cause,
		})
		if err != nil || response.Error != "" {
			t.Fatalf(
				"write %s marker: response=%+v error=%v\n%s",
				cause,
				response,
				err,
				smokeLogPath(t, logPath),
			)
		}
		if cause == "guest-shutdown" {
			if err := requestGuestShutdown(ctx, manager, instanceID); err != nil {
				t.Fatalf(
					"request guest shutdown: %v\n%s",
					err,
					smokeLogPath(t, logPath),
				)
			}
			waitForSmoke(t, 30*time.Second, func() bool {
				return manager.lookup(instanceID) == nil
			}, func() string {
				return "guest-shutdown microVM remained active\n" +
					smokeLogPath(t, logPath)
			})
		} else if err := manager.Remove(ctx, instanceID); err != nil {
			t.Fatalf("stop %s microVM: %v\n%s", cause, err, smokeLogPath(t, logPath))
		}
		nextGeneration := generation + 1
		readBytesBefore, writeBytesBefore := smokeProcessIO(t)
		if _, err := workspaceStore.AdvanceGeneration(
			ctx,
			workspacestore.AdvanceGenerationRequest{
				Mutation:           mutation("stop-qualified-" + cause),
				ExpectedGeneration: generation,
				NextGeneration:     nextGeneration,
			},
		); err != nil {
			t.Fatalf("advance after %s: %v", cause, err)
		}
		readBytesAfter, writeBytesAfter := smokeProcessIO(t)
		if readBytesAfter-readBytesBefore >= 32<<20 ||
			writeBytesAfter-writeBytesBefore >= 32<<20 {
			t.Fatalf(
				"%s generation advance performed image-sized I/O: read=%d write=%d",
				cause,
				readBytesAfter-readBytesBefore,
				writeBytesAfter-writeBytesBefore,
			)
		}
		inspection, err := workspaceStore.Inspect(ctx, workspaceID)
		if err != nil ||
			inspection.Generation != nextGeneration ||
			inspection.CapacityBytes != 64<<20 ||
			!inspection.Formatted ||
			inspection.RestorePending ||
			inspection.ActiveWriter {
			t.Fatalf("post-%s filesystem integrity = %#v, %v", cause, inspection, err)
		}
		report, err := workspaceStore.Reconcile(ctx)
		if err != nil || len(report.Workspaces) != 1 ||
			report.Workspaces[0] != inspection ||
			len(report.Receipts) != index+2 ||
			report.Receipts[len(report.Receipts)-1].Kind !=
				workspacestore.ReceiptGenerationAdvance {
			t.Fatalf("post-%s local receipt evidence = %#v, %v", cause, report, err)
		}
	}
	instanceID, logPath := boot(uint64(len(causes)+1), "final-verification")
	defer manager.Remove(context.Background(), instanceID)
	for _, cause := range causes {
		readMarker(instanceID, logPath, cause)
	}
}

// requestGuestShutdown makes the guest disappear the way a real guest-initiated
// stop does, so the runner observes an exit it did not request.
//
// Firecracker's SendCtrlAltDel cannot do this against the SecondBox guest. The
// guest init is a shell running as PID 1 and installs no Ctrl+Alt+Del handler,
// and the kernel's boot default is LINUX_REBOOT_CMD_CAD_OFF, so the action only
// delivers SIGINT to PID 1 — which PID 1 ignores when it has no handler
// installed. The microVM stays up and the caller waits for a stop that never
// arrives. Resetting through sysrq bypasses PID 1 entirely and is deterministic.
//
// The write is explicitly enabled first because /proc/sys/kernel/sysrq may ship
// a restrictive mask that silently drops the reboot request, and it is detached
// behind a short delay so this tool call can return its response before the
// guest goes away.
func requestGuestShutdown(ctx context.Context, manager *Manager, instanceID string) error {
	inst := manager.lookup(instanceID)
	if inst == nil {
		return fmt.Errorf("unknown microVM instance %q", instanceID)
	}
	response, err := inst.controlClient(5*time.Second).ExecuteTool(ctx, ToolExecRequest{
		Operation: ToolOpExec,
		Command:   "setsid",
		Args: []string{"-f", "sh", "-c",
			"exec >/dev/null 2>&1; echo 1 > /proc/sys/kernel/sysrq; sleep 1; echo b > /proc/sysrq-trigger"},
		TimeoutMillis: 5_000,
	})
	if err != nil {
		return fmt.Errorf("request guest shutdown: %w", err)
	}
	if response.ExitCode != 0 {
		return fmt.Errorf("request guest shutdown exited with status %d: %s", response.ExitCode, strings.TrimSpace(response.Stderr))
	}
	return nil
}

func smokeProcessIO(t *testing.T) (uint64, uint64) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatalf("read qualification process I/O counters: %v", err)
	}
	var readBytes, writeBytes uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("parse qualification process I/O counter %q: %v", line, err)
		}
		switch fields[0] {
		case "read_bytes:":
			readBytes = value
		case "write_bytes:":
			writeBytes = value
		}
	}
	return readBytes, writeBytes
}

func TestSmokeGoldenSnapshotCreateGeneratedImage(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_SNAPSHOT") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_SNAPSHOT=1 to create a local Firecracker snapshot")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:                  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:           requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:             requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:           requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMRunDir:                    filepath.Join(workDir, "run"),
		MicroVMLogDir:                    filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "snapshot-templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:             true,
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
		FirecrackerPath:                  requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		JailerPath:                       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH"),
		MicroVMKernelPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath:           requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:             requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256:           requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMRunDir:                    filepath.Join(workDir, "run"),
		MicroVMLogDir:                    filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "snapshot-templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"),
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"),
		MicroVMAllowUnjailed:             false,
		MicroVMJailerChrootBaseDir:       requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"),
		MicroVMJailerUIDStart:            requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START"),
		MicroVMJailerUIDCount:            requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_COUNT"),
		MicroVMJailerGID:                 requiredNonNegativeEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID"),
		MicroVMJailerCgroupVersion:       requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION"),
		MicroVMJailerParentCgroup:        requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT"),
		MicroVMBridgeName:                requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME"),
		MicroVMBridgeCIDR:                requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR"),
		MicroVMGuestIP:                   requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_GUEST_IP"),
		MicroVMTapPrefix:                 requiredEnv(t, "SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX"),
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
	if opts.SandboxPolicy == nil {
		opts.SandboxPolicy = &runtimemanager.SandboxRuntimePolicy{
			VCPUs:            cfg.MicroVMVCPUs,
			CPUMillis:        cfg.MicroVMVCPUs * 1000,
			MemoryMiB:        cfg.MicroVMMemoryMiB,
			WorkspaceSizeMiB: cfg.MicroVMWorkspaceSizeMiB,
			ProcessLimit:     128,
			// AssignmentBackend.StartAssignment is the only production caller
			// and always sets this. Without it the Workspace drive is attached
			// read-only and every smoke test that writes to the Workspace fails
			// with "read-only file system".
			WorkspaceWritable: true,
		}
	}
	return opts
}

func shortSmokeDir(t *testing.T) string {
	t.Helper()
	parent := strings.TrimSpace(
		os.Getenv("SECONDBOX_RUNNER_QUALIFICATION_TEMP_ROOT"),
	)
	if parent == "" {
		parent = "/tmp"
	}
	if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent ||
		parent == string(filepath.Separator) {
		t.Fatalf(
			"SECONDBOX_RUNNER_QUALIFICATION_TEMP_ROOT %q must be a clean non-root absolute path",
			parent,
		)
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
	return "jailer config: uid_start=" + strconv.Itoa(cfg.MicroVMJailerUIDStart) +
		" uid_count=" + strconv.Itoa(cfg.MicroVMJailerUIDCount) +
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
