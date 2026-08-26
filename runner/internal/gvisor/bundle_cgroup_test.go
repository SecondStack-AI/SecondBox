//go:build linux

package gvisor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSandboxCgroupParentPicksNearestDelegatingAncestor(t *testing.T) {
	podAncestry := map[string]string{
		"/kubepods.slice/kubepods-pod1234.slice": "cpuset cpu io memory pids",
		"/kubepods.slice":                        "cpuset cpu io memory pids",
	}
	parent := resolveSandboxCgroupParent(
		"0::/kubepods.slice/kubepods-pod1234.slice/cri-containerd-abcd.scope\n",
		func(candidate string) string { return podAncestry[candidate] },
	)
	if parent != "/kubepods.slice/kubepods-pod1234.slice" {
		t.Fatalf("pod parent = %q", parent)
	}
}

func TestResolveSandboxCgroupParentFallsBackToVisibleRoot(t *testing.T) {
	if parent := resolveSandboxCgroupParent("0::/\n", func(string) string { return "" }); parent != "/" {
		t.Fatalf("container-leaf parent = %q", parent)
	}
	if parent := resolveSandboxCgroupParent("", func(string) string { return "cpu memory" }); parent != "/" {
		t.Fatalf("unparsable parent = %q", parent)
	}
	pidsOnly := func(string) string { return "pids" }
	if parent := resolveSandboxCgroupParent("0::/system.slice/runner.service\n", pidsOnly); parent != "/" {
		t.Fatalf("undelegated ancestry parent = %q", parent)
	}
}

func TestSandboxCgroupDirectoryScopesByProfile(t *testing.T) {
	if sandboxCgroupDirectory(0) == sandboxCgroupDirectory(1) {
		t.Fatal("profiles must own distinct sandbox cgroup directories")
	}
	if instanceCgroupPath(0, "instance-a") == instanceCgroupPath(1, "instance-a") {
		t.Fatal("instance cgroup paths must differ across profiles")
	}
}

// TestRemoveCgroupDirectoryClearsDiscoveredNamesLiterally proves reconcile
// sweeps stale children by their literal on-disk names: the digest naming for
// fresh Instances must never be applied twice, or stale directories would
// survive and leave the profile root ENOTEMPTY.
func TestRemoveCgroupDirectoryClearsDiscoveredNamesLiterally(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, shortInstanceDirName("instance-9"))
	if err := os.MkdirAll(filepath.Join(stale, "leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeCgroupDirectory(stale); err != nil {
		t.Fatalf("literal removal failed: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale directory survived: %v", err)
	}
}

// TestInstanceBundleRendersPidCeiling proves every Instance spec carries the
// backend-owned host PID ceiling: without it a guest fork bomb could exhaust
// the pod or host process budget while ResourceLimitsReady is advertised.
func TestInstanceBundleRendersPidCeiling(t *testing.T) {
	dir := t.TempDir()
	bundle := instanceBundle{
		BundleDir: dir, FlatRootPath: filepath.Join(dir, "rootfs"),
		AgentBinaryPath: filepath.Join(dir, "agent"), WorkspaceMountpoint: filepath.Join(dir, "mnt"),
		SocketDirectory: filepath.Join(dir, "sockets"), RuntimePrivateDir: filepath.Join(dir, "private"),
		InstanceID: "ins_1", SandboxID: "sbx_1", SandboxGeneration: 1,
		GuestBuildID: "build", ImageDigest: "sha256:a", ToolchainDigest: "sha256:b",
		VCPUCount: 4, MemoryBytes: 1 << 30, CgroupsPath: "/x/y",
		NetworkNamespacePath: filepath.Join(dir, "netns"), ResolvConfPath: filepath.Join(dir, "resolv.conf"),
	}
	if err := writeInstanceBundle(bundle); err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Linux struct {
			Resources struct {
				Pids *struct {
					Limit int64 `json:"limit"`
				} `json:"pids"`
			} `json:"resources"`
		} `json:"linux"`
	}
	if err := json.Unmarshal(spec, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Linux.Resources.Pids == nil || decoded.Linux.Resources.Pids.Limit != instancePidCeiling(4) {
		t.Fatalf("rendered pids limit = %+v, want %d", decoded.Linux.Resources.Pids, instancePidCeiling(4))
	}
}
