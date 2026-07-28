package runnercontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

const (
	runnerRelayChunkBytes            = 64 << 10
	maxRunnerRelayOperationStates    = 1024
	maxRunnerRelayTerminalTombstones = 256
)

// BufferedExecResult is one bounded guest execution translated to runner wire types.
type BufferedExecResult struct {
	Stdout   []byte
	Stderr   []byte
	Terminal *runnerprotocol.ExecTerminal
}

// FileOperationResult is one guest filesystem result translated to runner wire types.
type FileOperationResult struct {
	Metadata *runnerprotocol.FileMetadata
	Content  []byte
	Terminal *runnerprotocol.FileTerminal
}

// DataPlaneBackend executes only assignment-bound operations on a retained guest session.
type DataPlaneBackend interface {
	ExecuteStreaming(
		context.Context,
		*runnerprotocol.AssignmentFence,
		*runnerprotocol.ExecOpen,
		<-chan ExecControl,
		func(runnerprotocol.ExecOutputChannel, []byte) error,
	) (*runnerprotocol.ExecTerminal, error)
	ExecuteFile(
		context.Context,
		*runnerprotocol.AssignmentFence,
		*runnerprotocol.FileOpen,
		[]byte,
	) (FileOperationResult, error)
}

type PTYDataPlaneBackend interface {
	ExecutePTY(
		context.Context,
		*runnerprotocol.AssignmentFence,
		*runnerprotocol.ExecOpen,
		<-chan PTYControl,
		func([]byte) error,
	) (*runnerprotocol.ExecTerminal, error)
}

type ExecControl struct {
	Input  *runnerprotocol.ExecInput
	Credit uint64
}

type PTYControl struct {
	Input   []byte
	Credit  uint64
	Rows    uint32
	Columns uint32
}

type runnerExecOperation struct {
	key              string
	fence            *runnerprotocol.AssignmentFence
	correlation      *runnerprotocol.Correlation
	operationID      string
	streamID         string
	nextIncoming     uint64
	lastIncoming     []byte
	nextOutgoing     uint64
	credit           *runnerCreditWindow
	controls         chan ExecControl
	ptyControls      chan PTYControl
	pty              bool
	cancel           context.CancelCauseFunc
	terminal         bool
	terminalFrame    *runnerprotocol.ExecFrame
	ptyTerminalFrame *runnerprotocol.PtyFrame
}

type runnerFileOperation struct {
	key           string
	fence         *runnerprotocol.AssignmentFence
	correlation   *runnerprotocol.Correlation
	operationID   string
	streamID      string
	open          *runnerprotocol.FileOpen
	nextIncoming  uint64
	lastIncoming  []byte
	nextOutgoing  uint64
	content       []byte
	credit        *runnerCreditWindow
	ctx           context.Context
	cancel        context.CancelCauseFunc
	started       bool
	terminal      bool
	terminalFrame *runnerprotocol.FileFrame
}

type runnerCreditWindow struct {
	mu        sync.Mutex
	available uint64
	notify    chan struct{}
}

func newRunnerCreditWindow() *runnerCreditWindow {
	return &runnerCreditWindow{notify: make(chan struct{}, 1)}
}

func (window *runnerCreditWindow) add(byteCount uint64) error {
	if byteCount == 0 {
		return fmt.Errorf("SecondBox runner stream credit must be positive")
	}
	window.mu.Lock()
	if ^uint64(0)-window.available < byteCount {
		window.mu.Unlock()
		return fmt.Errorf("SecondBox runner stream credit exceeds uint64 capacity")
	}
	window.available += byteCount
	window.mu.Unlock()
	select {
	case window.notify <- struct{}{}:
	default:
	}
	return nil
}

