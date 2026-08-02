package main

import (
	"strings"
	"testing"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestConfiguredRunnerFeaturesRequireLocalWorkspace(t *testing.T) {
	if _, err := configuredRunnerFeatures([]string{"evidence"}); err == nil ||
		!strings.Contains(err.Error(), "require local-workspace") {
		t.Fatalf("portable-only runner feature config error = %v", err)
	}
	features, err := configuredRunnerFeatures([]string{"evidence", "local-workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != 2 ||
		features[1] != runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE {
		t.Fatalf("configured features = %v", features)
	}
}
