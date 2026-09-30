package contracts

import (
	"bytes"
	"encoding/json"
	"time"
)

// RunnerCapabilityPerExecAttribution is advertised by a Runner with configured
// attributed gateway routing. Ordinary generations on it admit attributed execs.
const RunnerCapabilityPerExecAttribution = "per-exec-attribution"

// AttributedExecutionRequest binds one exec to application correlation and an
// absolute authority expiry. Neither field selects credentials.
type AttributedExecutionRequest struct {
	AuthorizationRef string    `json:"authorizationRef"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// rejectNullAttributedExecution keeps omission as the only way to request
// ordinary execution; the JSON decoder alone would treat null as omission.
func rejectNullAttributedExecution(data []byte) error {
	var members struct {
		AttributedExecution json.RawMessage `json:"attributedExecution"`
	}
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(members.AttributedExecution), []byte("null")) {
		return &RequestFieldError{Field: "attributedExecution", Reason: "must be an object, not null"}
	}
	return nil
}
