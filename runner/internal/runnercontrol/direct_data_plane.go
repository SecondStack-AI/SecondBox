package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type directDataPlaneSession struct {
	fence            *runnerprotocol.AssignmentFence
	correlation      *runnerprotocol.Correlation
	operationID      string
	streamID         string
	kind             runnerprotocol.DataPlaneSessionKind
	deadline         time.Time
	streamWindow     uint64
	credentialDigest [sha256.Size]byte

	mu       sync.Mutex
	claimed  bool
	consumed bool
	opened   bool
	closed   bool
	cancel   context.CancelCauseFunc
}

func (session *directDataPlaneSession) claim() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.claimed || session.closed {
		return false
	}
	session.claimed = true
	return true
}

func (session *directDataPlaneSession) release() {
	session.mu.Lock()
	session.claimed = false
	session.mu.Unlock()
}

type directDataPlaneRegistry struct {
	mu         sync.Mutex
	sessions   map[string]*directDataPlaneSession
	admissions map[string]chan *runnerprotocol.DataPlaneDirectAdmission
	admitted   chan struct{}
	stream     RunnerProtocolStream
	nextID     uint64
}

func newDirectDataPlaneRegistry() *directDataPlaneRegistry {
	return &directDataPlaneRegistry{
		sessions:   make(map[string]*directDataPlaneSession),
		admissions: make(map[string]chan *runnerprotocol.DataPlaneDirectAdmission),
		admitted:   make(chan struct{}),
	}
}

func (registry *directDataPlaneRegistry) bindStream(stream RunnerProtocolStream) {
	registry.mu.Lock()
	registry.stream = stream
	registry.mu.Unlock()
}

// currentStream is the Runner's control connection, or nil between connections.
func (registry *directDataPlaneRegistry) currentStream() RunnerProtocolStream {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.stream
}

func (registry *directDataPlaneRegistry) add(session *directDataPlaneSession) {
	key := hex.EncodeToString(session.credentialDigest[:])
	registry.mu.Lock()
	if _, exists := registry.sessions[key]; !exists {
		registry.sessions[key] = session
		close(registry.admitted)
		registry.admitted = make(chan struct{})
	}
	registry.mu.Unlock()
}

func (registry *directDataPlaneRegistry) await(
	ctx context.Context,
	digest [sha256.Size]byte,
) *directDataPlaneSession {
	deadline := time.Now().Add(directPortAdmittingFrameWait)
	for {
		registry.mu.Lock()
		var session *directDataPlaneSession
		for _, candidate := range registry.sessions {
			if subtle.ConstantTimeCompare(candidate.credentialDigest[:], digest[:]) == 1 {
				session = candidate
			}
		}
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

func (registry *directDataPlaneRegistry) remove(session *directDataPlaneSession) {
	registry.mu.Lock()
	delete(registry.sessions, hex.EncodeToString(session.credentialDigest[:]))
	registry.mu.Unlock()
}

func (registry *directDataPlaneRegistry) complete(operationID string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for key, session := range registry.sessions {
		if session.operationID == operationID {
			delete(registry.sessions, key)
		}
	}
}

func (registry *directDataPlaneRegistry) find(operationID string) *directDataPlaneSession {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, session := range registry.sessions {
		if session.operationID == operationID {
			return session
		}
	}
	return nil
}

func (registry *directDataPlaneRegistry) closeAssignment(assignmentID string, reason string) {
	registry.closeMatching(reason, func(session *directDataPlaneSession) bool {
		return session.fence.GetAssignmentId() == assignmentID
	})
}

func (registry *directDataPlaneRegistry) closeSession(operationID string, reason string) {
	registry.closeMatching(reason, func(session *directDataPlaneSession) bool {
		return session.operationID == operationID
	})
}

func (registry *directDataPlaneRegistry) closeAll(reason string) {
	registry.closeMatching(reason, func(*directDataPlaneSession) bool { return true })
}

func (registry *directDataPlaneRegistry) closeNonPTY(reason string) {
	registry.closeMatching(reason, func(session *directDataPlaneSession) bool {
		return session.kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY
	})
}

func (registry *directDataPlaneRegistry) closeMatching(
	reason string,
	matches func(*directDataPlaneSession) bool,
) {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	selected := make([]*directDataPlaneSession, 0)
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
		cancel := session.cancel
		session.mu.Unlock()
		if cancel != nil {
			cancel(errors.New(reason))
		}
	}
}

func (registry *directDataPlaneRegistry) nextAdmission() (
	string,
	chan *runnerprotocol.DataPlaneDirectAdmission,
	RunnerProtocolStream,
) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.nextID++
	messageID := fmt.Sprintf("data-plane-direct-%d", registry.nextID)
	responses := make(chan *runnerprotocol.DataPlaneDirectAdmission, 1)
	registry.admissions[messageID] = responses
	return messageID, responses, registry.stream
}

