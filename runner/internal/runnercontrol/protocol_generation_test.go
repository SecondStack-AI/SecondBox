package runnercontrol

import (
	"context"
	"errors"
	"strings"
	"testing"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type protocolGenerationBackend struct {
	recordingAssignmentBackend
}

func (*protocolGenerationBackend) OpenWorkspaceRelocationExport(
	context.Context,
	*runnerprotocol.LocalWorkspaceCommand,
) (WorkspaceRelocationExport, error) {
	return nil, errors.New("unused Workspace relocation export")
}

func (*protocolGenerationBackend) BeginWorkspaceRelocationImport(
	context.Context,
	*runnerprotocol.WorkspaceTransferFrame,
) (WorkspaceRelocationImport, error) {
	return nil, errors.New("unused Workspace relocation import")
}

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
		&protocolGenerationBackend{},
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
