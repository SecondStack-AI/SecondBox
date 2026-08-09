//go:build !linux

package install

import (
	"context"
	"errors"
)

type SystemHostApplyExecutor struct{ CallerUID int }

func (SystemHostApplyExecutor) EffectiveUID() int { return -1 }
func (SystemHostApplyExecutor) Revalidate(context.Context, InstallPlan, InstallReceipt) error {
	return errors.ErrUnsupported
}
func (SystemHostApplyExecutor) RevalidateTeardown(context.Context, InstallPlan, InstallReceipt) error {
	return errors.ErrUnsupported
}
func (SystemHostApplyExecutor) CreateDirectory(PlannedPath) error { return errors.ErrUnsupported }
func (SystemHostApplyExecutor) AllocateFilesystemImage(PlannedPath, int64) error {
	return errors.ErrUnsupported
}
func (SystemHostApplyExecutor) FormatBtrfs(context.Context, string, string) error {
	return errors.ErrUnsupported
}
func (SystemHostApplyExecutor) WriteMountUnit(PlannedPath, string) error {
	return errors.ErrUnsupported
}
func (SystemHostApplyExecutor) EnableMountUnit(context.Context, string) error {
	return errors.ErrUnsupported
}
func (SystemHostApplyExecutor) SecureDirectory(PlannedPath) error {
	return errors.ErrUnsupported
}
func (SystemHostApplyExecutor) ProveReflinkTopology(string, string, string) (string, error) {
	return "", errors.ErrUnsupported
}
func (SystemHostApplyExecutor) RemoveEmpty(CreatedResource) (bool, error) {
	return false, errors.ErrUnsupported
}
