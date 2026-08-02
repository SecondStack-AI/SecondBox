package runnercontrol

import (
	"context"
	"errors"
	"sync"
	"testing"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

// TestDirectPortConsumptionIsAnsweredInlineOnTheAuthenticatedStream proves the
// verdict is a stream answer rather than a durable frame, and that a home
// Runner's claim is spent through PostgreSQL rather than trusted.
func TestDirectPortConsumptionIsAnsweredInlineOnTheAuthenticatedStream(t *testing.T) {
	for name, testCase := range map[string]struct {
		admitter   DirectPortAdmitter
		wantKind   runnerv1.PortDirectAdmissionKind
		wantSpent  bool
		wantDetail bool
	}{
		"admitted": {
			admitter:  &recordingDirectPortAdmitter{},
			wantKind:  runnerv1.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_ADMITTED,
			wantSpent: true,
		},
		"denied_by_postgres": {
			admitter:   &recordingDirectPortAdmitter{failure: ports.ErrPortTokenConsumed},
			wantKind:   runnerv1.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_DENIED,
			wantSpent:  true,
			wantDetail: true,
		},
		"transport_unavailable": {
			wantKind:   runnerv1.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_DENIED,
			wantDetail: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := validRelayServerConfig()
			config.FrameRelay = &recordingFrameRelay{}
			config.EnabledFeatures = []runnerv1.RunnerFeature{
				runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
			}
			if testCase.admitter != nil {
				config.DirectPorts = testCase.admitter
			}
			server, err := NewServer(config)
			if err != nil {
				t.Fatal(err)
			}
			sender := &recordingControlPlaneSender{}
			consume := &runnerv1.PortDirectConsume{
				MessageId: "port-direct-1", Sequence: 4,
				Fence:       relayTestFence(),
				OperationId: "port-1", StreamId: "port-stream-1",
				CredentialDigest: []byte("credential-digest-value-000000000"),
			}
			if err := server.answerDirectPortConsumption(
				t.Context(), sender, "runner-1", consume, config.Now(),
			); err != nil {
				t.Fatal(err)
			}
			messages := sender.messages
			if len(messages) != 1 {
				t.Fatalf("direct Port admission frame count = %d", len(messages))
			}
			admission := messages[0].GetPortDirectAdmission()
			if admission == nil || admission.MessageId != consume.MessageId ||
				admission.OperationId != consume.OperationId ||
				admission.StreamId != consume.StreamId ||
				admission.Kind != testCase.wantKind {
				t.Fatalf("direct Port admission = %#v", admission)
			}
			if testCase.wantDetail == (admission.SafeDetail == "") {
				t.Fatalf("direct Port admission detail = %q", admission.SafeDetail)
			}
			if recorder, ok := testCase.admitter.(*recordingDirectPortAdmitter); ok {
				spent := recorder.spent()
				if (spent != nil) != testCase.wantSpent {
					t.Fatalf("credential consumption request = %#v", spent)
				}
				if spent != nil && (spent.RunnerID != "runner-1" ||
					spent.SessionID != consume.OperationId ||
					spent.AssignmentID != consume.Fence.AssignmentId ||
					spent.Generation != int64(consume.Fence.SandboxGeneration) ||
					string(spent.CredentialDigest) != string(consume.CredentialDigest)) {
					t.Fatalf("credential consumption authority = %#v", spent)
				}
			}
			// A verdict is never enqueued as a durable frame.
			if relay, ok := config.FrameRelay.(*recordingFrameRelay); ok && relay.claims > 0 {
				t.Fatal("direct Port admission reached the durable relay")
			}
		})
	}
}

func TestDirectPortConsumptionRejectsIncompleteIdentity(t *testing.T) {
	config := validRelayServerConfig()
	config.FrameRelay = &recordingFrameRelay{}
	config.DirectPorts = &recordingDirectPortAdmitter{}
	config.EnabledFeatures = []runnerv1.RunnerFeature{
		runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	for name, consume := range map[string]*runnerv1.PortDirectConsume{
		"absent":        nil,
		"no_fence":      {MessageId: "m", OperationId: "port-1", CredentialDigest: []byte("d")},
		"no_operation":  {MessageId: "m", Fence: relayTestFence(), CredentialDigest: []byte("d")},
		"no_credential": {MessageId: "m", Fence: relayTestFence(), OperationId: "port-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := server.answerDirectPortConsumption(
				t.Context(), &recordingControlPlaneSender{}, "runner-1", consume, config.Now(),
			); !errors.Is(err, ErrRunnerMessage) {
				t.Fatalf("incomplete direct Port consumption error = %v", err)
			}
		})
	}
}

func TestSessionClassifiesDirectPortConsumptionWithADurableEnvelope(t *testing.T) {
	session := negotiatedRelaySession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	event, err := session.Accept(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_PortDirectConsume{
			PortDirectConsume: &runnerv1.PortDirectConsume{
				MessageId: "port-direct-1", Sequence: 2,
				Fence: relayTestFence(), OperationId: "port-1", StreamId: "port-stream-1",
				CredentialDigest: []byte("credential-digest"),
			},
		},
	})
	if err != nil || event.Kind != EventPortDirect {
		t.Fatalf("direct Port consumption event = %#v, %v", event, err)
	}
	if durableRunnerEvent(event.Kind) {
		t.Fatal("direct Port consumption was classified as a durable batched event")
	}
	if _, err := session.Accept(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_PortDirectConsume{
			PortDirectConsume: &runnerv1.PortDirectConsume{
				Fence: relayTestFence(), OperationId: "port-1", StreamId: "port-stream-1",
				CredentialDigest: []byte("credential-digest"),
			},
		},
	}); !errors.Is(err, ErrRunnerMessage) {
		t.Fatalf("direct Port consumption without an envelope error = %v", err)
	}
}

type recordingDirectPortAdmitter struct {
	failure error
	mu      sync.Mutex
	request *DirectPortConsumption
}

func (admitter *recordingDirectPortAdmitter) ConsumeDirectPortSession(
	_ context.Context,
	input DirectPortConsumption,
) (PortTunnel, error) {
	admitter.mu.Lock()
	admitter.request = &input
	admitter.mu.Unlock()
	if admitter.failure != nil {
		return PortTunnel{}, admitter.failure
	}
	return PortTunnel{}, nil
}

func (admitter *recordingDirectPortAdmitter) spent() *DirectPortConsumption {
	admitter.mu.Lock()
	defer admitter.mu.Unlock()
	return admitter.request
}
