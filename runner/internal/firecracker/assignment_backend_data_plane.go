package firecracker

import (
	"bytes"
	"context"
	"fmt"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const runnerFileCreateMode uint32 = 0o600

// ExecuteStreaming bridges runner output through the retained guest session.
func (b *AssignmentBackend) ExecuteStreaming(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
	controls <-chan runnercontrol.ExecControl,
	emit func(runnerprotocol.ExecOutputChannel, []byte) error,
) (*runnerprotocol.ExecTerminal, error) {
	session, err := b.guestSessionForFence(fence)
	if err != nil {
		return nil, err
	}
	request, err := guestExecRequest(open)
	if err != nil {
		return nil, err
	}
	request.Streaming = open.Streaming
	guestControls := make(chan GuestExecControl, 256)
	controlCtx, stopControls := context.WithCancel(ctx)
	defer stopControls()
	go func() {
		defer close(guestControls)
		for {
			select {
			case control, open := <-controls:
				if !open {
					return
				}
				translated := GuestExecControl{Credit: control.Credit}
				if control.Input != nil {
					translated.Input = bytes.Clone(control.Input.Data)
					translated.EndOfInput = control.Input.EndOfInput
				}
				select {
				case guestControls <- translated:
				case <-controlCtx.Done():
					return
				}
			case <-controlCtx.Done():
				return
			}
		}
	}()
	result, err := session.ExecuteStreaming(
		ctx,
		fence.AssignmentId,
		request,
		guestControls,
		func(channel guestv1.ExecOutputChannel, data []byte) error {
			return emit(runnerExecOutputChannel(channel), data)
		},
	)
	if err != nil {
		return nil, err
	}
	return translateGuestExecResult(result).Terminal, nil
}

// ExecutePTY bridges one runner Terminal operation through the retained guest session.
func (b *AssignmentBackend) ExecutePTY(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
	controls <-chan runnercontrol.PTYControl,
	emit func([]byte) error,
) (*runnerprotocol.ExecTerminal, error) {
	session, err := b.guestSessionForFence(fence)
	if err != nil {
		return nil, err
	}
	request, err := guestExecRequest(open)
	if err != nil {
		return nil, err
	}
	guestControls := make(chan GuestPTYControl, 256)
	controlCtx, stopControls := context.WithCancel(ctx)
	defer stopControls()
	go func() {
		defer close(guestControls)
		for {
			select {
			case control, open := <-controls:
				if !open {
					return
				}
				translated := GuestPTYControl{
					Input: bytes.Clone(control.Input), Credit: control.Credit,
					Rows: control.Rows, Columns: control.Columns,
				}
				select {
				case guestControls <- translated:
				case <-controlCtx.Done():
					return
				}
			case <-controlCtx.Done():
				return
			}
		}
	}()
	result, err := session.ExecutePTY(
		ctx, fence.AssignmentId, request, guestControls, emit,
	)
	if err != nil {
		return nil, err
	}
	return translateGuestExecResult(BufferedGuestExecResult{
		Admission: result.Admission, Terminal: result.Terminal,
	}).Terminal, nil
}

// ExecuteBuffered translates one runner operation to the retained guest stream.
func (b *AssignmentBackend) ExecuteBuffered(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
) (runnercontrol.BufferedExecResult, error) {
	session, err := b.guestSessionForFence(fence)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	request, err := guestExecRequest(open)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	request.Streaming = false
	result, err := session.ExecuteBuffered(ctx, fence.AssignmentId, request)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	return translateGuestExecResult(result), nil
}

func guestExecRequest(open *runnerprotocol.ExecOpen) (*guestv1.ExecRequest, error) {
	if open == nil {
		return nil, fmt.Errorf("SecondBox Firecracker Exec Open is required")
	}
	request := &guestv1.ExecRequest{
		Cwd:              open.Cwd,
		DeadlineUnixMs:   open.DeadlineUnixMs,
		OutputLimitBytes: open.OutputLimitBytes,
		Stdin:            bytes.Clone(open.Stdin),
		Streaming:        open.Streaming,
	}
	if open.AllocatePty {
		if !open.Streaming ||
			open.PtyRows == 0 ||
			open.PtyRows > 65535 ||
			open.PtyColumns == 0 ||
			open.PtyColumns > 65535 {
			return nil, fmt.Errorf("SecondBox Firecracker Exec PTY dimensions or streaming mode is invalid")
		}
		request.Pty = &guestv1.PtyDimensions{
			Rows: open.PtyRows, Columns: open.PtyColumns,
		}
	} else if open.PtyRows != 0 || open.PtyColumns != 0 {
		return nil, fmt.Errorf("SecondBox Firecracker Exec PTY dimensions require PTY allocation")
	}
	switch command := open.Command.(type) {
	case *runnerprotocol.ExecOpen_Shell:
		request.Command = &guestv1.ExecRequest_Shell{Shell: command.Shell}
	case *runnerprotocol.ExecOpen_Argv:
		if command.Argv == nil {
			return nil, fmt.Errorf("SecondBox Firecracker Exec argv is required")
		}
		request.Command = &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{
			Argument: append([]string(nil), command.Argv.Argument...),
		}}
	default:
		return nil, fmt.Errorf("SecondBox Firecracker Exec command is required")
	}
	request.Environment = make([]*guestv1.EnvironmentEntry, 0, len(open.Environment))
	for _, entry := range open.Environment {
		if entry == nil {
			return nil, fmt.Errorf("SecondBox Firecracker Exec environment entry is nil")
		}
		request.Environment = append(request.Environment, &guestv1.EnvironmentEntry{
			Name:  entry.Name,
			Value: bytes.Clone(entry.Value),
		})
	}
	return request, nil
}

