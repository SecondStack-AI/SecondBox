package microvmguest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/creack/pty"
)

// protocolPTYProcess owns one real pseudoterminal and the exact process wait.
type protocolPTYProcess struct {
	command  *exec.Cmd
	master   *os.File
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

func startProtocolPTYProcess(
	command *exec.Cmd,
	dimensions *guestv1.PtyDimensions,
) (*protocolPTYProcess, error) {
	if command == nil || dimensions == nil ||
		dimensions.Rows == 0 || dimensions.Rows > 65535 ||
		dimensions.Columns == 0 || dimensions.Columns > 65535 {
		return nil, fmt.Errorf("guest PTY requires valid rows and columns")
	}
	if command.SysProcAttr != nil {
		command.SysProcAttr.Setpgid = false
	}
	master, err := pty.StartWithSize(command, &pty.Winsize{
		Rows: uint16(dimensions.Rows),
		Cols: uint16(dimensions.Columns),
	})
	if err != nil {
		return nil, err
	}
	return &protocolPTYProcess{
		command:  command,
		master:   master,
		waitDone: make(chan struct{}),
	}, nil
}

func (p *protocolPTYProcess) Read(value []byte) (int, error) {
	return p.master.Read(value)
}

func (p *protocolPTYProcess) Write(value []byte) (int, error) {
	return p.master.Write(value)
}

func (p *protocolPTYProcess) Resize(rows, columns uint32) error {
	if rows == 0 || rows > 65535 || columns == 0 || columns > 65535 {
		return fmt.Errorf("guest PTY resize requires valid rows and columns")
	}
	return pty.Setsize(p.master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)})
}

func (p *protocolPTYProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.command.Wait()
		p.waitErr = errors.Join(p.waitErr, p.master.Close())
		close(p.waitDone)
	})
	<-p.waitDone
	return p.waitErr
}

func (p *protocolPTYProcess) KillAndWait(ctx context.Context) error {
	if p.command.Process == nil {
		return fmt.Errorf("guest PTY process is not started")
	}
	killErr := syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	go p.Wait()
	select {
	case <-p.waitDone:
		var exitErr *exec.ExitError
		if errors.As(p.waitErr, &exitErr) {
			return killErr
		}
		return errors.Join(killErr, p.waitErr)
	case <-ctx.Done():
		return errors.Join(killErr, ctx.Err())
	}
}

func (p *protocolPTYProcess) ProcessState() *os.ProcessState {
	return p.command.ProcessState
}

func (p *protocolPTYProcess) Close() error {
	if p.master == nil {
		return nil
	}
	err := p.master.Close()
	if errors.Is(err, os.ErrClosed) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
