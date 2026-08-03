// Package portdirect defines the bounded caller-facing handshake that precedes
// every byte of a direct SecondBox data-plane connection.
//
// The caller connects over pinned TLS, writes exactly one credential frame, and
// reads exactly one admission frame. Port payload bytes flow raw in both
// directions only after an admitted verdict, so the framing never has to be
// re-entered mid-stream.
//
// The control plane owns an identical definition in pkg/portdirect. The two are
// separate because this privileged module deliberately shares no dependency
// graph with the control plane, the same reason the generated protocol code is
// duplicated. Any change here must be mirrored there.
package portdirect

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Magic identifies generation one of the direct data-plane handshake.
const Magic = "SBXDP1"

const (
	// MaximumCredentialBytes bounds the credential an unauthenticated peer can
	// force the Runner to buffer.
	MaximumCredentialBytes = 2048
	// MaximumDetailBytes bounds the safe detail returned with a verdict.
	MaximumDetailBytes = 128
)

// SessionKind identifies the admitted data-plane operation.
type SessionKind byte

const (
	SessionKindPort SessionKind = iota
	SessionKindExec
	SessionKindPTY
	SessionKindFile
)

func (kind SessionKind) String() string {
	switch kind {
	case SessionKindPort:
		return "port"
	case SessionKindExec:
		return "exec"
	case SessionKindPTY:
		return "pty"
	case SessionKindFile:
		return "file"
	default:
		return "unknown"
	}
}

func validSessionKind(kind SessionKind) bool {
	return kind >= SessionKindPort && kind <= SessionKindFile
}

// Credential is the bounded authority presented for one session kind.
type Credential struct {
	SessionKind SessionKind
	Value       string
}

// Verdict is the single admission outcome for one caller connection.
type Verdict byte

const (
	// VerdictAdmitted precedes bidirectional payload bytes.
	VerdictAdmitted Verdict = 0
	// VerdictDenied is terminal; the Runner closes the connection after it.
	VerdictDenied Verdict = 1
	// VerdictSessionKindUnsupported rejects a valid kind with no wired transport.
	VerdictSessionKindUnsupported Verdict = 2
)

// ErrHandshakeMalformed identifies a peer that is not speaking this protocol.
var ErrHandshakeMalformed = errors.New("SecondBox direct data-plane handshake is malformed")

// WriteCredential emits the leading credential frame.
func WriteCredential(writer io.Writer, kind SessionKind, credential string) error {
	if !validSessionKind(kind) {
		return fmt.Errorf("SecondBox direct data-plane session kind is invalid")
	}
	if len(credential) == 0 || len(credential) > MaximumCredentialBytes {
		return fmt.Errorf("SecondBox direct data-plane credential must contain 1 to %d bytes", MaximumCredentialBytes)
	}
	frame := make([]byte, 0, len(Magic)+3+len(credential))
	frame = append(frame, Magic...)
	frame = append(frame, byte(kind))
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(credential)))
	frame = append(frame, credential...)
	if _, err := writer.Write(frame); err != nil {
		return fmt.Errorf("SecondBox direct data-plane credential write: %w", err)
	}
	return nil
}

// ReadCredential consumes the leading credential frame and nothing beyond it.
func ReadCredential(reader io.Reader) (Credential, error) {
	header := make([]byte, len(Magic)+3)
	if _, err := io.ReadFull(reader, header); err != nil {
		return Credential{}, fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	if string(header[:len(Magic)]) != Magic {
		return Credential{}, ErrHandshakeMalformed
	}
	kind := SessionKind(header[len(Magic)])
	if !validSessionKind(kind) {
		return Credential{}, ErrHandshakeMalformed
	}
	length := int(binary.BigEndian.Uint16(header[len(Magic)+1:]))
	if length == 0 || length > MaximumCredentialBytes {
		return Credential{}, ErrHandshakeMalformed
	}
	credential := make([]byte, length)
	if _, err := io.ReadFull(reader, credential); err != nil {
		return Credential{}, fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	return Credential{SessionKind: kind, Value: string(credential)}, nil
}

// WriteVerdict emits the single admission frame. The detail is truncated rather
// than rejected so a denial always reaches the caller.
func WriteVerdict(writer io.Writer, verdict Verdict, detail string) error {
	if verdict != VerdictAdmitted && verdict != VerdictDenied &&
		verdict != VerdictSessionKindUnsupported {
		return fmt.Errorf("SecondBox direct data-plane verdict is invalid")
	}
	if len(detail) > MaximumDetailBytes {
		detail = detail[:MaximumDetailBytes]
	}
	frame := make([]byte, 0, len(Magic)+3+len(detail))
	frame = append(frame, Magic...)
	frame = append(frame, byte(verdict))
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(detail)))
	frame = append(frame, detail...)
	if _, err := writer.Write(frame); err != nil {
		return fmt.Errorf("SecondBox direct data-plane verdict write: %w", err)
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
	if verdict != VerdictAdmitted && verdict != VerdictDenied &&
		verdict != VerdictSessionKindUnsupported {
		return VerdictDenied, "", ErrHandshakeMalformed
	}
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

// TLSConfigForSPKIPin returns a TLS 1.3 client configuration for the exact
// certificate public key admitted by the control plane. Runners are commonly
// addressed by IP, so SPKI pinning avoids imposing a DNS naming scheme solely
// for hostname verification while still authenticating the presented key.
func TLSConfigForSPKIPin(pin string) (*tls.Config, error) {
	expected, err := hex.DecodeString(pin)
	if err != nil || len(expected) != sha256.Size || pin != hex.EncodeToString(expected) {
		return nil, fmt.Errorf("SecondBox direct data-plane certificate SPKI SHA-256 is invalid")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Verification is performed against the admitted SPKI below instead of a
		// hostname, which may be absent when the endpoint is an IP address.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("SecondBox direct data-plane TLS peer presented no certificate")
			}
			presented := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(presented[:], expected) != 1 {
				return fmt.Errorf("SecondBox direct data-plane certificate SPKI SHA-256 does not match admission")
			}
			return nil
		},
	}, nil
}