func (registry *directDataPlaneRegistry) forgetAdmission(messageID string) {
	registry.mu.Lock()
	delete(registry.admissions, messageID)
	registry.mu.Unlock()
}

func (registry *directDataPlaneRegistry) deliverAdmission(
	admission *runnerprotocol.DataPlaneDirectAdmission,
) error {
	if admission == nil || admission.MessageId == "" {
		return errors.New("SecondBox runner direct data-plane admission is incomplete")
	}
	registry.mu.Lock()
	responses := registry.admissions[admission.MessageId]
	registry.mu.Unlock()
	if responses == nil {
		return nil
	}
	select {
	case responses <- admission:
	default:
	}
	return nil
}

func (s *RunnerProtocolService) registerDirectDataPlaneSession(
	open *runnerprotocol.DataPlaneDirectOpen,
) error {
	if open == nil || open.Fence == nil || open.OperationId == "" || open.StreamId == "" ||
		(open.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC &&
			open.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE &&
			open.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY &&
			open.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT) ||
		open.DeadlineUnixMs == 0 || len(open.CredentialDigest) != sha256.Size {
		return errors.New("SecondBox runner direct data-plane Open is incomplete")
	}
	if open.Kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY &&
		open.StreamWindowBytes == 0 {
		return errors.New("SecondBox runner direct PTY replay window is incomplete")
	}
	if !time.Now().UTC().Before(time.UnixMilli(int64(open.DeadlineUnixMs)).UTC()) ||
		!s.hasActiveFence(open.Fence) {
		return nil
	}
	if err := s.validateOperationCorrelation(open.Fence, open.OperationId, open.Correlation); err != nil {
		return err
	}
	if open.Kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT {
		port := open.GetPort()
		if port == nil || port.DeadlineUnixMs != open.DeadlineUnixMs ||
			!bytes.Equal(port.CredentialDigest, open.CredentialDigest) {
			return errors.New("SecondBox runner direct Port command is incomplete")
		}
		return s.registerDirectPortSession(&runnerprotocol.PortFrame{
			Fence: cloneRunnerFence(open.Fence), OperationId: open.OperationId,
			StreamId: open.StreamId, Sequence: 1,
			Correlation: cloneRunnerCorrelation(open.Correlation),
		}, port)
	}
	session := &directDataPlaneSession{
		fence: cloneRunnerFence(open.Fence), correlation: cloneRunnerCorrelation(open.Correlation),
		operationID: open.OperationId, streamID: open.StreamId, kind: open.Kind,
		deadline:     time.UnixMilli(int64(open.DeadlineUnixMs)).UTC(),
		streamWindow: open.StreamWindowBytes,
	}
	copy(session.credentialDigest[:], open.CredentialDigest)
	s.directDataPlane.add(session)
	return nil
}

