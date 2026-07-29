package runnercontrol

import (
	"context"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// PortSessionAdmission is one authenticated request for a pinned Profile port.
type PortSessionAdmission struct {
	Session          contracts.PortSession
	StreamID         string
	ProjectID        string
	TenantRef        string
	SubjectRef       string
	ServiceAccountID string
	RequestID        string
	LeaseID          string
	IdempotencyKey   string
	RequestHash      string
	Now              time.Time
}

// PortTunnel is the private assignment-bound projection consumed by the proxy.
type PortTunnel struct {
	Session           contracts.PortSession
	ProjectID         string
	TenantRef         string
	SubjectRef        string
	ServiceAccountID  string
	RequestID         string
	LeaseID           string
	ProfileRevisionID string
	AssignmentID      string
	InstanceID        string
	RunnerID          string
	StreamID          string
	FencingToken      []byte
	GuestPort         int64
	StreamWindowBytes int64
}

// PortTunnelClose identifies one authenticated or already-consumed tunnel.
type PortTunnelClose struct {
	ProjectID        string
	TenantRef        string
	SubjectRef       string
	SandboxID        string
	SessionID        string
	Generation       int64
	ServiceAccountID string
	IdempotencyKey   string
	RequestHash      string
	Reason           string
	Now              time.Time
}

// PortTunnelEvent is one runner-to-client payload or terminal outcome.
type PortTunnelEvent struct {
	Sequence       int64
	Bytes          []byte
	TerminalKind   string
	TerminalDetail string
}

// PortSessionRelay persists port admission, single-use connection state, and bounded bytes.
type PortSessionRelay interface {
	AdmitPortSession(context.Context, PortSessionAdmission) (PortTunnel, bool, error)
	GetPortSession(context.Context, string, string, string, string, time.Time) (contracts.PortSession, error)
	ClosePortSession(context.Context, PortTunnelClose) (contracts.PortSession, error)
	ConsumePortSession(context.Context, string, string, string, time.Time) (PortTunnel, error)
	QueuePortClientBytes(context.Context, string, string, string, []byte, time.Time) error
	NextPortTunnelEvent(context.Context, string, string, string, int64, time.Time) (PortTunnelEvent, bool, error)
	AcknowledgePortTunnelEvent(context.Context, string, string, string, int64, time.Time) error
}
