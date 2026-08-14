package runnercontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

const (
	runnerDataPlaneChunkBytes            = 64 << 10
	maxRunnerDataPlaneOperationStates    = 1024
	maxRunnerDataPlaneTerminalTombstones = 256
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
	ExecuteBuffered(
		context.Context,
		*runnerprotocol.AssignmentFence,
		*runnerprotocol.ExecOpen,
	) (BufferedExecResult, error)
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
	done             chan struct{}
	ptyControls      chan PTYControl
	pty              bool
	cancel           context.CancelCauseFunc
	terminal         bool
	terminalFrame    *runnerprotocol.ExecFrame
	ptyTerminalFrame *runnerprotocol.PtyFrame
	ptyAttachment    *runnerPTYAttachment
	ptyReplay        []*runnerprotocol.PtyFrame
	ptyReplayBytes   uint64
	ptyWindowBytes   uint64
	outputLimitBytes uint64
	stdout           []byte
	stderr           []byte
}

type runnerPTYAttachment struct {
	stream      RunnerProtocolStream
	reconnectID string
	mu          sync.Mutex
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
	if err := validateRunnerDataPlaneFrameIdentity(frame.GetFence(), frame.GetOperationId(), frame.GetStreamId(), frame.GetSequence()); err != nil {
		return err
	}
	key := runnerDataPlaneOperationKey(frame.Fence, frame.OperationId, frame.StreamId)
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
			key:              key,
			fence:            cloneRunnerFence(frame.Fence),
			correlation:      cloneRunnerCorrelation(frame.Correlation),
			operationID:      frame.OperationId,
			streamID:         frame.StreamId,
			nextIncoming:     2,
			nextOutgoing:     1,
			credit:           newRunnerCreditWindow(),
			controls:         make(chan ExecControl, 256),
			done:             make(chan struct{}),
			pty:              frame.GetOpen().AllocatePty,
			outputLimitBytes: frame.GetOpen().OutputLimitBytes,
		}
		if state.pty {
			state.ptyControls = make(chan PTYControl, 256)
		}
		execCtx, cancel := context.WithCancelCause(ctx)
		if state.pty {
			execCtx, cancel = context.WithCancelCause(context.Background())
			if frame.GetOpen().DeadlineUnixMs != 0 {
				deadline := time.UnixMilli(int64(frame.GetOpen().DeadlineUnixMs))
				deadlineCtx, stopDeadline := context.WithDeadline(execCtx, deadline)
				priorCancel := cancel
				cancel = func(cause error) {
					stopDeadline()
					priorCancel(cause)
				}
				execCtx = deadlineCtx
			}
		}
		state.cancel = cancel
		state.lastIncoming = bytes.Clone(encoded)
		if len(s.execOperations) >= maxRunnerDataPlaneOperationStates {
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
		if frame.GetOpen().Streaming {
			go s.executeStreamingOperation(execCtx, stream, state, frame.GetOpen(), asyncErrors)
		} else {
			go s.executeBufferedOperation(execCtx, stream, state, frame.GetOpen(), asyncErrors)
		}
		return nil
	}
	if frame.GetCorrelation() != nil && !proto.Equal(frame.GetCorrelation(), state.correlation) {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner Exec correlation changed within the operation")
	}
	duplicate, err := acceptRunnerDataPlaneInput(state.nextIncoming, state.lastIncoming, frame.Sequence, encoded)
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
	terminal := state.terminal
	s.operationMu.Unlock()
	if terminal {
		// Credits and input already in flight when the terminal was emitted are
		// sequenced and acknowledged, but never enter the now-unowned backend
		// control queue. Exact duplicates above still replay the retained terminal.
		return nil
	}

	switch {
	case frame.GetCredit() != nil:
		if state.pty {
			return fmt.Errorf("SecondBox runner PTY credit requires a PtyFrame")
		}
		if err := state.credit.add(frame.GetCredit().ByteCount); err != nil {
			return err
		}
		return sendExecControl(ctx, state.done, state.controls, ExecControl{Credit: frame.GetCredit().ByteCount})
	case frame.GetInput() != nil:
		if state.pty {
			return fmt.Errorf("SecondBox runner PTY input requires a PtyFrame")
		}
		return sendExecControl(ctx, state.done, state.controls, ExecControl{
			Input: proto.Clone(frame.GetInput()).(*runnerprotocol.ExecInput),
		})
	case frame.GetCancel() != nil:
		state.cancel(context.Canceled)
		return nil
	default:
		return fmt.Errorf("SecondBox runner Exec accepts only Open, Input, Credit, and Cancel frames")
	}
}

