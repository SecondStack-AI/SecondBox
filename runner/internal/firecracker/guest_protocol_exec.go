package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"google.golang.org/protobuf/proto"
)

type BufferedGuestExecResult struct {
	Admission *guestv1.ExecAdmission
	Stdout    []byte
	Stderr    []byte
	Terminal  *guestv1.ExecTerminal
}

type guestExecOperationSender struct {
	session      *GuestProtocolSession
	binding      *guestv1.OperationBinding
	mu           sync.Mutex
	nextSequence uint64
}

func (sender *guestExecOperationSender) send(payload guestExecPayload) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	binding := cloneGuestOperationBinding(sender.binding)
	binding.Sequence = sender.nextSequence
	sender.nextSequence++
	frame := &guestv1.ExecFrame{Binding: binding}
	payload.apply(frame)
	sender.session.sendMu.Lock()
	defer sender.session.sendMu.Unlock()
	return sender.session.Stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Exec{Exec: frame},
	})
}

type guestExecPayload interface {
	apply(*guestv1.ExecFrame)
}

type guestExecCreditPayload struct {
	credit *guestv1.ByteCredit
}

func (payload guestExecCreditPayload) apply(frame *guestv1.ExecFrame) {
	frame.Payload = &guestv1.ExecFrame_Credit{Credit: payload.credit}
}

type guestExecInputPayload struct {
	input *guestv1.ExecInput
}

func (payload guestExecInputPayload) apply(frame *guestv1.ExecFrame) {
	frame.Payload = &guestv1.ExecFrame_Input{Input: payload.input}
}

type guestExecCancelPayload struct {
	cancel *guestv1.ExecCancel
}

func (payload guestExecCancelPayload) apply(frame *guestv1.ExecFrame) {
	frame.Payload = &guestv1.ExecFrame_Cancel{Cancel: payload.cancel}
}

type GuestExecControl struct {
	Input      []byte
	EndOfInput bool
	Credit     uint64
}

// ExecuteStreaming performs one bounded exec over the retained assignment-bound guest stream.
func (s *GuestProtocolSession) ExecuteStreaming(
	ctx context.Context,
	assignmentID string,
	request *guestv1.ExecRequest,
	controls <-chan GuestExecControl,
	emit func(guestv1.ExecOutputChannel, []byte) error,
) (result BufferedGuestExecResult, resultErr error) {
	if s == nil || s.Stream == nil || s.Binding == nil {
		return BufferedGuestExecResult{}, fmt.Errorf("guest protocol session is not ready")
	}
	if !s.EnabledFeatures[guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC] {
		return BufferedGuestExecResult{}, fmt.Errorf("guest protocol exec feature was not negotiated")
	}
	if strings.TrimSpace(assignmentID) == "" || request == nil || request.OutputLimitBytes == 0 {
		return BufferedGuestExecResult{}, fmt.Errorf("guest exec request is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return BufferedGuestExecResult{}, err
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	operationID, err := randomGuestOperationID()
	if err != nil {
		return BufferedGuestExecResult{}, err
	}
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
			Payload: &guestv1.ExecFrame_Request{Request: request},
		}},
	})
	s.sendMu.Unlock()
	if err != nil {
		return BufferedGuestExecResult{}, fmt.Errorf("send guest exec request: %w", err)
	}
	sender := &guestExecOperationSender{
		session: s, binding: binding, nextSequence: 2,
	}
	cancellationSendError := make(chan error, 1)
	stopCancellation := context.AfterFunc(ctx, func() {
		cancellationSendError <- sender.send(guestExecCancelPayload{
			cancel: &guestv1.ExecCancel{Reason: "runner operation context cancelled"},
		})
	})
	defer func() {
		if !stopCancellation() {
			resultErr = errors.Join(resultErr, <-cancellationSendError)
		}
	}()
	first, err := s.Stream.Recv()
	if err != nil {
		return BufferedGuestExecResult{}, fmt.Errorf("receive guest exec admission: %w", err)
	}
	execFrame := first.GetExec()
	if err := validateGuestExecResponseBinding(execFrame, binding, 1); err != nil {
		return BufferedGuestExecResult{}, err
	}
	admission := execFrame.GetAdmission()
	if admission == nil {
		return BufferedGuestExecResult{}, fmt.Errorf("guest exec first response was not admission")
	}
	result = BufferedGuestExecResult{Admission: admission}
	if admission.Kind != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		return result, nil
	}
	controlCtx, stopControls := context.WithCancel(ctx)
	defer stopControls()
	controlErrors := make(chan error, 1)
	go func() {
		for {
			select {
			case control, open := <-controls:
				if !open {
					return
				}
				var sendErr error
				switch {
				case control.Credit > 0:
					sendErr = sender.send(guestExecCreditPayload{
						credit: &guestv1.ByteCredit{ByteCount: control.Credit},
					})
				case control.Input != nil || control.EndOfInput:
					sendErr = sender.send(guestExecInputPayload{
						input: &guestv1.ExecInput{
							Data: bytes.Clone(control.Input), EndOfInput: control.EndOfInput,
						},
					})
				default:
					sendErr = fmt.Errorf("guest exec control is empty")
				}
				if sendErr != nil {
					select {
					case controlErrors <- sendErr:
					default:
					}
					return
				}
			case <-controlCtx.Done():
				return
			}
		}
	}()
	expectedSequence := uint64(2)
	for {
		response, err := s.Stream.Recv()
		if err != nil {
			select {
			case controlErr := <-controlErrors:
				return BufferedGuestExecResult{}, fmt.Errorf("forward guest exec control: %w", controlErr)
			default:
			}
			return BufferedGuestExecResult{}, fmt.Errorf("receive guest exec response: %w", err)
		}
		execFrame := response.GetExec()
		if err := validateGuestExecResponseBinding(execFrame, binding, expectedSequence); err != nil {
			return BufferedGuestExecResult{}, err
		}
		expectedSequence++
		if output := execFrame.GetOutput(); output != nil {
			if output.Channel != guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT &&
				output.Channel != guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR {
				return BufferedGuestExecResult{}, fmt.Errorf("guest exec returned unspecified output channel")
			}
			if err := emit(output.Channel, bytes.Clone(output.Data)); err != nil {
				return BufferedGuestExecResult{}, err
			}
			continue
		}
		if terminal := execFrame.GetTerminal(); terminal != nil {
			result.Terminal = terminal
			return result, nil
		}
		return BufferedGuestExecResult{}, fmt.Errorf("guest exec returned unsupported response frame")
	}
}

