package microvmguest

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const (
	fiFreeze = 0xC0045877
	fiThaw   = 0xC0045878
)

type LinuxFreezer struct{}

func (LinuxFreezer) Freeze(ctx context.Context, workspaceDir string) error {
	return ioctlWorkspace(ctx, workspaceDir, fiFreeze, "freeze")
}

func (LinuxFreezer) Thaw(ctx context.Context, workspaceDir string) error {
	return ioctlWorkspace(ctx, workspaceDir, fiThaw, "thaw")
}

func ioctlWorkspace(ctx context.Context, workspaceDir string, req uint, op string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(workspaceDir)
	if err != nil {
		return fmt.Errorf("open workspace for %s: %w", op, err)
	}
	defer f.Close()
	if err := unix.IoctlSetInt(int(f.Fd()), req, 0); err != nil {
		return fmt.Errorf("workspace %s ioctl: %w", op, err)
	}
	return nil
}
