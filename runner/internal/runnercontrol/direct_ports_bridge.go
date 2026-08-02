package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// errDirectPortDeadline distinguishes a session deadline from a peer close.
var errDirectPortDeadline = errors.New("SecondBox Port session deadline passed")

// bridgeDirectPortConnection copies bytes between one admitted caller socket and
// the existing guest-protocol Port stream.
//
// TCP flow control governs the caller leg. The guest-protocol credit window
// governs the guest leg, because backpressure must still reach the guest
// process. No payload byte is persisted on either leg.
func (s *RunnerProtocolService) bridgeDirectPortConnection(
	ctx context.Context,
	connection net.Conn,
	session *directPortSession,
) error {
	if s.portBackend == nil {
		s.finishDirectPortSession(
			session,
			runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE,
			"runner Port backend is unavailable",
		)
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "port backend unavailable")
		return errors.New("SecondBox runner Port backend is unavailable")
	}
	remaining := time.Until(session.deadline)
	if remaining <= 0 {
		s.finishDirectPortSession(
			session,
			runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED,
			"port session deadline passed",
		)
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "port session deadline passed")
		return errDirectPortDeadline
	}
	bridgeCtx, cancelBridge := context.WithCancelCause(ctx)
	defer cancelBridge(nil)
	deadlineTimer := time.AfterFunc(remaining, func() {
		cancelBridge(errDirectPortDeadline)
	})
	defer deadlineTimer.Stop()

	guest, err := s.portBackend.OpenPort(
		bridgeCtx,
		cloneRunnerFence(session.fence),
		&runnerprotocol.PortOpen{
			GuestPort:     session.guestPort,
			Protocol:      session.protocol,
			IdleTimeoutMs: uint64(remaining.Milliseconds()) + 1,
		},
	)
	if err != nil {
		s.finishDirectPortSession(
			session,
			runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE,
			"guest port is unavailable",
		)
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "guest port unavailable")
		return fmt.Errorf("SecondBox runner direct Port guest open: %w", err)
	}
	// Cancelling the bridge closes both legs, which is how a fence, a deadline,
	// a drain, or a lost control connection terminates admitted work. The
	// registration deliberately outlives the copy loops: the deferred cancel
	// above guarantees it runs exactly once, and both closes tolerate a repeat.
	context.AfterFunc(bridgeCtx, func() {
		_ = guest.Close()
		_ = connection.Close()
	})

	if !session.bindConnection(cancelBridge, func(
		kind runnerprotocol.PortTerminalKind,
		detail string,
	) {
		s.finishDirectPortSession(session, kind, detail)
	}) {
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "port session is closed")
		return errors.New("SecondBox Port session closed before its caller connection was bridged")
	}
	// Evidence precedes the protocol frame it describes, matching how every other
	// Runner operation orders definitive local outcomes against the wire. A sink
	// failure denies the connection rather than admitting work with no record of
	// it.
	if err := s.emitEvidence(
		ctx,
		runnerevidence.EventPortOpen,
		session.fence,
		session.correlation,
		session.operationID,
		"PORT_DIRECT_ADMITTED",
		"admitted",
	); err != nil {
		s.finishDirectPortSession(
			session,
			runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FAILED,
			"port open evidence could not be emitted",
		)
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "port open evidence failed")
		return err
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		s.finishDirectPortSession(
			session,
			runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FAILED,
			"caller socket deadline could not be cleared",
		)
		return err
	}
	if err := portdirect.WriteVerdict(connection, portdirect.VerdictAdmitted, ""); err != nil {
		s.finishDirectPortSession(
			session,
			runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FAILED,
			"caller socket rejected the admission frame",
		)
		return err
	}

	outcomes := make(chan error, 2)
	go func() {
		outcomes <- copyCallerToGuest(bridgeCtx, connection, guest)
	}()
	go func() {
		outcomes <- copyGuestToCaller(bridgeCtx, guest, connection)
	}()
	first := <-outcomes
	// Either side terminating closes both legs before the second copy loop is
	// awaited, so no goroutine outlives the connection it was serving.
	cancelBridge(first)
	closeErr := errors.Join(guest.Close(), connection.Close())
	<-outcomes
	kind, detail := directPortTerminal(bridgeCtx, first)
	s.finishDirectPortSession(session, kind, detail)
	if first == nil || errors.Is(first, io.EOF) {
		return closeErr
	}
	return errors.Join(first, closeErr)
}

func copyCallerToGuest(
	ctx context.Context,
	connection net.Conn,
	guest PortConnection,
) error {
	buffer := make([]byte, directPortChunkBytes)
	for {
		count, readErr := connection.Read(buffer)
		if count > 0 {
			if writeErr := guest.Write(ctx, buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			return readErr
		}
	}
}

func copyGuestToCaller(
	ctx context.Context,
	guest PortConnection,
	connection net.Conn,
) error {
	for {
		data, readErr := guest.Read(ctx, directPortChunkBytes)
		if len(data) > 0 {
			if _, writeErr := connection.Write(data); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			return readErr
		}
	}
}

func directPortTerminal(
	ctx context.Context,
	failure error,
) (runnerprotocol.PortTerminalKind, string) {
	switch {
	case errors.Is(context.Cause(ctx), errDirectPortDeadline):
		return runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED,
			"port session deadline passed"
	case failure == nil || errors.Is(failure, io.EOF):
		return runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED,
			"port connection closed"
	case errors.Is(failure, net.ErrClosed), errors.Is(failure, context.Canceled):
		return runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED,
			"port connection cancelled"
	default:
		return runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FAILED,
			"port connection failed"
	}
}

// finishDirectPortSession returns bounded proof of closure exactly once: one
// fixed-shape evidence record and one terminal frame that lets the control
// plane project the durable PortSession state.
func (s *RunnerProtocolService) finishDirectPortSession(
	session *directPortSession,
	kind runnerprotocol.PortTerminalKind,
	detail string,
) {
	session.mu.Lock()
	if session.finished {
		session.mu.Unlock()
		return
	}
	session.finished = true
	session.mu.Unlock()
	s.directPorts.remove(session)
	if err := s.emitEvidence(
		context.Background(),
		runnerevidence.EventPortTerminal,
		session.fence,
		session.correlation,
		session.operationID,
		kind.String(),
		terminalOutcome(kind.String()),
	); err != nil {
		reportDirectPortClosureFailure(session, "evidence", err)
	}
	stream := s.directPorts.currentStream()
	if stream == nil {
		return
	}
	if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Port{Port: &runnerprotocol.PortFrame{
			Fence:       cloneRunnerFence(session.fence),
			OperationId: session.operationID,
			StreamId:    session.streamID,
			Sequence:    1,
			Correlation: cloneRunnerCorrelation(session.correlation),
			Payload: &runnerprotocol.PortFrame_Terminal{Terminal: &runnerprotocol.PortTerminal{
				Kind: kind, SafeDetail: detail,
			}},
		}},
	}); err != nil {
		reportDirectPortClosureFailure(session, "terminal", err)
	}
}

func reportDirectPortClosureFailure(session *directPortSession, stage string, err error) {
	slog.Warn(
		"SecondBox runner direct Port closure proof failed",
		"stage", stage,
		"operationId", session.operationID,
		"sandboxId", session.fence.GetSandboxId(),
		"error", err,
	)
}
