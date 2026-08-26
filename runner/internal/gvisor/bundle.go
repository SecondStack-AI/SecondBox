//go:build linux

package gvisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The backend templates minimal OCI bundles directly: the pinned flat root is
// the read-only rootfs (runsc's in-memory overlay supplies writability), and
// every mutable surface enters through an explicit bind.

const (
	guestWorkspacePath      = "/workspace"
	guestRuntimePrivatePath = "/runtime-private"
	guestSocketDirectory    = "/secondbox-sockets"
	guestAgentPath          = "/secondbox-guest-agent"
	guestControlSocket      = guestSocketDirectory + "/control.sock"
	guestProtocolSocket     = guestSocketDirectory + "/protocol.sock"
)

type ociSpec struct {
	Version string     `json:"ociVersion"`
	Process ociProcess `json:"process"`
	Root    ociRoot    `json:"root"`
	Mounts  []ociMount `json:"mounts"`
	Linux   ociLinux   `json:"linux"`
}

type ociProcess struct {
	User ociUser  `json:"user"`
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
}

type ociUser struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces  []ociNamespace `json:"namespaces"`
	CgroupsPath string         `json:"cgroupsPath,omitempty"`
	Resources   *ociResources  `json:"resources,omitempty"`
}

type ociNamespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

type ociResources struct {
	CPU    *ociCPU    `json:"cpu,omitempty"`
	Memory *ociMemory `json:"memory,omitempty"`
}

type ociCPU struct {
	Quota  int64  `json:"quota,omitempty"`
	Period uint64 `json:"period,omitempty"`
}

type ociMemory struct {
	Limit int64 `json:"limit,omitempty"`
}

// instanceBundle is everything one Instance's bundle needs beyond the
// immutable backend configuration.
type instanceBundle struct {
	BundleDir            string
	FlatRootPath         string
	AgentBinaryPath      string
	WorkspaceMountpoint  string
	SocketDirectory      string
	RuntimePrivateDir    string
	InstanceID           string
	SandboxID            string
	SandboxGeneration    uint64
	GuestBuildID         string
	ImageDigest          string
	ToolchainDigest      string
	VCPUCount            uint32
	MemoryBytes          uint64
	CgroupsPath          string
	NetworkNamespacePath string
	ResolvConfPath       string
}

const cgroupCPUPeriodMicros = 100_000

// sandboxCgroupParent resolves where per-Instance cgroups nest: the nearest
// ancestor of the runner's own cgroup-v2 path that already delegates the cpu
// and memory controllers to children. Inside a Kubernetes pod that is the
// pod's slice, so every sandbox counts against the pod budget; a runner in a
// leaf container namespace or directly on a host resolves to the visible
// root, matching the flat layout.
func sandboxCgroupParent() string {
	content, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "/"
	}
	return resolveSandboxCgroupParent(string(content), func(candidate string) string {
		control, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", candidate, "cgroup.subtree_control"))
		if err != nil {
			return ""
		}
		return string(control)
	})
}

// sandboxCgroupDirectory names this runner's sandbox cgroup container. It
// carries the network profile because runners sharing one host can resolve
// the same delegating ancestor, and each runner may sweep or own only its
// profile's directory.
func sandboxCgroupDirectory(profile uint32) string {
	return fmt.Sprintf("secondbox-gvisor-p%d", profile)
}

// instanceCgroupPath names the per-Instance cgroup relative to the cgroup
// filesystem root; launch and teardown must agree on it.
func instanceCgroupPath(profile uint32, instanceID string) string {
	return filepath.Join(sandboxCgroupParent(), sandboxCgroupDirectory(profile), instanceID)
}

// removeInstanceCgroup sweeps the per-Instance cgroup after compute exit.
// runsc removes it on an orderly delete, but a forced kill leaves the
// directory behind, so teardown always sweeps and tolerates absence. The
// kernel returns EBUSY while the last sandbox processes drain, so removal
// retries across a short bound before reporting the leak.
func removeInstanceCgroup(profile uint32, instanceID string) error {
	path := filepath.Join("/sys/fs/cgroup", instanceCgroupPath(profile, instanceID))
	var joined error
	for attempt := 0; attempt < 40; attempt++ {
		if attempt > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		entries, err := os.ReadDir(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		joined = nil
		for _, entry := range entries {
			if entry.IsDir() {
				joined = errors.Join(joined, os.Remove(filepath.Join(path, entry.Name())))
			}
		}
		err = os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		joined = errors.Join(joined, err)
	}
	return fmt.Errorf("remove Instance cgroup %s: %w", instanceID, joined)
}

// reconcileStaleCgroups removes Instance cgroups left by an earlier runner
// generation of the same network profile; none of those can be live before
// this backend launches compute, and other profiles' directories belong to
// other runners.
func reconcileStaleCgroups(profile uint32) error {
	root := filepath.Join("/sys/fs/cgroup", sandboxCgroupParent(), sandboxCgroupDirectory(profile))
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read stale Instance cgroups: %w", err)
	}
	var joined error
	for _, entry := range entries {
		if entry.IsDir() {
			joined = errors.Join(joined, removeInstanceCgroup(profile, entry.Name()))
		}
	}
	if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		joined = errors.Join(joined, err)
	}
	if joined != nil {
		return fmt.Errorf("reconcile stale Instance cgroups: %w", joined)
	}
	return nil
}

