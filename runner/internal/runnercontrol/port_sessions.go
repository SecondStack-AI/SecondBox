package runnercontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

// PortConnection is a Runner-private stream to one guest loopback port.
type PortConnection interface {
	Read(context.Context, int) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
}

// PortBackend opens assignment-fenced guest connections without a host listener.
type PortBackend interface {
	OpenPort(context.Context, *runnerprotocol.AssignmentFence, *runnerprotocol.PortOpen) (PortConnection, error)
}

type runnerPortOperation struct {
	key           string
	fence         *runnerprotocol.AssignmentFence
	correlation   *runnerprotocol.Correlation
	operationID   string
	streamID      string
	nextIncoming  uint64
	lastIncoming  []byte
	nextOutgoing  uint64
	credit        *runnerCreditWindow
	connection    PortConnection
	cancel        context.CancelCauseFunc
	terminal      bool
	terminalFrame *runnerprotocol.PortFrame
}

func (s *RunnerProtocolService) handlePortFrame(
	ctx context.Context,
	stream RunnerProtocolStream,
	frame *runnerprotocol.PortFrame,
	enabled map[runnerprotocol.RunnerFeature]bool,
	asyncErrors chan<- error,
) error {
	if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY] {
		return fmt.Errorf("SecondBox runner Port proxy feature was not negotiated")
	}
	if err := validateRunnerDataPlaneFrameIdentity(frame.GetFence(), frame.GetOperationId(), frame.GetStreamId(), frame.GetSequence()); err != nil {
		return err
	}
	key := runnerDataPlaneOperationKey(frame.Fence, frame.OperationId, frame.StreamId)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(frame)
	if err != nil {
		return fmt.Errorf("SecondBox runner encode Port frame: %w", err)
	}
	// A direct PortSession is carried by a caller socket rather than by proxied
	// frames, so its only control-plane messages are the admitting Open and a
	// Cancel that revokes it.
	if frame.GetCancel() != nil && s.directPorts.hasSession(frame.OperationId) {
		reason := frame.GetCancel().GetReason()
		if reason == "" {
			reason = "port session cancelled"
		}
		s.directPorts.closeSession(frame.OperationId, reason)
		return nil
	}
	if direct := frame.GetDirectOpen(); direct != nil {
		if frame.Sequence != 1 {
			return fmt.Errorf("SecondBox runner direct Port stream must begin with sequence one")
		}
		if err := s.validateOperationCorrelation(frame.Fence, frame.OperationId, frame.GetCorrelation()); err != nil {
			return err
		}
		if !s.hasActiveFence(frame.Fence) {
			return s.sendUntrackedPortTerminal(stream, frame, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FENCED, "assignment fence is not active")
		}
		if s.portBackend == nil {
			return s.sendUntrackedPortTerminal(stream, frame, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE, "runner Port backend is unavailable")
		}
		if !s.dataPlane.ready() {
			return s.sendUntrackedPortTerminal(stream, frame, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FAILED, "runner data-plane listener is unavailable")
		}
		return s.registerDirectPortSession(frame, direct)
	}
	s.operationMu.Lock()
	state := s.portOperations[key]
	if state == nil {
		if frame.GetOpen() == nil || frame.Sequence != 1 {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner Port stream must begin with Open sequence one")
		}
		if err := s.validateOperationCorrelation(frame.Fence, frame.OperationId, frame.GetCorrelation()); err != nil {
			s.operationMu.Unlock()
			return err
		}
		if !s.hasActiveFence(frame.Fence) {
			s.operationMu.Unlock()
			return s.sendUntrackedPortTerminal(stream, frame, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FENCED, "assignment fence is not active")
		}
		if s.portBackend == nil {
			s.operationMu.Unlock()
			return s.sendUntrackedPortTerminal(stream, frame, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE, "runner Port backend is unavailable")
		}
		if len(s.portOperations) >= maxRunnerDataPlaneOperationStates {
			s.operationMu.Unlock()
			return s.sendUntrackedPortTerminal(stream, frame, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FAILED, "runner Port capacity is exhausted")
		}
		portCtx, cancel := context.WithCancelCause(ctx)
		connection, err := s.portBackend.OpenPort(portCtx, cloneRunnerFence(frame.Fence), proto.Clone(frame.GetOpen()).(*runnerprotocol.PortOpen))
		if err != nil {
			cancel(err)
			s.operationMu.Unlock()
			return s.sendUntrackedPortTerminal(stream, frame, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE, "guest port is unavailable")
		}
		state = &runnerPortOperation{
			key: key, fence: cloneRunnerFence(frame.Fence), operationID: frame.OperationId,
			correlation: cloneRunnerCorrelation(frame.Correlation),
			streamID:    frame.StreamId, nextIncoming: 2, lastIncoming: bytes.Clone(encoded),
			nextOutgoing: 1, credit: newRunnerCreditWindow(), connection: connection, cancel: cancel,
		}
		context.AfterFunc(portCtx, func() {
			_ = connection.Close()
		})
		s.portOperations[key] = state
		s.setActiveOperation(frame.Fence.AssignmentId, frame.OperationId, true)
		s.operationMu.Unlock()
		if err := s.sendPortCredit(stream, state, runnerDataPlaneChunkBytes); err != nil {
			return err
		}
		go s.pumpPortReads(portCtx, stream, state, asyncErrors)
		return nil
	}
	duplicate, sequenceErr := acceptRunnerDataPlaneInput(state.nextIncoming, state.lastIncoming, frame.Sequence, encoded)
	if duplicate {
		terminal := state.terminalFrame
		s.operationMu.Unlock()
		if terminal != nil {
			return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_Port{Port: proto.Clone(terminal).(*runnerprotocol.PortFrame)},
			})
		}
		return nil
	}
	if sequenceErr != nil {
		s.operationMu.Unlock()
		return sequenceErr
	}
	state.nextIncoming++
	state.lastIncoming = bytes.Clone(encoded)
	s.operationMu.Unlock()
	switch {
	case frame.GetBytes() != nil:
		data := frame.GetBytes().Data
		if len(data) == 0 || len(data) > runnerDataPlaneChunkBytes {
			return fmt.Errorf("SecondBox runner Port bytes exceed the frame bound")
		}
		if err := state.connection.Write(ctx, data); err != nil {
			return s.sendPortTerminal(stream, state, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_GUEST_UNAVAILABLE, "guest port write failed")
		}
		return s.sendPortCredit(stream, state, uint64(len(data)))
	case frame.GetCredit() != nil:
		return state.credit.add(frame.GetCredit().ByteCount)
	case frame.GetCancel() != nil:
		state.cancel(context.Canceled)
		return s.sendPortTerminal(stream, state, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED, "port session cancelled")
	default:
		return fmt.Errorf("SecondBox runner Port accepts only Open, Bytes, Credit, and Cancel frames")
	}
}

