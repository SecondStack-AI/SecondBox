package runnercontrol

import (
	"testing"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestExecutionImageProgressStagesHavePersistentNames(t *testing.T) {
	tests := map[runnerv1.AssignmentProgressStage]string{
		runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_RESOLVE:  "image_resolve",
		runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_DOWNLOAD: "image_download",
		runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_IMAGE_EXTRACT:  "image_extract",
	}
	for stage, want := range tests {
		got, err := assignmentProgressStageName(stage)
		if err != nil || got != want {
			t.Fatalf("assignment progress stage %s = %q, %v; want %q", stage, got, err, want)
		}
	}
}