func (window *runnerCreditWindow) take(ctx context.Context, maximum uint64) (uint64, error) {
	for {
		window.mu.Lock()
		if window.available > 0 {
			granted := min(window.available, maximum)
			window.available -= granted
			window.mu.Unlock()
			return granted, nil
		}
		window.mu.Unlock()
		select {
		case <-window.notify:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (s *RunnerProtocolService) handleExecFrame(
	ctx context.Context,
	stream RunnerProtocolStream,
	frame *runnerprotocol.ExecFrame,
	enabled map[runnerprotocol.RunnerFeature]bool,
	asyncErrors chan<- error,
) error {
	if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING] {
		return fmt.Errorf("SecondBox runner Exec feature was not negotiated")
	}
	if err := validateRunnerRelayFrameIdentity(frame.GetFence(), frame.GetOperationId(), frame.GetStreamId(), frame.GetSequence()); err != nil {
		return err
	}
	key := runnerRelayOperationKey(frame.Fence, frame.OperationId, frame.StreamId)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(frame)
	if err != nil {
		return fmt.Errorf("SecondBox runner encode Exec frame: %w", err)
	}

	s.operationMu.Lock()
	state := s.execOperations[key]
	if state == nil {
		if frame.GetOpen() == nil || frame.Sequence != 1 {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner Exec stream must begin with Open sequence one")
		}
		if err := s.validateOperationCorrelation(frame.Fence, frame.OperationId, frame.GetCorrelation()); err != nil {
			s.operationMu.Unlock()
			return err
		}
		state = &runnerExecOperation{
			key:          key,
			fence:        cloneRunnerFence(frame.Fence),
			correlation:  cloneRunnerCorrelation(frame.Correlation),
			operationID:  frame.OperationId,
			streamID:     frame.StreamId,
			nextIncoming: 2,
			nextOutgoing: 1,
			credit:       newRunnerCreditWindow(),
			controls:     make(chan ExecControl, 256),
			pty:          frame.GetOpen().AllocatePty,
		}
		if state.pty {
			state.ptyControls = make(chan PTYControl, 256)
		}
		execCtx, cancel := context.WithCancelCause(ctx)
		state.cancel = cancel
		state.lastIncoming = bytes.Clone(encoded)
		if len(s.execOperations) >= maxRunnerRelayOperationStates {
			s.operationMu.Unlock()
			return s.sendRunnerExecOperationTerminal(stream, state, runnerInfrastructureTerminal(
				runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
				runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_EXECUTION_NODE,
				true, "runner Exec operation capacity is exhausted",
			))
		}
		s.execOperations[key] = state
		s.operationMu.Unlock()

		if !s.hasActiveFence(frame.Fence) {
			return s.sendRunnerExecOperationTerminal(stream, state, runnerInfrastructureTerminal(
				runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_FENCED,
				runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GENERATION_FENCED,
				false, "assignment fence is not active",
			))
		}
		if s.dataPlaneBackend == nil {
			return s.sendRunnerExecOperationTerminal(stream, state, runnerInfrastructureTerminal(
				runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
				runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_EXECUTION_NODE,
				true, "runner data-plane backend is unavailable",
			))
		}
		if frame.GetOpen().AllocatePty {
			if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY] {
				return s.sendPTYTerminal(stream, state, runnerInfrastructureTerminal(
					runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
					runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_ADMISSION,
					false, "runner PTY feature was not negotiated",
				))
			}
			if _, ok := s.dataPlaneBackend.(PTYDataPlaneBackend); !ok {
				return s.sendPTYTerminal(stream, state, runnerInfrastructureTerminal(
					runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
					runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_EXECUTION_NODE,
					true, "runner PTY backend is unavailable",
				))
			}
			s.setActiveOperation(frame.Fence.AssignmentId, frame.OperationId, true)
			go s.executePTYOperation(execCtx, stream, state, frame.GetOpen(), asyncErrors)
			return nil
		}
		s.setActiveOperation(frame.Fence.AssignmentId, frame.OperationId, true)
		go s.executeStreamingOperation(execCtx, stream, state, frame.GetOpen(), asyncErrors)
		return nil
	}
	if frame.GetCorrelation() != nil && !proto.Equal(frame.GetCorrelation(), state.correlation) {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner Exec correlation changed within the operation")
	}
	duplicate, err := acceptRunnerRelayInput(state.nextIncoming, state.lastIncoming, frame.Sequence, encoded)
	if duplicate {
		terminal := state.terminalFrame
		ptyTerminal := state.ptyTerminalFrame
		pty := state.pty
		s.operationMu.Unlock()
		if pty && ptyTerminal != nil {
			return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_Pty{
					Pty: proto.Clone(ptyTerminal).(*runnerprotocol.PtyFrame),
				},
			})
		}
		if terminal != nil {
			return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_Exec{Exec: proto.Clone(terminal).(*runnerprotocol.ExecFrame)},
			})
		}
		return nil
	}
	if err != nil {
		s.operationMu.Unlock()
		return err
	}
	state.nextIncoming++
	state.lastIncoming = bytes.Clone(encoded)
	s.operationMu.Unlock()

	switch {
	case frame.GetCredit() != nil:
		if state.pty {
			return fmt.Errorf("SecondBox runner PTY credit requires a PtyFrame")
		}
		if err := state.credit.add(frame.GetCredit().ByteCount); err != nil {
			return err
		}
		return sendExecControl(ctx, state.controls, ExecControl{Credit: frame.GetCredit().ByteCount})
	case frame.GetInput() != nil:
		if state.pty {
			return fmt.Errorf("SecondBox runner PTY input requires a PtyFrame")
		}
		return sendExecControl(ctx, state.controls, ExecControl{
			Input: proto.Clone(frame.GetInput()).(*runnerprotocol.ExecInput),
		})
	case frame.GetCancel() != nil:
		state.cancel(context.Canceled)
		return nil
	default:
		return fmt.Errorf("SecondBox runner Exec accepts only Open, Input, Credit, and Cancel frames")
	}
}

