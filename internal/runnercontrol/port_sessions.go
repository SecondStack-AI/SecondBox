package runnercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// PortSessionAdmission is one authenticated request for a pinned Profile port.
type PortSessionAdmission struct {
	Session        contracts.PortSession
	StreamID       string
	TenantRef      string
	SubjectRef     string
	RequestID      string
	LeaseID        string
	IdempotencyKey string
	RequestHash    string
	// CredentialDigest binds the single-use credential to this admission so the
	// home Runner can reject a mismatch locally without ever holding the
	// credential itself.
	CredentialDigest []byte
	Now              time.Time
}

// PortTunnel is the private assignment-bound projection consumed by the proxy.
type PortTunnel struct {
	Session                     contracts.PortSession
	TenantRef                   string
	SubjectRef                  string
	RequestID                   string
	LeaseID                     string
	ProfileRevisionID           string
	AssignmentID                string
	InstanceID                  string
	RunnerID                    string
	StreamID                    string
	FencingToken                []byte
	GuestPort                   int64
	StreamWindowBytes           int64
	MaximumRequestBytes         int64
	MaximumResponseBytes        int64
	AcknowledgedInboundSequence int64
	// DataPlaneAddress is the home Runner's advertised caller-facing address. It
	// is returned only to an ingress holding the exact direct-endpoint grant.
	DataPlaneAddress string
	// DataPlaneCertificateSPKISHA256 is the admitted caller-facing certificate
	// public key. It is returned only with DataPlaneAddress.
	DataPlaneCertificateSPKISHA256 string
}

type dataPlaneEndpoint struct {
	Address               string `json:"address"`
	CertificateSPKISHA256 string `json:"certificateSpkiSha256"`
}

func encodeDataPlaneEndpoint(address string, certificateSPKISHA256 string) (string, error) {
	if err := validateAdvertisedDataPlaneAddress(address); err != nil {
		return "", err
	}
	if err := validateCertificateSPKISHA256(certificateSPKISHA256); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(dataPlaneEndpoint{
		Address: address, CertificateSPKISHA256: certificateSPKISHA256,
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func decodeDataPlaneEndpoint(encoded string) (dataPlaneEndpoint, error) {
	var endpoint dataPlaneEndpoint
	if err := json.Unmarshal([]byte(encoded), &endpoint); err != nil {
		return dataPlaneEndpoint{}, errors.New("SecondBox runner data-plane endpoint evidence is invalid")
	}
	if err := validateAdvertisedDataPlaneAddress(endpoint.Address); err != nil {
		return dataPlaneEndpoint{}, err
	}
	if err := validateCertificateSPKISHA256(endpoint.CertificateSPKISHA256); err != nil {
		return dataPlaneEndpoint{}, err
	}
	return endpoint, nil
}

// PortTunnelClose identifies one authenticated or already-consumed tunnel.
type PortTunnelClose struct {
	TenantRef      string
	SubjectRef     string
	SandboxID      string
	SessionID      string
	Generation     int64
	IdempotencyKey string
	RequestHash    string
	Reason         string
	Now            time.Time
}

// PortTunnelEvent is one runner-to-client payload or terminal outcome.
type PortTunnelEvent struct {
	Sequence       int64
	Bytes          []byte
	TerminalKind   string
	TerminalDetail string
}

// DirectPortConsumption is one home-Runner request to spend a single-use
// credential before it forwards any byte on a live socket.
type DirectPortConsumption struct {
	RunnerID         string
	SessionID        string
	AssignmentID     string
	Generation       int64
	FencingToken     []byte
	CredentialDigest []byte
	Now              time.Time
}

// RunnerDataPlaneFrame binds one payload-free Runner projection to the
// authenticated connection that delivered it.
type RunnerDataPlaneFrame struct {
	RunnerID     string
	ConnectionID string
	Message      *runnerv1.RunnerToControlPlane
}

// PortSessionStore persists Port admission, single-use connection state, and
// bounded accounting without retaining proxied payload bytes.
type PortSessionStore interface {
	AdmitPortSession(context.Context, PortSessionAdmission) (PortTunnel, bool, error)
	GetPortTunnel(context.Context, string, string, string, string, time.Time) (PortTunnel, error)
	ClosePortSession(context.Context, PortTunnelClose) (contracts.PortSession, error)
	ConsumePortSession(context.Context, string, string, string, time.Time) (PortTunnel, error)
	ConsumeDirectPortSession(context.Context, DirectPortConsumption) (PortTunnel, error)
	RecordPortClientBytes(context.Context, string, string, string, []byte, time.Time) error
	RecordPortTunnelAcknowledgement(context.Context, string, string, string, int64, time.Time) error
}

// PortSessionFrameRecorder projects payload-free Port frame accounting received
// on the authenticated Runner connection.
type PortSessionFrameRecorder interface {
	RecordPortSessionFrame(context.Context, RunnerDataPlaneFrame, time.Time) (bool, error)
}
