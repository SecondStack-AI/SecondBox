//go:build linux

package deployconfig

import "syscall"

func filesystemDevice(path string) (uint64, error) {
	var info syscall.Stat_t
	if err := syscall.Stat(path, &info); err != nil {
		return 0, err
	}
	return info.Dev, nil
}