func sendExecControl(ctx context.Context, controls chan<- ExecControl, control ExecControl) error {
	select {
	case controls <- control:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RunnerProtocolService) handlePTYFrame(
	ctx context.Context,
	stream RunnerProtocolStream,
	frame *runnerprotocol.PtyFrame,
	enabled map[runnerprotocol.RunnerFeature]bool,
) error {
	if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY] {
		return fmt.Errorf("SecondBox runner PTY feature was not negotiated")
	}
	if err := validateRunnerRelayFrameIdentity(
		frame.GetFence(), frame.GetOperationId(), frame.GetStreamId(), frame.GetSequence(),
	); err != nil {
		return err
	}
	key := runnerRelayOperationKey(frame.Fence, frame.OperationId, frame.StreamId)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(frame)
	if err != nil {
		return fmt.Errorf("SecondBox runner encode PTY frame: %w", err)
	}
	s.operationMu.Lock()
	state := s.execOperations[key]
	if state == nil || !state.pty {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner PTY operation is not active")
	}
	if frame.GetCorrelation() != nil && !proto.Equal(frame.GetCorrelation(), state.correlation) {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner PTY correlation changed within the operation")
	}
	duplicate, err := acceptRunnerRelayInput(
		state.nextIncoming, state.lastIncoming, frame.Sequence, encoded,
	)
	if duplicate {
		terminal := state.ptyTerminalFrame
		s.operationMu.Unlock()
		if terminal != nil {
			return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_Pty{
					Pty: proto.Clone(terminal).(*runnerprotocol.PtyFrame),
				},
			})
		}
		return nil
	}
	if err != nil {
		s.operationMu.Unlock()
		return err
	}
	state.nextIncoming++
	state.lastIncoming = bytes.Clone(encoded)
	terminal := state.terminal
	s.operationMu.Unlock()
	if terminal {
		return fmt.Errorf("SecondBox runner PTY operation is terminal")
	}
	var control PTYControl
	switch {
	case frame.GetInput() != nil:
		if len(frame.GetInput().Data) == 0 {
			return fmt.Errorf("SecondBox runner PTY input is empty")
		}
		control.Input = bytes.Clone(frame.GetInput().Data)
	case frame.GetCredit() != nil:
		if frame.GetCredit().ByteCount == 0 {
			return fmt.Errorf("SecondBox runner PTY credit must be positive")
		}
		control.Credit = frame.GetCredit().ByteCount
	case frame.GetResize() != nil:
		if frame.GetResize().Rows == 0 || frame.GetResize().Rows > 1000 ||
			frame.GetResize().Columns == 0 || frame.GetResize().Columns > 1000 {
			return fmt.Errorf("SecondBox runner PTY resize is invalid")
		}
		control.Rows = frame.GetResize().Rows
		control.Columns = frame.GetResize().Columns
	default:
		return fmt.Errorf("SecondBox runner PTY accepts only Input, Credit, and Resize frames")
	}
	select {
	case state.ptyControls <- control:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RunnerProtocolService) executePTYOperation(
	ctx context.Context,
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	open *runnerprotocol.ExecOpen,
	asyncErrors chan<- error,
) {
	defer s.setActiveOperation(state.fence.AssignmentId, state.operationID, false)
	backend := s.dataPlaneBackend.(PTYDataPlaneBackend)
	terminal, err := backend.ExecutePTY(
		ctx, cloneRunnerFence(state.fence), proto.Clone(open).(*runnerprotocol.ExecOpen),
		state.ptyControls,
		func(content []byte) error {
			return s.sendRunnerPTYBytes(stream, state, content)
		},
	)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
			terminal = &runnerprotocol.ExecTerminal{
				Kind:     runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
				ExitCode: -1, SafeDetail: "Terminal cancelled",
			}
		} else {
			terminal = runnerInfrastructureTerminal(
				runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
				runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GUEST_AGENT,
				true, "runner PTY bridge failed",
			)
		}
	}
	if terminal == nil {
		terminal = runnerInfrastructureTerminal(
			runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
			runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GUEST_AGENT,
			true, "runner PTY bridge returned no terminal outcome",
		)
	}
	if err := s.sendPTYTerminal(stream, state, terminal); err != nil {
		reportRunnerAsyncError(asyncErrors, err)
	}
}

