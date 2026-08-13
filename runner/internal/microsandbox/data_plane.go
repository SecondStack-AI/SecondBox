package microsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

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
	events, err := active.process.request(opCtx, &microsandboxprotocol.Envelope_Exec{Exec: request})
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	result, err := translateExecEvents(events, open.OutputLimitBytes, nil)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
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
	// Fixed Open stdin is sent atomically. Subsequent controls are drained while
	// the helper operation runs; the helper protocol rejects unsupported signal
	// values rather than silently dropping them.
	done := make(chan struct{})
	defer close(done)
	go drainExecControls(opCtx, controls, done)
	events, err := active.process.request(opCtx, &microsandboxprotocol.Envelope_Exec{Exec: request})
	if err != nil {
		return nil, err
	}
	result, err := translateExecEvents(events, open.OutputLimitBytes, emit)
	if err != nil {
		return nil, err
	}
	if !backend.operationFenceActive(active, fence) {
		return nil, staleOperationError()
	}
	return result.Terminal, nil
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
	done := make(chan struct{})
	defer close(done)
	go drainPTYControls(opCtx, controls, done)
	events, err := active.process.request(opCtx, &microsandboxprotocol.Envelope_Exec{Exec: request})
	if err != nil {
		return nil, err
	}
	result, err := translateExecEvents(events, open.OutputLimitBytes, func(_ runnerprotocol.ExecOutputChannel, data []byte) error { return emit(data) })
	if err != nil {
		return nil, err
	}
	if !backend.operationFenceActive(active, fence) {
		return nil, staleOperationError()
	}
	return result.Terminal, nil
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
	events, err := active.process.request(opCtx, &microsandboxprotocol.Envelope_File{File: request})
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
	return result, nil
}

