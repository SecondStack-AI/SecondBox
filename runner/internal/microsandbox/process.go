package microsandbox

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	microsandboxprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/microsandboxprotocol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

const helperStderrLimit = 16 << 10
const helperFileChunkBytes = 64 << 10

type helperProcess struct {
	command         *exec.Cmd
	control         net.Conn
	lifecycle       *os.File
	workspace       workspacestore.ComputeAttachment
	done            chan struct{}
	waitMu          sync.Mutex
	waitErr         error
	closeOnce       sync.Once
	closeErr        error
	stderr          *boundedBuffer
	requestMu       sync.Mutex
	nextRequestID   uint64
	materialization string
}

type helperExecControl struct {
	data    []byte
	eof     bool
	rows    uint32
	columns uint32
}

func launchHelper(
	ctx context.Context,
	config validatedConfig,
	assignment *runnerprotocol.AssignmentCommand,
	workspace workspacestore.ComputeAttachment,
) (*helperProcess, *microsandboxprotocol.ReadyEvent, error) {
	descriptors, err := platformSocketpair()
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox create helper socketpair: %w", err)
	}
	parentSocket := os.NewFile(uintptr(descriptors[0]), "secondbox-helper-parent")
	childSocket := os.NewFile(uintptr(descriptors[1]), "secondbox-helper-child")
	if parentSocket == nil || childSocket == nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox adopt helper socketpair")
	}
	defer parentSocket.Close()
	defer childSocket.Close()
	connection, err := net.FileConn(parentSocket)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox adopt helper control connection: %w", err)
	}
	cleanupConnection := true
	defer func() {
		if cleanupConnection {
			_ = connection.Close()
		}
	}()
	lifecycleRead, lifecycleWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox create lifecycle pipe: %w", err)
	}
	defer lifecycleRead.Close()
	cleanupLifecycle := true
	defer func() {
		if cleanupLifecycle {
			_ = lifecycleWrite.Close()
		}
	}()
	diagnosticRead, diagnosticWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox create diagnostic pipe: %w", err)
	}
	defer diagnosticRead.Close()
	defer diagnosticWrite.Close()

	stderr := &boundedBuffer{limit: helperStderrLimit}
	writerLock := workspace.LockDescriptor()
	if writerLock == nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox helper requires the Workspace writer-lock descriptor")
	}
	command := exec.Command(config.HelperExecutable, "serve")
	command.Env = []string{}
	// The writer-lock duplicate shares the runner's open file description, so
	// the exclusive Workspace lock outlives a crashed runner until the helper
	// finishes its final flush and exits.
	command.ExtraFiles = []*os.File{childSocket, workspace.Descriptor(), lifecycleRead, diagnosticWrite, writerLock}
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = stderr
	configureHelperProcess(command)
	if err := command.Start(); err != nil {
		return nil, nil, fmt.Errorf("SecondBox Microsandbox start helper: %w", err)
	}
	// Drop the runner's copies of the child-only descriptors immediately so a
	// pre-ready helper exit is observable as EOF instead of waiting for the
	// assignment deadline.
	if err := childSocket.Close(); err != nil {
		_ = command.Process.Kill()
		return nil, nil, fmt.Errorf("SecondBox Microsandbox close child control descriptor: %w", err)
	}
	if err := lifecycleRead.Close(); err != nil {
		_ = command.Process.Kill()
		return nil, nil, fmt.Errorf("SecondBox Microsandbox close child lifecycle descriptor: %w", err)
	}
	if err := diagnosticWrite.Close(); err != nil {
		_ = command.Process.Kill()
		return nil, nil, fmt.Errorf("SecondBox Microsandbox close child diagnostic descriptor: %w", err)
	}
	go func() { _, _ = io.Copy(stderr, diagnosticRead) }()
	process := &helperProcess{
		command:         command,
		control:         connection,
		lifecycle:       lifecycleWrite,
		workspace:       workspace,
		done:            make(chan struct{}),
		stderr:          stderr,
		nextRequestID:   1,
		materialization: config.MaterializationDigest,
	}
	go func() {
		process.waitMu.Lock()
		process.waitErr = command.Wait()
		process.waitMu.Unlock()
		close(process.done)
	}()

	deadline := time.UnixMilli(int64(assignment.DeadlineUnixMs))
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		process.forceStop()
		return nil, nil, fmt.Errorf("SecondBox Microsandbox set helper startup deadline: %w", err)
	}
	start, err := helperStartRequest(config, assignment, workspace)
	if err != nil {
		process.forceStop()
		return nil, nil, err
	}
	if err := microsandboxprotocol.WriteFrame(connection, &microsandboxprotocol.Envelope{
		ProtocolVersion: microsandboxprotocol.Version,
		RequestId:       1,
		Message:         &microsandboxprotocol.Envelope_Start{Start: start},
	}); err != nil {
		process.forceStop()
		return nil, nil, err
	}
	response, err := microsandboxprotocol.ReadFrame(connection)
	if err != nil {
		process.forceStop()
		return nil, nil, fmt.Errorf(
			"SecondBox Microsandbox wait for helper readiness: %w: process=%v stderr=%s",
			err, process.processWaitError(), stderr.String(),
		)
	}
	ready := response.GetReady()
	if response.RequestId != 1 || ready == nil ||
		ready.MaterializationDigest != config.MaterializationDigest ||
		ready.HelperVersion == "" || ready.DependencyVersion == "" ||
		ready.AgentProtocolGeneration != config.manifest.AgentProtocolGeneration ||
		!containsAll(ready.AgentFeatures, "agent-relay", "network-policy", "network-smoltcp") {
		process.forceStop()
		return nil, nil, fmt.Errorf("SecondBox Microsandbox helper readiness identity mismatch")
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		process.forceStop()
		return nil, nil, fmt.Errorf("SecondBox Microsandbox clear helper startup deadline: %w", err)
	}
	process.nextRequestID = 2
	cleanupConnection = false
	cleanupLifecycle = false
	return process, ready, nil
}