func (s *RunnerProtocolService) sendRunnerPTYBytes(
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	content []byte,
) error {
	if len(content) == 0 {
		return fmt.Errorf("SecondBox runner PTY backend emitted empty output")
	}
	for len(content) > 0 {
		size := min(len(content), runnerRelayChunkBytes)
		frame := &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
			StreamId: state.streamID, Sequence: state.nextOutgoing,
			Correlation: cloneRunnerCorrelation(state.correlation),
			Payload: &runnerprotocol.PtyFrame_Output{Output: &runnerprotocol.ExecOutput{
				Channel: runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
				Data:    bytes.Clone(content[:size]),
			}},
		}
		state.nextOutgoing++
		if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_Pty{Pty: frame},
		}); err != nil {
			return err
		}
		content = content[size:]
	}
	return nil
}

func (s *RunnerProtocolService) sendRunnerExecOperationTerminal(
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	terminal *runnerprotocol.ExecTerminal,
) error {
	if state.pty {
		return s.sendPTYTerminal(stream, state, terminal)
	}
	return s.sendExecTerminal(stream, state, terminal)
}

func (s *RunnerProtocolService) sendPTYTerminal(
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	terminal *runnerprotocol.ExecTerminal,
) error {
	s.operationMu.Lock()
	if state.terminal {
		frame := proto.Clone(state.ptyTerminalFrame).(*runnerprotocol.PtyFrame)
		s.operationMu.Unlock()
		return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_Pty{Pty: frame},
		})
	}
	frame := &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
		StreamId: state.streamID, Sequence: state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload: &runnerprotocol.PtyFrame_Terminal{
			Terminal: proto.Clone(terminal).(*runnerprotocol.ExecTerminal),
		},
	}
	state.nextOutgoing++
	state.terminal = true
	state.ptyTerminalFrame = proto.Clone(frame).(*runnerprotocol.PtyFrame)
	s.retainExecTerminalLocked(state.key)
	s.operationMu.Unlock()
	if err := s.emitEvidence(
		context.Background(), runnerevidence.EventExecTerminal,
		state.fence, state.correlation, state.operationID,
		terminal.Kind.String(), terminalOutcome(terminal.Kind.String()),
	); err != nil {
		return err
	}
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Pty{Pty: frame},
	})
}

func (s *RunnerProtocolService) executeStreamingOperation(
	ctx context.Context,
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	open *runnerprotocol.ExecOpen,
	asyncErrors chan<- error,
) {
	defer s.setActiveOperation(state.fence.AssignmentId, state.operationID, false)
	terminal, err := s.dataPlaneBackend.ExecuteStreaming(
		ctx, cloneRunnerFence(state.fence), proto.Clone(open).(*runnerprotocol.ExecOpen),
		state.controls,
		func(channel runnerprotocol.ExecOutputChannel, content []byte) error {
			return s.sendRunnerExecBytes(ctx, stream, state, channel, content)
		},
	)
	if err != nil {
		kind := runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED
		detail := "runner execution bridge failed"
		if errors.Is(err, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
			kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED
			detail = "command cancelled"
		}
		if kind == runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
			terminal = &runnerprotocol.ExecTerminal{Kind: kind, ExitCode: -1, SafeDetail: detail}
		} else {
			terminal = runnerInfrastructureTerminal(
				kind,
				runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GUEST_AGENT,
				true, detail,
			)
		}
	}
	if terminal == nil {
		terminal = runnerInfrastructureTerminal(
			runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
			runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GUEST_AGENT,
			true, "runner execution bridge returned no terminal outcome",
		)
	}
	if err := s.sendExecTerminal(stream, state, terminal); err != nil {
		reportRunnerAsyncError(asyncErrors, err)
	}
}

