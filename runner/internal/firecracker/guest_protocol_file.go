package firecracker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

type GuestFileReadResult struct {
	Metadata *guestv1.FileMetadata
	Content  []byte
}

func (s *GuestProtocolSession) WriteFile(
	ctx context.Context,
	assignmentID string,
	workspaceRelativePath string,
	content []byte,
	createMode uint32,
) error {
	checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	result, err := s.ExecuteFileOperation(ctx, assignmentID, &guestv1.FileRequest{
		Operation:             guestv1.FileOperation_FILE_OPERATION_WRITE,
		WorkspaceRelativePath: workspaceRelativePath,
		ExpectedSize:          uint64(len(content)),
		ExpectedChecksum:      checksum,
		CreateMode:            createMode,
	}, content)
	if err != nil {
		return fmt.Errorf("write guest file: %w", err)
	}
	terminal := result.Terminal
	if terminal == nil || terminal.Kind != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		return fmt.Errorf("guest file write failed: %s", terminal.GetSafeDetail())
	}
	return nil
}

func (s *GuestProtocolSession) ReadFile(
	ctx context.Context,
	assignmentID string,
	workspaceRelativePath string,
	maximumBytes uint64,
) (GuestFileReadResult, error) {
	if err := s.validateFileOperation(ctx, assignmentID, workspaceRelativePath); err != nil {
		return GuestFileReadResult{}, err
	}
	if maximumBytes == 0 {
		return GuestFileReadResult{}, fmt.Errorf("guest file read maximum bytes must be positive")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	binding, err := s.newFileOperationBinding(assignmentID)
	if err != nil {
		return GuestFileReadResult{}, err
	}
	if err := s.Stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
			Binding: binding,
			Payload: &guestv1.FileFrame_Request{Request: &guestv1.FileRequest{
				Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
				WorkspaceRelativePath: workspaceRelativePath,
			}},
		}},
	}); err != nil {
		return GuestFileReadResult{}, fmt.Errorf("send guest file read request: %w", err)
	}
	first, err := s.Stream.Recv()
	if err != nil {
		return GuestFileReadResult{}, fmt.Errorf("receive guest file read metadata: %w", err)
	}
	fileFrame := first.GetFile()
	if err := validateGuestFileResponseBinding(fileFrame, binding, 1); err != nil {
		return GuestFileReadResult{}, err
	}
	if terminal := fileFrame.GetTerminal(); terminal != nil {
		return GuestFileReadResult{}, fmt.Errorf("guest file read failed: %s", terminal.SafeDetail)
	}
	metadata := fileFrame.GetMetadata()
	if metadata == nil || metadata.Size > maximumBytes {
		return GuestFileReadResult{}, fmt.Errorf("guest file read metadata exceeds requested limit")
	}
	binding.Sequence = 2
	if err := s.Stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_File{File: &guestv1.FileFrame{
			Binding: binding,
			Payload: &guestv1.FileFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: maximumBytes}},
		}},
	}); err != nil {
		return GuestFileReadResult{}, fmt.Errorf("send guest file read credit: %w", err)
	}
	result := GuestFileReadResult{Metadata: metadata}
	expectedSequence := uint64(2)
	for {
		response, err := s.Stream.Recv()
		if err != nil {
			return GuestFileReadResult{}, fmt.Errorf("receive guest file read response: %w", err)
		}
		fileFrame := response.GetFile()
		if err := validateGuestFileResponseBinding(fileFrame, binding, expectedSequence); err != nil {
			return GuestFileReadResult{}, err
		}
		expectedSequence++
		if chunk := fileFrame.GetChunk(); chunk != nil {
			if chunk.Offset != uint64(len(result.Content)) ||
				uint64(len(result.Content))+uint64(len(chunk.Data)) > maximumBytes {
				return GuestFileReadResult{}, fmt.Errorf("guest file read chunk exceeded requested bounds")
			}
			result.Content = append(result.Content, chunk.Data...)
			continue
		}
		terminal := fileFrame.GetTerminal()
		if terminal == nil || terminal.Kind != guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
			return GuestFileReadResult{}, fmt.Errorf("guest file read failed: %s", terminal.GetSafeDetail())
		}
		if uint64(len(result.Content)) != metadata.Size {
			return GuestFileReadResult{}, fmt.Errorf("guest file read size did not match metadata")
		}
		return result, nil
	}
}

func (s *GuestProtocolSession) validateFileOperation(ctx context.Context, assignmentID, path string) error {
	if s == nil || s.Stream == nil || s.Binding == nil {
		return fmt.Errorf("guest protocol session is not ready")
	}
	if !s.EnabledFeatures[guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM] {
		return fmt.Errorf("guest protocol filesystem feature was not negotiated")
	}
	if strings.TrimSpace(assignmentID) == "" || strings.TrimSpace(path) == "" {
		return fmt.Errorf("guest file operation identity and path are required")
	}
	return ctx.Err()
}

func (s *GuestProtocolSession) newFileOperationBinding(assignmentID string) (*guestv1.OperationBinding, error) {
	operationID, err := randomGuestOperationID()
	if err != nil {
		return nil, err
	}
	return &guestv1.OperationBinding{
		Connection:   cloneGuestConnectionBinding(s.Binding),
		AssignmentId: assignmentID,
		OperationId:  operationID,
		StreamId:     operationID,
		Sequence:     1,
	}, nil
}

func validateGuestFileResponseBinding(frame *guestv1.FileFrame, request *guestv1.OperationBinding, sequence uint64) error {
	if frame == nil ||
		frame.Binding == nil ||
		frame.Binding.AssignmentId != request.AssignmentId ||
		frame.Binding.OperationId != request.OperationId ||
		frame.Binding.StreamId != request.StreamId ||
		frame.Binding.Sequence != sequence ||
		!sameConnectionBinding(frame.Binding.Connection, request.Connection) {
		return fmt.Errorf("guest file response binding mismatch")
	}
	return nil
}
