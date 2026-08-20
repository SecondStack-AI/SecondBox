package microsandbox

import (
	"errors"
	"reflect"
	"testing"

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
