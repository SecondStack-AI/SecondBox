//go:build linux || darwin

package materialization

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func flatRootOwner(info os.FileInfo) (uint32, uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("Unix stat identity is unavailable")
	}
	return stat.Uid, stat.Gid, nil
}

func flatRootXattrs(path string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return map[string][]byte{}, nil
	}
	buffer := make([]byte, size)
	read, err := unix.Llistxattr(path, buffer)
	if err != nil {
		return nil, err
	}
	buffer = buffer[:read]
	result := make(map[string][]byte)
	for len(buffer) > 0 {
		end := 0
		for end < len(buffer) && buffer[end] != 0 {
			end++
		}
		if end == len(buffer) {
			return nil, fmt.Errorf("extended attribute list is malformed")
		}
		name := string(buffer[:end])
		buffer = buffer[end+1:]
		valueSize, err := unix.Lgetxattr(path, name, nil)
		if errors.Is(err, unix.ENODATA) {
			return nil, fmt.Errorf("extended attribute %q changed while hashing", name)
		}
		if err != nil {
			return nil, err
		}
		value := make([]byte, valueSize)
		valueRead, err := unix.Lgetxattr(path, name, value)
		if err != nil {
			return nil, err
		}
		result[name] = value[:valueRead]
	}
	return result, nil
}