func (s *RunnerProtocolService) pumpPortReads(
	ctx context.Context,
	stream RunnerProtocolStream,
	state *runnerPortOperation,
	asyncErrors chan<- error,
) {
	for {
		credit, err := state.credit.take(ctx, runnerDataPlaneChunkBytes)
		if err != nil {
			return
		}
		data, err := state.connection.Read(ctx, int(credit))
		if len(data) > 0 {
			if sendErr := s.sendPortBytes(stream, state, data); sendErr != nil {
				reportRunnerAsyncError(asyncErrors, sendErr)
				return
			}
		}
		if err != nil {
			kind := runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FAILED
			detail := "guest port read failed"
			if errors.Is(err, io.EOF) {
				kind, detail = runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED, "guest port closed"
			}
			if sendErr := s.sendPortTerminal(stream, state, kind, detail); sendErr != nil {
				reportRunnerAsyncError(asyncErrors, sendErr)
			}
			return
		}
	}
}

func (s *RunnerProtocolService) sendPortBytes(
	stream RunnerProtocolStream,
	state *runnerPortOperation,
	data []byte,
) error {
	s.operationMu.Lock()
	if state.terminal {
		s.operationMu.Unlock()
		return nil
	}
	frame := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
		StreamId: state.streamID, Sequence: state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload:     &runnerprotocol.PortFrame_Bytes{Bytes: &runnerprotocol.PortBytes{Data: bytes.Clone(data)}},
	}
	state.nextOutgoing++
	s.operationMu.Unlock()
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Port{Port: frame},
	})
}

