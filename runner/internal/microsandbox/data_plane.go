package microsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	microsandboxprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const microsandboxFileCreateMode uint32 = 0o600

func (backend *AssignmentBackend) ExecuteBuffered(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
) (runnercontrol.BufferedExecResult, error) {
	active, opCtx, release, err := backend.acquireOperation(ctx, fence)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	defer release()
	request, err := microsandboxExecRequest(open, false)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	result := runnercontrol.BufferedExecResult{}
	terminal, err := active.process.execOperation(opCtx, request, nil, func(data *microsandboxprotocol.StreamData) error {
		if data.Channel == microsandboxprotocol.StreamChannel_STREAM_CHANNEL_STDERR {
			result.Stderr = append(result.Stderr, data.Data...)
		} else {
			result.Stdout = append(result.Stdout, data.Data...)
		}
		return nil
	})
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	result.Terminal = helperExecTerminal(terminal)
	if !backend.operationFenceActive(active, fence) {
		return runnercontrol.BufferedExecResult{}, staleOperationError()
	}
	return result, nil
}

func (backend *AssignmentBackend) ExecuteStreaming(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
	controls <-chan runnercontrol.ExecControl,
	emit func(runnerprotocol.ExecOutputChannel, []byte) error,
) (*runnerprotocol.ExecTerminal, error) {
	active, opCtx, release, err := backend.acquireOperation(ctx, fence)
	if err != nil {
		return nil, err
	}
	defer release()
	request, err := microsandboxExecRequest(open, false)
	if err != nil {
		return nil, err
	}
	helperControls := make(chan helperExecControl, 256)
	bridgeDone := make(chan struct{})
	credit := newMicrosandboxOutputCredit()
	go bridgeExecControls(opCtx, controls, helperControls, credit, bridgeDone)
	terminal, err := active.process.execOperation(opCtx, request, helperControls, func(data *microsandboxprotocol.StreamData) error {
		channel := runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT
		if data.Channel == microsandboxprotocol.StreamChannel_STREAM_CHANNEL_STDERR {
			channel = runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR
		}
		return emitExecWithCredit(opCtx, credit, channel, data.Data, emit)
	})
	close(bridgeDone)
	if err != nil {
		return nil, err
	}
	if !backend.operationFenceActive(active, fence) {
		return nil, staleOperationError()
	}
	return helperExecTerminal(terminal), nil
}

func (backend *AssignmentBackend) ExecutePTY(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
	controls <-chan runnercontrol.PTYControl,
	emit func([]byte) error,
) (*runnerprotocol.ExecTerminal, error) {
	active, opCtx, release, err := backend.acquireOperation(ctx, fence)
	if err != nil {
		return nil, err
	}
	defer release()
	request, err := microsandboxExecRequest(open, true)
	if err != nil {
		return nil, err
	}
	helperControls := make(chan helperExecControl, 256)
	bridgeDone := make(chan struct{})
	credit := newMicrosandboxOutputCredit()
	go bridgePTYControls(opCtx, controls, helperControls, credit, bridgeDone)
	terminal, err := active.process.execOperation(opCtx, request, helperControls, func(data *microsandboxprotocol.StreamData) error {
		return emitPTYWithCredit(opCtx, credit, data.Data, emit)
	})
	close(bridgeDone)
	if err != nil {
		return nil, err
	}
	if !backend.operationFenceActive(active, fence) {
		return nil, staleOperationError()
	}
	return helperExecTerminal(terminal), nil
}

func (backend *AssignmentBackend) ExecuteFile(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.FileOpen,
	content []byte,
) (runnercontrol.FileOperationResult, error) {
	active, opCtx, release, err := backend.acquireOperation(ctx, fence)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	defer release()
	request, err := microsandboxFileRequest(open, content)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	events, err := active.process.fileOperation(opCtx, request, content)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	result, err := translateFileEvents(events)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	if !backend.operationFenceActive(active, fence) {
		return runnercontrol.FileOperationResult{}, staleOperationError()
	}
	return finalizeFileResult(open, result), nil
}

// finalizeFileResult bounds a read and attaches computed metadata. Computed
// metadata states a fact about delivered content; a read that did not
// complete delivered none, so it keeps whatever the helper reported instead
// of a synthesized existing file.
func finalizeFileResult(open *runnerprotocol.FileOpen, result runnercontrol.FileOperationResult) runnercontrol.FileOperationResult {
	if open.Operation != runnerprotocol.FileOperation_FILE_OPERATION_READ {
		return result
	}
	if uint64(len(result.Content)) > open.ExpectedSize {
		result.Content = nil
		result.Terminal = &runnerprotocol.FileTerminal{Kind: runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED, SafeDetail: "file read limit exceeded"}
		return result
	}
	if result.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		return result
	}
	digest := sha256.Sum256(result.Content)
	if result.Metadata == nil {
		result.Metadata = &runnerprotocol.FileMetadata{Exists: true, Kind: runnerprotocol.FileKind_FILE_KIND_FILE}
	}
	result.Metadata.Size = uint64(len(result.Content))
	result.Metadata.Checksum = "sha256:" + hex.EncodeToString(digest[:])
	return result
}

