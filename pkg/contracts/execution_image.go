package contracts

import (
	"errors"
	"regexp"
	"strings"
)

var executionImageReferencePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]*/[a-zA-Z0-9][a-zA-Z0-9._/-]*(?::[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}|@sha256:[a-f0-9]{64})$`)

const (
	executionImageReferenceMetadata     = "executionImageReference"
	maximumExecutionImageReference      = 512
	RunnerCapabilityClientSelectedImage = "client-selected-image"
)

// ExecutionImage selects one signed OCI execution bundle for a lifecycle operation.
type ExecutionImage struct {
	Reference       string                   `json:"reference"`
	PullCredentials *RegistryPullCredentials `json:"pullCredentials,omitempty"`
}

// RegistryPullCredentials authorize one image pull and never enter durable records.
type RegistryPullCredentials struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// PublicExecutionImage is the non-secret image identity returned by the API.
type PublicExecutionImage struct {
	RequestedReference string `json:"requestedReference"`
	ResolvedDigest     string `json:"resolvedDigest,omitempty"`
}

func (image ExecutionImage) Validate() error {
	if image.Reference == "" || len(image.Reference) > maximumExecutionImageReference ||
		strings.TrimSpace(image.Reference) != image.Reference ||
		!executionImageReferencePattern.MatchString(image.Reference) {
		return errors.New("SecondBox execution image requires a bounded fully qualified tag or sha256 digest reference")
	}
	if image.PullCredentials != nil {
		if image.PullCredentials.Token == "" || len(image.PullCredentials.Token) > 8192 ||
			len(image.PullCredentials.Username) > 256 ||
			strings.TrimSpace(image.PullCredentials.Username) != image.PullCredentials.Username {
			return errors.New("SecondBox execution image pull credentials are invalid")
		}
	}
	return nil
}

func (image ExecutionImage) LifecycleMetadata() map[string]string {
	return map[string]string{executionImageReferenceMetadata: image.Reference}
}

func MergeExecutionImageMetadata(image ExecutionImage, attributed *AttributedExecutionRequest) map[string]string {
	metadata := image.LifecycleMetadata()
	if attributed != nil {
		for key, value := range attributed.AttributedExecutionMetadata() {
			metadata[key] = value
		}
	}
	return metadata
}

func ParseExecutionImageMetadata(metadata map[string]string) (PublicExecutionImage, error) {
	reference := metadata[executionImageReferenceMetadata]
	allowedLength := 1
	if metadata["executionAuthorizationRef"] != "" || metadata["executionExpiresAt"] != "" {
		allowedLength = 3
	}
	if len(metadata) != allowedLength || reference == "" {
		return PublicExecutionImage{}, errors.New("SecondBox lifecycle operation has no execution image selection")
	}
	image := ExecutionImage{Reference: reference}
	if err := image.Validate(); err != nil {
		return PublicExecutionImage{}, err
	}
	return PublicExecutionImage{RequestedReference: reference}, nil
}