func translateGuestExecResult(result BufferedGuestExecResult) runnercontrol.BufferedExecResult {
	translated := runnercontrol.BufferedExecResult{
		Stdout: bytes.Clone(result.Stdout),
		Stderr: bytes.Clone(result.Stderr),
	}
	if result.Terminal != nil {
		translated.Terminal = &runnerprotocol.ExecTerminal{
			Kind:                runnerExecTerminalKind(result.Terminal.Kind),
			ExitCode:            result.Terminal.ExitCode,
			SafeDetail:          result.Terminal.SafeDetail,
			Signal:              result.Terminal.Signal,
			SpawnFailureReason:  runnerSpawnFailureReason(result.Terminal.SpawnFailureReason),
			ElapsedMilliseconds: result.Terminal.ElapsedMilliseconds,
			LimitBytes:          result.Terminal.LimitBytes,
			Message:             result.Terminal.Message,
		}
	} else if result.Admission != nil &&
		result.Admission.Kind != guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED {
		translated.Terminal = &runnerprotocol.ExecTerminal{
			Kind:               runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED,
			ExitCode:           -1,
			SafeDetail:         result.Admission.SafeDetail,
			SpawnFailureReason: runnerSpawnFailureReason(result.Admission.SpawnFailureReason),
			Message:            result.Admission.Message,
		}
	}
	return translated
}

// ExecuteFile translates the complete runner v1 filesystem subset to the guest.
func (b *AssignmentBackend) ExecuteFile(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.FileOpen,
	content []byte,
) (runnercontrol.FileOperationResult, error) {
	session, err := b.guestSessionForFence(fence)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	if open == nil {
		return runnercontrol.FileOperationResult{}, fmt.Errorf("SecondBox Firecracker File Open is required")
	}
	request := &guestv1.FileRequest{
		Operation:             guestFileOperation(open.Operation),
		WorkspaceRelativePath: open.WorkspaceRelativePath,
		ExpectedSize:          open.ExpectedSize,
		ExpectedChecksum:      open.ExpectedChecksum,
		Recursive:             open.Recursive,
		Force:                 open.Force,
	}
	if open.Operation == runnerprotocol.FileOperation_FILE_OPERATION_WRITE {
		request.CreateMode = runnerFileCreateMode
	}
	result, err := session.ExecuteFileOperation(ctx, fence.AssignmentId, request, content)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	translated := runnercontrol.FileOperationResult{Content: bytes.Clone(result.Content)}
	if result.Metadata != nil {
		translated.Metadata = &runnerprotocol.FileMetadata{
			Exists:           result.Metadata.Exists,
			Size:             result.Metadata.Size,
			Mode:             result.Metadata.Mode,
			Checksum:         result.Metadata.Checksum,
			DirectChildren:   append([]string(nil), result.Metadata.DirectChildren...),
			Kind:             runnerFileKind(result.Metadata.Kind),
			ModifiedAtUnixMs: result.Metadata.ModifiedAtUnixMs,
		}
		translated.Metadata.DirectChildEntries = make(
			[]*runnerprotocol.FileMetadataEntry, 0, len(result.Metadata.DirectChildEntries),
		)
		for _, entry := range result.Metadata.DirectChildEntries {
			if entry == nil {
				continue
			}
			translated.Metadata.DirectChildEntries = append(
				translated.Metadata.DirectChildEntries,
				&runnerprotocol.FileMetadataEntry{
					Path: entry.Path, Kind: runnerFileKind(entry.Kind),
					Size: entry.Size, ModifiedAtUnixMs: entry.ModifiedAtUnixMs,
				},
			)
		}
	}
	if result.Terminal != nil {
		translated.Terminal = &runnerprotocol.FileTerminal{
			Kind:       runnerFileTerminalKind(result.Terminal.Kind),
			SafeDetail: result.Terminal.SafeDetail,
		}
	}
	return translated, nil
}