func microsandboxExecRequest(open *runnerprotocol.ExecOpen, requirePTY bool) (*microsandboxprotocol.ExecRequest, error) {
	if open == nil {
		return nil, fmt.Errorf("SecondBox Microsandbox Exec Open is required")
	}
	request := &microsandboxprotocol.ExecRequest{
		WorkingDirectory: open.Cwd, DeadlineUnixMs: open.DeadlineUnixMs,
		Pty: open.AllocatePty, Rows: open.PtyRows, Columns: open.PtyColumns,
		Stdin: bytes.Clone(open.Stdin), OutputLimitBytes: open.OutputLimitBytes, Streaming: open.Streaming,
	}
	if requirePTY && (!open.AllocatePty || !open.Streaming || open.PtyRows == 0 || open.PtyColumns == 0 || open.PtyRows > 65535 || open.PtyColumns > 65535) {
		return nil, fmt.Errorf("SecondBox Microsandbox PTY dimensions or streaming mode is invalid")
	}
	switch command := open.Command.(type) {
	case *runnerprotocol.ExecOpen_Shell:
		request.Argv = []string{"/bin/sh", "-c", command.Shell}
	case *runnerprotocol.ExecOpen_Argv:
		if command.Argv == nil || len(command.Argv.Argument) == 0 {
			return nil, fmt.Errorf("SecondBox Microsandbox Exec argv is required")
		}
		request.Argv = append([]string(nil), command.Argv.Argument...)
	default:
		return nil, fmt.Errorf("SecondBox Microsandbox Exec command is required")
	}
	for _, entry := range open.Environment {
		if entry == nil || strings.Contains(entry.Name, "=") || bytes.IndexByte(entry.Value, 0) >= 0 {
			return nil, fmt.Errorf("SecondBox Microsandbox Exec environment entry is invalid")
		}
		request.Environment = append(request.Environment, entry.Name+"="+string(entry.Value))
	}
	return request, nil
}

func microsandboxFileRequest(open *runnerprotocol.FileOpen, content []byte) (*microsandboxprotocol.FileRequest, error) {
	if open == nil {
		return nil, fmt.Errorf("SecondBox Microsandbox File Open is required")
	}
	request := &microsandboxprotocol.FileRequest{Path: open.WorkspaceRelativePath, Recursive: open.Recursive, Force: open.Force, Content: bytes.Clone(content), Mode: microsandboxFileCreateMode}
	switch open.Operation {
	case runnerprotocol.FileOperation_FILE_OPERATION_READ:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_READ
		request.Limit = open.ExpectedSize
		if request.Limit != ^uint64(0) {
			request.Limit++
		}
	case runnerprotocol.FileOperation_FILE_OPERATION_WRITE:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_WRITE
		request.Limit = open.ExpectedSize
		request.Content = nil
		if uint64(len(content)) != open.ExpectedSize {
			return nil, fmt.Errorf("SecondBox Microsandbox File content differs from expected size")
		}
		digest := sha256.Sum256(content)
		if open.ExpectedChecksum != "sha256:"+hex.EncodeToString(digest[:]) {
			return nil, fmt.Errorf("SecondBox Microsandbox File content differs from expected checksum")
		}
	case runnerprotocol.FileOperation_FILE_OPERATION_STAT:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_STAT
	case runnerprotocol.FileOperation_FILE_OPERATION_EXISTS:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_EXISTS
	case runnerprotocol.FileOperation_FILE_OPERATION_LIST:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_LIST
	case runnerprotocol.FileOperation_FILE_OPERATION_MKDIR:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_MKDIR
		request.Mode = 0o755
	case runnerprotocol.FileOperation_FILE_OPERATION_REMOVE:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_REMOVE
	default:
		return nil, fmt.Errorf("SecondBox Microsandbox File operation is unsupported")
	}
	return request, nil
}

func helperExecTerminal(value *microsandboxprotocol.TerminalEvent) *runnerprotocol.ExecTerminal {
	kind := runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED
	if value.Reason == "spawn-failed" {
		kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED
	}
	switch value.Reason {
	case "deadline-exceeded":
		kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED
	case "cancelled":
		kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED
	case "output-exhausted":
		kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED
	case "exited", "spawn-failed":
	default:
		kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED
	}
	return &runnerprotocol.ExecTerminal{Kind: kind, ExitCode: value.ExitCode, Signal: value.Signal, SpawnFailureReason: runnerprotocol.SpawnFailureReason(value.SpawnFailureReason), InfrastructureFailureReason: runnerprotocol.InfrastructureFailureReason(value.InfrastructureFailureReason), Retryable: value.Retryable, ElapsedMilliseconds: value.ElapsedMilliseconds, LimitBytes: value.LimitBytes, SafeDetail: value.Reason}
}

