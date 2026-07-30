package runnercontrol

import (
	"context"
	"crypto/x509"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestNewServerRequiresRelayOnlyForDataPlaneFeatures(t *testing.T) {
	config := validRelayServerConfig()
	config.EnabledFeatures = []runnerv1.RunnerFeature{
		runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
	}
	if _, err := NewServer(config); err != nil {
		t.Fatalf("control-only server: %v", err)
	}
	config.EnabledFeatures = append(
		config.EnabledFeatures,
		runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
	)
	if _, err := NewServer(config); err == nil {
		t.Fatal("data-plane server accepted a nil durable relay")
	}
	config.FrameRelay = &recordingFrameRelay{}
	if _, err := NewServer(config); err != nil {
		t.Fatalf("data-plane server with relay: %v", err)
	}
}

func TestClaimedRelayFrameAcknowledgesOnlySuccessfulSend(t *testing.T) {
	for _, test := range []struct {
		name      string
		sendError error
		delivered bool
	}{
		{name: "success", delivered: true},
		{name: "transport_failure", sendError: errors.New("transport failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := negotiatedRelaySession(t)
			if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
				t.Fatal(err)
			}
			relay := &recordingFrameRelay{
				delivery: RelayDelivery{
					ID: "relay-1",
					Message: &runnerv1.ControlPlaneToRunner{
						Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
							Fence: relayTestFence(), OperationId: "operation-1", StreamId: "stream-1", Sequence: 1,
							Payload: &runnerv1.ExecFrame_Open{Open: &runnerv1.ExecOpen{
								Command: &runnerv1.ExecOpen_Shell{Shell: "true"}, OutputLimitBytes: 1024,
							}},
						}},
					},
				},
			}
			server := &Server{config: ServerConfig{FrameRelay: relay, Now: time.Now}}
			sender := &recordingControlPlaneSender{err: test.sendError}
			err := server.sendClaimedRelayFrame(
				t.Context(), sender, session, "runner-1", "connection-1",
			)
			if test.sendError != nil && err == nil {
				t.Fatal("failed relay send returned nil")
			}
			if relay.delivered != test.delivered {
				t.Fatalf("relay delivered = %t, want %t", relay.delivered, test.delivered)
			}
		})
	}
}

func TestOutboundPumpPrioritizesControlCommandsOverRelayFrames(t *testing.T) {
	session := negotiatedRelaySession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	relay := &recordingFrameRelay{
		delivery: RelayDelivery{
			ID: "relay-1",
			Message: &runnerv1.ControlPlaneToRunner{
				Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
					Fence: relayTestFence(), OperationId: "operation-1", StreamId: "stream-1", Sequence: 1,
					Payload: &runnerv1.ExecFrame_Cancel{
						Cancel: &runnerv1.ExecCancel{Reason: "cancel"},
					},
				}},
			},
		},
	}
	state := &priorityStateStore{
		command: CommandDelivery{
			ID: "fence-1",
			Message: &runnerv1.ControlPlaneToRunner{
				Message: &runnerv1.ControlPlaneToRunner_Fence{Fence: &runnerv1.FenceCommand{
					MessageId: "fence-1", Sequence: 1, Fence: relayTestFence(),
					Reason: runnerv1.FenceReason_FENCE_REASON_OPERATOR_REQUEST,
				}},
			},
		},
	}
	server := &Server{config: ServerConfig{
		StateStore: state, FrameRelay: relay, Now: time.Now,
	}}
	sender := &recordingControlPlaneSender{}
	if err := server.sendNextOutboundFrame(
		t.Context(), sender, session, "runner-1", "connection-1",
	); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 || sender.messages[0].GetFence() == nil {
		t.Fatalf("priority outbound messages = %#v", sender.messages)
	}
	if relay.claims != 0 {
		t.Fatalf("relay claims = %d, want zero while control command pending", relay.claims)
	}
	if !state.delivered {
		t.Fatal("control command was not marked delivered")
	}
}

