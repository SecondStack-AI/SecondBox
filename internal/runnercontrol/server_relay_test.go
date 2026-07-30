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
		StateStore: state, FrameRelay: relay, CommandBatchSize: 1, Now: time.Now,
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

func TestOutboundPumpDrainsOnlyTheConfiguredCommandBatch(t *testing.T) {
	state := &queuedCommandStateStore{
		deliveries: []CommandDelivery{
			controlCommandDelivery("fence-1"),
			controlCommandDelivery("fence-2"),
			controlCommandDelivery("fence-3"),
		},
	}
	server := &Server{config: ServerConfig{
		StateStore: state, CommandBatchSize: 2, Now: time.Now,
	}}
	sender := &recordingControlPlaneSender{}
	if err := server.sendNextOutboundFrame(
		t.Context(), sender, nil, "runner-1", "connection-1",
	); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 2 {
		t.Fatalf("outbound batch sent %d commands, want 2", len(sender.messages))
	}
	if len(state.delivered) != 2 ||
		state.delivered[0] != "fence-1" ||
		state.delivered[1] != "fence-2" {
		t.Fatalf("delivered commands = %#v", state.delivered)
	}
	if state.claims != 2 {
		t.Fatalf("command claims = %d, want 2", state.claims)
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

func TestDurableEventBatchStopsBeforeHeartbeatOrderingBoundary(t *testing.T) {
	session := negotiatedSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	server := &Server{config: ServerConfig{
		EventBatchSize: 8,
		EventBatchWait: time.Second,
		Now:            func() time.Time { return now },
	}}
	first, err := server.acceptRunnerFrame(
		session,
		receivedRunnerFrame{message: evidenceFrame(2)},
	)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan receivedRunnerFrame, 2)
	received <- receivedRunnerFrame{message: evidenceFrame(3)}
	received <- receivedRunnerFrame{
		message: heartbeatFrame("runner-1", "connection-1", "heartbeat-4", 4),
	}
	records, pending, terminalErr := server.collectDurableEventBatch(
		t.Context(),
		session,
		first,
		received,
		nil,
	)
	if terminalErr != nil {
		t.Fatal(terminalErr)
	}
	if len(records) != 2 {
		t.Fatalf("durable event batch size = %d, want 2", len(records))
	}
	if pending == nil || pending.event.Kind != EventHeartbeat {
		t.Fatalf("pending ordering-boundary event = %#v, want heartbeat", pending)
	}
	for index, wantSequence := range []uint64{2, 3} {
		_, sequence, err := runnerEnvelope(records[index].Event.Message)
		if err != nil {
			t.Fatal(err)
		}
		if sequence != wantSequence {
			t.Fatalf("batch sequence[%d] = %d, want %d", index, sequence, wantSequence)
		}
		if !records[index].ReceivedAt.Equal(now) {
			t.Fatalf("batch receivedAt[%d] = %v, want %v", index, records[index].ReceivedAt, now)
		}
	}
}

func TestOutboundPumpReportsCommandDeliveryFailure(t *testing.T) {
	server := &Server{config: ServerConfig{
		StateStore:          failingCommandStateStore{},
		CommandPollInterval: time.Millisecond,
		CommandBatchSize:    1,
		Now:                 time.Now,
	}}
	failures := make(chan error, 1)
	go server.pumpOutboundFrames(
		t.Context(),
		&recordingControlPlaneSender{},
		nil,
		"runner-1",
		"connection-1",
		failures,
	)
	select {
	case err := <-failures:
		if !errors.Is(err, errCommandClaim) {
			t.Fatalf("outbound pump error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("outbound pump did not report command claim failure")
	}
}

func evidenceFrame(sequence uint64) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Evidence{
			Evidence: &runnerv1.Evidence{
				MessageId: "evidence-" + time.Unix(int64(sequence), 0).UTC().Format("150405"),
				Sequence:  sequence,
			},
		},
	}
}

var errCommandClaim = errors.New("command claim failed")

type failingCommandStateStore struct {
	relayStateStore
}

func (failingCommandStateStore) ClaimCommand(
	context.Context,
	string,
	string,
	time.Time,
) (CommandDelivery, bool, error) {
	return CommandDelivery{}, false, errCommandClaim
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

func (relayStateStore) RecordEvents(context.Context, []EventPersistenceRecord) error {
	return nil
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
		CommandBatchSize:    1,
		EventBatchSize:      1,
		EventBatchWait:      time.Millisecond,
		Now:                 time.Now,
		NewConnectionID:     func() string { return "connection-1" },
	}
}

type queuedCommandStateStore struct {
	relayStateStore
	deliveries []CommandDelivery
	delivered  []string
	claims     int
}

func (store *queuedCommandStateStore) ClaimCommand(
	context.Context,
	string,
	string,
	time.Time,
) (CommandDelivery, bool, error) {
	store.claims++
	if len(store.deliveries) == 0 {
		return CommandDelivery{}, false, nil
	}
	return store.deliveries[0], true, nil
}

func (store *queuedCommandStateStore) MarkCommandDelivered(
	_ context.Context,
	id string,
	_ string,
	_ time.Time,
) error {
	if len(store.deliveries) == 0 || store.deliveries[0].ID != id {
		return errors.New("unexpected queued command delivery")
	}
	store.delivered = append(store.delivered, id)
	store.deliveries = store.deliveries[1:]
	return nil
}

func controlCommandDelivery(id string) CommandDelivery {
	return CommandDelivery{
		ID: id,
		Message: &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Fence{Fence: &runnerv1.FenceCommand{
				MessageId: id, Sequence: 1, Fence: relayTestFence(),
				Reason: runnerv1.FenceReason_FENCE_REASON_OPERATOR_REQUEST,
			}},
		},
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