// ExecuteBuffered collects one bounded exec through the streaming primitive.
func (s *GuestProtocolSession) ExecuteBuffered(
	ctx context.Context,
	assignmentID string,
	request *guestv1.ExecRequest,
) (BufferedGuestExecResult, error) {
	request = cloneGuestExecRequest(request)
	request.Streaming = false
	controls := make(chan GuestExecControl, 1)
	controls <- GuestExecControl{Credit: request.OutputLimitBytes}
	close(controls)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	result, err := s.ExecuteStreaming(
		ctx,
		assignmentID,
		request,
		controls,
		func(channel guestv1.ExecOutputChannel, data []byte) error {
			switch channel {
			case guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT:
				_, err := stdout.Write(data)
				return err
			case guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR:
				_, err := stderr.Write(data)
				return err
			default:
				return fmt.Errorf("guest exec returned unspecified output channel")
			}
		},
	)
	result.Stdout = bytes.Clone(stdout.Bytes())
	result.Stderr = bytes.Clone(stderr.Bytes())
	return result, err
}

func cloneGuestExecRequest(request *guestv1.ExecRequest) *guestv1.ExecRequest {
	if request == nil {
		return &guestv1.ExecRequest{}
	}
	return proto.Clone(request).(*guestv1.ExecRequest)
}

func validateGuestExecResponseBinding(frame *guestv1.ExecFrame, request *guestv1.OperationBinding, sequence uint64) error {
	if frame == nil ||
		frame.Binding == nil ||
		frame.Binding.AssignmentId != request.AssignmentId ||
		frame.Binding.OperationId != request.OperationId ||
		frame.Binding.StreamId != request.StreamId ||
		frame.Binding.Sequence != sequence ||
		!sameConnectionBinding(frame.Binding.Connection, request.Connection) {
		return fmt.Errorf("buffered guest exec response binding mismatch")
	}
	return nil
}

func cloneGuestConnectionBinding(binding *guestv1.ConnectionBinding) *guestv1.ConnectionBinding {
	if binding == nil {
		return nil
	}
	return &guestv1.ConnectionBinding{
		InstanceId:        binding.InstanceId,
		SandboxId:         binding.SandboxId,
		SandboxGeneration: binding.SandboxGeneration,
		ConnectionNonce:   bytes.Clone(binding.ConnectionNonce),
	}
}

func cloneGuestOperationBinding(binding *guestv1.OperationBinding) *guestv1.OperationBinding {
	if binding == nil {
		return nil
	}
	return &guestv1.OperationBinding{
		Connection:   cloneGuestConnectionBinding(binding.Connection),
		AssignmentId: binding.AssignmentId,
		OperationId:  binding.OperationId,
		StreamId:     binding.StreamId,
		Sequence:     binding.Sequence,
	}
}

func randomGuestOperationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create guest operation ID: %w", err)
	}
	return "op-" + hex.EncodeToString(value[:]), nil
}
