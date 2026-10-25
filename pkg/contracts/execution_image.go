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

// MaximumCapacityWaitMilliseconds bounds how long one start may remain
// unplaced before it fails with capacity_wait_expired.
const MaximumCapacityWaitMilliseconds = int64(3_600_000)

// CapacityWaitBound is how many milliseconds a start may remain unplaced; zero
// leaves it unbounded. Its decoder refuses null, zero, fractions, and values
// above MaximumCapacityWaitMilliseconds, so a member that is present is
// always a bound.
type CapacityWaitBound int64

func (bound *CapacityWaitBound) UnmarshalJSON(data []byte) error {
	var value int64
	if bytes.Equal(data, []byte("null")) || json.Unmarshal(data, &value) != nil ||
		value < 1 || value > MaximumCapacityWaitMilliseconds {
		return &RequestFieldError{Reason: "must be an integer between 1 and 3600000"}
	}
	*bound = CapacityWaitBound(value)
	return nil
}

type StartSandboxRequest struct {
	Image ExecutionImage `json:"image,omitzero"`
	// CapacityWaitMilliseconds, when positive, bounds how long the start may
	// wait unplaced. Zero waits until the home Runner can place it.
	CapacityWaitMilliseconds CapacityWaitBound `json:"capacityWaitMilliseconds,omitempty"`
}

func (request *StartSandboxRequest) UnmarshalJSON(data []byte) error {
	var body struct {
		Image                    json.RawMessage `json:"image"`
		CapacityWaitMilliseconds json.RawMessage `json:"capacityWaitMilliseconds"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return err
	}
	request.Image = ExecutionImage{}
	request.CapacityWaitMilliseconds = 0
	if len(body.CapacityWaitMilliseconds) != 0 {
		if err := request.CapacityWaitMilliseconds.UnmarshalJSON(body.CapacityWaitMilliseconds); err != nil {
			var fieldError *RequestFieldError
			if errors.As(err, &fieldError) {
				return &RequestFieldError{Field: "capacityWaitMilliseconds", Reason: fieldError.Reason}
			}
			return err
		}
	}
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

// ExecutionImage selects one digest-pinned OCI execution bundle for a lifecycle operation.
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
