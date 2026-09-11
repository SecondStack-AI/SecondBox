// Package egressattribution frames Runner-established execution identity before guest bytes.
package egressattribution

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

const executionAttributionMagic = "SBXATTR1"
const maximumExecutionAttributionBytes = 4096

// ExecutionAttribution belongs to one accepted connection for its entire lifetime.
// It contains correlation evidence, never credentials or the raw assignment fence.
type ExecutionAttribution struct {
	TenantRef        string    `json:"tenantRef"`
	SubjectRef       string    `json:"subjectRef"`
	SandboxID        string    `json:"sandboxId"`
	InstanceID       string    `json:"instanceId"`
	AssignmentID     string    `json:"assignmentId"`
	Generation       int64     `json:"generation"`
	AuthorizationRef string    `json:"authorizationRef"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

func (attribution ExecutionAttribution) validate(now time.Time) error {
	for _, reference := range []string{
		attribution.TenantRef, attribution.SubjectRef, attribution.SandboxID,
		attribution.InstanceID, attribution.AssignmentID, attribution.AuthorizationRef,
	} {
		if reference == "" || len(reference) > 256 || strings.TrimSpace(reference) != reference ||
			strings.IndexFunc(reference, unicode.IsControl) >= 0 {
			return errors.New("SecondBox execution attribution reference is invalid")
		}
	}
	if attribution.Generation < 1 || !attribution.ExpiresAt.After(now) {
		return errors.New("SecondBox execution attribution generation or expiry is invalid")
	}
	return nil
}

// WriteExecutionAttribution must finish before the Runner forwards any guest bytes.
// The caller owns the connection's write deadline and closes it on failure.
func WriteExecutionAttribution(writer io.Writer, attribution ExecutionAttribution) error {
	if err := attribution.validate(time.Now()); err != nil {
		return err
	}
	payload, err := json.Marshal(attribution)
	if err != nil {
		return fmt.Errorf("SecondBox execution attribution encoding failed: %w", err)
	}
	if len(payload) > maximumExecutionAttributionBytes {
		return errors.New("SecondBox execution attribution exceeds frame bound")
	}
	frame := make([]byte, len(executionAttributionMagic)+4+len(payload))
	copy(frame, executionAttributionMagic)
	binary.BigEndian.PutUint32(frame[len(executionAttributionMagic):], uint32(len(payload)))
	copy(frame[len(executionAttributionMagic)+4:], payload)
	written, err := writer.Write(frame)
	if err != nil {
		return fmt.Errorf("SecondBox execution attribution write failed: %w", err)
	}
	if written != len(frame) {
		return fmt.Errorf("SecondBox execution attribution write failed: %w", io.ErrShortWrite)
	}
	return nil
}

// ReadExecutionAttribution is for an authenticated Runner Unix peer, never a guest TCP peer.
// The caller supplies a read deadline; bytes following the preface remain unread.
func ReadExecutionAttribution(reader io.Reader, now time.Time) (ExecutionAttribution, error) {
	var attribution ExecutionAttribution
	header := make([]byte, len(executionAttributionMagic)+4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return attribution, fmt.Errorf("SecondBox execution attribution header failed: %w", err)
	}
	if string(header[:len(executionAttributionMagic)]) != executionAttributionMagic {
		return attribution, errors.New("SecondBox execution attribution protocol is unsupported")
	}
	length := binary.BigEndian.Uint32(header[len(executionAttributionMagic):])
	if length == 0 || length > maximumExecutionAttributionBytes {
		return attribution, errors.New("SecondBox execution attribution frame size is invalid")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return attribution, fmt.Errorf("SecondBox execution attribution payload failed: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&attribution); err != nil {
		return ExecutionAttribution{}, fmt.Errorf("SecondBox execution attribution payload is invalid: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ExecutionAttribution{}, errors.New("SecondBox execution attribution has trailing data")
	}
	if err := attribution.validate(now); err != nil {
		return ExecutionAttribution{}, err
	}
	return attribution, nil
}
