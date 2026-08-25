//go:build linux

package install

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type OperationLock struct {
	file      *os.File
	directory string
}

func AcquireLock(directory string) (*OperationLock, error) {
	directoryFD, err := openDirectoryNoSymlinks(directory)
	if err != nil {
		return nil, installerError("open operation lock directory without symbolic links", err)
	}
	defer unix.Close(directoryFD)
	var directoryStat unix.Stat_t
	if err := unix.Fstat(directoryFD, &directoryStat); err != nil {
		return nil, installerError("inspect operation lock directory", err)
	}
	fd, err := unix.Openat(directoryFD, ".secondbox-install.lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, installerError("open operation lock", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, installerError("operation lock must be a singly-linked mode-0600 regular file", err)
	}
	// Narrow sudo helpers acquire the same deployment lock as the invoking
	// user. Keep the lock owned by the operation directory owner so the first
	// privileged phase cannot permanently fence out later unprivileged phases.
	if os.Geteuid() == 0 && (stat.Uid != directoryStat.Uid || stat.Gid != directoryStat.Gid) {
		if err := unix.Fchown(fd, int(directoryStat.Uid), int(directoryStat.Gid)); err != nil {
			_ = unix.Close(fd)
			return nil, installerError("assign operation lock to deployment owner", err)
		}
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return nil, installerError("reinspect operation lock ownership", err)
		}
	}
	if stat.Uid != directoryStat.Uid || stat.Gid != directoryStat.Gid {
		_ = unix.Close(fd)
		return nil, installerError("operation lock ownership does not match deployment directory", nil)
	}
	file := os.NewFile(uintptr(fd), ".secondbox-install.lock")
	if file == nil {
		_ = unix.Close(fd)
		return nil, installerError("adopt operation lock", nil)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, installerError("another install, resume, uninstall, or purge process owns this deployment", err)
	}
	return &OperationLock{file: file, directory: filepath.Clean(directory)}, nil
}

func (lock *OperationLock) heldFor(directory string) bool {
	return lock != nil && lock.file != nil && lock.directory == filepath.Clean(directory)
}

func (lock *OperationLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	if unlockErr != nil {
		return fmt.Errorf("SecondBox installer release operation lock: %w", unlockErr)
	}
	return closeErr
}
