package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

// GuestPTYControl is exactly one credit grant, input chunk, or terminal resize.
type GuestPTYControl struct {
	Input   []byte
	Credit  uint64
	Rows    uint32
	Columns uint32
}

// GuestPTYResult retains the stable session identity through terminal acknowledgement.
type GuestPTYResult struct {
	SessionID string
	Admission *guestv1.ExecAdmission
	Terminal  *guestv1.ExecTerminal
}

type guestPTYOperationSender struct {
	execSender *guestExecOperationSender
}

func (sender *guestPTYOperationSender) send(frame *guestv1.PtyFrame) error {
	sender.execSender.mu.Lock()
	defer sender.execSender.mu.Unlock()
	frame.Binding = cloneGuestOperationBinding(sender.execSender.binding)
	frame.Binding.Sequence = sender.execSender.nextSequence
	sender.execSender.nextSequence++
	sender.execSender.session.sendMu.Lock()
	defer sender.execSender.session.sendMu.Unlock()
	return sender.execSender.session.Stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Pty{Pty: frame},
	})
}

// ExecutePTY runs one real terminal operation over the retained Firecracker guest stream.
func (s *GuestProtocolSession) ExecutePTY(
	ctx context.Context,
	assignmentID string,
	request *guestv1.ExecRequest,
	controls <-chan GuestPTYControl,
	emit func([]byte) error,
) (result GuestPTYResult, resultErr error) {
	if s == nil || s.Stream == nil || s.Binding == nil {
		return GuestPTYResult{}, fmt.Errorf("guest protocol session is not ready")
	}
	if !s.EnabledFeatures[guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC] ||
		!s.EnabledFeatures[guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE] {
		return GuestPTYResult{}, fmt.Errorf("guest protocol PTY features were not negotiated")
	}
	if strings.TrimSpace(assignmentID) == "" ||
		request == nil ||
		request.Pty == nil ||
		request.Pty.Rows == 0 ||
		request.Pty.Rows > 65535 ||
		request.Pty.Columns == 0 ||
		request.Pty.Columns > 65535 ||
		!request.Streaming ||
		request.OutputLimitBytes == 0 ||
		emit == nil {
		return GuestPTYResult{}, fmt.Errorf("guest PTY request is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return GuestPTYResult{}, err
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	operationID, err := randomGuestOperationID()
	if err != nil {
		return GuestPTYResult{}, err
	}
	result.SessionID = operationID
	binding := &guestv1.OperationBinding{
		Connection:   cloneGuestConnectionBinding(s.Binding),
		AssignmentId: assignmentID,
		OperationId:  operationID,
		StreamId:     operationID,
		Sequence:     1,
	}
	s.sendMu.Lock()
	err = s.Stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Exec{Exec: &guestv1.ExecFrame{
			Binding: binding,
			Payload: &guestv1.ExecFrame_Request{Request: cloneGuestExecRequest(request)},
		}},
	})
	s.sendMu.Unlock()
	if err != nil {
		return result, fmt.Errorf("send guest PTY request: %w", err)
	}
	sender := &guestPTYOperationSender{execSender: &guestExecOperationSender{
		session: s, binding: binding, nextSequence: 2,
	}}
	cancellationSendError := make(chan error, 1)
	stopCancellation := context.AfterFunc(ctx, func() {
		cancellationSendError <- sender.send(&guestv1.PtyFrame{
			Payload: &guestv1.PtyFrame_Cancel{Cancel: &guestv1.ExecCancel{
				Reason: "runner PTY operation context cancelled",
			}},
		})
	})
	defer func() {
		if !stopCancellation() {
			resultErr = errors.Join(resultErr, <-cancellationSendError)
		}
	}()

	first, err := s.Stream.Recv()
	if err != nil {
		return result, fmt.Errorf("receive guest PTY admission: %w", err)
	}
	execFrame := first.GetExec()
	if err := validateGuestExecResponseBinding(execFrame, binding, 1); err != nil {
		return result, err
	}
	result.Admission = execFrame.GetAdmission()
	if result.Admission == nil {
		return result, fmt.Errorf("guest PTY first response was not admission")
	}
	if result.Admission.Kind != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		return result, nil
	}

	controlCtx, stopControls := context.WithCancel(ctx)
	defer stopControls()
	controlErrors := make(chan error, 1)
	go forwardGuestPTYControls(controlCtx, sender, controls, controlErrors)

	expectedSequence := uint64(2)
	for {
		response, err := s.Stream.Recv()
		if err != nil {
			select {
			case controlErr := <-controlErrors:
				return result, fmt.Errorf("forward guest PTY control: %w", controlErr)
			default:
			}
			return result, fmt.Errorf("receive guest PTY response: %w", err)
		}
		frame := response.GetPty()
		if err := validateGuestPTYResponseBinding(frame, binding, expectedSequence); err != nil {
			return result, err
		}
		expectedSequence++
		if output := frame.GetOutput(); output != nil {
			if output.Channel != guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT ||
				len(output.Data) == 0 {
				return result, fmt.Errorf("guest PTY returned invalid output")
			}
			if err := emit(bytes.Clone(output.Data)); err != nil {
				return result, err
			}
			continue
		}
		if terminal := frame.GetTerminal(); terminal != nil {
			result.Terminal = terminal
			return result, nil
		}
		return result, fmt.Errorf("guest PTY returned unsupported response frame")
	}
}

func forwardGuestPTYControls(
	ctx context.Context,
	sender *guestPTYOperationSender,
	controls <-chan GuestPTYControl,
	controlErrors chan<- error,
) {
	for {
		select {
		case control, open := <-controls:
			if !open {
				return
			}
			frame, err := guestPTYControlFrame(control)
			if err == nil {
				err = sender.send(frame)
			}
			if err != nil {
				select {
				case controlErrors <- err:
				default:
				}
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func guestPTYControlFrame(control GuestPTYControl) (*guestv1.PtyFrame, error) {
	kinds := 0
	if control.Input != nil {
		kinds++
	}
	if control.Credit != 0 {
		kinds++
	}
	if control.Rows != 0 || control.Columns != 0 {
		kinds++
	}
	if kinds != 1 {
		return nil, fmt.Errorf("guest PTY control must contain exactly one payload")
	}
	switch {
	case control.Input != nil:
		if len(control.Input) == 0 {
			return nil, fmt.Errorf("guest PTY input is empty")
		}
		return &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Input{
			Input: &guestv1.PtyInput{Data: bytes.Clone(control.Input)},
		}}, nil
	case control.Credit != 0:
		return &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Credit{
			Credit: &guestv1.ByteCredit{ByteCount: control.Credit},
		}}, nil
	default:
		if control.Rows == 0 || control.Rows > 65535 ||
			control.Columns == 0 || control.Columns > 65535 {
			return nil, fmt.Errorf("guest PTY resize dimensions are invalid")
		}
		return &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Resize{
			Resize: &guestv1.PtyResize{Rows: control.Rows, Columns: control.Columns},
		}}, nil
	}
}

func validateGuestPTYResponseBinding(
	frame *guestv1.PtyFrame,
	request *guestv1.OperationBinding,
	sequence uint64,
) error {
	if frame == nil ||
		frame.Binding == nil ||
		frame.Binding.AssignmentId != request.AssignmentId ||
		frame.Binding.OperationId != request.OperationId ||
		frame.Binding.StreamId != request.StreamId ||
		frame.Binding.Sequence != sequence ||
		!sameConnectionBinding(frame.Binding.Connection, request.Connection) {
		return fmt.Errorf("Firecracker guest PTY response binding mismatch")
	}
	return nil
}