func resolveSandboxCgroupParent(selfCgroup string, readSubtreeControl func(string) string) string {
	own := ""
	for _, line := range strings.Split(selfCgroup, "\n") {
		if rest, found := strings.CutPrefix(line, "0::"); found {
			own = rest
			break
		}
	}
	for current := filepath.Clean("/" + own); current != "/"; current = filepath.Dir(current) {
		controllers := strings.Fields(readSubtreeControl(current))
		if slices.Contains(controllers, "cpu") && slices.Contains(controllers, "memory") {
			return current
		}
	}
	return "/"
}

func writeInstanceBundle(bundle instanceBundle) error {
	if bundle.BundleDir == "" || bundle.FlatRootPath == "" || bundle.AgentBinaryPath == "" ||
		bundle.WorkspaceMountpoint == "" || bundle.SocketDirectory == "" ||
		bundle.RuntimePrivateDir == "" || bundle.InstanceID == "" || bundle.SandboxID == "" ||
		bundle.SandboxGeneration == 0 || bundle.VCPUCount == 0 || bundle.MemoryBytes == 0 ||
		bundle.CgroupsPath == "" || bundle.NetworkNamespacePath == "" || bundle.ResolvConfPath == "" {
		return fmt.Errorf("SecondBox gVisor instance bundle is incomplete")
	}
	if err := os.MkdirAll(bundle.BundleDir, 0o700); err != nil {
		return fmt.Errorf("create bundle directory: %w", err)
	}
	namespaces := []ociNamespace{
		{Type: "pid"}, {Type: "mount"}, {Type: "ipc"}, {Type: "uts"},
	}
	if bundle.NetworkNamespacePath != "" {
		namespaces = append(namespaces, ociNamespace{Type: "network", Path: bundle.NetworkNamespacePath})
	}
	spec := ociSpec{
		Version: "1.2.0",
		Process: ociProcess{
			User: ociUser{UID: 0, GID: 0},
			Args: []string{
				guestAgentPath,
				"-control-unix-socket", guestControlSocket,
				"-protocol-unix-socket", guestProtocolSocket,
				"-workspace", guestWorkspacePath,
				"-runtime-private", guestRuntimePrivatePath,
				"-instance-id", bundle.InstanceID,
				"-sandbox-id", bundle.SandboxID,
				"-sandbox-generation", strconv.FormatUint(bundle.SandboxGeneration, 10),
				"-guest-build-id", bundle.GuestBuildID,
				"-image-manifest-digest", bundle.ImageDigest,
				"-toolchain-manifest-digest", bundle.ToolchainDigest,
				"-heartbeat-interval", "5s",
			},
			Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			Cwd: "/",
		},
		Root: ociRoot{Path: bundle.FlatRootPath, Readonly: true},
		Mounts: []ociMount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs"},
			{Destination: guestAgentPath, Type: "bind", Source: bundle.AgentBinaryPath, Options: []string{"bind", "ro"}},
			{Destination: guestWorkspacePath, Type: "bind", Source: bundle.WorkspaceMountpoint, Options: []string{"bind", "rw"}},
			{Destination: guestSocketDirectory, Type: "bind", Source: bundle.SocketDirectory, Options: []string{"bind", "rw"}},
			{Destination: guestRuntimePrivatePath, Type: "bind", Source: bundle.RuntimePrivateDir, Options: []string{"bind", "rw"}},
			{Destination: "/etc/resolv.conf", Type: "bind", Source: bundle.ResolvConfPath, Options: []string{"bind", "ro"}},
		},
		Linux: ociLinux{
			Namespaces:  namespaces,
			CgroupsPath: bundle.CgroupsPath,
			Resources: &ociResources{
				CPU: &ociCPU{
					Quota:  int64(bundle.VCPUCount) * cgroupCPUPeriodMicros,
					Period: cgroupCPUPeriodMicros,
				},
				Memory: &ociMemory{Limit: int64(bundle.MemoryBytes)},
			},
		},
	}
	encoded, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bundle spec: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bundle.BundleDir, "config.json"), append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write bundle config: %w", err)
	}
	return nil
}
