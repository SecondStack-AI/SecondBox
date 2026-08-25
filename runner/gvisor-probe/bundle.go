package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
}

const guestBinaryName = "guest"

// writeBundle assembles one OCI bundle: a read-only rootfs holding only the
// static probe guest binary, plus the requested bind mounts.
func writeBundle(bundleDir, guestBinary string, config bundleConfig) error {
	if len(config.GuestArgs) == 0 {
		return fmt.Errorf("bundle requires guest arguments")
	}
	rootfs := filepath.Join(bundleDir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return fmt.Errorf("create rootfs: %w", err)
	}
	if err := copyExecutable(guestBinary, filepath.Join(rootfs, guestBinaryName)); err != nil {
		return fmt.Errorf("install guest binary: %w", err)
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
			Args: append([]string{"/" + guestBinaryName}, config.GuestArgs...),
			Env:  []string{"PATH=/"},
			Cwd:  "/",
		},
		Root:   ociRoot{Path: "rootfs", Readonly: true},
		Mounts: mounts,
		Linux: ociLinux{
			Namespaces: []ociNamespace{
				{Type: "pid"}, {Type: "mount"}, {Type: "ipc"}, {Type: "uts"},
			},
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
