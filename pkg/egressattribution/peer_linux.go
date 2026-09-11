package egressattribution

import (
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// ReadRunnerExecutionAttribution authenticates the configured host peer before reading its preface.
// The caller must close on failure and retain ExpiresAt as the connection's maximum lifetime.
func ReadRunnerExecutionAttribution(connection *net.UnixConn, runnerUID uint32, deadline time.Time) (ExecutionAttribution, error) {
	if connection == nil || !deadline.After(time.Now()) {
		return ExecutionAttribution{}, errors.New("SecondBox execution attribution requires a Unix connection and future deadline")
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return ExecutionAttribution{}, fmt.Errorf("SecondBox execution attribution peer socket failed: %w", err)
	}
	var peer *unix.Ucred
	var peerError error
	if err := raw.Control(func(descriptor uintptr) {
		peer, peerError = unix.GetsockoptUcred(int(descriptor), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return ExecutionAttribution{}, fmt.Errorf("SecondBox execution attribution peer access failed: %w", err)
	}
	if peerError != nil {
		return ExecutionAttribution{}, fmt.Errorf("SecondBox execution attribution peer credentials failed: %w", peerError)
	}
	if peer == nil || peer.Uid != runnerUID {
		return ExecutionAttribution{}, errors.New("SecondBox execution attribution Runner peer denied")
	}
	if err := connection.SetReadDeadline(deadline); err != nil {
		return ExecutionAttribution{}, fmt.Errorf("SecondBox execution attribution deadline failed: %w", err)
	}
	attribution, err := ReadExecutionAttribution(connection, time.Now())
	if err != nil {
		return ExecutionAttribution{}, err
	}
	if err := attribution.validate(time.Now()); err != nil {
		return ExecutionAttribution{}, err
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return ExecutionAttribution{}, fmt.Errorf("SecondBox execution attribution deadline reset failed: %w", err)
	}
	return attribution, nil
}
