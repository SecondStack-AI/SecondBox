package microvmguest

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// AssignmentBindRequest carries the identity a template-mode guest installs once,
// after post-restore hardening and before its protocol listener accepts.
type AssignmentBindRequest struct {
	InstanceID              string `json:"instanceId"`
	SandboxID               string `json:"sandboxId"`
	SandboxGeneration       uint64 `json:"sandboxGeneration"`
	GuestBuildID            string `json:"guestBuildId"`
	ImageManifestDigest     string `json:"imageManifestDigest"`
	ToolchainManifestDigest string `json:"toolchainManifestDigest"`
	HeartbeatIntervalMs     uint64 `json:"heartbeatIntervalMs"`
}

// ErrAssignmentBindNotHardened is returned when a bind arrives before the guest
// has accepted fresh host entropy and a corrected clock through /restore/harden.
var ErrAssignmentBindNotHardened = errors.New("assignment bind requires post-restore hardening")

// ErrAssignmentAlreadyBound is returned for every bind after the first. A
// template-mode guest installs exactly one identity for its whole lifetime.
var ErrAssignmentAlreadyBound = errors.New("assignment identity is already installed")

// AssignmentGate holds the mutable template-mode state of one guest: whether
// post-restore hardening ran, and the single identity installed by the one
// permitted assignment bind. A nil gate means the guest took its identity from
// boot arguments and can never be bound.
type AssignmentGate struct {
	mu       sync.Mutex
	hardened bool
	bound    bool
	identity ProtocolIdentity
	boundCh  chan struct{}
}

func NewAssignmentGate() *AssignmentGate {
	return &AssignmentGate{boundCh: make(chan struct{})}
}

// MarkHardened records that /restore/harden succeeded. It is the precondition
// for Bind.
func (g *AssignmentGate) MarkHardened() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hardened = true
}

// Bind installs the assignment identity exactly once. It refuses a bind before
// hardening and every bind after the first.
func (g *AssignmentGate) Bind(req AssignmentBindRequest) (ProtocolIdentity, error) {
	identity, err := req.protocolIdentity()
	if err != nil {
		return ProtocolIdentity{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.hardened {
		return ProtocolIdentity{}, ErrAssignmentBindNotHardened
	}
	if g.bound {
		return ProtocolIdentity{}, ErrAssignmentAlreadyBound
	}
	g.identity = identity
	g.bound = true
	close(g.boundCh)
	return identity, nil
}

// Identity reports the installed identity. The second result is false until the
// one permitted bind succeeds.
func (g *AssignmentGate) Identity() (ProtocolIdentity, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.bound {
		return ProtocolIdentity{}, false
	}
	return g.identity, true
}

// Bound is closed when the assignment identity is installed.
func (g *AssignmentGate) Bound() <-chan struct{} { return g.boundCh }

func (req AssignmentBindRequest) protocolIdentity() (ProtocolIdentity, error) {
	if strings.TrimSpace(req.InstanceID) == "" ||
		strings.TrimSpace(req.SandboxID) == "" ||
		req.SandboxGeneration == 0 ||
		strings.TrimSpace(req.GuestBuildID) == "" ||
		strings.TrimSpace(req.ImageManifestDigest) == "" ||
		strings.TrimSpace(req.ToolchainManifestDigest) == "" {
		return ProtocolIdentity{}, fmt.Errorf("assignment bind identity is incomplete")
	}
	interval := time.Duration(req.HeartbeatIntervalMs) * time.Millisecond
	if interval < time.Millisecond || interval > maxGuestHeartbeatInterval {
		return ProtocolIdentity{}, fmt.Errorf(
			"assignment bind heartbeat interval must be from 1ms through %s",
			maxGuestHeartbeatInterval,
		)
	}
	return ProtocolIdentity{
		InstanceID:              req.InstanceID,
		SandboxID:               req.SandboxID,
		SandboxGeneration:       req.SandboxGeneration,
		GuestBuildID:            req.GuestBuildID,
		ImageManifestDigest:     req.ImageManifestDigest,
		ToolchainManifestDigest: req.ToolchainManifestDigest,
		HeartbeatInterval:       interval,
	}, nil
}

// GateListener wraps the guest protocol listener so a template-mode guest refuses
// every connection until its assignment identity is installed. The socket is bound
// before capture and stays bound across snapshot resume; only acceptance waits.
func (g *AssignmentGate) GateListener(inner net.Listener) net.Listener {
	return &assignmentGatedListener{inner: inner, gate: g}
}

type assignmentGatedListener struct {
	inner net.Listener
	gate  *AssignmentGate
}

func (l *assignmentGatedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			return nil, err
		}
		if _, bound := l.gate.Identity(); bound {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func (l *assignmentGatedListener) Close() error { return l.inner.Close() }

func (l *assignmentGatedListener) Addr() net.Addr { return l.inner.Addr() }
