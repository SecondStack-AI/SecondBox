package firecracker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

const guestFileWriteChunkSize = 64 << 10

type GuestFileOperationResult struct {
	Metadata *guestv1.FileMetadata
	Content  []byte
	Terminal *guestv1.FileTerminal
}

type guestFileOperationSender struct {
	session      *GuestProtocolSession
	binding      *guestv1.OperationBinding
	mu           sync.Mutex
	nextSequence uint64
}

func (sender *guestFileOperationSender) send(frame *guestv1.FileFrame) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	frame.Binding = cloneGuestOperationBinding(sender.binding)
	frame.Binding.Sequence = sender.nextSequence
	sender.nextSequence++
	sender.session.sendMu.Lock()
	defer sender.session.sendMu.Unlock()
	return sender.session.Stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_File{File: frame},
	})
}

func (sender *guestFileOperationSender) sendWriteContent(content []byte) error {
	for offset := 0; offset < len(content); offset += guestFileWriteChunkSize {
		end := min(offset+guestFileWriteChunkSize, len(content))
		if err := sender.send(&guestv1.FileFrame{
			Payload: &guestv1.FileFrame_Chunk{Chunk: &guestv1.FileChunk{
				Offset: uint64(offset),
				Data:   content[offset:end],
			}},
		}); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteFileOperation performs one serialized descriptor-pinned filesystem
// operation while preserving the guest's typed terminal outcome.
func (s *GuestProtocolSession) ExecuteFileOperation(
	ctx context.Context,
	assignmentID string,
	request *guestv1.FileRequest,
	content []byte,
) (result GuestFileOperationResult, resultErr error) {
	if request == nil {
		return GuestFileOperationResult{}, fmt.Errorf("guest file request is required")
	}
	if err := s.validateFileOperation(ctx, assignmentID, request.WorkspaceRelativePath); err != nil {
		return GuestFileOperationResult{}, err
	}
	if request.Operation == guestv1.FileOperation_FILE_OPERATION_UNSPECIFIED {
		return GuestFileOperationResult{}, fmt.Errorf("guest file operation is unspecified")
	}
	if request.Operation == guestv1.FileOperation_FILE_OPERATION_WRITE &&
		uint64(len(content)) != request.ExpectedSize {
		return GuestFileOperationResult{}, fmt.Errorf("guest file write content does not match declared size")
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	binding, err := s.newFileOperationBinding(assignmentID)
	if err != nil {
		return GuestFileOperationResult{}, err
	}
	s.sendMu.Lock()
	err = s.Stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
			Binding: binding,
			Payload: &guestv1.FileFrame_Request{
				Request: request,
			},
		}},
	})
	s.sendMu.Unlock()
	if err != nil {
		return GuestFileOperationResult{}, fmt.Errorf("send guest file request: %w", err)
	}
	sender := &guestFileOperationSender{
		session: s, binding: binding, nextSequence: 2,
	}
	if request.Operation == guestv1.FileOperation_FILE_OPERATION_WRITE {
		if err := sender.sendWriteContent(content); err != nil {
			return GuestFileOperationResult{}, fmt.Errorf("send guest file write content: %w", err)
		}
	}
	stopCancellation := func() bool { return true }
	cancellationSendError := make(chan error, 1)
	if request.Operation == guestv1.FileOperation_FILE_OPERATION_READ ||
		request.Operation == guestv1.FileOperation_FILE_OPERATION_WRITE {
		stopCancellation = context.AfterFunc(ctx, func() {
			cancellationSendError <- sender.send(&guestv1.FileFrame{
				Payload: &guestv1.FileFrame_Cancel{Cancel: &guestv1.ExecCancel{
					Reason: "runner operation context cancelled",
				}},
			})
		})
		defer func() {
			if !stopCancellation() {
				resultErr = errors.Join(resultErr, <-cancellationSendError)
			}
		}()
	}

	first, err := s.Stream.Recv()
	if err != nil {
		return GuestFileOperationResult{}, fmt.Errorf("receive guest file response: %w", err)
	}
	frame := first.GetFile()
	if err := validateGuestFileResponseBinding(frame, binding, 1); err != nil {
		return GuestFileOperationResult{}, err
	}
	if terminal := frame.GetTerminal(); terminal != nil {
		return GuestFileOperationResult{Terminal: terminal}, nil
	}
	metadata := frame.GetMetadata()
	if metadata == nil {
		return GuestFileOperationResult{}, fmt.Errorf("guest file first response was neither metadata nor terminal")
	}
	result = GuestFileOperationResult{Metadata: metadata}
	expectedSequence := uint64(2)
	if request.Operation == guestv1.FileOperation_FILE_OPERATION_READ {
		if request.ExpectedSize == 0 {
			return GuestFileOperationResult{}, fmt.Errorf("guest file read requires a positive maximum size")
		}
		if metadata.Size > request.ExpectedSize {
			return s.declineOversizedGuestRead(sender, binding, metadata)
		}
		if ctx.Err() == nil {
			err = sender.send(&guestv1.FileFrame{
				Payload: &guestv1.FileFrame_Credit{
					Credit: &guestv1.ByteCredit{ByteCount: request.ExpectedSize},
				},
			})
		}
		if err != nil {
			return GuestFileOperationResult{}, fmt.Errorf("send guest file read credit: %w", err)
		}
	}
	for {
		response, err := s.Stream.Recv()
		if err != nil {
			return GuestFileOperationResult{}, fmt.Errorf("receive guest file continuation: %w", err)
		}
		frame := response.GetFile()
		if err := validateGuestFileResponseBinding(frame, binding, expectedSequence); err != nil {
			return GuestFileOperationResult{}, err
		}
		expectedSequence++
		if chunk := frame.GetChunk(); chunk != nil {
			if request.Operation != guestv1.FileOperation_FILE_OPERATION_READ ||
				chunk.Offset != uint64(len(result.Content)) ||
				uint64(len(result.Content))+uint64(len(chunk.Data)) > request.ExpectedSize {
				return GuestFileOperationResult{}, fmt.Errorf("guest file read chunk is reordered or exceeds bounds")
			}
			result.Content = append(result.Content, chunk.Data...)
			continue
		}
		terminal := frame.GetTerminal()
		if terminal == nil {
			return GuestFileOperationResult{}, fmt.Errorf("guest file continuation is unsupported")
		}
		result.Terminal = terminal
		if request.Operation == guestv1.FileOperation_FILE_OPERATION_READ &&
			terminal.Kind == guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED &&
			uint64(len(result.Content)) != metadata.Size {
			return GuestFileOperationResult{}, fmt.Errorf("guest file read content does not match metadata size")
		}
		return result, nil
	}
}

