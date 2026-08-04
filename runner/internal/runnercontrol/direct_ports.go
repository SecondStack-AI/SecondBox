package runnercontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

// directPortSession is the assignment-bound state the Runner holds for one
// admitted PortSession on the direct transport.
//
// Every field is authority the control plane already established durably. The
// Runner compares a presented credential against it locally so an
// unauthenticated peer cannot force control-plane work, and then spends the
// credential against PostgreSQL, which remains the only consumption authority.
type directPortSession struct {
	fence            *runnerprotocol.AssignmentFence
	correlation      *runnerprotocol.Correlation
	operationID      string
	streamID         string
	portName         string
	protocol         string
	guestPort        uint32
	leaseID          string
	deadline         time.Time
	credentialDigest [sha256.Size]byte

	mu       sync.Mutex
	claimed  bool
	closed   bool
	finished bool
	cancel   context.CancelCauseFunc
	terminal func(kind runnerprotocol.PortTerminalKind, detail string)
}

// claim enforces local single use before any control-plane round trip.
func (session *directPortSession) claim() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.claimed || session.closed {
		return false
	}
	session.claimed = true
	return true
}

func (session *directPortSession) release() {
	session.mu.Lock()
	session.claimed = false
	session.mu.Unlock()
}

func (session *directPortSession) bindConnection(
	cancel context.CancelCauseFunc,
	terminal func(runnerprotocol.PortTerminalKind, string),
) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return false
	}
	session.cancel, session.terminal = cancel, terminal
	return true
}

// directPortRegistry tracks admitted direct PortSessions and the in-flight
// consumption round trips for one control-plane connection.
type directPortRegistry struct {
	mu         sync.Mutex
	sessions   map[string]*directPortSession
	admissions map[string]chan *runnerprotocol.PortDirectAdmission
	admitted   chan struct{}
	stream     RunnerProtocolStream
	nextID     uint64
}

func newDirectPortRegistry() *directPortRegistry {
	return &directPortRegistry{
		sessions:   make(map[string]*directPortSession),
		admissions: make(map[string]chan *runnerprotocol.PortDirectAdmission),
		admitted:   make(chan struct{}),
	}
}

func (registry *directPortRegistry) bindStream(stream RunnerProtocolStream) {
	registry.mu.Lock()
	registry.stream = stream
	registry.mu.Unlock()
}

func (registry *directPortRegistry) currentStream() RunnerProtocolStream {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.stream
}

// add records one admitted session idempotently. A redelivered admitting frame
// must not replace a session that already has a live caller connection, because
// the replacement would be invisible to fencing and teardown.
func (registry *directPortRegistry) add(session *directPortSession) {
	key := hex.EncodeToString(session.credentialDigest[:])
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.sessions[key]; exists {
		return
	}
	registry.sessions[key] = session
	// Broadcast to every connection already waiting for its admitting frame.
	close(registry.admitted)
	registry.admitted = make(chan struct{})
}

func (registry *directPortRegistry) remove(session *directPortSession) {
	registry.mu.Lock()
	delete(registry.sessions, hex.EncodeToString(session.credentialDigest[:]))
	registry.mu.Unlock()
}

// lookup resolves a presented credential by the full SHA-256 digest of the
// credential itself, so a hit is a whole-digest match and a near miss is
// indistinguishable from an unknown credential. The comparison that has to be
// constant time is the one against the durable digest, and the control plane
// performs it while spending the credential.
func (registry *directPortRegistry) lookup(digest [sha256.Size]byte) *directPortSession {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.lookupLocked(digest)
}

func (registry *directPortRegistry) lookupLocked(digest [sha256.Size]byte) *directPortSession {
	return registry.sessions[hex.EncodeToString(digest[:])]
}

