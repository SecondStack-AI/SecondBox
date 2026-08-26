//go:build linux

package gvisor

import "testing"

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
