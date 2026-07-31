// Package portdirect defines the bounded caller-facing handshake that precedes
// every byte of a direct SecondBox Port connection.
//
// The caller connects, writes exactly one credential frame, and reads exactly
// one admission frame. Payload bytes flow raw in both directions only after an
// admitted verdict, so the framing never has to be re-entered mid-stream.
//
// The control plane owns an identical definition in pkg/portdirect. The two are
// separate because this privileged module deliberately shares no dependency
// graph with the control plane, the same reason the generated protocol code is
// duplicated. Any change here must be mirrored there.
package portdirect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Magic identifies generation one of the direct Port handshake.
const Magic = "SBXPORT1"

const (
	// MaximumCredentialBytes bounds the credential an unauthenticated peer can
	// force the Runner to buffer.
	MaximumCredentialBytes = 2048
	// MaximumDetailBytes bounds the safe detail returned with a verdict.
	MaximumDetailBytes = 128
)

// Verdict is the single admission outcome for one caller connection.
type Verdict byte

const (
	// VerdictAdmitted precedes bidirectional payload bytes.
	VerdictAdmitted Verdict = 0
	// VerdictDenied is terminal; the Runner closes the connection after it.
	VerdictDenied Verdict = 1
)

// ErrHandshakeMalformed identifies a peer that is not speaking this protocol.
var ErrHandshakeMalformed = errors.New("SecondBox direct Port handshake is malformed")

// WriteCredential emits the leading credential frame.
func WriteCredential(writer io.Writer, credential string) error {
	if len(credential) == 0 || len(credential) > MaximumCredentialBytes {
		return fmt.Errorf("SecondBox direct Port credential must contain 1 to %d bytes", MaximumCredentialBytes)
	}
	frame := make([]byte, 0, len(Magic)+2+len(credential))
	frame = append(frame, Magic...)
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(credential)))
	frame = append(frame, credential...)
	if _, err := writer.Write(frame); err != nil {
		return fmt.Errorf("SecondBox direct Port credential write: %w", err)
	}
	return nil
}

// ReadCredential consumes the leading credential frame and nothing beyond it.
func ReadCredential(reader io.Reader) (string, error) {
	header := make([]byte, len(Magic)+2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	if string(header[:len(Magic)]) != Magic {
		return "", ErrHandshakeMalformed
	}
	length := int(binary.BigEndian.Uint16(header[len(Magic):]))
	if length == 0 || length > MaximumCredentialBytes {
		return "", ErrHandshakeMalformed
	}
	credential := make([]byte, length)
	if _, err := io.ReadFull(reader, credential); err != nil {
		return "", fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	return string(credential), nil
}

// WriteVerdict emits the single admission frame. The detail is truncated rather
// than rejected so a denial always reaches the caller.
func WriteVerdict(writer io.Writer, verdict Verdict, detail string) error {
	if len(detail) > MaximumDetailBytes {
		detail = detail[:MaximumDetailBytes]
	}
	frame := make([]byte, 0, len(Magic)+3+len(detail))
	frame = append(frame, Magic...)
	frame = append(frame, byte(verdict))
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(detail)))
	frame = append(frame, detail...)
	if _, err := writer.Write(frame); err != nil {
		return fmt.Errorf("SecondBox direct Port verdict write: %w", err)
	}
	return nil
}

// ReadVerdict consumes the single admission frame and nothing beyond it.
func ReadVerdict(reader io.Reader) (Verdict, string, error) {
	header := make([]byte, len(Magic)+3)
	if _, err := io.ReadFull(reader, header); err != nil {
		return VerdictDenied, "", fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	if string(header[:len(Magic)]) != Magic {
		return VerdictDenied, "", ErrHandshakeMalformed
	}
	verdict := Verdict(header[len(Magic)])
	length := int(binary.BigEndian.Uint16(header[len(Magic)+1:]))
	if length > MaximumDetailBytes {
		return VerdictDenied, "", ErrHandshakeMalformed
	}
	detail := make([]byte, length)
	if _, err := io.ReadFull(reader, detail); err != nil {
		return VerdictDenied, "", fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	return verdict, string(detail), nil
}