func runnerInfrastructureTerminal(
	kind runnerprotocol.ExecTerminalKind,
	reason runnerprotocol.InfrastructureFailureReason,
	retryable bool,
	message string,
) *runnerprotocol.ExecTerminal {
	return &runnerprotocol.ExecTerminal{
		Kind: kind, ExitCode: -1, SafeDetail: message,
		InfrastructureFailureReason: reason, Retryable: retryable, Message: message,
	}
}

func (s *RunnerProtocolService) sendRunnerExecBytes(
	ctx context.Context,
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	channel runnerprotocol.ExecOutputChannel,
	content []byte,
) error {
	credit := uint64(0)
	for len(content) > 0 {
		if credit == 0 {
			var err error
			credit, err = state.credit.take(ctx, uint64(min(len(content), runnerRelayChunkBytes)))
			if err != nil {
				return err
			}
		}
		size := len(content)
		if size > runnerRelayChunkBytes {
			size = runnerRelayChunkBytes
		}
		if uint64(size) > credit {
			size = int(credit)
		}
		frame := &runnerprotocol.ExecFrame{
			Fence:       cloneRunnerFence(state.fence),
			OperationId: state.operationID,
			StreamId:    state.streamID,
			Sequence:    state.nextOutgoing,
			Correlation: cloneRunnerCorrelation(state.correlation),
			Payload: &runnerprotocol.ExecFrame_Output{Output: &runnerprotocol.ExecOutput{
				Channel: channel,
				Data:    bytes.Clone(content[:size]),
			}},
		}
		state.nextOutgoing++
		if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_Exec{Exec: frame},
		}); err != nil {
			return err
		}
		content = content[size:]
		credit -= uint64(size)
	}
	return nil
}

func (s *RunnerProtocolService) sendExecTerminal(
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	terminal *runnerprotocol.ExecTerminal,
) error {
	s.operationMu.Lock()
	if state.terminal {
		frame := proto.Clone(state.terminalFrame).(*runnerprotocol.ExecFrame)
		s.operationMu.Unlock()
		return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_Exec{Exec: frame},
		})
	}
	frame := &runnerprotocol.ExecFrame{
		Fence:       cloneRunnerFence(state.fence),
		OperationId: state.operationID,
		StreamId:    state.streamID,
		Sequence:    state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload:     &runnerprotocol.ExecFrame_Terminal{Terminal: proto.Clone(terminal).(*runnerprotocol.ExecTerminal)},
	}
	state.nextOutgoing++
	state.terminal = true
	state.terminalFrame = proto.Clone(frame).(*runnerprotocol.ExecFrame)
	s.retainExecTerminalLocked(state.key)
	s.operationMu.Unlock()
	if err := s.emitEvidence(
		context.Background(),
		runnerevidence.EventExecTerminal,
		state.fence,
		state.correlation,
		state.operationID,
		terminal.Kind.String(),
		terminalOutcome(terminal.Kind.String()),
	); err != nil {
		return err
	}
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Exec{Exec: frame},
	})
}

