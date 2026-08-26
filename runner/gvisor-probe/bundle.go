package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The probe generates minimal OCI bundles directly so the exact spec shape a
// future backend must template is recorded as evidence, without importing an
// OCI runtime library.

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

func bundleNamespaces(networkNamespacePath string) []ociNamespace {
	namespaces := []ociNamespace{
		{Type: "pid"}, {Type: "mount"}, {Type: "ipc"}, {Type: "uts"},
	}
	if networkNamespacePath != "" {
		namespaces = append(namespaces, ociNamespace{Type: "network", Path: networkNamespacePath})
	}
	return namespaces
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

type bindMount struct {
	Source      string
	Destination string
	ReadOnly    bool
}

type bundleConfig struct {
	GuestArgs   []string
	Binds       []bindMount
	CgroupsPath string
	Resources   *ociResources
	// Entrypoint overrides the default /guest argv entirely when set;
	// GuestArgs is ignored in that case.
	Entrypoint []string
	// ExtraBinaries installs additional executables into the rootfs root,
	// keyed by their in-sandbox name.
	ExtraBinaries map[string]string
	// RootfsFiles writes plain files into the rootfs, keyed by rootfs-relative
	// path (for example "etc/resolv.conf").
	RootfsFiles map[string][]byte
	// NetworkNamespacePath joins the sandbox to an existing network namespace
	// so runsc attaches its netstack to the interfaces found there.
	NetworkNamespacePath string
}

const guestBinaryName = "guest"

// writeBundle assembles one OCI bundle: a read-only rootfs holding only the
// static probe guest binary, plus the requested bind mounts.
func writeBundle(bundleDir, guestBinary string, config bundleConfig) error {
	arguments := config.Entrypoint
	if len(arguments) == 0 {
		if len(config.GuestArgs) == 0 {
			return fmt.Errorf("bundle requires guest arguments or an entrypoint")
		}
		arguments = append([]string{"/" + guestBinaryName}, config.GuestArgs...)
	}
	rootfs := filepath.Join(bundleDir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return fmt.Errorf("create rootfs: %w", err)
	}
	if err := copyExecutable(guestBinary, filepath.Join(rootfs, guestBinaryName)); err != nil {
		return fmt.Errorf("install guest binary: %w", err)
	}
	for name, source := range config.ExtraBinaries {
		if name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("extra binary name %q must be a bare file name", name)
		}
		if err := copyExecutable(source, filepath.Join(rootfs, name)); err != nil {
			return fmt.Errorf("install extra binary %s: %w", name, err)
		}
	}
	for relativePath, content := range config.RootfsFiles {
		if relativePath == "" || strings.HasPrefix(relativePath, "/") || strings.Contains(relativePath, "..") {
			return fmt.Errorf("rootfs file path %q must be rootfs-relative", relativePath)
		}
		destination := filepath.Join(rootfs, relativePath)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("create rootfs directory for %s: %w", relativePath, err)
		}
		if err := os.WriteFile(destination, content, 0o644); err != nil {
			return fmt.Errorf("write rootfs file %s: %w", relativePath, err)
		}
	}

	mounts := []ociMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/probe-tmp", Type: "tmpfs", Source: "tmpfs"},
	}
	for _, bind := range config.Binds {
		options := []string{"bind"}
		if bind.ReadOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}
		mounts = append(mounts, ociMount{
			Destination: bind.Destination,
			Type:        "bind",
			Source:      bind.Source,
			Options:     options,
		})
	}

	spec := ociSpec{
		Version: "1.2.0",
		Process: ociProcess{
			User: ociUser{UID: 0, GID: 0},
			Args: arguments,
			Env:  []string{"PATH=/"},
			Cwd:  "/",
		},
		Root:   ociRoot{Path: "rootfs", Readonly: true},
		Mounts: mounts,
		Linux: ociLinux{
			Namespaces:  bundleNamespaces(config.NetworkNamespacePath),
			CgroupsPath: config.CgroupsPath,
			Resources:   config.Resources,
		},
	}
	encoded, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode spec: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}
	return nil
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}
