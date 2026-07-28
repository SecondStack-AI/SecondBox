package microvmguest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"golang.org/x/sys/unix"
)

const maxProtocolExecOutputBytes uint64 = 16 << 20

const maxProtocolExecTerminalTombstones = 256

var (
	errProtocolExecCancelled       = errors.New("guest exec cancelled")
	errProtocolExecDeadline        = errors.New("guest exec deadline exceeded")
	errProtocolExecOutputExhausted = errors.New("guest exec output limit exhausted")
)

type protocolConnection struct {
	service           *ProtocolService
	stream            guestv1.GuestAgent_ConnectServer
	binding           *guestv1.ConnectionBinding
	enabled           map[guestv1.GuestFeature]bool
	sendMu            sync.Mutex
	asyncMu           sync.Mutex
	asyncErr          error
	mu                sync.Mutex
	execs             map[string]*protocolExecState
	files             map[string]*protocolFileState
	ports             map[string]*protocolPortState
	execTerminalOrder []string
	wait              sync.WaitGroup
}

type protocolExecState struct {
	binding      *guestv1.OperationBinding
	nextIncoming uint64
	cancel       context.CancelCauseFunc
	credit       chan uint64
	creditUnused uint64
	input        chan protocolExecInput
	inputClosed  bool
	streaming    bool
	completed    bool
	pty          *protocolPTYProcess
}

type protocolExecInput struct {
	data       []byte
	endOfInput bool
}

func (s *ProtocolService) serveNegotiated(
	stream guestv1.GuestAgent_ConnectServer,
	binding *guestv1.ConnectionBinding,
	enabled []guestv1.GuestFeature,
) error {
	connection := &protocolConnection{
		service: s,
		stream:  stream,
		binding: cloneConnectionBinding(binding),
		enabled: make(map[guestv1.GuestFeature]bool, len(enabled)),
		execs:   map[string]*protocolExecState{},
		files:   map[string]*protocolFileState{},
		ports:   map[string]*protocolPortState{},
	}
	for _, feature := range enabled {
		connection.enabled[feature] = true
	}
	err := connection.receive()
	connection.cancelAllExecs()
	connection.cancelAllFiles()
	connection.cancelAllPorts()
	connection.wait.Wait()
	return errors.Join(err, connection.recordedAsyncError())
}

func (c *protocolConnection) recordAsyncError(action string, err error) {
	if err == nil {
		return
	}
	c.asyncMu.Lock()
	c.asyncErr = errors.Join(c.asyncErr, fmt.Errorf("%s: %w", action, err))
	c.asyncMu.Unlock()
}

func (c *protocolConnection) recordedAsyncError() error {
	c.asyncMu.Lock()
	defer c.asyncMu.Unlock()
	return c.asyncErr
}

func (c *protocolConnection) receive() error {
	for {
		frame, err := c.stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive negotiated guest protocol frame: %w", err)
		}
		if frame.GetHello() != nil {
			return fmt.Errorf("guest protocol renegotiation requires a new connection")
		}
		if execFrame := frame.GetExec(); execFrame != nil {
			if err := c.handleExecFrame(execFrame); err != nil {
				return err
			}
			continue
		}
		if ptyFrame := frame.GetPty(); ptyFrame != nil {
			if err := c.handlePTYFrame(ptyFrame); err != nil {
				return err
			}
			continue
		}
		if fileFrame := frame.GetFile(); fileFrame != nil {
			if err := c.handleFileFrame(fileFrame); err != nil {
				return err
			}
			continue
		}
		if portFrame := frame.GetPort(); portFrame != nil {
			if err := c.handlePortFrame(portFrame); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("guest protocol frame kind is unsupported")
	}
}