func (s *RunnerProtocolService) handleDataPlaneCancel(
	command *runnerprotocol.DataPlaneCancelCommand,
) error {
	if command == nil || command.Fence == nil || command.OperationId == "" ||
		command.StreamId == "" || command.Reason == "" ||
		(command.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC &&
			command.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE &&
			command.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY &&
			command.Kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT) {
		return errors.New("SecondBox runner data-plane cancellation command is incomplete")
	}
	if command.Kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT {
		s.directPorts.closeSession(command.OperationId, command.Reason)
		key := runnerDataPlaneOperationKey(command.Fence, command.OperationId, command.StreamId)
		s.operationMu.Lock()
		state := s.portOperations[key]
		if state != nil && !state.terminal {
			state.cancel(context.Canceled)
		}
		s.operationMu.Unlock()
		return nil
	}
	if admitted := s.directDataPlane.find(command.OperationId); admitted != nil &&
		admitted.streamID == command.StreamId && admitted.kind == command.Kind &&
		proto.Equal(admitted.fence, command.Fence) {
		retained, err := s.retainUnopenedDirectOperation(admitted)
		if err != nil || retained != nil {
			s.directDataPlane.closeSession(command.OperationId, command.Reason)
			if err != nil {
				return err
			}
			return s.reportCancelledOperationTerminal(retained)
		}
	}
	key := runnerDataPlaneOperationKey(command.Fence, command.OperationId, command.StreamId)
	var retained *runnerprotocol.RunnerToControlPlane
	running := false
	absent := false
	s.operationMu.Lock()
	if command.Kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC ||
		command.Kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY {
		if state := s.execOperations[key]; state != nil {
			absent = state.terminal && state.correlation == nil
			switch {
			case state.ptyTerminalFrame != nil:
				retained = &runnerprotocol.RunnerToControlPlane{Message: &runnerprotocol.RunnerToControlPlane_Pty{
					Pty: proto.Clone(state.ptyTerminalFrame).(*runnerprotocol.PtyFrame),
				}}
			case state.terminalFrame != nil:
				retained = &runnerprotocol.RunnerToControlPlane{Message: &runnerprotocol.RunnerToControlPlane_Exec{
					Exec: proto.Clone(state.terminalFrame).(*runnerprotocol.ExecFrame),
				}}
			case !state.terminal:
				state.confirmCancel, running = true, true
				state.cancel(context.Canceled)
			}
		}
	} else if state := s.fileOperations[key]; state != nil {
		absent = state.terminal && state.correlation == nil
		switch {
		case state.terminalFrame != nil:
			retained = &runnerprotocol.RunnerToControlPlane{Message: &runnerprotocol.RunnerToControlPlane_File{
				File: proto.Clone(state.terminalFrame).(*runnerprotocol.FileFrame),
			}}
		case !state.terminal:
			state.confirmCancel, running = true, true
			state.cancel(context.Canceled)
		}
	}
	if s.execOperations[key] == nil && s.fileOperations[key] == nil {
		absent = true
		// Active operations are never evicted. Absence under operationMu
		// proves this fence has no worker; retain a terminal before a late
		// Open can start one. Cancellation confirmation needs only identity,
		// not the evicted result or its correlation.
		retained = s.recordAbsentOperationTerminalLocked(key, &directDataPlaneSession{
			fence: command.Fence, operationID: command.OperationId,
			streamID: command.StreamId, kind: command.Kind,
		})
	}
	s.operationMu.Unlock()
	if !running {
		// A running operation keeps its direct connection until it sends its
		// terminal there; a session whose Open has not arrived must not start.
		s.directDataPlane.closeSession(command.OperationId, command.Reason)
	}
	if retained != nil {
		if absent {
			// This is an observation of absence, not a reconstructed execution
			// result. Repeat its audit on replay, including after a sink failure.
			if err := s.emitEvidence(context.Background(), runnerevidence.EventOperationAbsent,
				command.Fence, nil, command.OperationId, "OPERATION_ABSENT", "cancelled",
			); err != nil {
				return err
			}
		}
		return s.reportCancelledOperationTerminal(retained)
	}
	return nil
}

// retainUnopenedDirectOperation retains a cancelled terminal, as the first
// frame of its stream, for an admitted direct session whose Open has not
// arrived. The control plane counts such a session as running from credential
// consumption, so a cancellation needs this terminal to confirm it; a late
// Open meets it and is discarded. It returns nil once the Open has arrived.
func (s *RunnerProtocolService) retainUnopenedDirectOperation(
	session *directDataPlaneSession,
) (*runnerprotocol.RunnerToControlPlane, error) {
	key := runnerDataPlaneOperationKey(session.fence, session.operationID, session.streamID)
	s.operationMu.Lock()
	if s.execOperations[key] != nil || s.fileOperations[key] != nil {
		s.operationMu.Unlock()
		return nil, nil
	}
	retained := s.recordAbsentOperationTerminalLocked(key, session)
	s.operationMu.Unlock()
	event := runnerevidence.EventExecTerminal
	kind := runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String()
	if session.kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE {
		event = runnerevidence.EventFileTerminal
		kind = runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String()
	}
	return retained, s.emitEvidence(
		context.Background(), event, session.fence, session.correlation, session.operationID,
		kind, terminalOutcome(kind),
	)
}

