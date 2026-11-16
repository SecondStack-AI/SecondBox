//go:build linux

// Package subreaper lets a runner supervisor process adopt and reap the
// descendants that its child orphans. The runner is PID 1 of its container and
// reaps only the commands it starts, so an orphan that reaches it stays a
// zombie for the life of the container.
package subreaper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// KillAndReapDescendants kills every remaining child, adopted orphans included,
// and reaps it, until no child is left. Children of a killed child reparent
// here and are killed in the next round. The same os/exec precondition as for
// WaitForDescendants applies.
func KillAndReapDescendants() error {
	self := os.Getpid()
	for {
		children, err := childPIDs(self)
		if err != nil {
			return err
		}
		for _, pid := range children {
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("kill SecondBox supervised descendant %d: %w", pid, err)
			}
		}
		var status syscall.WaitStatus
		_, err = syscall.Wait4(-1, &status, 0, nil)
		switch {
		case err == nil, errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			return nil
		default:
			return fmt.Errorf("reap SecondBox supervised descendant: %w", err)
		}
	}
}

// childPIDs scans /proc for the processes whose parent is parent.
func childPIDs(parent int) ([]int, error) {
	statPaths, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return nil, fmt.Errorf("list SecondBox supervised descendants: %w", err)
	}
	var children []int
	for _, statPath := range statPaths {
		stat, err := os.ReadFile(statPath)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", statPath, err)
		}
		// The command name may contain spaces and parentheses; the state and
		// parent PID follow its last closing parenthesis.
		nameEnd := strings.LastIndexByte(string(stat), ')')
		if nameEnd < 0 {
			return nil, fmt.Errorf("parse %s: unexpected format", statPath)
		}
		fields := strings.Fields(string(stat)[nameEnd+1:])
		if len(fields) < 2 {
			return nil, fmt.Errorf("parse %s: unexpected format", statPath)
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("parse %s parent PID: %w", statPath, err)
		}
		if ppid != parent {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(statPath)))
		if err != nil {
			return nil, fmt.Errorf("parse %s PID: %w", statPath, err)
		}
		children = append(children, pid)
	}
	return children, nil
}
