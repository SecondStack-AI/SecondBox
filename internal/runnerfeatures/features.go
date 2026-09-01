package runnerfeatures

import (
	"errors"
	"fmt"
	"slices"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

// Parse resolves the configured control-plane Runner feature names.
func Parse(names []string) ([]runnerv1.RunnerFeature, error) {
	known := map[string]runnerv1.RunnerFeature{
		"exec-streaming":        runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
		"file-streaming":        runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING,
		"pty":                   runnerv1.RunnerFeature_RUNNER_FEATURE_PTY,
		"port-proxy":            runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
		"evidence":              runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		"local-workspace":       runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
		"tenant-egress-context": runnerv1.RunnerFeature_RUNNER_FEATURE_TENANT_EGRESS_CONTEXT,
	}
	features := make([]runnerv1.RunnerFeature, 0, len(names))
	for _, name := range names {
		feature, exists := known[name]
		if !exists {
			return nil, fmt.Errorf("SecondBox runner feature %q is unsupported", name)
		}
		features = append(features, feature)
	}
	if !slices.Contains(
		features,
		runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	) {
		return nil, errors.New("SecondBox runner features require local-workspace")
	}
	// Generation 4 makes context-aware assignments a compiled protocol fact,
	// not an operator-disableable capability or mixed-fleet compatibility knob.
	if !slices.Contains(
		features,
		runnerv1.RunnerFeature_RUNNER_FEATURE_TENANT_EGRESS_CONTEXT,
	) {
		features = append(
			features,
			runnerv1.RunnerFeature_RUNNER_FEATURE_TENANT_EGRESS_CONTEXT,
		)
	}
	return features, nil
}
