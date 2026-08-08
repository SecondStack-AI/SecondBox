//go:build darwin

package install

import (
	"fmt"
	"syscall"
)

func filesystemDeviceIdentity(stat *syscall.Stat_t) string {
	return fmt.Sprint(stat.Dev)
}
