package contracts

import (
	"bytes"
	"encoding/json"
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

type StartSandboxRequest struct {
	Image ExecutionImage `json:"image,omitzero"`
}

func (request *StartSandboxRequest) UnmarshalJSON(data []byte) error {
	var body struct {
		Image json.RawMessage `json:"image"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return err
	}
	request.Image = ExecutionImage{}
	if len(body.Image) == 0 {
		return nil
	}
	if bytes.Equal(body.Image, []byte("null")) {
		return &RequestFieldError{Field: "image", Reason: "must be an object, not null"}
	}
	decoder = json.NewDecoder(bytes.NewReader(body.Image))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request.Image); err != nil {
		return err
	}
	return request.Image.Validate()
}

// ExecutionImage selects one signed OCI execution bundle for a lifecycle operation.
type ExecutionImage struct {
	Reference string `json:"reference"`
}

func (image *ExecutionImage) UnmarshalJSON(data []byte) error {
	type executionImageFields ExecutionImage
	var decoded executionImageFields
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*image = ExecutionImage(decoded)
	if image.Validate() != nil {
		return &RequestFieldError{Field: "reference", Reason: "must be a fully qualified tag or sha256 digest reference of at most 512 bytes"}
	}
	return nil
}

type PrepareImageRequest struct {
	Image   ExecutionImage `json:"image"`
	Profile string         `json:"profile,omitempty"`
}

type ImagePreparation struct {
	Image           PublicExecutionImage `json:"image"`
	TargetRunners   int                  `json:"targetRunners"`
	PreparedRunners int                  `json:"preparedRunners"`
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
	return nil
}

func (image ExecutionImage) LifecycleMetadata() map[string]string {
	if image.Reference == "" {
		return map[string]string{}
	}
	return map[string]string{executionImageReferenceMetadata: image.Reference}
}

func ExecutionImageDigestReference(reference, digest string) string {
	repository, _, _ := strings.Cut(reference, "@")
	if separator := strings.LastIndex(repository, ":"); separator > strings.LastIndex(repository, "/") {
		repository = repository[:separator]
	}
	return repository + "@" + digest
}

func ParseExecutionImageMetadata(metadata map[string]string) (PublicExecutionImage, error) {
	reference := metadata[executionImageReferenceMetadata]
	if len(metadata) != 1 || reference == "" {
		return PublicExecutionImage{}, errors.New("SecondBox lifecycle operation has no execution image selection")
	}
	image := ExecutionImage{Reference: reference}
	if err := image.Validate(); err != nil {
		return PublicExecutionImage{}, err
	}
	return PublicExecutionImage{RequestedReference: reference}, nil
}
