// Package conformance provides reusable assignment-backend contract tests.
package conformance

import (
	"context"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type Fixture struct {
	Backend    runnercontrol.AssignmentBackend
	Assignment *runnerprotocol.AssignmentCommand
}

func Run(t *testing.T, newFixture func(*testing.T) Fixture) {
	t.Helper()
	t.Run("rejects incomplete assignment", func(t *testing.T) {
		fixture := newFixture(t)
		if err := fixture.Backend.ValidateAssignment(context.Background(), nil); err == nil {
			t.Fatal("backend accepted an incomplete assignment")
		}
	})
	t.Run("idempotent start and exact fencing", func(t *testing.T) {
		fixture := newFixture(t)
		if err := fixture.Backend.ValidateAssignment(context.Background(), fixture.Assignment); err != nil {
			t.Fatalf("validate assignment: %v", err)
		}
		var stages []runnerprotocol.AssignmentProgressStage
		first, err := fixture.Backend.StartAssignment(
			context.Background(),
			fixture.Assignment,
			func(stage runnerprotocol.AssignmentProgressStage) error {
				stages = append(stages, stage)
				return nil
			},
		)
		if err != nil {
			t.Fatalf("start assignment: %v", err)
		}
		wantStages := []runnerprotocol.AssignmentProgressStage{
			runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY,
			runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_WORKSPACE_MATERIALIZE,
			runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_NETWORK_SETUP,
			runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_FIRECRACKER_LAUNCH,
			runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_GUEST_NEGOTIATION,
			runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY,
		}
		if !equalStages(stages, wantStages) {
			t.Fatalf("assignment progress = %v, want %v", stages, wantStages)
		}
		second, err := fixture.Backend.StartAssignment(
			context.Background(),
			fixture.Assignment,
			func(runnerprotocol.AssignmentProgressStage) error { return nil },
		)
		if err != nil || second != first {
			t.Fatalf("idempotent start = %+v, %v; want %+v", second, err, first)
		}
		mismatched := proto.Clone(fixture.Assignment).(*runnerprotocol.AssignmentCommand)
		mismatched.Fence.FencingToken = []byte("different-token")
		if _, err := fixture.Backend.StartAssignment(
			context.Background(),
			mismatched,
			func(runnerprotocol.AssignmentProgressStage) error { return nil },
		); err == nil {
			t.Fatal("backend accepted reused assignment ID with different fencing")
		}
		if _, err := fixture.Backend.FenceAssignment(context.Background(), &runnerprotocol.FenceCommand{
			Fence: mismatched.Fence,
		}); err == nil {
			t.Fatal("backend accepted mismatched fence")
		}
		evidence, err := fixture.Backend.FenceAssignment(context.Background(), &runnerprotocol.FenceCommand{
			Fence: fixture.Assignment.Fence,
		})
		if err != nil ||
			evidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED ||
			evidence.TerminationEvidenceDigest == "" {
			t.Fatalf("exact fence evidence = %+v, %v", evidence, err)
		}
		evidence, err = fixture.Backend.FenceAssignment(context.Background(), &runnerprotocol.FenceCommand{
			Fence: fixture.Assignment.Fence,
		})
		if err != nil || evidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED {
			t.Fatalf("repeated fence evidence = %+v, %v", evidence, err)
		}
	})
}

func equalStages(left, right []runnerprotocol.AssignmentProgressStage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