func containsAll(values []string, required ...string) bool {
	for _, requirement := range required {
		if !slices.Contains(values, requirement) {
			return false
		}
	}
	return true
}

func helperStartRequest(
	config validatedConfig,
	assignment *runnerprotocol.AssignmentCommand,
	workspace workspacestore.ComputeAttachment,
) (*microsandboxprotocol.StartRequest, error) {
	uuid, err := decodeWorkspaceUUID(workspace.FilesystemUUID())
	if err != nil {
		return nil, err
	}
	networkPolicy, err := translateNetworkPolicy(assignment.NetworkPolicy)
	if err != nil {
		return nil, err
	}
	return &microsandboxprotocol.StartRequest{
		MaterializationDigest:  config.MaterializationDigest,
		GuestArchitecture:      assignment.Requirements.Architecture,
		VcpuCount:              assignment.Requirements.VcpuCount,
		MemoryBytes:            assignment.Requirements.MemoryBytes,
		FlatRootDigest:         config.manifest.FlatRootDigest,
		NetworkPolicy:          networkPolicy,
		StableWorkspaceBlockId: workspace.StableBlockID(),
		WorkspaceCapacityBytes: uint64(workspace.CapacityBytes()),
		WorkspaceUuid:          uuid,
		FlatRootPath:           config.FlatRootPath,
		LibkrunfwPath:          config.LibkrunfwPath,
		AgentdPath:             config.AgentdPath,
	}, nil
}

func (process *helperProcess) shutdown(ctx context.Context) error {
	process.requestMu.Lock()
	requestID := process.nextRequestID
	process.nextRequestID++
	deadline := time.Now().Add(10 * time.Second)
	if value, ok := ctx.Deadline(); ok {
		deadline = value
	}
	err := process.control.SetDeadline(deadline)
	if err == nil {
		err = microsandboxprotocol.WriteFrame(process.control, &microsandboxprotocol.Envelope{
			ProtocolVersion: microsandboxprotocol.Version,
			RequestId:       requestID,
			Message: &microsandboxprotocol.Envelope_Shutdown{Shutdown: &microsandboxprotocol.ShutdownRequest{
				FlushDeadlineUnixMs: uint64(deadline.UnixMilli()),
			}},
		})
	}
	if err == nil {
		var response *microsandboxprotocol.Envelope
		response, err = microsandboxprotocol.ReadFrame(process.control)
		if err == nil && (response.RequestId != requestID || response.GetTerminal() == nil) {
			err = fmt.Errorf("SecondBox Microsandbox helper returned invalid shutdown terminal")
		}
	}
	process.requestMu.Unlock()

	select {
	case <-process.done:
		return errors.Join(err, normalizeProcessExit(process.processWaitError()), process.closeResources())
	case <-ctx.Done():
		killErr := process.command.Process.Kill()
		<-process.done
		return errors.Join(err, ctx.Err(), killErr, normalizeKilledExit(process.processWaitError()), process.closeResources())
	}
}

