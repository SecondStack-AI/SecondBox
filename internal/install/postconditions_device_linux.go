//go:build linux

package install

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func filesystemDeviceIdentity(stat *syscall.Stat_t) string {
	return fmt.Sprintf("%d:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)))
}
