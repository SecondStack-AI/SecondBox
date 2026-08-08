//go:build !linux && !darwin

package install

type OperationLock struct{}

func AcquireLock(string) (*OperationLock, error) {
	return nil, installerError("operation locking is unsupported on this host", nil)
}
func (*OperationLock) Close() error { return nil }
