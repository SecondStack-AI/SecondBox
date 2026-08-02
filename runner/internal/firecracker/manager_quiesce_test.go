package firecracker

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A frozen Workspace filesystem is consistent on disk, so the VMM is terminated
// directly instead of being asked to exit and then waited out. The VMM does not
// exit on SIGTERM, so the escalation grace period previously expired on every
// stop and dominated stop latency.
func TestStopTerminatesImmediatelyWhenWorkspaceIsQuiesced(t *testing.T) {
	manager := newWarmToolTestManager(t)
	var frozen bool
	manager.freezeWorkspace = func(context.Context, string) (BackupResponse, error) {
		frozen = true
		return BackupResponse{}, nil
	}
	var mu sync.Mutex
	var signals []syscall.Signal
	manager.signalInstance = func(_ string, signal syscall.Signal) error {
		mu.Lock()
		defer mu.Unlock()
		signals = append(signals, signal)
		return nil
	}
	inst := &instance{
		id: "fc-agent-cmp-quiesced", sandboxID: "agent", sandboxGeneration: 1,
		compartmentID: "cmp_quiesced", requestID: "request-test", operationID: "operation-test",
		leaseID: "lease-test", assignmentID: "assignment-test",
		jailedProcess: true, done: make(chan struct{}),
	}
	close(inst.done)

	if err := manager.stopInstance(context.Background(), inst, false); err != nil {
		t.Fatalf("stopInstance: %v", err)
	}
	if !frozen {
		t.Fatal("stop did not freeze the Workspace before terminating the microVM")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(signals) == 0 {
		t.Fatal("stop sent no termination signal")
	}
	if signals[0] != syscall.SIGKILL {
		t.Fatalf("first termination signal = %v, want SIGKILL after a successful freeze", signals[0])
	}
}

// When the guest cannot be reached the filesystem may still hold dirty pages,
// so the slower escalation that asks the VMM to exit first is retained.
func TestStopAsksTheVMMToExitWhenTheWorkspaceCannotBeQuiesced(t *testing.T) {
	manager := newWarmToolTestManager(t)
	manager.freezeWorkspace = func(context.Context, string) (BackupResponse, error) {
		return BackupResponse{}, errors.New("guest unreachable")
	}
	var mu sync.Mutex
	var signals []syscall.Signal
	manager.signalInstance = func(_ string, signal syscall.Signal) error {
		mu.Lock()
		defer mu.Unlock()
		signals = append(signals, signal)
		return nil
	}
	inst := &instance{
		id: "fc-agent-cmp-unreachable", sandboxID: "agent", sandboxGeneration: 1,
		compartmentID: "cmp_unreachable", requestID: "request-test", operationID: "operation-test",
		leaseID: "lease-test", assignmentID: "assignment-test",
		jailedProcess: true, done: make(chan struct{}),
	}
	close(inst.done)

	if err := manager.stopInstance(context.Background(), inst, false); err != nil {
		t.Fatalf("stopInstance: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(signals) == 0 {
		t.Fatal("stop sent no termination signal")
	}
	if signals[0] != syscall.SIGTERM {
		t.Fatalf("first termination signal = %v, want SIGTERM without a successful freeze", signals[0])
	}
}

func TestTerminationGraceDependsOnQuiescedWorkspace(t *testing.T) {
	manager := newWarmToolTestManager(t)
	if got := manager.terminationGrace(true); got != quiescedGrace {
		t.Fatalf("quiesced grace = %s, want %s", got, quiescedGrace)
	}
	if got := manager.terminationGrace(false); got != quiesceUnavailableGrace {
		t.Fatalf("unquiesced grace = %s, want %s", got, quiesceUnavailableGrace)
	}
	if quiescedGrace >= quiesceUnavailableGrace {
		t.Fatal("a quiesced Workspace must not wait as long as an unreachable guest")
	}
}

// The freeze attempt must not itself become a new stall when the guest is gone.
func TestWorkspaceQuiesceIsBounded(t *testing.T) {
	manager := newWarmToolTestManager(t)
	manager.freezeWorkspace = func(ctx context.Context, _ string) (BackupResponse, error) {
		<-ctx.Done()
		return BackupResponse{}, ctx.Err()
	}
	inst := &instance{id: "fc-agent-cmp-hung", done: make(chan struct{})}
	startedAt := time.Now()
	if manager.quiesceWorkspace(context.Background(), inst) {
		t.Fatal("a hung freeze reported the Workspace as quiesced")
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("quiesce attempt blocked for %s", elapsed)
	}
}

// The guest agent is never listening on the first dial, so the reconnect
// cadence decides when negotiation completes. gRPC's default one-second base
// delay made startup wait for the backoff boundary rather than for the guest.
func TestGuestProtocolRetriesOnAMillisecondCadence(t *testing.T) {
	params := guestProtocolConnectParams()
	if params.Backoff.BaseDelay > 50*time.Millisecond {
		t.Fatalf("guest dial base delay = %s, want a millisecond-scale retry", params.Backoff.BaseDelay)
	}
	if params.Backoff.MaxDelay > 500*time.Millisecond {
		t.Fatalf("guest dial maximum delay = %s, want it bounded well below a second", params.Backoff.MaxDelay)
	}
	if params.Backoff.Multiplier <= 1 {
		t.Fatalf("guest dial multiplier = %v, want growth between attempts", params.Backoff.Multiplier)
	}
	if params.MinConnectTimeout <= 0 {
		t.Fatal("guest dial must keep a positive connect timeout")
	}
}