func (s *RunnerProtocolService) sendPortCredit(
	stream RunnerProtocolStream,
	state *runnerPortOperation,
	credit uint64,
) error {
	if credit == 0 {
		return fmt.Errorf("SecondBox runner Port credit must be positive")
	}
	s.operationMu.Lock()
	if state.terminal {
		s.operationMu.Unlock()
		return nil
	}
	frame := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
		StreamId: state.streamID, Sequence: state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload:     &runnerprotocol.PortFrame_Credit{Credit: &runnerprotocol.StreamCredit{ByteCount: credit}},
	}
	state.nextOutgoing++
	s.operationMu.Unlock()
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Port{Port: frame},
	})
}

func (s *RunnerProtocolService) sendPortTerminal(
	stream RunnerProtocolStream,
	state *runnerPortOperation,
	kind runnerprotocol.PortTerminalKind,
	detail string,
) error {
	s.operationMu.Lock()
	if state.terminal {
		frame := proto.Clone(state.terminalFrame).(*runnerprotocol.PortFrame)
		s.operationMu.Unlock()
		return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_Port{Port: frame},
		})
	}
	frame := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
		StreamId: state.streamID, Sequence: state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload: &runnerprotocol.PortFrame_Terminal{Terminal: &runnerprotocol.PortTerminal{
			Kind: kind, SafeDetail: detail,
		}},
	}
	state.nextOutgoing++
	state.terminal = true
	state.terminalFrame = proto.Clone(frame).(*runnerprotocol.PortFrame)
	s.retainPortTerminalLocked(state.key)
	s.setActiveOperation(state.fence.AssignmentId, state.operationID, false)
	s.operationMu.Unlock()
	if err := s.emitEvidence(
		context.Background(),
		runnerevidence.EventPortTerminal,
		state.fence,
		state.correlation,
		state.operationID,
		kind.String(),
		terminalOutcome(kind.String()),
	); err != nil {
		return err
	}
	closeErr := state.connection.Close()
	return errors.Join(closeErr, s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Port{Port: frame},
	}))
}

func (s *RunnerProtocolService) retainPortTerminalLocked(key string) {
	if key == "" || s.portOperations[key] == nil {
		return
	}
	s.portTerminalOrder = append(s.portTerminalOrder, key)
	for len(s.portTerminalOrder) > maxRunnerDataPlaneTerminalTombstones {
		oldest := s.portTerminalOrder[0]
		s.portTerminalOrder = s.portTerminalOrder[1:]
		if state := s.portOperations[oldest]; state != nil && state.terminal {
			delete(s.portOperations, oldest)
		}
	}
}

func (s *RunnerProtocolService) sendUntrackedPortTerminal(
	stream RunnerProtocolStream,
	source *runnerprotocol.PortFrame,
	kind runnerprotocol.PortTerminalKind,
	detail string,
) error {
	if err := s.emitEvidence(
		context.Background(),
		runnerevidence.EventPortTerminal,
		source.Fence,
		source.Correlation,
		source.OperationId,
		kind.String(),
		terminalOutcome(kind.String()),
	); err != nil {
		return err
	}
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Port{Port: &runnerprotocol.PortFrame{
			Fence: cloneRunnerFence(source.Fence), OperationId: source.OperationId,
			StreamId: source.StreamId, Sequence: 1,
			Correlation: cloneRunnerCorrelation(source.Correlation),
			Payload: &runnerprotocol.PortFrame_Terminal{Terminal: &runnerprotocol.PortTerminal{
				Kind: kind, SafeDetail: detail,
			}},
		}},
	})
}