func (b *AssignmentBackend) guestSessionForFence(
	fence *runnerprotocol.AssignmentFence,
) (*GuestProtocolSession, error) {
	if fence == nil {
		return nil, fmt.Errorf("SecondBox Firecracker operation fence is required")
	}
	b.mu.Lock()
	active, exists := b.assignments[fence.AssignmentId]
	b.mu.Unlock()
	if !exists || !sameAssignmentFence(active.fence, fence) {
		return nil, fmt.Errorf("SecondBox Firecracker operation fence is stale")
	}
	instance := b.manager.lookup(active.backendReference)
	if instance == nil ||
		instance.guestProtocolSession == nil ||
		instance.guestProtocolSession.Binding == nil ||
		instance.guestProtocolSession.Binding.InstanceId != fence.InstanceId ||
		instance.guestProtocolSession.Binding.SandboxId != fence.SandboxId ||
		instance.guestProtocolSession.Binding.SandboxGeneration != fence.SandboxGeneration {
		return nil, fmt.Errorf("SecondBox Firecracker retained guest protocol session is unavailable")
	}
	return instance.guestProtocolSession, nil
}

func runnerExecTerminalKind(kind guestv1.ExecTerminalKind) runnerprotocol.ExecTerminalKind {
	switch kind {
	case guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED:
		return runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED
	case guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED:
		return runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED
	case guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED:
		return runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED
	case guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED:
		return runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED
	case guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED:
		return runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED
	default:
		return runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_GUEST_AGENT_FAILED
	}
}

func runnerExecOutputChannel(channel guestv1.ExecOutputChannel) runnerprotocol.ExecOutputChannel {
	switch channel {
	case guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT:
		return runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT
	case guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR:
		return runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR
	default:
		return runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_UNSPECIFIED
	}
}

func runnerSpawnFailureReason(reason guestv1.SpawnFailureReason) runnerprotocol.SpawnFailureReason {
	switch reason {
	case guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND:
		return runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND
	case guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_PERMISSION_DENIED:
		return runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_PERMISSION_DENIED
	case guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD:
		return runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD
	case guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE:
		return runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE
	default:
		return runnerprotocol.SpawnFailureReason_SPAWN_FAILURE_REASON_UNSPECIFIED
	}
}

func guestFileOperation(operation runnerprotocol.FileOperation) guestv1.FileOperation {
	switch operation {
	case runnerprotocol.FileOperation_FILE_OPERATION_READ:
		return guestv1.FileOperation_FILE_OPERATION_READ
	case runnerprotocol.FileOperation_FILE_OPERATION_WRITE:
		return guestv1.FileOperation_FILE_OPERATION_WRITE
	case runnerprotocol.FileOperation_FILE_OPERATION_STAT:
		return guestv1.FileOperation_FILE_OPERATION_STAT
	case runnerprotocol.FileOperation_FILE_OPERATION_LIST:
		return guestv1.FileOperation_FILE_OPERATION_LIST_DIRECT_CHILDREN
	case runnerprotocol.FileOperation_FILE_OPERATION_EXISTS:
		return guestv1.FileOperation_FILE_OPERATION_EXISTS
	case runnerprotocol.FileOperation_FILE_OPERATION_MKDIR:
		return guestv1.FileOperation_FILE_OPERATION_MKDIR
	case runnerprotocol.FileOperation_FILE_OPERATION_REMOVE:
		return guestv1.FileOperation_FILE_OPERATION_REMOVE
	default:
		return guestv1.FileOperation_FILE_OPERATION_UNSPECIFIED
	}
}

func runnerFileKind(kind guestv1.FileKind) runnerprotocol.FileKind {
	switch kind {
	case guestv1.FileKind_FILE_KIND_FILE:
		return runnerprotocol.FileKind_FILE_KIND_FILE
	case guestv1.FileKind_FILE_KIND_DIRECTORY:
		return runnerprotocol.FileKind_FILE_KIND_DIRECTORY
	case guestv1.FileKind_FILE_KIND_SYMBOLIC_LINK:
		return runnerprotocol.FileKind_FILE_KIND_SYMBOLIC_LINK
	default:
		return runnerprotocol.FileKind_FILE_KIND_UNSPECIFIED
	}
}

func runnerFileTerminalKind(kind guestv1.FileTerminalKind) runnerprotocol.FileTerminalKind {
	switch kind {
	case guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED
	case guestv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND
	case guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH,
		guestv1.FileTerminalKind_FILE_TERMINAL_KIND_SYMLINK_REJECTED:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH
	case guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED
	case guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH
	case guestv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED
	case guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED
	default:
		return runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED
	}
}

var _ runnercontrol.DataPlaneBackend = (*AssignmentBackend)(nil)