func (process *helperProcess) execOperation(
	ctx context.Context,
	request *microsandboxprotocol.ExecRequest,
	controls <-chan helperExecControl,
	emit func(*microsandboxprotocol.StreamData) error,
) (*microsandboxprotocol.TerminalEvent, error) {
	process.requestMu.Lock()
	defer process.requestMu.Unlock()
	requestID := process.nextRequestID
	process.nextRequestID++
	if err := process.control.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := microsandboxprotocol.WriteFrame(process.control, &microsandboxprotocol.Envelope{
		ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID,
		Message: &microsandboxprotocol.Envelope_Exec{Exec: request},
	}); err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox write helper Exec: %w", err)
	}
	writesDone := make(chan struct{})
	stopWrites := make(chan struct{})
	cancelRequest := make(chan struct{}, 1)
	go func() {
		defer close(writesDone)
		sequence := uint64(1)
		for {
			var envelope *microsandboxprotocol.Envelope
			select {
			case <-stopWrites:
				return
			case <-cancelRequest:
				envelope = &microsandboxprotocol.Envelope{ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID, StreamId: requestID, Sequence: sequence, Message: &microsandboxprotocol.Envelope_Cancel{Cancel: &microsandboxprotocol.CancelRequest{TargetRequestId: requestID}}}
			case <-ctx.Done():
				envelope = &microsandboxprotocol.Envelope{ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID, StreamId: requestID, Sequence: sequence, Message: &microsandboxprotocol.Envelope_Cancel{Cancel: &microsandboxprotocol.CancelRequest{TargetRequestId: requestID}}}
			case control, ok := <-controls:
				if !ok {
					return
				}
				if control.rows != 0 || control.columns != 0 {
					envelope = &microsandboxprotocol.Envelope{ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID, StreamId: requestID, Sequence: sequence, Message: &microsandboxprotocol.Envelope_Pty{Pty: &microsandboxprotocol.PtyRequest{Rows: control.rows, Columns: control.columns}}}
				} else {
					envelope = &microsandboxprotocol.Envelope{ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID, StreamId: requestID, Sequence: sequence, Message: &microsandboxprotocol.Envelope_StreamData{StreamData: &microsandboxprotocol.StreamData{Data: bytes.Clone(control.data), Eof: control.eof, Channel: microsandboxprotocol.StreamChannel_STREAM_CHANNEL_STDIN}}}
				}
			}
			_ = process.control.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if microsandboxprotocol.WriteFrame(process.control, envelope) != nil {
				return
			}
			sequence++
			if ctx.Err() != nil {
				return
			}
		}
	}()
	defer func() { close(stopWrites); <-writesDone; _ = process.control.SetDeadline(time.Time{}) }()
	deadline := time.Now().Add(24 * time.Hour)
	if value, ok := ctx.Deadline(); ok {
		deadline = value.Add(5 * time.Second)
	}
	if request.DeadlineUnixMs != 0 {
		value := time.UnixMilli(int64(request.DeadlineUnixMs)).Add(5 * time.Second)
		if value.Before(deadline) {
			deadline = value
		}
	}
	if err := process.control.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	stopContextDeadline := context.AfterFunc(ctx, func() {
		_ = process.control.SetReadDeadline(time.Now().Add(5 * time.Second))
	})
	defer stopContextDeadline()
	var emitErr error
	for {
		event, err := microsandboxprotocol.ReadFrame(process.control)
		if err != nil {
			process.forceStop()
			return nil, fmt.Errorf(
				"SecondBox Microsandbox read helper Exec: %w: process=%v stderr=%s",
				err, process.processWaitError(), process.stderr.String(),
			)
		}
		if event.RequestId != requestID {
			return nil, fmt.Errorf("SecondBox Microsandbox helper Exec response identity mismatch")
		}
		if diagnostic := event.GetDiagnostic(); diagnostic != nil {
			return nil, fmt.Errorf("SecondBox Microsandbox helper %s: %s", diagnostic.Code, diagnostic.Text)
		}
		if data := event.GetStreamData(); data != nil {
			if emitErr == nil {
				if err := emit(data); err != nil {
					emitErr = err
					_ = process.control.SetReadDeadline(time.Now().Add(5 * time.Second))
					select {
					case cancelRequest <- struct{}{}:
					default:
					}
				}
			}
			continue
		}
		if terminal := event.GetTerminal(); terminal != nil {
			return terminal, emitErr
		}
		return nil, fmt.Errorf("SecondBox Microsandbox helper Exec event is unsupported")
	}
}