func translateFileEvents(events []*microsandboxprotocol.Envelope) (runnercontrol.FileOperationResult, error) {
	result := runnercontrol.FileOperationResult{}
	for _, event := range events {
		if data := event.GetStreamData(); data != nil {
			if data.Channel != microsandboxprotocol.StreamChannel_STREAM_CHANNEL_FILE {
				return runnercontrol.FileOperationResult{}, fmt.Errorf("SecondBox Microsandbox File returned an unexpected stream channel")
			}
			result.Content = append(result.Content, data.Data...)
		}
		if metadata := event.GetFileMetadata(); metadata != nil {
			result.Metadata = &runnerprotocol.FileMetadata{Exists: metadata.Exists, Size: metadata.Size, Mode: metadata.Mode, Checksum: metadata.Checksum, DirectChildren: append([]string(nil), metadata.DirectChildren...), Kind: runnerprotocol.FileKind(metadata.Kind), ModifiedAtUnixMs: metadata.ModifiedAtUnixMs}
			for _, entry := range metadata.DirectChildEntries {
				result.Metadata.DirectChildEntries = append(result.Metadata.DirectChildEntries, &runnerprotocol.FileMetadataEntry{Path: entry.Path, Kind: runnerprotocol.FileKind(entry.Kind), Size: entry.Size, ModifiedAtUnixMs: entry.ModifiedAtUnixMs})
			}
		}
		if terminal := event.GetTerminal(); terminal != nil {
			kind := runnerprotocol.FileTerminalKind(terminal.FileTerminalKind)
			switch kind {
			case runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FENCED,
				runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED:
			default:
				return runnercontrol.FileOperationResult{}, fmt.Errorf("SecondBox Microsandbox File returned unknown terminal kind %d", terminal.FileTerminalKind)
			}
			result.Terminal = &runnerprotocol.FileTerminal{Kind: kind, SafeDetail: terminal.Reason}
		}
	}
	if result.Terminal == nil {
		return result, fmt.Errorf("SecondBox Microsandbox File completed without terminal")
	}
	return result, nil
}

func bridgeExecControls(ctx context.Context, controls <-chan runnercontrol.ExecControl, output chan<- helperExecControl, credit *microsandboxOutputCredit, done <-chan struct{}) {
	defer close(output)
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case control, ok := <-controls:
			if !ok {
				return
			}
			if control.Credit != 0 {
				credit.add(control.Credit)
			}
			if control.Input != nil {
				select {
				case output <- helperExecControl{data: bytes.Clone(control.Input.Data), eof: control.Input.EndOfInput}:
				case <-ctx.Done():
					return
				case <-done:
					return
				}
			}
		}
	}
}
func bridgePTYControls(ctx context.Context, controls <-chan runnercontrol.PTYControl, output chan<- helperExecControl, credit *microsandboxOutputCredit, done <-chan struct{}) {
	defer close(output)
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case control, ok := <-controls:
			if !ok {
				return
			}
			if control.Credit != 0 {
				credit.add(control.Credit)
			}
			if len(control.Input) == 0 && control.Rows == 0 && control.Columns == 0 {
				continue
			}
			translated := helperExecControl{data: bytes.Clone(control.Input), rows: control.Rows, columns: control.Columns}
			select {
			case output <- translated:
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}
}

type microsandboxOutputCredit struct {
	mu        sync.Mutex
	available uint64
	wake      chan struct{}
}

func newMicrosandboxOutputCredit() *microsandboxOutputCredit {
	return &microsandboxOutputCredit{wake: make(chan struct{}, 1)}
}

func (credit *microsandboxOutputCredit) add(value uint64) {
	credit.mu.Lock()
	if ^uint64(0)-credit.available < value {
		credit.available = ^uint64(0)
	} else {
		credit.available += value
	}
	credit.mu.Unlock()
	select {
	case credit.wake <- struct{}{}:
	default:
	}
}

func (credit *microsandboxOutputCredit) take(ctx context.Context, maximum uint64) (uint64, error) {
	for {
		credit.mu.Lock()
		if credit.available != 0 {
			value := min(credit.available, maximum)
			credit.available -= value
			credit.mu.Unlock()
			return value, nil
		}
		credit.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-credit.wake:
		}
	}
}

func emitExecWithCredit(ctx context.Context, credit *microsandboxOutputCredit, channel runnerprotocol.ExecOutputChannel, data []byte, emit func(runnerprotocol.ExecOutputChannel, []byte) error) error {
	for len(data) != 0 {
		allowed, err := credit.take(ctx, uint64(len(data)))
		if err != nil {
			return err
		}
		if err := emit(channel, data[:allowed]); err != nil {
			return err
		}
		data = data[allowed:]
	}
	return nil
}

func emitPTYWithCredit(ctx context.Context, credit *microsandboxOutputCredit, data []byte, emit func([]byte) error) error {
	for len(data) != 0 {
		allowed, err := credit.take(ctx, uint64(len(data)))
		if err != nil {
			return err
		}
		if err := emit(data[:allowed]); err != nil {
			return err
		}
		data = data[allowed:]
	}
	return nil
}

func staleOperationError() error {
	return errors.New("SecondBox Microsandbox operation was fenced before terminal publication")
}

var _ runnercontrol.DataPlaneBackend = (*AssignmentBackend)(nil)
var _ runnercontrol.PTYDataPlaneBackend = (*AssignmentBackend)(nil)
