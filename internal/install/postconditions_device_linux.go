//go:build linux

package install

import (
	"encoding/hex"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	btrfsSuperMagic = 0x9123683e
	xfsSuperMagic   = 0x58465342

	// These requests are stable Linux UAPI values from BTRFS_IOC_FS_INFO and
	// XFS_IOC_FSGEOMETRY_V4. Both return the filesystem's on-disk UUID rather
	// than the transient block or loop device assigned to this mount.
	btrfsIOCFSInfo     = 0x8400941f
	xfsIOCFsGeometryV4 = 0x8070587c
)

func workspaceFilesystemIdentity(path string) (identity string, resultErr error) {
	fd, err := openDirectoryReadOnlyNoSymlinks(path)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(fd)) }()
	return workspaceFilesystemIdentityFromFD(fd)
}

func workspaceFilesystemIdentityFromFD(fd int) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return "", err
	}
	return workspaceFilesystemIdentityForType(uint64(stat.Type), func(request uintptr, buffer []byte) error {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(unsafe.Pointer(&buffer[0])))
		if errno != 0 {
			return errno
		}
		return nil
	})
}

func workspaceFilesystemIdentityForType(filesystemType uint64, ioctl func(uintptr, []byte) error) (string, error) {
	var (
		filesystem string
		request    uintptr
		buffer     []byte
		uuidOffset int
	)
	switch filesystemType {
	case btrfsSuperMagic:
		filesystem, request, buffer, uuidOffset = "btrfs", btrfsIOCFSInfo, make([]byte, 1024), 16
	case xfsSuperMagic:
		filesystem, request, buffer, uuidOffset = "xfs", xfsIOCFsGeometryV4, make([]byte, 112), 64
	default:
		return "", fmt.Errorf("unsupported Workspace filesystem magic %#x", filesystemType)
	}
	if err := ioctl(request, buffer); err != nil {
		return "", fmt.Errorf("read %s filesystem UUID: %w", filesystem, err)
	}
	uuid := buffer[uuidOffset : uuidOffset+16]
	if allZero(uuid) {
		return "", fmt.Errorf("read %s filesystem UUID: kernel returned an empty UUID", filesystem)
	}
	return filesystem + "-uuid:" + formatFilesystemUUID(uuid), nil
}

func formatFilesystemUUID(value []byte) string {
	encoded := make([]byte, hex.EncodedLen(len(value)))
	hex.Encode(encoded, value)
	return string(encoded[0:8]) + "-" + string(encoded[8:12]) + "-" + string(encoded[12:16]) + "-" + string(encoded[16:20]) + "-" + string(encoded[20:32])
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