func (c *protocolConnection) handlePTYFrame(frame *guestv1.PtyFrame) error {
	if !c.enabled[guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE] {
		return fmt.Errorf("guest protocol PTY feature was not negotiated")
	}
	if frame == nil || frame.Binding == nil || !sameProtocolConnectionBinding(frame.Binding.Connection, c.binding) {
		return fmt.Errorf("guest protocol PTY binding mismatch")
	}
	key := protocolOperationKey(frame.Binding)
	c.mu.Lock()
	state := c.execs[key]
	if state == nil || state.pty == nil || state.completed {
		c.mu.Unlock()
		return fmt.Errorf("guest protocol PTY operation is not active")
	}
	if frame.Binding.Sequence != state.nextIncoming {
		c.mu.Unlock()
		return fmt.Errorf("guest protocol PTY sequence mismatch: got %d, want %d", frame.Binding.Sequence, state.nextIncoming)
	}
	state.nextIncoming++
	terminal := state.pty
	c.mu.Unlock()
	switch {
	case frame.GetInput() != nil:
		if len(frame.GetInput().Data) == 0 {
			return fmt.Errorf("guest protocol PTY input is empty")
		}
		_, err := terminal.Write(frame.GetInput().Data)
		return err
	case frame.GetResize() != nil:
		return terminal.Resize(frame.GetResize().Rows, frame.GetResize().Columns)
	case frame.GetCredit() != nil:
		if frame.GetCredit().ByteCount == 0 {
			return fmt.Errorf("guest protocol PTY credit must be positive")
		}
		select {
		case state.credit <- frame.GetCredit().ByteCount:
			return nil
		case <-c.stream.Context().Done():
			return c.stream.Context().Err()
		}
	case frame.GetCancel() != nil:
		state.cancel(errProtocolExecCancelled)
		return nil
	default:
		return fmt.Errorf("guest protocol PTY frame payload is unsupported")
	}
}

func (c *protocolConnection) handleExecFrame(frame *guestv1.ExecFrame) error {
	if !c.enabled[guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC] {
		return fmt.Errorf("guest protocol exec feature was not negotiated")
	}
	if frame == nil || frame.Binding == nil || !sameProtocolConnectionBinding(frame.Binding.Connection, c.binding) {
		return fmt.Errorf("guest protocol exec binding mismatch")
	}
	key := protocolOperationKey(frame.Binding)
	if key == "" {
		return fmt.Errorf("guest protocol exec operation identity is incomplete")
	}
	if request := frame.GetRequest(); request != nil {
		if frame.Binding.Sequence != 1 {
			return fmt.Errorf("guest protocol exec request sequence must begin at one")
		}
		if request.Pty != nil && !c.enabled[guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE] {
			return fmt.Errorf("guest protocol PTY feature was not negotiated")
		}
		c.mu.Lock()
		if _, exists := c.execs[key]; exists {
			c.mu.Unlock()
			return fmt.Errorf("guest protocol exec operation is already active")
		}
		execCtx, cancel := context.WithCancelCause(c.stream.Context())
		state := &protocolExecState{
			binding:      cloneOperationBinding(frame.Binding),
			nextIncoming: 2,
			cancel:       cancel,
			credit:       make(chan uint64, 16),
			input:        make(chan protocolExecInput, 256),
			streaming:    request.Streaming,
		}
		c.execs[key] = state
		c.wait.Add(1)
		c.mu.Unlock()
		go func() {
			defer c.wait.Done()
			defer c.completeExec(key, state)
			c.runExec(execCtx, state, request)
		}()
		return nil
	}
	c.mu.Lock()
	state := c.execs[key]
	if state == nil {
		c.mu.Unlock()
		return fmt.Errorf("guest protocol exec operation is not active")
	}
	if frame.Binding.Sequence != state.nextIncoming {
		c.mu.Unlock()
		return fmt.Errorf("guest protocol exec sequence mismatch: got %d, want %d", frame.Binding.Sequence, state.nextIncoming)
	}
	input := frame.GetInput()
	if input != nil {
		if state.pty != nil {
			c.mu.Unlock()
			return fmt.Errorf("guest protocol PTY input requires a PTY frame")
		}
		if !state.streaming {
			c.mu.Unlock()
			return fmt.Errorf("guest protocol exec input requires a streaming request")
		}
		if state.inputClosed {
			c.mu.Unlock()
			return fmt.Errorf("guest protocol exec input follows end of input")
		}
		if len(input.Data) == 0 && !input.EndOfInput {
			c.mu.Unlock()
			return fmt.Errorf("guest protocol exec input is empty")
		}
		if input.EndOfInput {
			state.inputClosed = true
		}
	}
	state.nextIncoming++
	completed := state.completed
	c.mu.Unlock()
	if completed {
		if frame.GetCredit() != nil || frame.GetCancel() != nil {
			return nil
		}
		return fmt.Errorf("guest protocol completed exec accepts only trailing credit or cancel")
	}
	switch {
	case frame.GetCredit() != nil:
		if frame.GetCredit().ByteCount == 0 {
			return fmt.Errorf("guest protocol exec credit must be positive")
		}
		select {
		case state.credit <- frame.GetCredit().ByteCount:
			return nil
		case <-c.stream.Context().Done():
			return c.stream.Context().Err()
		}
	case input != nil:
		select {
		case state.input <- protocolExecInput{
			data: bytes.Clone(input.Data), endOfInput: input.EndOfInput,
		}:
			return nil
		case <-c.stream.Context().Done():
			return c.stream.Context().Err()
		}
	case frame.GetCancel() != nil:
		state.cancel(errProtocolExecCancelled)
		return nil
	default:
		return fmt.Errorf("guest protocol exec frame payload is unsupported")
	}
}

