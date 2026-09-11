package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
)

const RunnerCapabilityAttributedExecution = "attributed-execution"

type StartSandboxRequest struct {
	AttributedExecution *AttributedExecutionRequest `json:"attributedExecution,omitempty"`
}

func (request *StartSandboxRequest) UnmarshalJSON(data []byte) error {
	var body struct {
		AttributedExecution json.RawMessage `json:"attributedExecution"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return err
	}
	request.AttributedExecution = nil
	if len(body.AttributedExecution) == 0 {
		return nil
	}
	if bytes.Equal(body.AttributedExecution, []byte("null")) {
		return errors.New("SecondBox attributed execution cannot be null")
	}
	request.AttributedExecution = new(AttributedExecutionRequest)
	decoder = json.NewDecoder(bytes.NewReader(body.AttributedExecution))
	decoder.DisallowUnknownFields()
	return decoder.Decode(request.AttributedExecution)
}

// AttributedExecutionRequest fixes application correlation and the host deadline
// before one exclusive generation starts. Neither field selects credentials.
type AttributedExecutionRequest struct {
	AuthorizationRef string    `json:"authorizationRef"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// AttributedExecutionMetadata uses the existing durable lifecycle intent and
// Operation metadata, so admission and scheduling read the same binding.
func (request AttributedExecutionRequest) AttributedExecutionMetadata() map[string]string {
	return map[string]string{
		"executionAuthorizationRef": request.AuthorizationRef,
		"executionExpiresAt":        request.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func ParseAttributedExecutionMetadata(metadata map[string]string) (*AttributedExecutionRequest, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	reference := metadata["executionAuthorizationRef"]
	expiresAt, err := time.Parse(time.RFC3339Nano, metadata["executionExpiresAt"])
	if len(metadata) != 2 || err != nil || expiresAt.IsZero() || reference == "" || len(reference) > 256 ||
		strings.TrimSpace(reference) != reference || strings.IndexFunc(reference, unicode.IsControl) >= 0 {
		return nil, errors.New("SecondBox attributed execution requires a bounded authorization reference and explicit expiry")
	}
	return &AttributedExecutionRequest{AuthorizationRef: reference, ExpiresAt: expiresAt.UTC()}, nil
}
