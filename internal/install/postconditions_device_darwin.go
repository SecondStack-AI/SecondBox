//go:build darwin

package install

import (
	"fmt"
	"os"
	"syscall"
)

func workspaceFilesystemIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("Workspace filesystem identity is unavailable")
	}
	return fmt.Sprint(stat.Dev), nil
}