func (s *RunnerProtocolService) recordAbsentOperationTerminalLocked(
	key string,
	session *directDataPlaneSession,
) *runnerprotocol.RunnerToControlPlane {
	fence, correlation := session.fence, session.correlation
	if session.kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE {
		frame := &runnerprotocol.FileFrame{
			Fence: cloneRunnerFence(fence), OperationId: session.operationID, StreamId: session.streamID,
			Sequence: 1, Correlation: cloneRunnerCorrelation(correlation),
			Payload: &runnerprotocol.FileFrame_Terminal{Terminal: &runnerprotocol.FileTerminal{
				Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED, SafeDetail: "filesystem operation cancelled",
			}},
		}
		s.fileOperations[key] = &runnerFileOperation{
			key: key, fence: cloneRunnerFence(fence), correlation: cloneRunnerCorrelation(correlation),
			operationID: session.operationID, streamID: session.streamID,
			nextIncoming: 1, nextOutgoing: 2, started: true, terminal: true,
			terminalFrame: proto.Clone(frame).(*runnerprotocol.FileFrame),
		}
		s.retainFileTerminalLocked(key)
		return &runnerprotocol.RunnerToControlPlane{Message: &runnerprotocol.RunnerToControlPlane_File{File: frame}}
	}
	terminal := &runnerprotocol.ExecTerminal{
		Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED, ExitCode: -1, SafeDetail: "command cancelled",
	}
	done := make(chan struct{})
	close(done)
	state := &runnerExecOperation{
		key: key, fence: cloneRunnerFence(fence), correlation: cloneRunnerCorrelation(correlation),
		operationID: session.operationID, streamID: session.streamID,
		nextIncoming: 1, nextOutgoing: 2, done: done, terminal: true,
	}
	var message *runnerprotocol.RunnerToControlPlane
	if session.kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY {
		frame := &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(fence), OperationId: session.operationID, StreamId: session.streamID,
			Sequence: 1, Correlation: cloneRunnerCorrelation(correlation),
			Payload: &runnerprotocol.PtyFrame_Terminal{Terminal: terminal},
		}
		state.pty, state.ptyTerminalFrame = true, proto.Clone(frame).(*runnerprotocol.PtyFrame)
		state.ptyReplay = []*runnerprotocol.PtyFrame{proto.Clone(frame).(*runnerprotocol.PtyFrame)}
		message = &runnerprotocol.RunnerToControlPlane{Message: &runnerprotocol.RunnerToControlPlane_Pty{Pty: frame}}
	} else {
		frame := &runnerprotocol.ExecFrame{
			Fence: cloneRunnerFence(fence), OperationId: session.operationID, StreamId: session.streamID,
			Sequence: 1, Correlation: cloneRunnerCorrelation(correlation),
			Payload: &runnerprotocol.ExecFrame_BufferedResult{BufferedResult: &runnerprotocol.ExecBufferedResult{
				Terminal: terminal,
			}},
		}
		state.terminalFrame = proto.Clone(frame).(*runnerprotocol.ExecFrame)
		message = &runnerprotocol.RunnerToControlPlane{Message: &runnerprotocol.RunnerToControlPlane_Exec{Exec: frame}}
	}
	s.execOperations[key] = state
	s.retainExecTerminalLocked(key)
	return message
}

