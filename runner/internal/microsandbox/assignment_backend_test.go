package microsandbox

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestCleanupStackRunsArmedStepsInReverseOrder(t *testing.T) {
	stack := &cleanupStack{}
	var order []string
	stack.push(func() error { order = append(order, "capacity"); return nil })
	workspace := stack.push(func() error { order = append(order, "workspace"); return errors.New("workspace cleanup") })
	stack.push(func() error { order = append(order, "helper-network-socket"); return nil })
	stack.disarm(workspace)
	err := stack.run()
	if err != nil || !reflect.DeepEqual(order, []string{"helper-network-socket", "capacity"}) {
		t.Fatalf("cleanup = %v, %v", order, err)
	}
}

func TestBackendMetricsHaveFixedDimensions(t *testing.T) {
	backend := &AssignmentBackend{}
	snapshot := backend.MetricsSnapshot()
	if snapshot.Dimensions.BackendKind != "microsandbox" || snapshot.Dimensions.HostPlatform == "" {
		t.Fatalf("metrics dimensions = %#v", snapshot.Dimensions)
	}
	if snapshot.Dimensions != backend.DiagnosticDimensions() {
		t.Fatalf("diagnostic dimensions differ: %#v", backend.DiagnosticDimensions())
	}
}

func TestAssignmentFailuresExposeProviderNeutralClasses(t *testing.T) {
	tests := []struct {
		name         string
		failure      error
		wantDecision runnerprotocol.AssignmentDecision
		wantTerminal runnerprotocol.AssignmentTerminalKind
	}{
		{"incompatible", incompatibleAssignment(errors.New("shape")), runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_INCOMPATIBLE_PROFILE, runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_ADMISSION_FAILED},
		{"capacity", capacityAssignment(errors.New("capacity")), runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_CAPACITY, runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_ADMISSION_FAILED},
		{"artifact", artifactAssignment(errors.New("digest")), runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_ARTIFACT, runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_ADMISSION_FAILED},
		{"infrastructure", infrastructureAssignment(errors.New("helper")), runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_PREREQUISITE, runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_INFRASTRUCTURE_FAILED},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var classified assignmentFailure
			if !errors.As(test.failure, &classified) || classified.AssignmentDecision() != test.wantDecision || classified.AssignmentTerminal() != test.wantTerminal {
				t.Fatalf("classification = %#v", classified)
			}
		})
	}
}

// TestStartAssignmentReplayWaitsForTheClaimedLaunch proves the atomic
// claim-or-retrieve: a concurrent identical start neither launches a second
// helper nor fails early, but waits for the claimed launch and returns its
// backend reference.
func TestStartAssignmentReplayWaitsForTheClaimedLaunch(t *testing.T) {
	fence := &runnerprotocol.AssignmentFence{
		AssignmentId: "assignment-claim", SandboxId: "sandbox-claim",
		InstanceId: "instance-claim", SandboxGeneration: 1, FencingToken: []byte{1},
	}
	claim := &activeAssignment{fence: cloneFence(fence), launched: make(chan struct{})}
	claim.launchDone = sync.OnceFunc(func() { close(claim.launched) })
	backend := &AssignmentBackend{assignments: map[string]*activeAssignment{
		fence.AssignmentId: claim,
	}}

	replayed := make(chan runnercontrol.BackendInstance, 1)
	go func() {
		instance, err := backend.StartAssignment(
			context.Background(),
			&runnerprotocol.AssignmentCommand{Fence: cloneFence(fence)},
			func(runnerprotocol.AssignmentProgressStage) error { return nil },
		)
		if err != nil {
			t.Errorf("replayed start = %v", err)
		}
		replayed <- instance
	}()
	select {
	case instance := <-replayed:
		t.Fatalf("replayed start did not wait for the claimed launch: %#v", instance)
	case <-time.After(50 * time.Millisecond):
	}

	backend.mu.Lock()
	claim.backendRef = "microsandbox:12345"
	backend.mu.Unlock()
	claim.launchDone()
	instance := <-replayed
	if instance.BackendKind != "microsandbox" || instance.BackendReference != "microsandbox:12345" {
		t.Fatalf("replayed start reference = %#v", instance)
	}

	// A replay whose claimed launch failed (entry removed) surfaces an error
	// instead of a second launch attempt.
	backend.mu.Lock()
	delete(backend.assignments, fence.AssignmentId)
	failed := &activeAssignment{fence: cloneFence(fence), launched: make(chan struct{})}
	failed.launchDone = sync.OnceFunc(func() { close(failed.launched) })
	backend.assignments[fence.AssignmentId] = failed
	backend.mu.Unlock()
	go func() {
		backend.mu.Lock()
		delete(backend.assignments, fence.AssignmentId)
		backend.mu.Unlock()
		failed.launchDone()
	}()
	if _, err := backend.StartAssignment(
		context.Background(),
		&runnerprotocol.AssignmentCommand{Fence: cloneFence(fence)},
		func(runnerprotocol.AssignmentProgressStage) error { return nil },
	); err == nil {
		t.Fatal("replay of a failed launch must surface an error")
	}
}
