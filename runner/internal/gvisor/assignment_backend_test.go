//go:build linux

package gvisor

import (
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// TestMarkAssignmentReadyPublishesARetainedSupervisorExit proves an Instance
// whose supervisor died before readiness acknowledgment still delivers its
// termination: the observed exit is retained and published by the readiness
// path instead of being dropped forever.
func TestMarkAssignmentReadyPublishesARetainedSupervisorExit(t *testing.T) {
	fence := &runnerprotocol.AssignmentFence{
		AssignmentId: "assignment-exit", SandboxId: "sandbox-exit",
		InstanceId: "instance-exit", SandboxGeneration: 1, FencingToken: []byte{1},
	}
	done := make(chan struct{})
	close(done)
	active := &activeAssignment{
		fence:       cloneFence(fence),
		correlation: &runnerprotocol.Correlation{},
		backendRef:  "gvisor:test",
		done:        done,
	}
	backend := &AssignmentBackend{
		assignments:       map[string]*activeAssignment{fence.AssignmentId: active},
		instanceTerminals: make(chan runnercontrol.BackendInstanceTerminal, 1),
	}

	backend.observeExit(active)
	select {
	case terminal := <-backend.instanceTerminals:
		t.Fatalf("pre-ready exit published a terminal early: %#v", terminal)
	default:
	}
	if !active.exitPending {
		t.Fatal("pre-ready exit was not retained")
	}

	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatalf("ready after retained exit = %v", err)
	}
	select {
	case terminal := <-backend.instanceTerminals:
		if !sameFence(terminal.Fence, fence) ||
			terminal.Reason != runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE {
			t.Fatalf("published terminal = %#v", terminal)
		}
	default:
		t.Fatal("readiness did not publish the retained supervisor exit")
	}
	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatalf("repeated ready = %v", err)
	}
	select {
	case terminal := <-backend.instanceTerminals:
		t.Fatalf("repeated ready duplicated the terminal: %#v", terminal)
	default:
	}
}
