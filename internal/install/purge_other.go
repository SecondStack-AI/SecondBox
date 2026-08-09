//go:build !linux

package install

import (
	"context"
	"errors"
	"time"
)

func PurgeAcceptedHost(context.Context, string, string, int, func() time.Time) (InstallReceipt, error) {
	return InstallReceipt{}, installerError("privileged purge requires Linux openat2 confinement", errors.ErrUnsupported)
}

func ValidateAcceptedHostPurge(string, string, int) error {
	return installerError("privileged purge validation requires Linux openat2 confinement", errors.ErrUnsupported)
}

func PurgeVerifiedArtifacts(InstallPlan, InstallReceipt, func() time.Time, func(InstallReceipt) error) (InstallReceipt, error) {
	return InstallReceipt{}, installerError("verified artifact purge requires Linux openat2 confinement", errors.ErrUnsupported)
}

func ValidatePurgeVerifiedArtifacts(InstallPlan, InstallReceipt) error {
	return installerError("verified artifact purge validation requires Linux openat2 confinement", errors.ErrUnsupported)
}

func PurgeUserResources(InstallPlan, InstallReceipt, func() time.Time, func(InstallReceipt) error) (InstallReceipt, error) {
	return InstallReceipt{}, installerError("resource purge requires Linux openat2 confinement", errors.ErrUnsupported)
}

func ValidatePurgeUserResources(InstallPlan, InstallReceipt) error {
	return installerError("resource purge validation requires Linux openat2 confinement", errors.ErrUnsupported)
}
