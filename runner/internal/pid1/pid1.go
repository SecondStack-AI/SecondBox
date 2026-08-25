// Package pid1 keeps the guest agent correct when it is a sandbox's initial
// process: orphaned grandchildren reparent to PID 1 and must be reaped, and
// shutdown must propagate to every process remaining in the PID namespace.
// Every entry point is a no-op when the agent is not PID 1, so microVM guests
// running under an init are unchanged.
package pid1

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	spawnMu sync.Mutex
	managed int
)

func enabled() bool {
	return os.Getpid() == 1
}

// GuardedStart runs start under the spawn lock and counts the resulting
// managed child. The orphan reaper only collects exit statuses while no
// managed child exists, so it can never steal a status an os/exec caller is
// waiting for. Callers pair every successful GuardedStart with one Release
// after their own wait returns.
func GuardedStart(start func() error) error {
	if !enabled() {
		return start()
	}
	spawnMu.Lock()
	defer spawnMu.Unlock()
	if err := start(); err != nil {
		return err
	}
	managed++
	return nil
}

// Release marks one managed child as waited by its owner.
func Release() {
	if !enabled() {
		return
	}
	spawnMu.Lock()
	if managed > 0 {
		managed--
	}
	spawnMu.Unlock()
}

// StartReaper collects orphaned children whenever no managed child is alive.
// While a managed child runs, orphan zombies accumulate temporarily and are
// collected when the operation's own wait completes.
func StartReaper(ctx context.Context) {
	if !enabled() {
		return
	}
	notifications := make(chan os.Signal, 1)
	signal.Notify(notifications, unix.SIGCHLD)
	go func() {
		defer signal.Stop(notifications)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-notifications:
			case <-ticker.C:
			}
			reapIdleOrphans()
		}
	}()
}

func reapIdleOrphans() {
	spawnMu.Lock()
	defer spawnMu.Unlock()
	if managed > 0 {
		return
	}
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return
		}
	}
}

// ShutdownNamespace propagates termination to every process left in the PID
// namespace: SIGTERM to all, one bounded grace interval, then SIGKILL, then a
// final reap sweep. From PID 1, kill(-1) signals only this namespace.
func ShutdownNamespace(grace time.Duration) {
	if !enabled() {
		return
	}
	_ = unix.Kill(-1, unix.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !anyChildAlive() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = unix.Kill(-1, unix.SIGKILL)
	drainDeadline := time.Now().Add(time.Second)
	for time.Now().Before(drainDeadline) && anyChildAlive() {
		time.Sleep(10 * time.Millisecond)
	}
}

func anyChildAlive() bool {
	var status unix.WaitStatus
	for {
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if pid == 0 {
			// Children exist but none has exited yet.
			return true
		}
		if pid < 0 || err != nil {
			// ECHILD: nothing left to wait for.
			return false
		}
	}
}