func (s *RunnerProtocolService) serveDirectTypedConnection(
	ctx context.Context,
	connection net.Conn,
	credential portdirect.Credential,
) (returnErr error) {
	digest := sha256.Sum256([]byte(credential.Value))
	session := s.directDataPlane.await(ctx, digest)
	if session == nil {
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "credential rejected")
		return errors.New("SecondBox runner direct data-plane credential was rejected")
	}
	wantKind := runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC
	switch credential.SessionKind {
	case portdirect.SessionKindFile:
		wantKind = runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE
	case portdirect.SessionKindPTY:
		wantKind = runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY
	}
	if session.kind != wantKind || !time.Now().UTC().Before(session.deadline) ||
		!s.hasActiveFence(session.fence) ||
		s.drainPhase() != runnerprotocol.DrainPhase_DRAIN_PHASE_ACTIVE || !session.claim() {
		_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, "session rejected")
		return errors.New("SecondBox runner direct data-plane session was rejected")
	}
	if err := connection.SetDeadline(time.Now().Add(directPortAdmissionTimeout)); err != nil {
		session.release()
		return err
	}
	session.mu.Lock()
	consumed := session.consumed
	session.mu.Unlock()
	if !consumed {
		admitted, detail, err := s.consumeDirectDataPlaneCredential(ctx, session)
		if err != nil || !admitted {
			session.release()
			if detail == "" {
				detail = "credential rejected"
			}
			_ = portdirect.WriteVerdict(connection, portdirect.VerdictDenied, detail)
			return err
		}
		session.mu.Lock()
		session.consumed = true
		session.mu.Unlock()
	}
	if session.kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY {
		defer s.directDataPlane.remove(session)
		defer func() {
			session.mu.Lock()
			opened := session.opened
			session.mu.Unlock()
			if !opened {
				_, err := s.retainUnopenedDirectOperation(session)
				returnErr = errors.Join(returnErr, err)
			}
		}()
	} else {
		defer session.release()
	}
	operationCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	session.mu.Lock()
	session.cancel = func(cause error) {
		cancel(cause)
		_ = connection.Close()
	}
	session.mu.Unlock()
	deliveryDeadline := session.deadline
	if session.kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC {
		// Match the control plane's bounded Exec terminal-delivery grace.
		deliveryDeadline = deliveryDeadline.Add(30 * time.Second)
	}
	if err := connection.SetDeadline(deliveryDeadline); err != nil {
		return err
	}
	if err := portdirect.WriteVerdict(connection, portdirect.VerdictAdmitted, ""); err != nil {
		return err
	}
	stream := &directTypedStream{connection: connection, deadline: deliveryDeadline}
	defer s.detachPTYAttachmentsForStream(stream)
	enabled := map[runnerprotocol.RunnerFeature]bool{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING: true,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING: true,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY:            true,
	}
	asyncErrors := make(chan error, 1)
	first := true
	for {
		payload, err := portdirect.ReadTypedMessage(connection)
		if err != nil {
			cancel(err)
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || stream.lingerExpired(err) {
				return nil
			}
			return err
		}
		message := &runnerprotocol.ControlPlaneToRunner{}
		if err := proto.Unmarshal(payload, message); err != nil {
			return fmt.Errorf("SecondBox runner direct data-plane message decoding: %w", err)
		}
		if first {
			if err := validateDirectFirstMessage(session, message); err != nil {
				return err
			}
			first = false
		}
		if err := s.handleCommand(operationCtx, stream, message, enabled, asyncErrors); err != nil {
			return err
		}
		select {
		case err := <-asyncErrors:
			return err
		default:
		}
	}
}

func validateDirectFirstMessage(
	session *directDataPlaneSession,
	message *runnerprotocol.ControlPlaneToRunner,
) error {
	var fence *runnerprotocol.AssignmentFence
	var operationID, streamID string
	var sequence uint64
	if session.kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC {
		frame := message.GetExec()
		if frame == nil || frame.GetOpen() == nil {
			return errors.New("SecondBox runner direct Exec stream must begin with Open")
		}
		if frame.GetOpen().AttributedExecution != nil {
			return errors.New("SecondBox runner direct Exec stream cannot carry attribution")
		}
		fence, operationID, streamID, sequence = frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence
	} else if session.kind == runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY {
		session.mu.Lock()
		opened := session.opened
		session.mu.Unlock()
		if opened {
			frame := message.GetPty()
			if frame == nil || frame.GetAttach() == nil {
				return errors.New("SecondBox runner direct PTY reconnect must begin with Attach")
			}
			fence, operationID, streamID, sequence = frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence
		} else {
			frame := message.GetExec()
			if frame == nil || frame.GetOpen() == nil || !frame.GetOpen().GetAllocatePty() {
				return errors.New("SecondBox runner direct PTY stream must begin with Open")
			}
			fence, operationID, streamID, sequence = frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence
		}
	} else {
		frame := message.GetFile()
		if frame == nil || frame.GetOpen() == nil {
			return errors.New("SecondBox runner direct File stream must begin with Open")
		}
		fence, operationID, streamID, sequence = frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence
	}
	if operationID != session.operationID || streamID != session.streamID || sequence == 0 ||
		!proto.Equal(fence, session.fence) {
		return errors.New("SecondBox runner direct data-plane message identity mismatch")
	}
	if session.kind != runnerprotocol.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY ||
		message.GetExec() != nil {
		if sequence != 1 {
			return errors.New("SecondBox runner direct data-plane Open sequence is invalid")
		}
		session.mu.Lock()
		session.opened = true
		session.mu.Unlock()
	}
	return nil
}

