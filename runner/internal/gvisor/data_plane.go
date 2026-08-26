//go:build linux

package gvisor

import (
	"context"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// The data plane maps the provider-neutral runner interfaces directly onto
// the retained negotiated guest session through the shared session bridges;
// no relay process exists in this backend. Every operation checks the full
// assignment fence at admission and again at terminal publication.

func (backend *AssignmentBackend) sessionForOperation(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
) (*activeAssignment, *firecracker.GuestProtocolSession, context.Context, func(), error) {
	if fence == nil {
		return nil, nil, nil, nil, fmt.Errorf("SecondBox gVisor operation fence is required")
	}
	active, opCtx, release, err := backend.acquireOperation(ctx, fence)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if active.session == nil {
		release()
		return nil, nil, nil, nil, fmt.Errorf("SecondBox gVisor retained guest session is unavailable")
	}
	return active, active.session, opCtx, release, nil
}

// confirmTerminalFence rejects a result whose assignment was fenced while the
// operation ran, so a stale generation never publishes a terminal.
func (backend *AssignmentBackend) confirmTerminalFence(
	active *activeAssignment,
	fence *runnerprotocol.AssignmentFence,
) error {
	if !backend.operationFenceActive(active, fence) {
		return fmt.Errorf("SecondBox gVisor operation fence became stale before terminal publication")
	}
	return nil
}

func (backend *AssignmentBackend) ExecuteBuffered(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
) (runnercontrol.BufferedExecResult, error) {
	active, session, opCtx, release, err := backend.sessionForOperation(ctx, fence)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	defer release()
	result, err := firecracker.ExecuteBufferedOverSession(opCtx, session, fence.AssignmentId, open)
	if err != nil {
		return runnercontrol.BufferedExecResult{}, err
	}
	if err := backend.confirmTerminalFence(active, fence); err != nil {
		return runnercontrol.BufferedExecResult{}, err
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
	active, session, opCtx, release, err := backend.sessionForOperation(ctx, fence)
	if err != nil {
		return nil, err
	}
	defer release()
	terminal, err := firecracker.ExecuteStreamingOverSession(opCtx, session, fence.AssignmentId, open, controls, emit)
	if err != nil {
		return nil, err
	}
	if err := backend.confirmTerminalFence(active, fence); err != nil {
		return nil, err
	}
	return terminal, nil
}

func (backend *AssignmentBackend) ExecutePTY(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.ExecOpen,
	controls <-chan runnercontrol.PTYControl,
	emit func([]byte) error,
) (*runnerprotocol.ExecTerminal, error) {
	active, session, opCtx, release, err := backend.sessionForOperation(ctx, fence)
	if err != nil {
		return nil, err
	}
	defer release()
	terminal, err := firecracker.ExecutePTYOverSession(opCtx, session, fence.AssignmentId, open, controls, emit)
	if err != nil {
		return nil, err
	}
	if err := backend.confirmTerminalFence(active, fence); err != nil {
		return nil, err
	}
	return terminal, nil
}

func (backend *AssignmentBackend) ExecuteFile(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.FileOpen,
	content []byte,
) (runnercontrol.FileOperationResult, error) {
	active, session, opCtx, release, err := backend.sessionForOperation(ctx, fence)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	defer release()
	result, err := firecracker.ExecuteFileOverSession(opCtx, session, fence.AssignmentId, open, content)
	if err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	if err := backend.confirmTerminalFence(active, fence); err != nil {
		return runnercontrol.FileOperationResult{}, err
	}
	return result, nil
}

// portConnection releases its operation reservation when the relay closes.
type portConnection struct {
	runnercontrol.PortConnection
	release func()
}

func (connection portConnection) Close() error {
	err := connection.PortConnection.Close()
	connection.release()
	return err
}

func (backend *AssignmentBackend) OpenPort(
	ctx context.Context,
	fence *runnerprotocol.AssignmentFence,
	open *runnerprotocol.PortOpen,
) (runnercontrol.PortConnection, error) {
	_, session, opCtx, release, err := backend.sessionForOperation(ctx, fence)
	if err != nil {
		return nil, err
	}
	relay, err := firecracker.OpenPortOverSession(opCtx, session, fence.AssignmentId, open)
	if err != nil {
		release()
		return nil, err
	}
	return portConnection{PortConnection: relay, release: release}, nil
}

var _ runnercontrol.DataPlaneBackend = (*AssignmentBackend)(nil)
var _ runnercontrol.PTYDataPlaneBackend = (*AssignmentBackend)(nil)
var _ runnercontrol.PortBackend = (*AssignmentBackend)(nil)