func (s *RunnerProtocolService) handleFileFrame(
	ctx context.Context,
	stream RunnerProtocolStream,
	frame *runnerprotocol.FileFrame,
	enabled map[runnerprotocol.RunnerFeature]bool,
	asyncErrors chan<- error,
) error {
	if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING] {
		return fmt.Errorf("SecondBox runner File feature was not negotiated")
	}
	if err := validateRunnerRelayFrameIdentity(frame.GetFence(), frame.GetOperationId(), frame.GetStreamId(), frame.GetSequence()); err != nil {
		return err
	}
	key := runnerRelayOperationKey(frame.Fence, frame.OperationId, frame.StreamId)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(frame)
	if err != nil {
		return fmt.Errorf("SecondBox runner encode File frame: %w", err)
	}

	s.operationMu.Lock()
	state := s.fileOperations[key]
	if state == nil {
		open := frame.GetOpen()
		if open == nil || frame.Sequence != 1 {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner File stream must begin with Open sequence one")
		}
		if err := s.validateOperationCorrelation(frame.Fence, frame.OperationId, frame.GetCorrelation()); err != nil {
			s.operationMu.Unlock()
			return err
		}
		if open.Operation == runnerprotocol.FileOperation_FILE_OPERATION_UNSPECIFIED ||
			strings.TrimSpace(open.WorkspaceRelativePath) == "" {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner File Open is incomplete")
		}
		if open.Operation == runnerprotocol.FileOperation_FILE_OPERATION_WRITE &&
			strings.TrimSpace(open.ExpectedChecksum) == "" {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner File write requires an expected checksum")
		}
		if open.Operation == runnerprotocol.FileOperation_FILE_OPERATION_READ && open.ExpectedSize == 0 {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner File read requires a positive maximum size")
		}
		fileCtx, cancel := context.WithCancelCause(ctx)
		state = &runnerFileOperation{
			key:          key,
			fence:        cloneRunnerFence(frame.Fence),
			correlation:  cloneRunnerCorrelation(frame.Correlation),
			operationID:  frame.OperationId,
			streamID:     frame.StreamId,
			open:         proto.Clone(open).(*runnerprotocol.FileOpen),
			nextIncoming: 2,
			lastIncoming: bytes.Clone(encoded),
			nextOutgoing: 1,
			credit:       newRunnerCreditWindow(),
			ctx:          fileCtx,
			cancel:       cancel,
		}
		if len(s.fileOperations) >= maxRunnerRelayOperationStates {
			s.operationMu.Unlock()
			return s.sendFileTerminal(stream, state, &runnerprotocol.FileTerminal{
				Kind:       runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED,
				SafeDetail: "runner File operation capacity is exhausted",
			})
		}
		s.fileOperations[key] = state
		s.operationMu.Unlock()

		if !s.hasActiveFence(frame.Fence) {
			return s.sendFileTerminal(stream, state, &runnerprotocol.FileTerminal{
				Kind:       runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FENCED,
				SafeDetail: "assignment fence is not active",
			})
		}
		if s.dataPlaneBackend == nil {
			return s.sendFileTerminal(stream, state, &runnerprotocol.FileTerminal{
				Kind:       runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED,
				SafeDetail: "runner data-plane backend is unavailable",
			})
		}
		s.setActiveOperation(frame.Fence.AssignmentId, frame.OperationId, true)
		if open.Operation != runnerprotocol.FileOperation_FILE_OPERATION_WRITE || open.ExpectedSize == 0 {
			state.started = true
			go s.executeFileOperation(fileCtx, stream, state, asyncErrors)
		}
		return nil
	}
	if frame.GetCorrelation() != nil && !proto.Equal(frame.GetCorrelation(), state.correlation) {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner File correlation changed within the operation")
	}
	duplicate, err := acceptRunnerRelayInput(state.nextIncoming, state.lastIncoming, frame.Sequence, encoded)
	if duplicate {
		terminal := state.terminalFrame
		s.operationMu.Unlock()
		if terminal != nil {
			return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_File{File: proto.Clone(terminal).(*runnerprotocol.FileFrame)},
			})
		}
		return nil
	}
	if err != nil {
		s.operationMu.Unlock()
		return err
	}
	state.nextIncoming++
	state.lastIncoming = bytes.Clone(encoded)

	switch {
	case frame.GetCredit() != nil:
		credit := frame.GetCredit().ByteCount
		s.operationMu.Unlock()
		return state.credit.add(credit)
	case frame.GetCancel() != nil:
		s.operationMu.Unlock()
		state.cancel(context.Canceled)
		return nil
	case frame.GetChunk() != nil:
		if state.open.Operation != runnerprotocol.FileOperation_FILE_OPERATION_WRITE {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner File chunks are only accepted for writes")
		}
		chunk := frame.GetChunk()
		if chunk.Offset != uint64(len(state.content)) ||
			uint64(len(state.content))+uint64(len(chunk.Data)) > state.open.ExpectedSize {
			s.operationMu.Unlock()
			return fmt.Errorf("SecondBox runner File write chunk is reordered or exceeds declared size")
		}
		state.content = append(state.content, chunk.Data...)
		start := uint64(len(state.content)) == state.open.ExpectedSize && !state.started
		if start {
			state.started = true
		}
		s.operationMu.Unlock()
		if start {
			go s.executeFileOperation(state.ctx, stream, state, asyncErrors)
		}
		return nil
	default:
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner File accepts only Open, Chunk, Credit, and Cancel frames")
	}
}

