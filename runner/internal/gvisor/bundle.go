//go:build linux

package gvisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
}

const cgroupCPUPeriodMicros = 100_000

func writeInstanceBundle(bundle instanceBundle) error {
	if bundle.BundleDir == "" || bundle.FlatRootPath == "" || bundle.AgentBinaryPath == "" ||
		bundle.WorkspaceMountpoint == "" || bundle.SocketDirectory == "" ||
		bundle.RuntimePrivateDir == "" || bundle.InstanceID == "" || bundle.SandboxID == "" ||
		bundle.SandboxGeneration == 0 || bundle.VCPUCount == 0 || bundle.MemoryBytes == 0 ||
		bundle.CgroupsPath == "" {
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
