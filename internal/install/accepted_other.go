//go:build !linux

package install

import "errors"

func ReadAccepted(string, string, int) (InstallPlan, InstallReceipt, error) {
	return InstallPlan{}, InstallReceipt{}, installerError("accepted installer operations require Linux openat2 path confinement", errors.ErrUnsupported)
}

func ReadHostApply(string, string, int) (InstallPlan, InstallReceipt, error) {
	return InstallPlan{}, InstallReceipt{}, installerError("host apply requires Linux openat2 path confinement", errors.ErrUnsupported)
}

func ReadOperation(string, int) (InstallPlan, InstallReceipt, error) {
	return InstallPlan{}, InstallReceipt{}, installerError("installer operations require Linux openat2 path confinement", errors.ErrUnsupported)
}

func ReadOperationReadOnly(string, int) (InstallPlan, InstallReceipt, error) {
	return InstallPlan{}, InstallReceipt{}, installerError("installer operations require Linux openat2 path confinement", errors.ErrUnsupported)
}

func RecoverOperation(string, int, *OperationLock) (InstallPlan, InstallReceipt, error) {
	return InstallPlan{}, InstallReceipt{}, installerError("installer operations require Linux openat2 path confinement", errors.ErrUnsupported)
}

func SaveReceipt(string, InstallPlan, InstallReceipt, int) error {
	return installerError("installer operations require Linux openat2 path confinement", errors.ErrUnsupported)
}

func SaveOperation(string, InstallPlan, InstallReceipt, int) error {
	return installerError("installer operation commits require Linux openat2 path confinement", errors.ErrUnsupported)
}

func writeReceiptAtomic(string, InstallReceipt, int) error {
	return installerError("receipt updates require Linux path confinement", errors.ErrUnsupported)
}