func (s *RunnerProtocolService) executeFileOperation(
	ctx context.Context,
	stream RunnerProtocolStream,
	state *runnerFileOperation,
	asyncErrors chan<- error,
) {
	defer s.setActiveOperation(state.fence.AssignmentId, state.operationID, false)
	result, err := s.dataPlaneBackend.ExecuteFile(
		ctx,
		cloneRunnerFence(state.fence),
		proto.Clone(state.open).(*runnerprotocol.FileOpen),
		bytes.Clone(state.content),
	)
	if err != nil {
		kind := runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED
		detail := "runner filesystem bridge failed"
		if errors.Is(err, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
			kind = runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED
			detail = "filesystem operation cancelled"
		}
		result.Terminal = &runnerprotocol.FileTerminal{Kind: kind, SafeDetail: detail}
	}
	if result.Metadata != nil {
		if err := s.sendFileMetadata(stream, state, result.Metadata); err != nil {
			reportRunnerAsyncError(asyncErrors, err)
			return
		}
	}
	if len(result.Content) > 0 {
		if err := s.sendRunnerFileBytes(ctx, stream, state, result.Content); err != nil {
			reportRunnerAsyncError(asyncErrors, err)
			return
		}
	}
	if result.Terminal == nil {
		result.Terminal = &runnerprotocol.FileTerminal{
			Kind:       runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED,
			SafeDetail: "runner filesystem bridge returned no terminal outcome",
		}
	}
	if err := s.sendFileTerminal(stream, state, result.Terminal); err != nil {
		reportRunnerAsyncError(asyncErrors, err)
	}
}

func (s *RunnerProtocolService) sendFileMetadata(
	stream RunnerProtocolStream,
	state *runnerFileOperation,
	metadata *runnerprotocol.FileMetadata,
) error {
	frame := &runnerprotocol.FileFrame{
		Fence:       cloneRunnerFence(state.fence),
		OperationId: state.operationID,
		StreamId:    state.streamID,
		Sequence:    state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload: &runnerprotocol.FileFrame_Metadata{
			Metadata: proto.Clone(metadata).(*runnerprotocol.FileMetadata),
		},
	}
	state.nextOutgoing++
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_File{File: frame},
	})
}

func (s *RunnerProtocolService) sendRunnerFileBytes(
	ctx context.Context,
	stream RunnerProtocolStream,
	state *runnerFileOperation,
	content []byte,
) error {
	credit := uint64(0)
	offset := uint64(0)
	for len(content) > 0 {
		if credit == 0 {
			var err error
			credit, err = state.credit.take(ctx, uint64(min(len(content), runnerRelayChunkBytes)))
			if err != nil {
				return err
			}
		}
		size := len(content)
		if size > runnerRelayChunkBytes {
			size = runnerRelayChunkBytes
		}
		if uint64(size) > credit {
			size = int(credit)
		}
		frame := &runnerprotocol.FileFrame{
			Fence:       cloneRunnerFence(state.fence),
			OperationId: state.operationID,
			StreamId:    state.streamID,
			Sequence:    state.nextOutgoing,
			Correlation: cloneRunnerCorrelation(state.correlation),
			Payload: &runnerprotocol.FileFrame_Chunk{Chunk: &runnerprotocol.FileChunk{
				Offset: offset,
				Data:   bytes.Clone(content[:size]),
			}},
		}
		state.nextOutgoing++
		if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_File{File: frame},
		}); err != nil {
			return err
		}
		content = content[size:]
		offset += uint64(size)
		credit -= uint64(size)
	}
	return nil
}

func (s *RunnerProtocolService) sendFileTerminal(
	stream RunnerProtocolStream,
	state *runnerFileOperation,
	terminal *runnerprotocol.FileTerminal,
) error {
	s.operationMu.Lock()
	if state.terminal {
		frame := proto.Clone(state.terminalFrame).(*runnerprotocol.FileFrame)
		s.operationMu.Unlock()
		return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_File{File: frame},
		})
	}
	frame := &runnerprotocol.FileFrame{
		Fence:       cloneRunnerFence(state.fence),
		OperationId: state.operationID,
		StreamId:    state.streamID,
		Sequence:    state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload: &runnerprotocol.FileFrame_Terminal{
			Terminal: proto.Clone(terminal).(*runnerprotocol.FileTerminal),
		},
	}
	state.nextOutgoing++
	state.terminal = true
	state.terminalFrame = proto.Clone(frame).(*runnerprotocol.FileFrame)
	s.retainFileTerminalLocked(state.key)
	s.operationMu.Unlock()
	if err := s.emitEvidence(
		context.Background(),
		runnerevidence.EventFileTerminal,
		state.fence,
		state.correlation,
		state.operationID,
		terminal.Kind.String(),
		terminalOutcome(terminal.Kind.String()),
	); err != nil {
		return err
	}
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_File{File: frame},
	})
}

