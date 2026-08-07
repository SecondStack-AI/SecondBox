package microvmguest

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// templateWorkspaceDevice is the guest block device a template's per-Instance
// Workspace image is staged onto. It is part of the template compatibility key:
// the device topology is captured, only the backing file changes per Instance.
const templateWorkspaceDevice = "/dev/vdb"

type LinuxWorkspaceMounter struct{}

func (LinuxWorkspaceMounter) Mount(_ context.Context, workspaceDir string, writable bool) error {
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return fmt.Errorf("create Workspace mount point: %w", err)
	}
	flags := uintptr(unix.MS_NOATIME)
	if !writable {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(templateWorkspaceDevice, workspaceDir, "ext4", flags, ""); err != nil {
		return fmt.Errorf("mount Workspace %s at %s: %w", templateWorkspaceDevice, workspaceDir, err)
	}
	return nil
}