func (c *protocolConnection) runExec(ctx context.Context, state *protocolExecState, request *guestv1.ExecRequest) {
	startedAt := time.Now()
	var deadlineTimer *time.Timer
	if request != nil && request.DeadlineUnixMs > 0 {
		deadline := time.UnixMilli(int64(request.DeadlineUnixMs))
		if deadline.After(time.Now()) {
			deadlineTimer = time.AfterFunc(time.Until(deadline), func() {
				state.cancel(errProtocolExecDeadline)
			})
			defer deadlineTimer.Stop()
		}
	}
	cmd, cwd, stdin, output, rejection := c.prepareExec(ctx, state, request)
	if rejection != nil {
		c.recordAsyncError("send guest exec admission rejection", c.sendExec(state, &guestv1.ExecFrame{
			Payload: &guestv1.ExecFrame_Admission{Admission: rejection},
		}))
		return
	}
	defer func() {
		c.recordAsyncError("close guest exec working directory", cwd.Close())
	}()
	var ptyProcess *protocolPTYProcess
	var ptyOutputDone chan error
	var startErr error
	if request.Pty != nil {
		ptyProcess, startErr = startProtocolPTYProcess(cmd, request.Pty)
		if startErr == nil {
			c.mu.Lock()
			state.pty = ptyProcess
			c.mu.Unlock()
			output.send = func(_ guestv1.ExecOutputChannel, data []byte) error {
				return c.sendPTYOutput(ctx, state, data)
			}
			ptyOutputDone = make(chan error, 1)
			go func() {
				_, copyErr := io.Copy(protocolExecChannelWriter{output: output}, ptyProcess)
				if copyErr != nil &&
					!errors.Is(copyErr, os.ErrClosed) &&
					!errors.Is(copyErr, syscall.EIO) {
					state.cancel(copyErr)
					ptyOutputDone <- copyErr
					return
				}
				ptyOutputDone <- nil
			}()
			stdin = ptyProcess
		}
	} else {
		startErr = cmd.Start()
	}
	if startErr != nil {
		if sendErr := c.sendExec(state, &guestv1.ExecFrame{
			Payload: &guestv1.ExecFrame_Admission{Admission: &guestv1.ExecAdmission{
				Kind: guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED,
			}},
		}); sendErr != nil {
			c.recordAsyncError("send guest exec admission", sendErr)
			return
		}
		terminal := &guestv1.ExecTerminal{
			Kind:               guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED,
			ExitCode:           -1,
			SafeDetail:         "command could not be spawned",
			SpawnFailureReason: protocolSpawnFailureReason(startErr),
			Message:            "command could not be spawned",
		}
		if request.Pty != nil {
			c.recordAsyncError("send guest PTY spawn failure", c.sendPTY(state, &guestv1.PtyFrame{
				Payload: &guestv1.PtyFrame_Terminal{Terminal: terminal},
			}))
		} else {
			c.recordAsyncError("send guest exec spawn failure", c.sendExec(state, &guestv1.ExecFrame{
				Payload: &guestv1.ExecFrame_Terminal{Terminal: terminal},
			}))
		}
		return
	}
	if err := c.sendExec(state, &guestv1.ExecFrame{
		Payload: &guestv1.ExecFrame_Admission{Admission: &guestv1.ExecAdmission{
			Kind:      guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_ACCEPTED,
			ProcessId: strconv.Itoa(cmd.Process.Pid),
		}},
	}); err != nil {
		state.cancel(errProtocolExecCancelled)
	}
	if stdin != nil {
		closeStdin := sync.OnceFunc(func() {
			c.recordAsyncError("close guest exec standard input", stdin.Close())
		})
		defer closeStdin()
		inputContext, stopInput := context.WithCancel(ctx)
		defer stopInput()
		go c.forwardExecInput(inputContext, state, stdin, closeStdin, request.Stdin)
	}
	processDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				c.recordAsyncError("kill cancelled guest exec process group", err)
			}
		case <-processDone:
		}
	}()
	var waitErr error
	if ptyProcess != nil {
		waitErr = ptyProcess.Wait()
		if outputErr := <-ptyOutputDone; outputErr != nil {
			waitErr = errors.Join(waitErr, outputErr)
		}
	} else {
		waitErr = cmd.Wait()
	}
	close(processDone)

	cause := context.Cause(ctx)
	terminal := &guestv1.ExecTerminal{Kind: guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED}
	switch {
	case errors.Is(cause, errProtocolExecOutputExhausted):
		terminal.Kind = guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED
		terminal.ExitCode = -1
		terminal.SafeDetail = "command output limit exhausted"
	case errors.Is(cause, errProtocolExecDeadline):
		terminal.Kind = guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED
		terminal.ExitCode = -1
		terminal.SafeDetail = "command deadline exceeded"
	case cause != nil:
		terminal.Kind = guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED
		terminal.ExitCode = -1
		terminal.SafeDetail = "command cancelled"
	case waitErr == nil:
		terminal.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				terminal.Signal = int32(status.Signal())
				terminal.ExitCode = int32(128 + status.Signal())
			} else {
				terminal.ExitCode = int32(exitErr.ExitCode())
			}
		} else {
			terminal.Kind = guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED
			terminal.ExitCode = -1
			terminal.SafeDetail = "command wait failed"
		}
	}
	if !request.Streaming {
		outputContext := ctx
		if terminal.Kind == guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
			outputContext = context.WithoutCancel(ctx)
		}
		if err := c.sendExecOutput(outputContext, state, guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, []byte(output.stdout.String())); err != nil {
			c.recordAsyncError("send guest exec stdout", err)
			return
		}
		if err := c.sendExecOutput(outputContext, state, guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR, []byte(output.stderr.String())); err != nil {
			c.recordAsyncError("send guest exec stderr", err)
			return
		}
	}
	terminal.ElapsedMilliseconds = uint64(time.Since(startedAt).Milliseconds())
	if terminal.Kind == guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
		terminal.LimitBytes = request.OutputLimitBytes
	}
	if request.Pty != nil {
		c.recordAsyncError(
			"send guest PTY terminal",
			c.sendPTY(state, &guestv1.PtyFrame{Payload: &guestv1.PtyFrame_Terminal{Terminal: terminal}}),
		)
	} else {
		c.recordAsyncError(
			"send guest exec terminal",
			c.sendExec(state, &guestv1.ExecFrame{Payload: &guestv1.ExecFrame_Terminal{Terminal: terminal}}),
		)
	}
}

