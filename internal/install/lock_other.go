//go:build !linux && !darwin

package install

type OperationLock struct{}

func (*OperationLock) heldFor(string) bool { return false }

func AcquireLock(string) (*OperationLock, error) {
	return nil, installerError("operation locking is unsupported on this host", nil)
}
func (*OperationLock) Close() error { return nil }
