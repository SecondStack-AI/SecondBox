package runnercontrol

import (
	"errors"
	"io"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestRunnerUpgradeRegistersTheNegotiatedLowerProtocolGeneration(t *testing.T) {
	stream := &recordingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{{
			Message: &runnerprotocol.ControlPlaneToRunner_Welcome{
				Welcome: &runnerprotocol.RunnerWelcome{
					ConnectionId:        "rolling-control-plane-connection",
					SelectedVersion:     1,
					EnabledFeatures:     []runnerprotocol.RunnerFeature{runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE},
					HeartbeatIntervalMs: 60_000,
				},
			},
		}},
	}
	backend := &recordingAssignmentBackend{readiness: BackendReadiness{
		Capacity: &runnerprotocol.Capacity{},
		Reserved: &runnerprotocol.Capacity{},
		Capabilities: &runnerprotocol.RunnerCapabilities{
			KvmReady: true, JailerReady: true, CgroupReady: true,
			NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
			GuestProtocolGenerations: &runnerprotocol.ProtocolVersionRange{Minimum: 1, Maximum: 1},
		},
	}}
	config := testRunnerConfig()
	config.SoftwareVersion = "2.0.0"
	config.ProtocolMaximum = 2
	service, err := NewRunnerProtocolService(
		config,
		backend,
		staticProtocolConnector{stream: stream},
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := service.Run(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("Run() error = %v, want stream EOF after registration", err)
	}
	if got := stream.outbound[0].GetHello().GetSupportedVersions(); got.GetMinimum() != 1 || got.GetMaximum() != 2 {
		t.Fatalf("Runner Hello window = %d..%d, want 1..2", got.GetMinimum(), got.GetMaximum())
	}
	if got := stream.outbound[1].GetRegistration().GetProtocolVersion(); got != 1 {
		t.Fatalf("Runner Registration protocol generation = %d, want negotiated generation 1", got)
	}
}
