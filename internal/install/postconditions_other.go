//go:build !linux && !darwin

package install

import "errors"

func ValidateRecordedResources(InstallPlan, InstallReceipt) error { return errors.ErrUnsupported }
func ValidateComposeProjectEvidence(InstallReceipt, string) error { return errors.ErrUnsupported }
func ValidatePlannedPath(PlannedPath) error                       { return errors.ErrUnsupported }