func (s *RunnerProtocolService) retainExecTerminalLocked(key string) {
	if key == "" || s.execOperations[key] == nil {
		return
	}
	s.execTerminalOrder = append(s.execTerminalOrder, key)
	for len(s.execTerminalOrder) > maxRunnerRelayTerminalTombstones {
		oldest := s.execTerminalOrder[0]
		s.execTerminalOrder = s.execTerminalOrder[1:]
		if state := s.execOperations[oldest]; state != nil && state.terminal {
			delete(s.execOperations, oldest)
		}
	}
}

func (s *RunnerProtocolService) retainFileTerminalLocked(key string) {
	if key == "" || s.fileOperations[key] == nil {
		return
	}
	s.fileTerminalOrder = append(s.fileTerminalOrder, key)
	for len(s.fileTerminalOrder) > maxRunnerRelayTerminalTombstones {
		oldest := s.fileTerminalOrder[0]
		s.fileTerminalOrder = s.fileTerminalOrder[1:]
		if state := s.fileOperations[oldest]; state != nil && state.terminal {
			delete(s.fileOperations, oldest)
		}
	}
}

func validateRunnerRelayFrameIdentity(
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	streamID string,
	sequence uint64,
) error {
	if fence == nil ||
		strings.TrimSpace(fence.AssignmentId) == "" ||
		strings.TrimSpace(fence.SandboxId) == "" ||
		strings.TrimSpace(fence.InstanceId) == "" ||
		fence.SandboxGeneration == 0 ||
		len(fence.FencingToken) == 0 ||
		strings.TrimSpace(operationID) == "" ||
		strings.TrimSpace(streamID) == "" ||
		sequence == 0 {
		return fmt.Errorf("SecondBox runner relay frame identity is incomplete")
	}
	return nil
}

func (s *RunnerProtocolService) validateOperationCorrelation(
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	correlation *runnerprotocol.Correlation,
) error {
	if correlation == nil ||
		strings.TrimSpace(correlation.RequestId) == "" ||
		correlation.OperationId != operationID ||
		correlation.SandboxId != fence.SandboxId ||
		correlation.InstanceId != fence.InstanceId ||
		correlation.SandboxGeneration != fence.SandboxGeneration ||
		correlation.AssignmentId != fence.AssignmentId ||
		correlation.RunnerId != s.config.RunnerID {
		return fmt.Errorf("SecondBox runner operation correlation is incomplete or inconsistent")
	}
	return nil
}

func runnerRelayOperationKey(
	fence *runnerprotocol.AssignmentFence,
	operationID string,
	streamID string,
) string {
	return strings.Join([]string{
		fence.AssignmentId,
		fmt.Sprintf("%d", fence.SandboxGeneration),
		operationID,
		streamID,
	}, "\x00")
}

func acceptRunnerRelayInput(
	next uint64,
	previous []byte,
	sequence uint64,
	encoded []byte,
) (bool, error) {
	if sequence+1 == next && bytes.Equal(previous, encoded) {
		return true, nil
	}
	if sequence != next {
		return false, fmt.Errorf("SecondBox runner relay frame sequence is reordered")
	}
	return false, nil
}

func cloneRunnerFence(fence *runnerprotocol.AssignmentFence) *runnerprotocol.AssignmentFence {
	if fence == nil {
		return nil
	}
	return &runnerprotocol.AssignmentFence{
		AssignmentId:      fence.AssignmentId,
		SandboxId:         fence.SandboxId,
		InstanceId:        fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration,
		FencingToken:      bytes.Clone(fence.FencingToken),
	}
}

func cloneRunnerCorrelation(correlation *runnerprotocol.Correlation) *runnerprotocol.Correlation {
	if correlation == nil {
		return nil
	}
	return proto.Clone(correlation).(*runnerprotocol.Correlation)
}

func sameRunnerFence(left, right *runnerprotocol.AssignmentFence) bool {
	return left != nil &&
		right != nil &&
		left.AssignmentId == right.AssignmentId &&
		left.SandboxId == right.SandboxId &&
		left.InstanceId == right.InstanceId &&
		left.SandboxGeneration == right.SandboxGeneration &&
		bytes.Equal(left.FencingToken, right.FencingToken)
}

func reportRunnerAsyncError(target chan<- error, err error) {
	select {
	case target <- err:
	default:
	}
}