func (s *RunnerProtocolService) consumeDirectDataPlaneCredential(
	ctx context.Context,
	session *directDataPlaneSession,
) (bool, string, error) {
	messageID, responses, stream := s.directDataPlane.nextAdmission()
	defer s.directDataPlane.forgetAdmission(messageID)
	if stream == nil {
		return false, "control-plane connection unavailable", errors.New("SecondBox runner direct data-plane admission has no control-plane connection")
	}
	if err := s.sendSequencedRunnerFrame(stream, func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
		return &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_DataPlaneDirectConsume{
				DataPlaneDirectConsume: &runnerprotocol.DataPlaneDirectConsume{
					MessageId: messageID, Sequence: sequence,
					Fence: cloneRunnerFence(session.fence), OperationId: session.operationID,
					StreamId: session.streamID, Correlation: cloneRunnerCorrelation(session.correlation),
					Kind: session.kind, CredentialDigest: session.credentialDigest[:],
				},
			},
		}
	}); err != nil {
		return false, "control-plane connection unavailable", err
	}
	timer := time.NewTimer(directPortAdmissionTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, "runner stopping", ctx.Err()
	case <-timer.C:
		return false, "admission timed out", errors.New("SecondBox runner direct data-plane admission timed out")
	case admission := <-responses:
		if admission.Admission != runnerprotocol.DataPlaneDirectAdmissionKind_DATA_PLANE_DIRECT_ADMISSION_KIND_ADMITTED {
			return false, admission.SafeDetail, nil
		}
		if admission.OperationId != session.operationID || admission.StreamId != session.streamID ||
			admission.Kind != session.kind || !proto.Equal(admission.Fence, session.fence) {
			return false, "admission identity mismatch", errors.New("SecondBox runner direct data-plane admission identity mismatch")
		}
		return true, "", nil
	}
}

// Credit bounds queued bytes; this timeout bounds a socket write after the
// peer stops reading. In particular, cancellation must be able to unwind a
// blocked output write and confirm the operation on the control connection.
const directDataPlaneWriteTimeout = 5 * time.Second

// After its terminal the Runner keeps reading until the control plane closes.
// Closing with unread credit in the receive buffer resets the connection, and
// the reset can discard the terminal before the control plane reads it.
const directDataPlaneTerminalLinger = 5 * time.Second

type directTypedStream struct {
	connection   net.Conn
	deadline     time.Time
	mu           sync.Mutex
	terminalSent bool
}

func (stream *directTypedStream) Send(message *runnerprotocol.RunnerToControlPlane) error {
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return fmt.Errorf("SecondBox runner direct data-plane message encoding: %w", err)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	deadline := time.Now().Add(directDataPlaneWriteTimeout)
	if !stream.deadline.IsZero() && stream.deadline.Before(deadline) {
		deadline = stream.deadline
	}
	if err := stream.connection.SetWriteDeadline(deadline); err != nil {
		return errors.Join(err, stream.connection.Close())
	}
	if err := portdirect.WriteTypedMessage(stream.connection, payload); err != nil {
		return errors.Join(err, stream.connection.Close())
	}
	terminal := message.GetExec().GetTerminal() != nil ||
		message.GetExec().GetBufferedResult() != nil || message.GetPty().GetTerminal() != nil ||
		message.GetFile().GetTerminal() != nil
	if !terminal {
		return nil
	}
	stream.terminalSent = true
	linger := time.Now().Add(directDataPlaneTerminalLinger)
	if !stream.deadline.IsZero() && stream.deadline.Before(linger) {
		linger = stream.deadline
	}
	if err := stream.connection.SetReadDeadline(linger); err != nil {
		return errors.Join(err, stream.connection.Close())
	}
	return nil
}

func (stream *directTypedStream) lingerExpired(readErr error) bool {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.terminalSent && errors.Is(readErr, os.ErrDeadlineExceeded)
}

func (*directTypedStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	return nil, errors.New("SecondBox runner direct data-plane stream does not receive through Recv")
}
