package runnercontrol

import (
	"slices"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestLoadRunnerProtocolConfigRequestsOnlyImplementedFeatures(t *testing.T) {
	for name, value := range map[string]string{
		"SECONDBOX_RUNNER_ID":                        "runner-1",
		"SECONDBOX_RUNNER_POOL_ID":                   "pool-1",
		"SECONDBOX_RUNNER_SOFTWARE_VERSION":          "1.0.0",
		"SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS":     "127.0.0.1:9443",
		"SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME": "control-plane",
		"SECONDBOX_RUNNER_CLIENT_CERTIFICATE":        "/run/identity/runner.crt",
		"SECONDBOX_RUNNER_CLIENT_KEY":                "/run/identity/runner.key",
		"SECONDBOX_RUNNER_CONTROL_PLANE_CA":          "/run/identity/runner-ca.crt",
		"SECONDBOX_RUNNER_CREDENTIAL":                "runner-test-credential-material-0000000000",
	} {
		t.Setenv(name, value)
	}

	config, _, err := LoadRunnerProtocolConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := []runnerprotocol.RunnerFeature{
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
	}
	if !slices.Equal(config.MandatoryFeatures, want) {
		t.Fatalf("Runner requested features = %v, want %v", config.MandatoryFeatures, want)
	}
}
