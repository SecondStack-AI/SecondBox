package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

const (
	defaultShellRows    = 24
	defaultShellColumns = 80
	// defaultShellCreditBytes is the outstanding output credit requested before
	// the session's own window is known. A full-screen repaint costs several
	// kilobytes, so a smaller default would stall every repaint behind extra
	// credit round trips. The session's pinned window always clamps this.
	defaultShellCreditBytes = 64 << 10
	shellInputChunkBytes    = 4096
)

type shellTerminalController interface {
	IsTerminal(int) bool
	MakeRaw(int) (any, error)
	Restore(int, any) error
	GetSize(int) (int, int, error)
}

type systemShellTerminalController struct{}

func (systemShellTerminalController) IsTerminal(fileDescriptor int) bool {
	return fileDescriptor >= 0 && term.IsTerminal(fileDescriptor)
}

func (systemShellTerminalController) MakeRaw(fileDescriptor int) (any, error) {
	return term.MakeRaw(fileDescriptor)
}

func (systemShellTerminalController) Restore(fileDescriptor int, state any) error {
	terminalState, ok := state.(*term.State)
	if !ok {
		return errors.New("SecondBox CLI Terminal state is invalid")
	}
	return term.Restore(fileDescriptor, terminalState)
}

func (systemShellTerminalController) GetSize(fileDescriptor int) (int, int, error) {
	return term.GetSize(fileDescriptor)
}

type sandboxShellEnvironment struct {
	input           io.Reader
	output          io.Writer
	inputFD         int
	outputFD        int
	terminal        shellTerminalController
	resizeEvents    <-chan struct{}
	stopResize      func()
	httpClient      *http.Client
	websocketDialer *websocket.Dialer
}

