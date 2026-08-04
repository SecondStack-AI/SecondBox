package runnercontrol

import (
	"context"
	"crypto/x509"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
)

func TestNewServerRequiresEachConfiguredDataPlaneTransport(t *testing.T) {
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
	config.LiveDataPlane = NewLiveDataPlaneBroker()
	if _, err := NewServer(config); err != nil {
		t.Fatalf("Exec server with live data plane: %v", err)
	}
	config.EnabledFeatures = append(
		config.EnabledFeatures,
		runnerv1.RunnerFeature_RUNNER_FEATURE_PTY,
	)
	if _, err := NewServer(config); err != nil {
		t.Fatalf("PTY server with live data plane: %v", err)
	}
	config.EnabledFeatures = append(
		config.EnabledFeatures,
		runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
	)
	if _, err := NewServer(config); err == nil {
		t.Fatal("Port proxy server accepted a nil durable relay")
	}
	config.FrameRelay = &recordingFrameRelay{}
	if _, err := NewServer(config); err != nil {
		t.Fatalf("Port proxy server with relay: %v", err)
	}
}

func TestLiveDataPlaneConnectionReplacementClosesPriorRoutes(t *testing.T) {
	broker := NewLiveDataPlaneBroker()
	first := &recordingControlPlaneSender{}
	firstSession := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1", SupportedVersions: VersionRange{Minimum: 1, Maximum: 1},
		ConnectionID: "connection-1",
	})
	if _, err := broker.AttachConnection("runner-1", "connection-1", first, firstSession); err != nil {
		t.Fatal(err)
	}
	stream, err := broker.Open("runner-1", "exec", "operation-1", "stream-1")
	if err != nil {
		t.Fatal(err)
	}
	second := &recordingControlPlaneSender{}
	secondSession := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1", SupportedVersions: VersionRange{Minimum: 1, Maximum: 1},
		ConnectionID: "connection-2",
	})
	if _, err := broker.AttachConnection("runner-1", "connection-2", second, secondSession); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&runnerv1.ControlPlaneToRunner{}); !errors.Is(err, ErrLiveDataPlaneUnavailable) {
		t.Fatalf("send on replaced live route error = %v", err)
	}
	if _, err := stream.Receive(t.Context()); !errors.Is(err, ErrLiveDataPlaneUnavailable) {
		t.Fatalf("receive on replaced live route error = %v", err)
	}
	if len(first.messages) != 0 || len(second.messages) != 0 {
		t.Fatalf("replaced route sent frames = %d/%d", len(first.messages), len(second.messages))
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
	more, err := server.sendNextOutboundFrame(
		t.Context(), sender, session, "runner-1", "connection-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !more {
		t.Fatal("full control-command batch did not report remaining work")
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
	more, err := server.sendNextOutboundFrame(
		t.Context(), sender, nil, "runner-1", "connection-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !more {
		t.Fatal("full control-command batch did not report remaining work")
	}
	if len(sender.messages) != 2 {
		t.Fatalf("outbound batch sent %d commands, want 2", len(sender.messages))
	}
	if len(state.delivered) != 2 ||
		state.delivered[0] != "fence-1" ||
		state.delivered[1] != "fence-2" {
		t.Fatalf("delivered commands = %#v", state.delivered)
	}
	if state.claims != 1 {
		t.Fatalf("command batch claims = %d, want 1", state.claims)
	}
}

func TestOutboundDrainContinuesAcrossCommandBatches(t *testing.T) {
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
	if err := server.drainOutboundFrames(
		t.Context(), sender, nil, "runner-1", "connection-1",
	); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 3 || len(state.delivered) != 3 {
		t.Fatalf(
			"outbound drain sent %d and persisted %d commands, want 3",
			len(sender.messages),
			len(state.delivered),
		)
	}
	if state.claims != 2 {
		t.Fatalf("outbound drain batch claims = %d, want 2", state.claims)
	}
}

func TestOutboundBatchPersistsSuccessfulPrefixBeforeSendFailure(t *testing.T) {
	state := &queuedCommandStateStore{
		deliveries: []CommandDelivery{
			controlCommandDelivery("fence-1"),
			controlCommandDelivery("fence-2"),
			controlCommandDelivery("fence-3"),
		},
	}
	sendErr := errors.New("stream send failed")
	server := &Server{config: ServerConfig{
		StateStore: state, CommandBatchSize: 3, Now: time.Now,
	}}
	sender := &prefixFailingControlPlaneSender{failAt: 1, err: sendErr}
	_, err := server.sendNextOutboundFrame(
		t.Context(), sender, nil, "runner-1", "connection-1",
	)
	if !errors.Is(err, sendErr) {
		t.Fatalf("outbound batch error = %v, want send failure", err)
	}
	if len(state.delivered) != 1 || state.delivered[0] != "fence-1" {
		t.Fatalf("persisted successful prefix = %#v, want fence-1", state.delivered)
	}
	if len(state.deliveries) != 2 || state.deliveries[0].ID != "fence-2" {
		t.Fatalf("undelivered suffix = %#v", state.deliveries)
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
		WorkWakeups:         worknotify.NewHub(),
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

func TestOutboundPumpDrainsACommittedNotificationBeforeFallbackPoll(t *testing.T) {
	hub := worknotify.NewHub()
	state := &wakeupCommandStateStore{
		emptyPass: make(chan struct{}, 1),
		delivered: make(chan struct{}, 1),
	}
	server := &Server{config: ServerConfig{
		StateStore:          state,
		CommandPollInterval: time.Hour,
		CommandBatchSize:    1,
		WorkWakeups:         hub,
		Now:                 time.Now,
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failures := make(chan error, 1)
	go server.pumpOutboundFrames(
		ctx,
		&recordingControlPlaneSender{},
		nil,
		"runner-1",
		"connection-1",
		failures,
	)
	select {
	case <-state.emptyPass:
	case <-time.After(time.Second):
		t.Fatal("outbound pump did not perform its immediate drain")
	}
	state.enqueue(controlCommandDelivery("fence-wakeup"))
	startedAt := time.Now()
	hub.Publish(worknotify.KindRunnerCommand, "runner-1")
	select {
	case <-state.delivered:
		if time.Since(startedAt) > 500*time.Millisecond {
			t.Fatalf("committed notification delivery took %s", time.Since(startedAt))
		}
	case err := <-failures:
		t.Fatalf("outbound pump failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("outbound pump waited for its fallback poll after a notification")
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

func (failingCommandStateStore) ClaimCommands(
	context.Context,
	string,
	string,
	int64,
	time.Time,
) ([]CommandDelivery, error) {
	return nil, errCommandClaim
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

type prefixFailingControlPlaneSender struct {
	calls  int
	failAt int
	err    error
}

func (sender *prefixFailingControlPlaneSender) Send(*runnerv1.ControlPlaneToRunner) error {
	call := sender.calls
	sender.calls++
	if call == sender.failAt {
		return sender.err
	}
	return nil
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

func (relayStateStore) ClaimCommands(
	context.Context,
	string,
	string,
	int64,
	time.Time,
) ([]CommandDelivery, error) {
	return []CommandDelivery{}, nil
}

func (relayStateStore) MarkCommandsDelivered(
	context.Context,
	[]CommandDelivery,
	string,
) error {
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
		WorkWakeups:         worknotify.NewHub(),
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

type wakeupCommandStateStore struct {
	relayStateStore
	mu        sync.Mutex
	delivery  *CommandDelivery
	emptyPass chan struct{}
	delivered chan struct{}
}

func (store *wakeupCommandStateStore) enqueue(delivery CommandDelivery) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.delivery = &delivery
}

func (store *wakeupCommandStateStore) ClaimCommands(
	context.Context,
	string,
	string,
	int64,
	time.Time,
) ([]CommandDelivery, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.delivery == nil {
		select {
		case store.emptyPass <- struct{}{}:
		default:
		}
		return []CommandDelivery{}, nil
	}
	return []CommandDelivery{*store.delivery}, nil
}

func (store *wakeupCommandStateStore) MarkCommandsDelivered(
	_ context.Context,
	deliveries []CommandDelivery,
	_ string,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(deliveries) != 1 ||
		store.delivery == nil ||
		store.delivery.ID != deliveries[0].ID {
		return errors.New("unexpected wakeup command delivery")
	}
	store.delivery = nil
	select {
	case store.delivered <- struct{}{}:
	default:
	}
	return nil
}

func (store *queuedCommandStateStore) ClaimCommands(
	_ context.Context,
	_ string,
	_ string,
	limit int64,
	_ time.Time,
) ([]CommandDelivery, error) {
	store.claims++
	if len(store.deliveries) == 0 {
		return []CommandDelivery{}, nil
	}
	count := min(len(store.deliveries), int(limit))
	return append([]CommandDelivery(nil), store.deliveries[:count]...), nil
}

func (store *queuedCommandStateStore) MarkCommandsDelivered(
	_ context.Context,
	deliveries []CommandDelivery,
	_ string,
) error {
	if len(store.deliveries) < len(deliveries) {
		return errors.New("unexpected queued command delivery count")
	}
	for index := range deliveries {
		if store.deliveries[index].ID != deliveries[index].ID {
			return errors.New("unexpected queued command delivery")
		}
		store.delivered = append(store.delivered, deliveries[index].ID)
	}
	store.deliveries = store.deliveries[len(deliveries):]
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

func (store *priorityStateStore) ClaimCommands(
	context.Context,
	string,
	string,
	int64,
	time.Time,
) ([]CommandDelivery, error) {
	return []CommandDelivery{store.command}, nil
}

func (store *priorityStateStore) MarkCommandsDelivered(
	_ context.Context,
	deliveries []CommandDelivery,
	_ string,
) error {
	if len(deliveries) != 1 || deliveries[0].ID != store.command.ID {
		return errors.New("unexpected command delivery")
	}
	store.delivered = true
	return nil
}
