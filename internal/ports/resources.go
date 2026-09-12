package ports

import (
	"errors"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var ErrResourcesExceedProfile = errors.New("SecondBox requested resources exceed Profile ceiling")

// ResourcesExceedProfileError preserves the resolved request and its immutable ceiling.
type ResourcesExceedProfileError struct {
	Ceiling   contracts.SandboxResourceRequest
	Requested contracts.SandboxResources
}

func (err *ResourcesExceedProfileError) Error() string { return ErrResourcesExceedProfile.Error() }
func (err *ResourcesExceedProfileError) Unwrap() error { return ErrResourcesExceedProfile }

var ErrResourcesFixedByProfile = errors.New("SecondBox requested resources differ from the fixed Profile size")

// ResourcesFixedByProfileError identifies the exact allocation required for resume.
type ResourcesFixedByProfileError struct {
	Fixed     contracts.SandboxResourceRequest
	Requested contracts.SandboxResources
}

func (err *ResourcesFixedByProfileError) Error() string { return ErrResourcesFixedByProfile.Error() }
func (err *ResourcesFixedByProfileError) Unwrap() error { return ErrResourcesFixedByProfile }