func microsandboxExecRequest(open *runnerprotocol.ExecOpen, requirePTY bool) (*microsandboxprotocol.ExecRequest, error) {
	if open == nil {
		return nil, fmt.Errorf("SecondBox Microsandbox Exec Open is required")
	}
	request := &microsandboxprotocol.ExecRequest{
		WorkingDirectory: open.Cwd, DeadlineUnixMs: open.DeadlineUnixMs,
		Pty: open.AllocatePty, Rows: open.PtyRows, Columns: open.PtyColumns,
		Stdin: bytes.Clone(open.Stdin), OutputLimitBytes: open.OutputLimitBytes,
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
	request := &microsandboxprotocol.FileRequest{Path: open.WorkspaceRelativePath, Recursive: open.Recursive, Content: bytes.Clone(content), Mode: microsandboxFileCreateMode}
	switch open.Operation {
	case runnerprotocol.FileOperation_FILE_OPERATION_READ:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_READ
	case runnerprotocol.FileOperation_FILE_OPERATION_WRITE:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_WRITE
		if uint64(len(content)) != open.ExpectedSize {
			return nil, fmt.Errorf("SecondBox Microsandbox File content differs from expected size")
		}
	case runnerprotocol.FileOperation_FILE_OPERATION_STAT, runnerprotocol.FileOperation_FILE_OPERATION_EXISTS:
		request.Operation = microsandboxprotocol.Operation_OPERATION_FILE_STAT
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

func translateExecEvents(events []*microsandboxprotocol.Envelope, limit uint64, emit func(runnerprotocol.ExecOutputChannel, []byte) error) (runnercontrol.BufferedExecResult, error) {
	result := runnercontrol.BufferedExecResult{}
	var used uint64
	for _, event := range events {
		if data := event.GetStreamData(); data != nil {
			if uint64(len(data.Data)) > limit-used {
				result.Terminal = &runnerprotocol.ExecTerminal{Kind: runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED, ExitCode: -1, LimitBytes: limit, SafeDetail: "output limit exhausted"}
				return result, nil
			}
			used += uint64(len(data.Data))
			channel := runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT
			if data.Channel == microsandboxprotocol.StreamChannel_STREAM_CHANNEL_STDERR {
				channel = runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR
			}
			if emit != nil {
				if err := emit(channel, data.Data); err != nil {
					return result, err
				}
			} else if channel == runnerprotocol.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR {
				result.Stderr = append(result.Stderr, data.Data...)
			} else {
				result.Stdout = append(result.Stdout, data.Data...)
			}
		}
		if terminal := event.GetTerminal(); terminal != nil {
			result.Terminal = helperExecTerminal(terminal)
		}
	}
	if result.Terminal == nil {
		return result, fmt.Errorf("SecondBox Microsandbox Exec completed without terminal")
	}
	return result, nil
}

func helperExecTerminal(value *microsandboxprotocol.TerminalEvent) *runnerprotocol.ExecTerminal {
	kind := runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED
	if value.Reason == "spawn-failed" {
		kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED
	}
	if value.Reason != "exited" && value.Reason != "spawn-failed" {
		kind = runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_INFRASTRUCTURE_FAILED
	}
	return &runnerprotocol.ExecTerminal{Kind: kind, ExitCode: value.ExitCode, Signal: value.Signal, SpawnFailureReason: runnerprotocol.SpawnFailureReason(value.SpawnFailureReason), InfrastructureFailureReason: runnerprotocol.InfrastructureFailureReason(value.InfrastructureFailureReason), Retryable: value.Retryable, ElapsedMilliseconds: value.ElapsedMilliseconds, LimitBytes: value.LimitBytes, SafeDetail: value.Reason}
}

func translateFileEvents(events []*microsandboxprotocol.Envelope) (runnercontrol.FileOperationResult, error) {
	result := runnercontrol.FileOperationResult{}
	for _, event := range events {
		if data := event.GetStreamData(); data != nil {
			result.Content = append(result.Content, data.Data...)
		}
		if metadata := event.GetFileMetadata(); metadata != nil {
			result.Metadata = &runnerprotocol.FileMetadata{Exists: metadata.Exists, Size: metadata.Size, Mode: metadata.Mode, Checksum: metadata.Checksum, DirectChildren: append([]string(nil), metadata.DirectChildren...), Kind: runnerprotocol.FileKind(metadata.Kind), ModifiedAtUnixMs: metadata.ModifiedAtUnixMs}
			for _, entry := range metadata.DirectChildEntries {
				result.Metadata.DirectChildEntries = append(result.Metadata.DirectChildEntries, &runnerprotocol.FileMetadataEntry{Path: entry.Path, Kind: runnerprotocol.FileKind(entry.Kind), Size: entry.Size, ModifiedAtUnixMs: entry.ModifiedAtUnixMs})
			}
		}
		if terminal := event.GetTerminal(); terminal != nil {
			kind := runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED
			if !terminal.Success {
				kind = runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_FAILED
				if strings.Contains(strings.ToLower(terminal.Reason), "not found") || strings.Contains(terminal.Reason, "ENOENT") {
					kind = runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND
				}
			}
			result.Terminal = &runnerprotocol.FileTerminal{Kind: kind, SafeDetail: terminal.Reason}
		}
	}
	if result.Terminal == nil {
		return result, fmt.Errorf("SecondBox Microsandbox File completed without terminal")
	}
	return result, nil
}

func drainExecControls(ctx context.Context, controls <-chan runnercontrol.ExecControl, done <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case _, ok := <-controls:
			if !ok {
				return
			}
		}
	}
}
func drainPTYControls(ctx context.Context, controls <-chan runnercontrol.PTYControl, done <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case _, ok := <-controls:
			if !ok {
				return
			}
		}
	}
}
func staleOperationError() error {
	return errors.New("SecondBox Microsandbox operation was fenced before terminal publication")
}

var _ runnercontrol.DataPlaneBackend = (*AssignmentBackend)(nil)
var _ runnercontrol.PTYDataPlaneBackend = (*AssignmentBackend)(nil)
