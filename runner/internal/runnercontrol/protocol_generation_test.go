package runnercontrol

import (
	"strings"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestRunnerRejectsNegotiatedProtocolGenerationOutsideSupportedWindow(t *testing.T) {
	stream := &recordingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{{
			Message: &runnerprotocol.ControlPlaneToRunner_Welcome{
				Welcome: &runnerprotocol.RunnerWelcome{
					ConnectionId:        "unsupported-generation-connection",
					SelectedVersion:     3,
					HeartbeatIntervalMs: 60_000,
				},
			},
		}},
	}
	config := testRunnerConfig()
	config.ProtocolMinimum = 1
	config.ProtocolMaximum = 2
	service, err := NewRunnerProtocolService(
		config,
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: stream},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.runProtocolSession(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Welcome selected invalid connection parameters") {
		t.Fatalf("out-of-window negotiated generation error = %v", err)
	}
	if len(stream.outbound) != 1 || stream.outbound[0].GetHello() == nil {
		t.Fatalf("outbound frames before rejection = %#v, want Hello only", stream.outbound)
	}
}