func (process *helperProcess) fileOperation(
	ctx context.Context,
	request *microsandboxprotocol.FileRequest,
	content []byte,
) ([]*microsandboxprotocol.Envelope, error) {
	process.requestMu.Lock()
	defer process.requestMu.Unlock()
	requestID := process.nextRequestID
	process.nextRequestID++
	deadline := time.Now().Add(24 * time.Hour)
	if value, ok := ctx.Deadline(); ok {
		deadline = value.Add(5 * time.Second)
	}
	if err := process.control.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox clear helper File deadline: %w", err)
	}
	// Every request and content frame writes under the caller's deadline and
	// observes cancellation: a helper that stops reading must not pin this
	// serialization past the fencing deadline. A frame that fails part-way
	// leaves the shared stream desynchronized, so only helper termination is
	// a safe recovery.
	if err := process.control.SetWriteDeadline(deadline); err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox bound helper File writes: %w", err)
	}
	stopWriteCancel := context.AfterFunc(ctx, func() {
		_ = process.control.SetWriteDeadline(time.Now().Add(5 * time.Second))
	})
	failedWrite := func(action string, err error) ([]*microsandboxprotocol.Envelope, error) {
		stopWriteCancel()
		process.forceStop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("SecondBox Microsandbox %s: %w", action, err)
	}
	if err := microsandboxprotocol.WriteFrame(process.control, &microsandboxprotocol.Envelope{
		ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID,
		Message: &microsandboxprotocol.Envelope_File{File: request},
	}); err != nil {
		return failedWrite("write helper File request", err)
	}
	if request.Operation == microsandboxprotocol.Operation_OPERATION_FILE_WRITE {
		sequence := uint64(1)
		for len(content) != 0 {
			count := min(len(content), helperFileChunkBytes)
			if err := microsandboxprotocol.WriteFrame(process.control, &microsandboxprotocol.Envelope{
				ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID, StreamId: requestID, Sequence: sequence,
				Message: &microsandboxprotocol.Envelope_StreamData{StreamData: &microsandboxprotocol.StreamData{
					Data: bytes.Clone(content[:count]), Channel: microsandboxprotocol.StreamChannel_STREAM_CHANNEL_FILE,
				}},
			}); err != nil {
				return failedWrite("write helper File content", err)
			}
			sequence++
			content = content[count:]
		}
		if err := microsandboxprotocol.WriteFrame(process.control, &microsandboxprotocol.Envelope{
			ProtocolVersion: microsandboxprotocol.Version, RequestId: requestID, StreamId: requestID, Sequence: sequence,
			Message: &microsandboxprotocol.Envelope_StreamData{StreamData: &microsandboxprotocol.StreamData{
				Eof: true, Channel: microsandboxprotocol.StreamChannel_STREAM_CHANNEL_FILE,
			}},
		}); err != nil {
			return failedWrite("finish helper File content", err)
		}
	}
	stopWriteCancel()
	if err := process.control.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := process.control.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	stopContextDeadline := context.AfterFunc(ctx, func() {
		_ = process.control.SetReadDeadline(time.Now().Add(5 * time.Second))
	})
	defer func() {
		stopContextDeadline()
		_ = process.control.SetDeadline(time.Time{})
	}()
	var events []*microsandboxprotocol.Envelope
	for {
		event, err := microsandboxprotocol.ReadFrame(process.control)
		if err != nil {
			if ctx.Err() != nil {
				process.forceStop()
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("SecondBox Microsandbox read helper File: %w", err)
		}
		if event.RequestId != requestID {
			return nil, fmt.Errorf("SecondBox Microsandbox helper File response identity mismatch")
		}
		if diagnostic := event.GetDiagnostic(); diagnostic != nil {
			return nil, fmt.Errorf("SecondBox Microsandbox helper %s: %s", diagnostic.Code, diagnostic.Text)
		}
		events = append(events, event)
		if event.GetTerminal() != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return events, nil
		}
	}
}

func (process *helperProcess) forceStop() {
	if process == nil {
		return
	}
	_ = process.command.Process.Kill()
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
	}
	_ = process.closeResources()
}

func (process *helperProcess) processWaitError() error {
	process.waitMu.Lock()
	defer process.waitMu.Unlock()
	return process.waitErr
}

func (process *helperProcess) closeResources() error {
	process.closeOnce.Do(func() {
		process.closeErr = errors.Join(process.control.Close(), process.lifecycle.Close(), process.workspace.Close())
	})
	return process.closeErr
}

func normalizeProcessExit(err error) error {
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 0 {
		return nil
	}
	return fmt.Errorf("SecondBox Microsandbox helper exited: %w", err)
}

func normalizeKilledExit(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return normalizeProcessExit(err)
}

type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	bytes bytes.Buffer
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	written := len(value)
	remaining := buffer.limit - buffer.bytes.Len()
	if remaining > 0 {
		_, _ = buffer.bytes.Write(value[:min(remaining, len(value))])
	}
	return written, nil
}

func (buffer *boundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return strings.TrimSpace(buffer.bytes.String())
}

func decodeWorkspaceUUID(value string) ([]byte, error) {
	compact := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	if len(compact) != 32 {
		return nil, fmt.Errorf("SecondBox Microsandbox Workspace UUID is invalid")
	}
	decoded, err := hex.DecodeString(compact)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox Workspace UUID is invalid")
	}
	return decoded, nil
}

var _ io.Writer = (*boundedBuffer)(nil)
