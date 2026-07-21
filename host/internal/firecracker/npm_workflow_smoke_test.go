package microvm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcy/internal/config"
	"agentcy/internal/egressproxy"
	"agentcy/internal/runtimemanager"
)

func TestSmokeToolExecutorNPMWorkflow(t *testing.T) {
	if os.Getenv("AG_MICROVM_NPM_WORKFLOW_SMOKE") != "1" {
		t.Skip("set AG_MICROVM_NPM_WORKFLOW_SMOKE=1 to run an npm workflow inside the tool executor")
	}
	sourceDir := requiredEnv(t, "AG_MICROVM_NPM_WORKFLOW_SOURCE_DIR")
	workDir := shortSmokeDir(t)
	rootfsPath := requiredEnv(t, "AG_MICROVM_ROOTFS_PATH")
	sharedImagePath := os.Getenv("AG_MICROVM_SHARED_IMAGE_PATH")
	cfg := &config.Config{
		FirecrackerPath:            requiredEnv(t, "AG_FIRECRACKER_PATH"),
		JailerPath:                 os.Getenv("AG_FIRECRACKER_JAILER_PATH"),
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
		MicroVMVCPUs:               envIntOrDefault("AG_MICROVM_VCPUS", 1),
		MicroVMCPUTemplate:         os.Getenv("AG_MICROVM_CPU_TEMPLATE"),
		MicroVMWorkspaceSizeMiB:    envIntOrDefault("AG_MICROVM_WORKSPACE_SIZE_MIB", 4096),
		MicroVMAllowUnjailed:       strings.EqualFold(strings.TrimSpace(os.Getenv("AG_MICROVM_ALLOW_UNJAILED")), "true"),
		MicroVMJailerChrootBaseDir: os.Getenv("AG_MICROVM_JAILER_CHROOT_BASE_DIR"),
		MicroVMJailerUID:           envIntOrDefault("AG_MICROVM_JAILER_UID", os.Geteuid()),
		MicroVMJailerGID:           envIntOrDefault("AG_MICROVM_JAILER_GID", os.Getegid()),
		MicroVMJailerCgroupVersion: envIntOrDefault("AG_MICROVM_JAILER_CGROUP_VERSION", 2),
		MicroVMJailerParentCgroup:  envOrDefault("AG_MICROVM_JAILER_PARENT_CGROUP", "agentcy"),
		MicroVMBridgeName:          os.Getenv("AG_MICROVM_BRIDGE_NAME"),
		MicroVMBridgeCIDR:          os.Getenv("AG_MICROVM_BRIDGE_CIDR"),
		MicroVMGuestIP:             os.Getenv("AG_MICROVM_GUEST_IP"),
		MicroVMTapPrefix:           envOrDefault("AG_MICROVM_TAP_PREFIX", "agfc"),
		EgressProxyURL:             os.Getenv("AG_EGRESS_PROXY_URL"),
		EgressProxyNoProxy:         os.Getenv("AG_EGRESS_PROXY_NO_PROXY"),
	}
	mgr, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if controlURL := strings.TrimSpace(os.Getenv("AG_EGRESS_PROXY_CONTROL_URL")); controlURL != "" {
		mgr.SetSourceBindingRegistrar(egressproxy.NewControlClient(controlURL, os.Getenv("AG_EGRESS_PROXY_CONTROL_TOKEN")))
	}
	ctx := context.Background()
	agentID := envOrDefault("AG_MICROVM_NPM_WORKFLOW_AGENT_ID", "0123456789abcdef")
	instanceID, err := mgr.createAndStart(ctx, agentID, runtimemanager.StartOpts{
		Timezone:       "UTC",
		CompartmentID:  "cmp_npm_workflow",
		RuntimeClass:   runtimemanager.RuntimeClassToolExecutor,
		ActorPrincipal: strings.TrimSpace(os.Getenv("AG_MICROVM_NPM_WORKFLOW_ACTOR_PRINCIPAL")),
		ProxyEgress:    npmWorkflowProxyEgress(),
	})
	if err != nil {
		t.Fatalf("start tool executor microVM: %v", err)
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
		return "tool executor heartbeat error: " + errorString(heartbeatErr) + "\n" + smokeLogPath(t, logPath)
	})

	tarball, err := tarProject(sourceDir)
	if err != nil {
		t.Fatalf("tar project: %v", err)
	}
	writeResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation:     ToolOpWriteFile,
		Path:          "project.tgz",
		Encoding:      "base64",
		ContentBase64: base64.StdEncoding.EncodeToString(tarball),
		TimeoutMillis: 120000,
	})
	if err != nil || writeResp.Error != "" || writeResp.ExitCode != 0 {
		t.Fatalf("upload project tarball into tool executor: resp=%+v err=%v\n%s", writeResp, err, smokeLogPath(t, logPath))
	}
	copyResp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation:     ToolOpExec,
		Command:       "sh",
		Args:          []string{"-lc", "tar -xzf project.tgz && rm project.tgz"},
		TimeoutMillis: 120000,
	})
	if err != nil || copyResp.Error != "" || copyResp.ExitCode != 0 {
		t.Fatalf("copy project into tool executor: resp=%+v err=%v\n%s", copyResp, err, smokeLogPath(t, logPath))
	}

	workflow := strings.TrimSpace(os.Getenv("AG_MICROVM_NPM_WORKFLOW_COMMAND"))
	if workflow == "" {
		workflow = strings.Join([]string{
			"set -eux",
			"uname -a",
			"node -v",
			"npm -v",
			"npm ci --ignore-scripts --no-audit --no-fund",
			"npm run build",
		}, "\n")
	}
	resp, err := mgr.ExecuteTool(ctx, instanceID, ToolExecRequest{
		Operation:     ToolOpExec,
		Command:       "sh",
		Args:          []string{"-lc", workflow},
		TimeoutMillis: int64(envIntOrDefault("AG_MICROVM_NPM_WORKFLOW_TIMEOUT_MS", 600000)),
	})
	if err != nil || resp.Error != "" || resp.ExitCode != 0 {
		t.Fatalf("npm workflow failed: resp=%+v err=%v\n%s", resp, err, smokeLogPath(t, logPath))
	}
	t.Logf("npm workflow stdout:\n%s", resp.Stdout)
	if resp.Stderr != "" {
		t.Logf("npm workflow stderr:\n%s", resp.Stderr)
	}
}

func npmWorkflowProxyEgress() *runtimemanager.ProxyEgressConfig {
	proxyURL := strings.TrimSpace(os.Getenv("AG_EGRESS_PROXY_URL"))
	if proxyURL == "" {
		return nil
	}
	return &runtimemanager.ProxyEgressConfig{
		Enabled:             true,
		ProxyURL:            proxyURL,
		NoProxy:             strings.TrimSpace(os.Getenv("AG_EGRESS_PROXY_NO_PROXY")),
		CACertPath:          firstNonEmptyString(os.Getenv("AG_EGRESS_PROXY_CA_CERT_HOST_PATH"), os.Getenv("AG_EGRESS_PROXY_CA_CERT_PATH")),
		TransparentHTTPPort: envIntOrDefault("AG_EGRESS_PROXY_TRANSPARENT_HTTP_PORT", 0),
		PlaceID:             strings.TrimSpace(os.Getenv("AG_MICROVM_NPM_WORKFLOW_PLACE_ID")),
		AllowedHosts:        []string{"registry.npmjs.org", "github.com", "api.github.com"},
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func tarProject(root string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		base := d.Name()
		if d.IsDir() && (base == ".git" || base == "node_modules" || base == "dist" || base == ".astro") {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