// declineOversizedGuestRead refuses a read whose file is larger than the
// admitted bound before any content credit is granted. The guest is told to
// stop and its terminal frame is consumed so the session stays in sequence,
// and the caller receives the typed limit outcome instead of a bridge failure.
func (s *GuestProtocolSession) declineOversizedGuestRead(
	sender *guestFileOperationSender,
	binding *guestv1.OperationBinding,
	metadata *guestv1.FileMetadata,
) (GuestFileOperationResult, error) {
	const detail = "file exceeds the admitted read bound"
	if err := sender.send(&guestv1.FileFrame{
		Payload: &guestv1.FileFrame_Cancel{Cancel: &guestv1.ExecCancel{Reason: detail}},
	}); err != nil {
		return GuestFileOperationResult{}, fmt.Errorf("send guest oversized read cancellation: %w", err)
	}
	expectedSequence := uint64(2)
	for {
		response, err := s.Stream.Recv()
		if err != nil {
			return GuestFileOperationResult{}, fmt.Errorf("receive guest oversized read outcome: %w", err)
		}
		frame := response.GetFile()
		if err := validateGuestFileResponseBinding(frame, binding, expectedSequence); err != nil {
			return GuestFileOperationResult{}, err
		}
		expectedSequence++
		if frame.GetTerminal() == nil {
			continue
		}
		return GuestFileOperationResult{
			Metadata: metadata,
			Terminal: &guestv1.FileTerminal{
				Kind:       guestv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED,
				SafeDetail: detail,
			},
		}, nil
	}
}
