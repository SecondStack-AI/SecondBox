//go:build linux

// Package subreaper lets a runner supervisor process adopt and reap the
// descendants that its child orphans. The runner is PID 1 of its container and
// reaps only the commands it starts, so an orphan that reaches it stays a
// zombie for the life of the container.
package subreaper

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// Enable makes the calling process the reaper of every descendant orphaned
// below it.
func Enable() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("configure SecondBox child subreaper: %w", err)
	}
	return nil
}

// WaitForDescendants blocks until every remaining child, adopted orphans
// included, has exited and been reaped. Call it only after os/exec has waited
// for each command it started: a wait for any child would take their exit
// statuses.
func WaitForDescendants() error {
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(-1, &status, 0, nil)
		switch {
		case err == nil:
			continue
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			return nil
		default:
			return fmt.Errorf("reap SecondBox supervised descendant: %w", err)
		}
	}
}