func sendExecControl(ctx context.Context, done <-chan struct{}, controls chan<- ExecControl, control ExecControl) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case controls <- control:
		return nil
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("SecondBox runner Exec control queue is full")
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
	if err := validateRunnerDataPlaneFrameIdentity(
		frame.GetFence(), frame.GetOperationId(), frame.GetStreamId(), frame.GetSequence(),
	); err != nil {
		return err
	}
	key := runnerDataPlaneOperationKey(frame.Fence, frame.OperationId, frame.StreamId)
	if frame.GetAttach() != nil {
		return s.attachPTYStream(stream, key, frame)
	}
	if frame.GetDetach() != nil {
		return s.detachPTYAttachment(key, frame.GetDetach().ReconnectId)
	}
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
	duplicate, err := acceptRunnerDataPlaneInput(
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

func (s *RunnerProtocolService) attachPTYStream(
	stream RunnerProtocolStream,
	key string,
	frame *runnerprotocol.PtyFrame,
) error {
	attach := frame.GetAttach()
	result := &runnerprotocol.PtyAttachResult{
		Kind:          runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED,
		AfterSequence: frame.GetAttach().AfterSequence,
	}
	s.operationMu.Lock()
	state := s.execOperations[key]
	if state == nil || !state.pty || state.ptyWindowBytes != 0 &&
		state.ptyWindowBytes != attach.StreamWindowBytes {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner PTY attachment identity is invalid")
	}
	if attach.ReconnectId == "" || attach.AfterSequence < -1 || attach.StreamWindowBytes == 0 {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner PTY attachment is incomplete")
	}
	// The Runner owns the live input sequence across a control-plane restart.
	// Returning it at attach prevents a reconnecting client from replaying an
	// input frame that the Runner accepted after the last durable checkpoint.
	nextInputSequence := state.nextIncoming
	result.NextInputSequence = &nextInputSequence
	if state.ptyAttachment != nil {
		result.Kind = runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ALREADY_ATTACHED
		result.SafeDetail = "Terminal already has an active attachment"
	} else if len(state.ptyReplay) > 0 &&
		attach.AfterSequence < int64(state.ptyReplay[0].Sequence)-2 {
		result.Kind = runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_REPLAY_EVICTED
		result.SafeDetail = "Terminal replay sequence was evicted"
	} else if attach.AfterSequence >= int64(state.nextOutgoing)-1 {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner PTY attachment sequence is invalid")
	}
	attachment := &runnerPTYAttachment{stream: stream, reconnectID: attach.ReconnectId}
	attachment.mu.Lock()
	var replay []*runnerprotocol.PtyFrame
	if result.Kind == runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED {
		state.ptyWindowBytes = attach.StreamWindowBytes
		state.ptyAttachment = attachment
		for _, retained := range state.ptyReplay {
			if int64(retained.Sequence)-1 > attach.AfterSequence {
				replay = append(replay, proto.Clone(retained).(*runnerprotocol.PtyFrame))
			}
		}
	}
	resultFrame := &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
		StreamId: state.streamID, Sequence: uint64(attach.AfterSequence + 2),
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload:     &runnerprotocol.PtyFrame_AttachResult{AttachResult: result},
	}
	s.operationMu.Unlock()

	err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Pty{Pty: resultFrame},
	})
	for _, retained := range replay {
		if err != nil {
			break
		}
		err = s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_Pty{Pty: retained},
		})
	}
	attachment.mu.Unlock()
	if err != nil && result.Kind == runnerprotocol.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED {
		err = errors.Join(err, s.detachPTYAttachment(key, attach.ReconnectId))
	}
	return err
}