func TestRunnerReceivePumpExitsWhenOwnerCancelsBlockedDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	received := make(chan receivedRunnerFrame, 1)
	secondReceive := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		pumpRunnerFrames(ctx, func() (*runnerv1.RunnerToControlPlane, error) {
			if calls.Add(1) == 2 {
				close(secondReceive)
			}
			return &runnerv1.RunnerToControlPlane{}, nil
		}, received)
		close(done)
	}()
	<-secondReceive
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner receive pump leaked after owner cancellation")
	}
}

type recordingFrameRelay struct {
	delivery  RelayDelivery
	delivered bool
	claims    int
}

func (relay *recordingFrameRelay) ClaimOutboundFrame(
	context.Context,
	string,
	string,
	time.Time,
) (RelayDelivery, bool, error) {
	relay.claims++
	if relay.delivery.Message == nil || relay.delivered {
		return RelayDelivery{}, false, nil
	}
	return relay.delivery, true, nil
}

func (relay *recordingFrameRelay) MarkOutboundFrameDelivered(
	_ context.Context,
	id string,
	_ string,
	_ int64,
	_ time.Time,
) error {
	if id != relay.delivery.ID {
		return errors.New("unexpected delivery")
	}
	relay.delivered = true
	return nil
}

func (*recordingFrameRelay) PersistInboundFrame(
	context.Context,
	InboundRelayFrame,
	time.Time,
) (bool, error) {
	return true, nil
}

type recordingControlPlaneSender struct {
	err      error
	messages []*runnerv1.ControlPlaneToRunner
}

func (sender *recordingControlPlaneSender) Send(message *runnerv1.ControlPlaneToRunner) error {
	sender.messages = append(sender.messages, message)
	return sender.err
}

type relayCredentialVerifier struct{}

func (relayCredentialVerifier) VerifyClientCertificate(
	context.Context,
	*x509.Certificate,
	string,
) (RunnerIdentity, error) {
	return RunnerIdentity{}, nil
}

type relayStateStore struct{}

func (relayStateStore) OpenConnection(context.Context, RunnerIdentity, string, uint32, time.Time) error {
	return nil
}

func (relayStateStore) CloseConnection(context.Context, string, string, time.Time) error {
	return nil
}

func (relayStateStore) RecordRegistration(context.Context, *runnerv1.RunnerRegistration, time.Time) (bool, error) {
	return true, nil
}

func (relayStateStore) RecordHeartbeat(context.Context, *runnerv1.RunnerHeartbeat, time.Time) (bool, error) {
	return true, nil
}

func (relayStateStore) RecordEvent(context.Context, Event, time.Time) (bool, error) {
	return true, nil
}

func (relayStateStore) ClaimCommand(context.Context, string, string, time.Time) (CommandDelivery, bool, error) {
	return CommandDelivery{}, false, nil
}

func (relayStateStore) MarkCommandDelivered(context.Context, string, string, time.Time) error {
	return nil
}

func validRelayServerConfig() ServerConfig {
	return ServerConfig{
		CredentialVerifier:  relayCredentialVerifier{},
		StateStore:          relayStateStore{},
		SupportedVersions:   VersionRange{Minimum: 1, Maximum: 1},
		HeartbeatInterval:   time.Second,
		CommandPollInterval: time.Millisecond,
		Now:                 time.Now,
		NewConnectionID:     func() string { return "connection-1" },
	}
}

type priorityStateStore struct {
	relayStateStore
	command   CommandDelivery
	delivered bool
}

func (store *priorityStateStore) ClaimCommand(
	context.Context,
	string,
	string,
	time.Time,
) (CommandDelivery, bool, error) {
	return store.command, true, nil
}

func (store *priorityStateStore) MarkCommandDelivered(
	_ context.Context,
	id string,
	_ string,
	_ time.Time,
) error {
	if id != store.command.ID {
		return errors.New("unexpected command delivery")
	}
	store.delivered = true
	return nil
}