type protocolExecOutput struct {
	mu     sync.Mutex
	used   uint64
	limit  uint64
	cancel context.CancelCauseFunc
	stdout strings.Builder
	stderr strings.Builder
	send   func(guestv1.ExecOutputChannel, []byte) error
}

type protocolExecChannelWriter struct {
	output *protocolExecOutput
	stderr bool
}

func (w protocolExecChannelWriter) Write(value []byte) (int, error) {
	w.output.mu.Lock()
	defer w.output.mu.Unlock()
	originalLength := len(value)
	remaining := w.output.limit - w.output.used
	if uint64(len(value)) > remaining {
		value = value[:remaining]
	}
	if len(value) > 0 {
		w.output.used += uint64(len(value))
		channel := guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT
		if w.stderr {
			channel = guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR
		}
		if w.output.send != nil {
			if err := w.output.send(channel, bytes.Clone(value)); err != nil {
				w.output.cancel(err)
				return originalLength, nil
			}
		} else if w.stderr {
			_, _ = w.output.stderr.Write(value)
		} else {
			_, _ = w.output.stdout.Write(value)
		}
	}
	if uint64(originalLength) > remaining {
		w.output.cancel(errProtocolExecOutputExhausted)
	}
	return originalLength, nil
}

func (c *protocolConnection) prepareExec(
	ctx context.Context,
	state *protocolExecState,
	request *guestv1.ExecRequest,
) (*exec.Cmd, *os.File, io.WriteCloser, *protocolExecOutput, *guestv1.ExecAdmission) {
	invalid := func(
		detail string,
		reason guestv1.SpawnFailureReason,
	) (*exec.Cmd, *os.File, io.WriteCloser, *protocolExecOutput, *guestv1.ExecAdmission) {
		return nil, nil, nil, nil, &guestv1.ExecAdmission{
			Kind:       guestv1.ExecAdmissionKind_EXEC_ADMISSION_KIND_INVALID_REQUEST,
			SafeDetail: detail, SpawnFailureReason: reason, Message: detail,
		}
	}
	if request == nil || request.OutputLimitBytes == 0 || request.OutputLimitBytes > maxProtocolExecOutputBytes {
		return invalid("output limit is invalid", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
	}
	if request.Pty != nil && !request.Streaming {
		return invalid("PTY execution requires streaming", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
	}
	if request.Pty != nil &&
		(request.Pty.Rows == 0 ||
			request.Pty.Rows > 65535 ||
			request.Pty.Columns == 0 ||
			request.Pty.Columns > 65535) {
		return invalid("PTY dimensions are invalid", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
	}
	var command string
	var args []string
	switch typed := request.Command.(type) {
	case *guestv1.ExecRequest_Shell:
		if strings.TrimSpace(typed.Shell) == "" {
			return invalid("shell command is empty", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
		}
		command = "/bin/sh"
		args = []string{"-c", typed.Shell}
	case *guestv1.ExecRequest_Argv:
		if typed.Argv == nil || len(typed.Argv.Argument) == 0 || strings.TrimSpace(typed.Argv.Argument[0]) == "" {
			return invalid("argv command is empty", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
		}
		command = typed.Argv.Argument[0]
		args = append([]string(nil), typed.Argv.Argument[1:]...)
	default:
		return invalid("command kind is missing", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
	}
	root, cwdPath, err := c.service.server.toolWorkspacePath(request.Cwd)
	if err != nil {
		return invalid("working directory is invalid", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD)
	}
	cwd, err := openWorkspaceTarget(root, cwdPath, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return invalid("working directory is unavailable", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_INVALID_CWD)
	}
	if request.DeadlineUnixMs > 0 {
		deadline := time.UnixMilli(int64(request.DeadlineUnixMs))
		if !deadline.After(time.Now()) {
			c.recordAsyncError("close rejected guest exec working directory", cwd.Close())
			return invalid("deadline must be in the future", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
		}
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = "/proc/self/fd/" + strconv.FormatUint(uint64(cwd.Fd()), 10)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env, err := c.service.server.toolCommandEnv(protocolEnvironment(request.Environment))
	if err != nil {
		c.recordAsyncError("close rejected guest exec working directory", cwd.Close())
		return invalid("environment is invalid", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
	}
	cmd.Env = env
	var stdin io.WriteCloser
	if request.Pty == nil {
		if request.Streaming {
			stdin, err = cmd.StdinPipe()
			if err != nil {
				c.recordAsyncError("close rejected guest exec working directory", cwd.Close())
				return invalid("standard input is unavailable", guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE)
			}
		} else {
			cmd.Stdin = bytes.NewReader(request.Stdin)
		}
	}
	output := &protocolExecOutput{limit: request.OutputLimitBytes, cancel: state.cancel}
	if request.Streaming {
		output.send = func(channel guestv1.ExecOutputChannel, data []byte) error {
			return c.sendExecOutput(ctx, state, channel, data)
		}
	}
	if request.Pty == nil {
		cmd.Stdout = protocolExecChannelWriter{output: output}
		cmd.Stderr = protocolExecChannelWriter{output: output, stderr: true}
	}
	return cmd, cwd, stdin, output, nil
}

func (c *protocolConnection) forwardExecInput(
	ctx context.Context,
	state *protocolExecState,
	stdin io.Writer,
	closeStdin func(),
	initial []byte,
) {
	write := func(value []byte) bool {
		if len(value) == 0 {
			return true
		}
		if _, err := stdin.Write(value); err != nil {
			if ctx.Err() == nil {
				state.cancel(err)
			}
			return false
		}
		return true
	}
	if !write(initial) {
		return
	}
	for {
		select {
		case input := <-state.input:
			if !write(input.data) {
				return
			}
			if input.endOfInput {
				closeStdin()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func protocolSpawnFailureReason(err error) guestv1.SpawnFailureReason {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_NOT_FOUND
	case errors.Is(err, os.ErrPermission):
		return guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_PERMISSION_DENIED
	default:
		return guestv1.SpawnFailureReason_SPAWN_FAILURE_REASON_MALFORMED_EXECUTABLE
	}
}

func protocolEnvironment(entries []*guestv1.EnvironmentEntry) map[string]string {
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry != nil {
			env[entry.Name] = string(entry.Value)
		}
	}
	return env
}

func (c *protocolConnection) sendExecOutput(
	ctx context.Context,
	state *protocolExecState,
	channel guestv1.ExecOutputChannel,
	data []byte,
) error {
	for len(data) > 0 {
		if state.creditUnused == 0 {
			select {
			case state.creditUnused = <-state.credit:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		size := state.creditUnused
		if size > uint64(len(data)) {
			size = uint64(len(data))
		}
		if err := c.sendExec(state, &guestv1.ExecFrame{
			Payload: &guestv1.ExecFrame_Output{Output: &guestv1.ExecOutput{
				Channel: channel,
				Data:    append([]byte(nil), data[:size]...),
			}},
		}); err != nil {
			return err
		}
		data = data[size:]
		state.creditUnused -= size
	}
	return nil
}

func (c *protocolConnection) sendPTYOutput(
	ctx context.Context,
	state *protocolExecState,
	data []byte,
) error {
	for len(data) > 0 {
		if state.creditUnused == 0 {
			select {
			case state.creditUnused = <-state.credit:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		size := state.creditUnused
		if size > uint64(len(data)) {
			size = uint64(len(data))
		}
		if err := c.sendPTY(state, &guestv1.PtyFrame{
			Payload: &guestv1.PtyFrame_Output{Output: &guestv1.ExecOutput{
				Channel: guestv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
				Data:    append([]byte(nil), data[:size]...),
			}},
		}); err != nil {
			return err
		}
		data = data[size:]
		state.creditUnused -= size
	}
	return nil
}

func (c *protocolConnection) sendPTY(state *protocolExecState, frame *guestv1.PtyFrame) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	frame.Binding = cloneOperationBinding(state.binding)
	frame.Binding.Sequence = state.binding.Sequence
	state.binding.Sequence++
	return c.stream.Send(&guestv1.GuestToRunner{
		Message: &guestv1.GuestToRunner_Pty{Pty: frame},
	})
}

func (c *protocolConnection) sendExec(state *protocolExecState, frame *guestv1.ExecFrame) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	frame.Binding = cloneOperationBinding(state.binding)
	frame.Binding.Sequence = state.binding.Sequence
	state.binding.Sequence++
	return c.stream.Send(&guestv1.GuestToRunner{
		Message: &guestv1.GuestToRunner_Exec{Exec: frame},
	})
}

func (c *protocolConnection) removeExec(key string) {
	c.mu.Lock()
	delete(c.execs, key)
	c.mu.Unlock()
}

func (c *protocolConnection) completeExec(key string, state *protocolExecState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.execs[key]
	if current != state {
		return
	}
	state.completed = true
	c.execTerminalOrder = append(c.execTerminalOrder, key)
	for len(c.execTerminalOrder) > maxProtocolExecTerminalTombstones {
		oldest := c.execTerminalOrder[0]
		c.execTerminalOrder = c.execTerminalOrder[1:]
		if retained := c.execs[oldest]; retained != nil && retained.completed {
			delete(c.execs, oldest)
		}
	}
}

func (c *protocolConnection) cancelAllExecs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.execs {
		state.cancel(errProtocolExecCancelled)
	}
}

func sameProtocolConnectionBinding(left, right *guestv1.ConnectionBinding) bool {
	return left != nil &&
		right != nil &&
		left.InstanceId == right.InstanceId &&
		left.SandboxId == right.SandboxId &&
		left.SandboxGeneration == right.SandboxGeneration &&
		string(left.ConnectionNonce) == string(right.ConnectionNonce)
}

func protocolOperationKey(binding *guestv1.OperationBinding) string {
	if binding == nil ||
		strings.TrimSpace(binding.AssignmentId) == "" ||
		strings.TrimSpace(binding.OperationId) == "" ||
		strings.TrimSpace(binding.StreamId) == "" {
		return ""
	}
	return binding.OperationId + "\x00" + binding.StreamId
}

func cloneOperationBinding(binding *guestv1.OperationBinding) *guestv1.OperationBinding {
	if binding == nil {
		return nil
	}
	return &guestv1.OperationBinding{
		Connection:   cloneConnectionBinding(binding.Connection),
		AssignmentId: binding.AssignmentId,
		OperationId:  binding.OperationId,
		StreamId:     binding.StreamId,
		Sequence:     binding.Sequence,
	}
}