func (s *RunnerProtocolService) detachPTYAttachment(key string, reconnectID string) error {
	if reconnectID == "" {
		return fmt.Errorf("SecondBox runner PTY detach identity is incomplete")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	state := s.execOperations[key]
	if state == nil || !state.pty {
		return fmt.Errorf("SecondBox runner PTY operation is not active")
	}
	if state.ptyAttachment == nil || state.ptyAttachment.reconnectID != reconnectID {
		return nil
	}
	attachment := state.ptyAttachment
	attachment.mu.Lock()
	state.ptyAttachment = nil
	attachment.mu.Unlock()
	return nil
}

func (s *RunnerProtocolService) detachPTYAttachmentsForStream(stream RunnerProtocolStream) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	for _, state := range s.execOperations {
		if state.ptyAttachment != nil && state.ptyAttachment.stream == stream {
			attachment := state.ptyAttachment
			attachment.mu.Lock()
			state.ptyAttachment = nil
			attachment.mu.Unlock()
		}
	}
}

func (s *RunnerProtocolService) retainAndSendPTYFrame(
	state *runnerExecOperation,
	frame *runnerprotocol.PtyFrame,
) error {
	retained := proto.Clone(frame).(*runnerprotocol.PtyFrame)
	frameBytes := uint64(len(retained.GetOutput().GetData()))
	s.operationMu.Lock()
	retained.Sequence = state.nextOutgoing
	state.nextOutgoing++
	state.ptyReplay = append(state.ptyReplay, retained)
	state.ptyReplayBytes += frameBytes
	for state.ptyReplayBytes > state.ptyWindowBytes && len(state.ptyReplay) > 1 {
		state.ptyReplayBytes -= uint64(len(state.ptyReplay[0].GetOutput().GetData()))
		state.ptyReplay = state.ptyReplay[1:]
	}
	attachment := state.ptyAttachment
	s.operationMu.Unlock()
	if attachment == nil {
		return nil
	}
	attachment.mu.Lock()
	err := s.sendRunnerFrame(attachment.stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Pty{
			Pty: proto.Clone(retained).(*runnerprotocol.PtyFrame),
		},
	})
	attachment.mu.Unlock()
	if err != nil {
		s.operationMu.Lock()
		if state.ptyAttachment == attachment {
			state.ptyAttachment = nil
		}
		s.operationMu.Unlock()
	}
	return nil
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
		} else if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			terminal = &runnerprotocol.ExecTerminal{
				Kind:     runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED,
				ExitCode: -1, SafeDetail: "Terminal deadline exceeded",
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
	_ RunnerProtocolStream,
	state *runnerExecOperation,
	content []byte,
) error {
	if len(content) == 0 {
		return fmt.Errorf("SecondBox runner PTY backend emitted empty output")
	}
	for len(content) > 0 {
		s.operationMu.Lock()
		windowBytes := state.ptyWindowBytes
		s.operationMu.Unlock()
		if windowBytes == 0 {
			return fmt.Errorf("SecondBox runner PTY replay window is unavailable")
		}
		size := min(len(content), runnerDataPlaneChunkBytes, int(windowBytes))
		frame := &runnerprotocol.PtyFrame{
			Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
			StreamId: state.streamID, Sequence: state.nextOutgoing,
			Correlation: cloneRunnerCorrelation(state.correlation),
			Payload: &runnerprotocol.PtyFrame_Output{Output: &runnerprotocol.ExecOutput{
				Channel: runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
				Data:    bytes.Clone(content[:size]),
			}},
		}
		if err := s.retainAndSendPTYFrame(state, frame); err != nil {
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
	_ RunnerProtocolStream,
	state *runnerExecOperation,
	terminal *runnerprotocol.ExecTerminal,
) error {
	s.operationMu.Lock()
	if state.terminal {
		s.operationMu.Unlock()
		return nil
	}
	frame := &runnerprotocol.PtyFrame{
		Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
		StreamId: state.streamID, Sequence: state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload: &runnerprotocol.PtyFrame_Terminal{
			Terminal: proto.Clone(terminal).(*runnerprotocol.ExecTerminal),
		},
	}
	state.terminal = true
	close(state.done)
	state.ptyTerminalFrame = proto.Clone(frame).(*runnerprotocol.PtyFrame)
	s.retainExecTerminalLocked(state.key)
	s.operationMu.Unlock()
	retainErr := s.retainAndSendPTYFrame(state, frame)
	evidenceErr := s.emitEvidence(
		context.Background(), runnerevidence.EventExecTerminal,
		state.fence, state.correlation, state.operationID,
		terminal.Kind.String(), terminalOutcome(terminal.Kind.String()),
	)
	return errors.Join(retainErr, evidenceErr)
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

func (s *RunnerProtocolService) executeBufferedOperation(
	ctx context.Context,
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	open *runnerprotocol.ExecOpen,
	asyncErrors chan<- error,
) {
	defer s.setActiveOperation(state.fence.AssignmentId, state.operationID, false)
	result, err := s.dataPlaneBackend.ExecuteBuffered(
		ctx, cloneRunnerFence(state.fence), proto.Clone(open).(*runnerprotocol.ExecOpen),
	)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
			result.Terminal = &runnerprotocol.ExecTerminal{
				Kind:     runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED,
				ExitCode: -1, SafeDetail: "command cancelled",
			}
		} else {
			result.Terminal = runnerInfrastructureTerminal(
				runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
				runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GUEST_AGENT,
				true, "runner buffered execution failed",
			)
		}
	}
	if result.Terminal == nil {
		result.Terminal = runnerInfrastructureTerminal(
			runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_RUNNER_FAILED,
			runnerprotocol.InfrastructureFailureReason_INFRASTRUCTURE_FAILURE_REASON_GUEST_AGENT,
			true, "runner buffered execution returned no terminal outcome",
		)
	}
	if err := s.sendExecBufferedResult(stream, state, result); err != nil {
		reportRunnerAsyncError(asyncErrors, err)
	}
}

func (s *RunnerProtocolService) sendExecBufferedResult(
	stream RunnerProtocolStream,
	state *runnerExecOperation,
	result BufferedExecResult,
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
		Fence: cloneRunnerFence(state.fence), OperationId: state.operationID,
		StreamId: state.streamID, Sequence: state.nextOutgoing,
		Correlation: cloneRunnerCorrelation(state.correlation),
		Payload: &runnerprotocol.ExecFrame_BufferedResult{BufferedResult: &runnerprotocol.ExecBufferedResult{
			Stdout: bytes.Clone(result.Stdout), Stderr: bytes.Clone(result.Stderr),
			Terminal: proto.Clone(result.Terminal).(*runnerprotocol.ExecTerminal),
		}},
	}
	state.nextOutgoing++
	state.terminal = true
	close(state.done)
	state.terminalFrame = proto.Clone(frame).(*runnerprotocol.ExecFrame)
	s.retainExecTerminalLocked(state.key)
	s.operationMu.Unlock()
	if err := s.emitEvidence(
		context.Background(), runnerevidence.EventExecTerminal,
		state.fence, state.correlation, state.operationID,
		result.Terminal.Kind.String(), terminalOutcome(result.Terminal.Kind.String()),
	); err != nil {
		return err
	}
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Exec{Exec: frame},
	})
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
			credit, err = state.credit.take(ctx, uint64(min(len(content), runnerDataPlaneChunkBytes)))
			if err != nil {
				return err
			}
		}
		size := len(content)
		if size > runnerDataPlaneChunkBytes {
			size = runnerDataPlaneChunkBytes
		}
		if uint64(size) > credit {
			size = int(credit)
		}
		if uint64(len(state.stdout))+uint64(len(state.stderr))+uint64(size) > state.outputLimitBytes {
			return fmt.Errorf("SecondBox runner streaming Exec output exceeds the configured limit")
		}
		switch channel {
		case runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT:
			state.stdout = append(state.stdout, content[:size]...)
		case runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR:
			state.stderr = append(state.stderr, content[:size]...)
		default:
			return fmt.Errorf("SecondBox runner streaming Exec output channel is invalid")
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
		Payload: &runnerprotocol.ExecFrame_BufferedResult{BufferedResult: &runnerprotocol.ExecBufferedResult{
			Stdout: bytes.Clone(state.stdout), Stderr: bytes.Clone(state.stderr),
			Terminal: proto.Clone(terminal).(*runnerprotocol.ExecTerminal),
		}},
	}
	state.nextOutgoing++
	state.terminal = true
	close(state.done)
	state.terminalFrame = proto.Clone(frame).(*runnerprotocol.ExecFrame)
	state.stdout, state.stderr = nil, nil
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
	if err := validateRunnerDataPlaneFrameIdentity(frame.GetFence(), frame.GetOperationId(), frame.GetStreamId(), frame.GetSequence()); err != nil {
		return err
	}
	key := runnerDataPlaneOperationKey(frame.Fence, frame.OperationId, frame.StreamId)
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
		if len(s.fileOperations) >= maxRunnerDataPlaneOperationStates {
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
	duplicate, err := acceptRunnerDataPlaneInput(state.nextIncoming, state.lastIncoming, frame.Sequence, encoded)
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
			credit, err = state.credit.take(ctx, uint64(min(len(content), runnerDataPlaneChunkBytes)))
			if err != nil {
				return err
			}
		}
		size := len(content)
		if size > runnerDataPlaneChunkBytes {
			size = runnerDataPlaneChunkBytes
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
	for len(s.execTerminalOrder) > maxRunnerDataPlaneTerminalTombstones {
		oldest := s.execTerminalOrder[0]
		s.execTerminalOrder = s.execTerminalOrder[1:]
		if state := s.execOperations[oldest]; state != nil && state.terminal {
			s.directDataPlane.complete(state.operationID)
			delete(s.execOperations, oldest)
		}
	}
}

func (s *RunnerProtocolService) retainFileTerminalLocked(key string) {
	if key == "" || s.fileOperations[key] == nil {
		return
	}
	s.fileTerminalOrder = append(s.fileTerminalOrder, key)
	for len(s.fileTerminalOrder) > maxRunnerDataPlaneTerminalTombstones {
		oldest := s.fileTerminalOrder[0]
		s.fileTerminalOrder = s.fileTerminalOrder[1:]
		if state := s.fileOperations[oldest]; state != nil && state.terminal {
			delete(s.fileOperations, oldest)
		}
	}
}

func validateRunnerDataPlaneFrameIdentity(
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
		return fmt.Errorf("SecondBox runner data-plane frame identity is incomplete")
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

func runnerDataPlaneOperationKey(
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

func acceptRunnerDataPlaneInput(
	next uint64,
	previous []byte,
	sequence uint64,
	encoded []byte,
) (bool, error) {
	if sequence+1 == next && bytes.Equal(previous, encoded) {
		return true, nil
	}
	if sequence != next {
		return false, fmt.Errorf("SecondBox runner data-plane frame sequence is reordered")
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

func reportRunnerAsyncError(target chan<- error, err error) {
	select {
	case target <- err:
	default:
	}
}