func runSandboxShellCommand(
	ctx context.Context,
	rawURL string,
	token string,
	tenantRef string,
	subjectRef string,
	args []string,
	environment sandboxShellEnvironment,
) (resultErr error) {
	flags := flag.NewFlagSet("sandbox shell", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sandboxID := flags.String("sandbox", "", "Sandbox ID")
	generationText := flags.String("generation", "", "current Sandbox generation")
	sessionID := flags.String("session", "", "existing Terminal session ID to reconnect")
	idempotencyKey := flags.String("idempotency-key", "", "Terminal creation idempotency key")
	leaseID := flags.String("lease", "", "active Lease ID")
	command := flags.String("command", "/bin/sh", "shell command")
	cwd := flags.String("cwd", "", "workspace-relative working directory")
	var environmentValues repeatedValues
	flags.Var(&environmentValues, "env", "environment name=value; repeatable")
	rows := flags.Int("rows", 0, "initial Terminal rows; defaults to the local TTY or 24")
	columns := flags.Int("columns", 0, "initial Terminal columns; defaults to the local TTY or 80")
	deadlineMilliseconds := flags.Int64(
		"deadline-milliseconds", 3_600_000, "Terminal process deadline in milliseconds",
	)
	detachable := flags.Bool("detachable", false, "allow bounded reconnect after disconnect")
	creditBytes := flags.Int64(
		"credit-bytes", defaultShellCreditBytes, "outstanding Terminal output credit",
	)
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI parse sandbox shell options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("SecondBox CLI unexpected sandbox shell arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(token) == "" ||
		strings.TrimSpace(tenantRef) == "" || strings.TrimSpace(subjectRef) == "" {
		return errors.New(
			"SecondBox CLI sandbox shell requires --url, --token, --tenant-ref, and --subject-ref" +
				sessionSourceHint,
		)
	}
	if strings.TrimSpace(*sandboxID) == "" || strings.TrimSpace(*generationText) == "" {
		return errors.New("SecondBox CLI sandbox shell requires --sandbox and --generation")
	}
	generation, err := strconv.ParseInt(*generationText, 10, 64)
	if err != nil || generation < 1 {
		return errors.New("SecondBox CLI sandbox shell --generation must be a positive integer")
	}
	if *creditBytes < 1 || *deadlineMilliseconds < 1 {
		return errors.New("SecondBox CLI sandbox shell credit and deadline must be positive")
	}
	if (*rows == 0) != (*columns == 0) ||
		*rows < 0 || *rows > 1000 || *columns < 0 || *columns > 1000 {
		return errors.New("SecondBox CLI sandbox shell rows and columns must be supplied together and be at most 1000")
	}
	if *sessionID == "" &&
		(strings.TrimSpace(*idempotencyKey) == "" || strings.TrimSpace(*leaseID) == "") {
		return errors.New("SecondBox CLI sandbox shell creation requires --idempotency-key and --lease")
	}
	if *sessionID != "" && (*idempotencyKey != "" || *leaseID != "") {
		return errors.New("SecondBox CLI sandbox shell reconnect cannot combine --session with creation authority")
	}
	if environment.input == nil || environment.output == nil {
		return errors.New("SecondBox CLI sandbox shell input and output are required")
	}
	if environment.httpClient == nil {
		environment.httpClient = http.DefaultClient
	}
	if environment.terminal == nil {
		environment.terminal = systemShellTerminalController{}
	}
	interactive := environment.terminal.IsTerminal(environment.inputFD) &&
		environment.terminal.IsTerminal(environment.outputFD)
	if *rows == 0 {
		*rows, *columns = defaultShellRows, defaultShellColumns
		if interactive {
			width, height, sizeErr := environment.terminal.GetSize(environment.outputFD)
			if sizeErr != nil {
				return fmt.Errorf("SecondBox CLI read local Terminal size: %w", sizeErr)
			}
			*rows, *columns = height, width
		}
	}
	if interactive {
		state, rawErr := environment.terminal.MakeRaw(environment.inputFD)
		if rawErr != nil {
			return fmt.Errorf("SecondBox CLI enter raw Terminal mode: %w", rawErr)
		}
		defer func() {
			resultErr = errors.Join(
				resultErr,
				environment.terminal.Restore(environment.inputFD, state),
			)
		}()
	}

	client, err := secondboxclient.NewSecondBoxSubjectClient(
		rawURL, token, tenantRef, subjectRef, environment.httpClient,
	)
	if err != nil {
		return err
	}
	handle := secondboxclient.NewSandboxHandle(client, secondboxclient.Sandbox{
		ID: secondboxclient.OpaqueID(*sandboxID), Generation: generation,
	})
	var session secondboxclient.TerminalSession
	if *sessionID != "" {
		session, err = handle.GetTerminal(ctx, secondboxclient.OpaqueID(*sessionID))
	} else {
		values, parseErr := parsePairs(environmentValues)
		if parseErr != nil {
			return fmt.Errorf("SecondBox CLI sandbox shell environment: %w", parseErr)
		}
		request := secondboxclient.CreateTerminalRequest{
			Command: secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{
				Mode: "shell", Command: *command,
			}},
			Environment:          secondboxclient.StringMap(values),
			Rows:                 *rows,
			Columns:              *columns,
			DeadlineMilliseconds: *deadlineMilliseconds,
			Detachable:           *detachable,
		}
		if *cwd != "" {
			workspacePath := secondboxclient.WorkspacePath(*cwd)
			request.Cwd = &workspacePath
		}
		session, err = handle.CreateTerminal(ctx, request, *idempotencyKey, *leaseID)
	}
	if err != nil {
		return err
	}
	terminalConnection, err := handle.ConnectTerminal(ctx, session, environment.websocketDialer)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, terminalConnection.Close())
	}()
	shellDone := make(chan struct{})
	defer close(shellDone)
	if err := terminalConnection.GrantOutput(
		shellOutputCredit(*creditBytes, session.StreamWindowBytes),
	); err != nil {
		return err
	}

	resizeEvents, stopResizeEvents := shellResizeEvents(environment.resizeEvents)
	stopResize := stopResizeEvents
	if environment.stopResize != nil {
		stopResize = func() {
			stopResizeEvents()
			environment.stopResize()
		}
	}
	defer stopResize()
	inputErrors := make(chan error, 1)
	go func() {
		inputErrors <- pumpSandboxShellInput(environment.input, terminalConnection, interactive)
	}()
	resizeErrors := make(chan error, 1)
	go func() {
		for {
			select {
			case _, open := <-resizeEvents:
				if !open {
					return
				}
				width, height, sizeErr := environment.terminal.GetSize(environment.outputFD)
				if sizeErr == nil {
					sizeErr = terminalConnection.Resize(height, width)
				}
				if sizeErr != nil {
					select {
					case resizeErrors <- sizeErr:
					default:
					}
					_ = terminalConnection.Close()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = terminalConnection.Cancel()
			_ = terminalConnection.Close()
		case <-shellDone:
		}
	}()

	for {
		frame, receiveErr := terminalConnection.Receive()
		if receiveErr != nil {
			select {
			case inputErr := <-inputErrors:
				if inputErr != nil {
					return inputErr
				}
			case resizeErr := <-resizeErrors:
				return fmt.Errorf("SecondBox CLI resize remote Terminal: %w", resizeErr)
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return receiveErr
		}
		switch {
		case frame.TerminalOutputFrame != nil:
			content, decodeErr := base64.StdEncoding.Strict().DecodeString(
				frame.TerminalOutputFrame.DataBase64,
			)
			if decodeErr != nil {
				return errors.New("SecondBox CLI Terminal output is not canonical base64")
			}
			if _, err := environment.output.Write(content); err != nil {
				return fmt.Errorf("SecondBox CLI write Terminal output: %w", err)
			}
			if err := terminalConnection.GrantOutput(int64(len(content))); err != nil {
				return err
			}
		case frame.StreamOutcomeFrame != nil:
			return sandboxShellOutcomeError(frame.StreamOutcomeFrame.Outcome)
		default:
			return errors.New("SecondBox CLI received an unsupported Terminal frame")
		}
	}
}

// shellOutputCredit clamps the requested credit to the session's pinned window.
//
// The window is immutable ProfileRevision policy and granting past it fails the
// session, so the client publishes what it can spend rather than guessing. A
// server that reports no window leaves the requested value untouched, which
// keeps an older control plane usable.
func shellOutputCredit(requested int64, streamWindowBytes int64) int64 {
	if streamWindowBytes > 0 && requested > streamWindowBytes {
		return streamWindowBytes
	}
	return requested
}

func pumpSandboxShellInput(
	input io.Reader,
	terminalConnection *secondboxclient.Terminal,
	interactive bool,
) error {
	buffer := make([]byte, shellInputChunkBytes)
	for {
		count, err := input.Read(buffer)
		if count > 0 {
			if sendErr := terminalConnection.SendInput(buffer[:count]); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if interactive {
					return nil
				}
				return terminalConnection.SendInput([]byte{0x04})
			}
			return fmt.Errorf("SecondBox CLI read Terminal input: %w", err)
		}
	}
}

// sandboxShellOutcomeError names the remote shell over the single shared
// interpretation of the terminal outcome union.
func sandboxShellOutcomeError(outcome secondboxclient.ExecOutcome) error {
	err := secondboxclient.ExecOutcomeError(outcome)
	if err == nil {
		return nil
	}
	return fmt.Errorf("SecondBox remote shell: %w", err)
}

func shellResizeEvents(injected <-chan struct{}) (<-chan struct{}, func()) {
	if injected != nil {
		return injected, func() {}
	}
	signals := make(chan os.Signal, 1)
	events := make(chan struct{}, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	stop := make(chan struct{})
	go func() {
		defer close(events)
		for {
			select {
			case <-signals:
				select {
				case events <- struct{}{}:
				default:
				}
			case <-stop:
				return
			}
		}
	}()
	return events, func() {
		signal.Stop(signals)
		close(stop)
	}
}