// awaitSession resolves a presented credential, waiting a bounded time for the
// admitting frame to arrive.
//
// A caller can legitimately connect before the Runner has been told about its
// session: the control plane commits the admission and hands the caller its
// endpoint in one response, while the admitting frame reaches the Runner over
// the durable command path. Waiting turns that ordering into correct admission
// instead of a spurious denial. An unknown credential still ends in denial, and
// the wait stays inside the handshake deadline that already bounds how long an
// unauthenticated peer can hold a Runner goroutine.
func (registry *directPortRegistry) awaitSession(
	ctx context.Context,
	digest [sha256.Size]byte,
	within time.Duration,
) *directPortSession {
	deadline := time.Now().Add(within)
	for {
		registry.mu.Lock()
		session := registry.lookupLocked(digest)
		admitted := registry.admitted
		registry.mu.Unlock()
		if session != nil {
			return session
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-admitted:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (registry *directPortRegistry) hasSession(operationID string) bool {
	if operationID == "" {
		return false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, session := range registry.sessions {
		if session.operationID == operationID {
			return true
		}
	}
	return false
}

func (registry *directPortRegistry) closeAssignment(assignmentID string, reason string) {
	if assignmentID == "" {
		return
	}
	registry.closeMatching(
		runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FENCED,
		reason,
		func(session *directPortSession) bool {
			return session.fence.GetAssignmentId() == assignmentID
		},
	)
}

func (registry *directPortRegistry) closeSession(operationID string, reason string) {
	if operationID == "" {
		return
	}
	registry.closeMatching(
		runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED,
		reason,
		func(session *directPortSession) bool {
			return session.operationID == operationID
		},
	)
}

func (registry *directPortRegistry) closeAll(reason string) {
	registry.closeMatching(
		runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FENCED,
		reason,
		func(*directPortSession) bool { return true },
	)
}

func (registry *directPortRegistry) closeMatching(
	kind runnerprotocol.PortTerminalKind,
	reason string,
	matches func(*directPortSession) bool,
) {
	registry.mu.Lock()
	selected := make([]*directPortSession, 0, len(registry.sessions))
	for key, session := range registry.sessions {
		if matches(session) {
			selected = append(selected, session)
			delete(registry.sessions, key)
		}
	}
	registry.mu.Unlock()
	for _, session := range selected {
		session.mu.Lock()
		session.closed = true
		cancel, terminal := session.cancel, session.terminal
		session.mu.Unlock()
		if terminal != nil {
			terminal(kind, reason)
		}
		if cancel != nil {
			cancel(errors.New(reason))
		}
	}
}

func (registry *directPortRegistry) nextAdmission() (string, chan *runnerprotocol.PortDirectAdmission) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.nextID++
	messageID := fmt.Sprintf("port-direct-%d", registry.nextID)
	responses := make(chan *runnerprotocol.PortDirectAdmission, 1)
	registry.admissions[messageID] = responses
	return messageID, responses
}

func (registry *directPortRegistry) forgetAdmission(messageID string) {
	registry.mu.Lock()
	delete(registry.admissions, messageID)
	registry.mu.Unlock()
}

func (registry *directPortRegistry) deliverAdmission(
	admission *runnerprotocol.PortDirectAdmission,
) error {
	if admission == nil || admission.MessageId == "" {
		return fmt.Errorf("SecondBox runner direct Port admission is incomplete")
	}
	registry.mu.Lock()
	responses := registry.admissions[admission.MessageId]
	registry.mu.Unlock()
	if responses == nil {
		// A verdict for an abandoned connection is not a protocol failure; the
		// credential was still spent exactly once.
		return nil
	}
	select {
	case responses <- admission:
	default:
	}
	return nil
}

// registerDirectPortSession records one admitted PortSession that will be
// carried by a caller connection rather than by durable data-plane frames.
func (s *RunnerProtocolService) registerDirectPortSession(
	frame *runnerprotocol.PortFrame,
	open *runnerprotocol.PortDirectOpen,
) error {
	if open == nil || open.GuestPort == 0 || open.GuestPort > 65535 ||
		(open.Protocol != "tcp" && open.Protocol != "http") ||
		open.PortName == "" || open.DeadlineUnixMs == 0 ||
		len(open.CredentialDigest) != sha256.Size || open.LeaseId == "" {
		return fmt.Errorf("SecondBox runner direct Port Open is incomplete")
	}
	session := &directPortSession{
		fence:       cloneRunnerFence(frame.Fence),
		correlation: cloneRunnerCorrelation(frame.Correlation),
		operationID: frame.OperationId,
		streamID:    frame.StreamId,
		portName:    open.PortName,
		protocol:    open.Protocol,
		guestPort:   open.GuestPort,
		leaseID:     open.LeaseId,
		deadline:    time.UnixMilli(int64(open.DeadlineUnixMs)).UTC(),
	}
	copy(session.credentialDigest[:], open.CredentialDigest)
	s.directPorts.add(session)
	return nil
}

// serveDirectPortConnection admits one caller connection and bridges it to the
// existing guest-protocol Port stream. No payload byte is ever persisted.
func (s *RunnerProtocolService) serveDirectPortConnection(
	ctx context.Context,
	connection net.Conn,
) {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(directPortHandshakeTimeout)); err != nil {
		return
	}
	credential, err := portdirect.ReadCredential(connection)
	if err != nil {
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "handshake rejected")
		return
	}
	if credential.SessionKind == portdirect.SessionKindExec ||
		credential.SessionKind == portdirect.SessionKindPTY ||
		credential.SessionKind == portdirect.SessionKindFile {
		_ = s.serveDirectTypedConnection(ctx, connection, credential)
		return
	}
	if credential.SessionKind != portdirect.SessionKindPort {
		_ = portdirect.WriteVerdict(
			connection,
			portdirect.VerdictSessionKindUnsupported,
			credential.SessionKind.String()+" session kind is not implemented",
		)
		return
	}
	digest := sha256.Sum256([]byte(credential.Value))
	session := s.directPorts.awaitSession(ctx, digest, directPortAdmittingFrameWait)
	if session == nil {
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "credential rejected")
		return
	}
	// Local rejection before the round trip keeps an unauthenticated peer from
	// forcing control-plane work.
	if reason := s.rejectDirectPortLocally(session); reason != "" {
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, reason)
		return
	}
	if !session.claim() {
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "credential already used")
		return
	}
	// The handshake deadline bounds the unauthenticated phase. A peer that has
	// presented a locally accepted credential is no longer unauthenticated, so
	// the consumption round trip gets its own bound rather than sharing one.
	if err := connection.SetDeadline(time.Now().Add(directPortAdmissionTimeout)); err != nil {
		session.release()
		return
	}
	admitted, detail, err := s.consumeDirectPortCredential(ctx, session)
	if err != nil || !admitted {
		session.release()
		if detail == "" {
			detail = "credential rejected"
		}
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, detail)
		return
	}
	if err := s.bridgeDirectPortConnection(ctx, connection, session); err != nil {
		slog.Info(
			"SecondBox runner direct Port connection closed",
			"operationId", session.operationID,
			"sandboxId", session.fence.GetSandboxId(),
			"error", err,
		)
	}
}

