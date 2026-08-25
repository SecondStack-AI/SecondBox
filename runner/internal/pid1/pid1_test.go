package pid1

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Real PID-1 behavior can only be observed as a sandbox's initial process;
// the gVisor qualification suites exercise that path. These tests pin the
// contract that every entry point is inert outside PID 1, so microVM guests
// running under an init are untouched.

func TestGuardedStartRunsAndPropagatesOutsidePID1(t *testing.T) {
	ran := false
	if err := GuardedStart(func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("start function did not run")
	}
	sentinel := errors.New("start failed")
	if err := GuardedStart(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if managed != 0 {
		t.Fatalf("managed = %d outside PID 1, want 0", managed)
	}
}

func TestLifecycleEntryPointsAreInertOutsidePID1(t *testing.T) {
	Release()
	ctx, cancel := context.WithCancel(context.Background())
	StartReaper(ctx)
	cancel()
	start := time.Now()
	ShutdownNamespace(5 * time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ShutdownNamespace outside PID 1 took %s, want immediate return", elapsed)
	}
}
