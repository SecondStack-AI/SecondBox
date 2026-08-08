//go:build darwin

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type OperationLock struct{ file *os.File }

func AcquireLock(directory string) (*OperationLock, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, installerError("lock directory must be an existing non-symbolic-link directory", err)
	}
	file, err := os.OpenFile(filepath.Join(directory, ".secondbox-install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, installerError("open operation lock", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, installerError("another install, resume, uninstall, or purge process owns this deployment", err)
	}
	return &OperationLock{file: file}, nil
}
func (lock *OperationLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	if unlockErr != nil {
		return fmt.Errorf("SecondBox installer release operation lock: %w", unlockErr)
	}
	return closeErr
}