// rejectDirectPortLocally returns a bounded safe detail when the session state
// the Runner already holds cannot admit this connection.
func (s *RunnerProtocolService) rejectDirectPortLocally(session *directPortSession) string {
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	switch {
	case closed:
		return "port session is closed"
	case !time.Now().UTC().Before(session.deadline):
		return "port session deadline passed"
	case !s.hasActiveFence(session.fence):
		return "assignment fence is not active"
	case s.drainPhase() != runnerprotocol.DrainPhase_DRAIN_PHASE_ACTIVE:
		return "runner is draining"
	default:
		return ""
	}
}

// consumeDirectPortCredential spends the single-use credential through the
// authenticated control connection. PostgreSQL remains the single consumption
// authority, so a replayed or already-consumed credential fails there.
func (s *RunnerProtocolService) consumeDirectPortCredential(
	ctx context.Context,
	session *directPortSession,
) (bool, string, error) {
	stream := s.directPorts.currentStream()
	if stream == nil {
		return false, "control-plane connection unavailable", errors.New(
			"SecondBox runner direct Port admission has no control-plane connection",
		)
	}
	messageID, responses := s.directPorts.nextAdmission()
	defer s.directPorts.forgetAdmission(messageID)
	if err := s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_PortDirectConsume{
					PortDirectConsume: &runnerprotocol.PortDirectConsume{
						MessageId:        messageID,
						Sequence:         sequence,
						Fence:            cloneRunnerFence(session.fence),
						OperationId:      session.operationID,
						StreamId:         session.streamID,
						Correlation:      cloneRunnerCorrelation(session.correlation),
						CredentialDigest: session.credentialDigest[:],
					},
				},
			}
		},
	); err != nil {
		return false, "control-plane connection unavailable", err
	}
	timer := time.NewTimer(directPortAdmissionTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, "runner stopping", ctx.Err()
	case <-timer.C:
		return false, "admission timed out", errors.New(
			"SecondBox runner direct Port admission timed out",
		)
	case admission := <-responses:
		if admission.GetKind() !=
			runnerprotocol.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_ADMITTED {
			return false, admission.GetSafeDetail(), nil
		}
		if admission.GetOperationId() != session.operationID ||
			!proto.Equal(admission.GetFence(), session.fence) {
			return false, "admission identity mismatch", errors.New(
				"SecondBox runner direct Port admission identity does not match the session",
			)
		}
		return true, "", nil
	}
}
