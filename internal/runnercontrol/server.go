package runnercontrol

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

const runnerCredentialMetadata = "x-secondbox-runner-credential"

// CredentialVerifier maps the pre-shared credential and mTLS peer to runner authority.
type CredentialVerifier interface {
	VerifyClientCertificate(context.Context, *x509.Certificate, string) (RunnerIdentity, error)
}

// ProtocolStateStore persists connection and runner evidence across replicas.
type ProtocolStateStore interface {
	OpenConnection(context.Context, RunnerIdentity, string, uint32, time.Time) error
	CloseConnection(context.Context, string, string, time.Time) error
	RecordRegistration(context.Context, *runnerv1.RunnerRegistration, time.Time) (bool, error)
	RecordHeartbeat(context.Context, *runnerv1.RunnerHeartbeat, time.Time) (bool, error)
	RecordEvents(context.Context, []EventPersistenceRecord) error
	ClaimCommands(context.Context, string, string, int64, time.Time) ([]CommandDelivery, error)
	MarkCommandsDelivered(context.Context, []CommandDelivery, string) error
}

// EventPersistenceRecord keeps the receive timestamp attached to one ordered durable event.
type EventPersistenceRecord struct {
	Event      Event
	ReceivedAt time.Time
}

// ServerConfig contains explicit protocol compatibility and durable dependencies.
type ServerConfig struct {
	CredentialVerifier  CredentialVerifier
	StateStore          ProtocolStateStore
	FrameRelay          ProtocolFrameRelay
	SupportedVersions   VersionRange
	EnabledFeatures     []runnerv1.RunnerFeature
	HeartbeatInterval   time.Duration
	CommandPollInterval time.Duration
	CommandBatchSize    int64
	EventBatchSize      int
	EventBatchWait      time.Duration
	WorkWakeups         worknotify.Source
	Now                 func() time.Time
	NewConnectionID     func() string
}

// Server terminates the authenticated runner-initiated gRPC stream.
type Server struct {
	runnerv1.UnimplementedRunnerControlServer
	config ServerConfig
}

type controlPlaneFrameSender interface {
	Send(*runnerv1.ControlPlaneToRunner) error
}

type receivedRunnerFrame struct {
	message *runnerv1.RunnerToControlPlane
	err     error
}

type acceptedRunnerEvent struct {
	event      Event
	receivedAt time.Time
}

// NewServer validates the control-plane runner protocol composition.
func NewServer(config ServerConfig) (*Server, error) {
	if config.CredentialVerifier == nil ||
		config.StateStore == nil ||
		config.SupportedVersions.Minimum == 0 ||
		config.SupportedVersions.Minimum > config.SupportedVersions.Maximum ||
		config.HeartbeatInterval <= 0 ||
		config.CommandPollInterval <= 0 ||
		config.CommandBatchSize <= 0 ||
		config.EventBatchSize <= 0 ||
		config.EventBatchWait <= 0 ||
		config.WorkWakeups == nil ||
		config.Now == nil ||
		config.NewConnectionID == nil {
		return nil, errors.New("SecondBox runner control server requires credential, state, protocol, heartbeat, work wakeups, clock, and connection configuration")
	}
	for _, feature := range config.EnabledFeatures {
		if (feature == runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING ||
			feature == runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING ||
			feature == runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY) &&
			config.FrameRelay == nil {
			return nil, errors.New("SecondBox runner control data-plane features require a durable frame relay")
		}
	}
	return &Server{config: config}, nil
}

// Connect negotiates one mTLS-authenticated outbound runner connection.
func (server *Server) Connect(stream runnerv1.RunnerControl_ConnectServer) (returnError error) {
	identity, err := server.peerIdentity(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("SecondBox runner control receive Hello: %w", err)
	}
	connectionID := server.config.NewConnectionID()
	if connectionID == "" {
		return errors.New("SecondBox runner control connection generator returned an empty identifier")
	}
	session := NewSession(SessionConfig{
		AuthenticatedRunnerID: identity.RunnerID,
		SupportedVersions:     server.config.SupportedVersions,
		EnabledFeatures:       server.config.EnabledFeatures,
		HeartbeatInterval:     server.config.HeartbeatInterval,
		ConnectionID:          connectionID,
	})
	negotiation, err := session.Accept(first)
	if err != nil {
		return err
	}
	if negotiation.Response == nil {
		return errors.New("SecondBox runner control negotiation produced no response")
	}
	if err := stream.Send(negotiation.Response); err != nil {
		return fmt.Errorf("SecondBox runner control send negotiation: %w", err)
	}
	if negotiation.Kind == EventRejection {
		return nil
	}
	if err := server.config.StateStore.OpenConnection(
		stream.Context(), identity, connectionID,
		negotiation.GetWelcome().SelectedVersion, server.config.Now(),
	); err != nil {
		return err
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(
			context.WithoutCancel(stream.Context()),
			5*time.Second,
		)
		defer cancel()
		returnError = errors.Join(returnError, server.config.StateStore.CloseConnection(
			closeContext,
			identity.RunnerID,
			connectionID,
			server.config.Now(),
		))
	}()
	received := make(chan receivedRunnerFrame, 1)
	go pumpRunnerFrames(stream.Context(), stream.Recv, received)
	var outboundFailures <-chan error
	var pending *acceptedRunnerEvent
	for {
		var accepted acceptedRunnerEvent
		if pending != nil {
			accepted = *pending
			pending = nil
		} else {
			select {
			case <-stream.Context().Done():
				return stream.Context().Err()
			case frame := <-received:
				accepted, err = server.acceptRunnerFrame(session, frame)
				if err != nil {
					return err
				}
			case err := <-outboundFailures:
				return err
			}
		}
		if durableRunnerEvent(accepted.event.Kind) {
			batch, next, terminalErr := server.collectDurableEventBatch(
				stream.Context(),
				session,
				accepted,
				received,
				outboundFailures,
			)
			persistContext := stream.Context()
			cancelPersistence := func() {}
			if terminalErr != nil {
				persistContext, cancelPersistence = context.WithTimeout(
					context.WithoutCancel(stream.Context()),
					5*time.Second,
				)
			}
			persistErr := server.persistDurableEventBatch(persistContext, batch)
			cancelPersistence()
			if persistErr != nil || terminalErr != nil {
				return errors.Join(persistErr, terminalErr)
			}
			pending = next
			continue
		}
		if err := server.persistEvent(
			stream.Context(),
			accepted.event,
			accepted.receivedAt,
		); err != nil {
			return err
		}
		if accepted.event.Kind == EventRegistration {
			failures := make(chan error, 1)
			outboundFailures = failures
			go server.pumpOutboundFrames(
				stream.Context(),
				stream,
				session,
				identity.RunnerID,
				connectionID,
				failures,
			)
		}
	}
}

func (server *Server) acceptRunnerFrame(
	session *Session,
	frame receivedRunnerFrame,
) (acceptedRunnerEvent, error) {
	if frame.err != nil {
		return acceptedRunnerEvent{}, fmt.Errorf("SecondBox runner control receive: %w", frame.err)
	}
	event, err := session.Accept(frame.message)
	if err != nil {
		return acceptedRunnerEvent{}, err
	}
	return acceptedRunnerEvent{event: event, receivedAt: server.config.Now()}, nil
}

func (server *Server) collectDurableEventBatch(
	ctx context.Context,
	session *Session,
	first acceptedRunnerEvent,
	received <-chan receivedRunnerFrame,
	outboundFailures <-chan error,
) ([]EventPersistenceRecord, *acceptedRunnerEvent, error) {
	records := []EventPersistenceRecord{{
		Event: first.event, ReceivedAt: first.receivedAt,
	}}
	if server.config.EventBatchSize == 1 {
		return records, nil, nil
	}
	timer := time.NewTimer(server.config.EventBatchWait)
	defer timer.Stop()
	for len(records) < server.config.EventBatchSize {
		select {
		case <-ctx.Done():
			return records, nil, ctx.Err()
		case frame := <-received:
			accepted, err := server.acceptRunnerFrame(session, frame)
			if err != nil {
				return records, nil, err
			}
			if !durableRunnerEvent(accepted.event.Kind) {
				return records, &accepted, nil
			}
			records = append(records, EventPersistenceRecord{
				Event: accepted.event, ReceivedAt: accepted.receivedAt,
			})
		case err := <-outboundFailures:
			return records, nil, err
		case <-timer.C:
			return records, nil, nil
		}
	}
	return records, nil, nil
}

func (server *Server) persistDurableEventBatch(
	ctx context.Context,
	records []EventPersistenceRecord,
) error {
	persistStartedAt := time.Now()
	if err := server.config.StateStore.RecordEvents(ctx, records); err != nil {
		return err
	}
	persistedAt := server.config.Now()
	persistenceDuration := time.Since(persistStartedAt)
	for _, record := range records {
		_, sequence, err := runnerEnvelope(record.Event.Message)
		if err != nil {
			return err
		}
		slog.Info(
			"SecondBox runner control event persisted",
			"runnerId", record.Event.RunnerID,
			"kind", runnerEventSubtype(record.Event.Message),
			"sequence", sequence,
			"batchSize", len(records),
			"persistenceMs", persistenceDuration.Milliseconds(),
			"observedToPersistMs",
			runnerObservedToPersistDuration(record.Event.Message, persistedAt).Milliseconds(),
		)
	}
	return nil
}

func pumpRunnerFrames(
	ctx context.Context,
	recv func() (*runnerv1.RunnerToControlPlane, error),
	received chan<- receivedRunnerFrame,
) {
	for {
		message, err := recv()
		select {
		case received <- receivedRunnerFrame{message: message, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (server *Server) pumpOutboundFrames(
	ctx context.Context,
	stream controlPlaneFrameSender,
	session *Session,
	runnerID string,
	connectionID string,
	failures chan<- error,
) {
	ticker := time.NewTicker(server.config.CommandPollInterval)
	defer ticker.Stop()
	wakeups, cancelWakeups := server.config.WorkWakeups.Subscribe(
		worknotify.KindRunnerCommand,
		runnerID,
	)
	defer cancelWakeups()
	for {
		if err := server.drainOutboundFrames(
			ctx, stream, session, runnerID, connectionID,
		); err != nil {
			select {
			case failures <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wakeups:
		}
	}
}

func (server *Server) drainOutboundFrames(
	ctx context.Context,
	stream controlPlaneFrameSender,
	session *Session,
	runnerID string,
	connectionID string,
) error {
	for {
		more, err := server.sendNextOutboundFrame(
			ctx, stream, session, runnerID, connectionID,
		)
		if err != nil || !more {
			return err
		}
	}
}

func (server *Server) sendNextOutboundFrame(
	ctx context.Context,
	stream controlPlaneFrameSender,
	session *Session,
	runnerID string,
	connectionID string,
) (bool, error) {
	deliveryStartedAt := time.Now()
	claimAt := server.config.Now()
	deliveries, err := server.config.StateStore.ClaimCommands(
		ctx,
		runnerID,
		connectionID,
		server.config.CommandBatchSize,
		claimAt,
	)
	if err != nil {
		return false, err
	}
	claimDuration := time.Since(deliveryStartedAt)
	if len(deliveries) == 0 {
		return false, server.sendClaimedRelayFrame(
			ctx, stream, session, runnerID, connectionID,
		)
	}
	deliveryDurations := make([]time.Duration, len(deliveries))
	streamSendDurations := make([]time.Duration, len(deliveries))
	for index := range deliveries {
		delivery := &deliveries[index]
		streamSendStartedAt := time.Now()
		if err := stream.Send(delivery.Message); err != nil {
			persistErr := server.config.StateStore.MarkCommandsDelivered(
				ctx,
				deliveries[:index],
				connectionID,
			)
			if persistErr == nil {
				logCommandDeliveries(
					runnerID,
					deliveries[:index],
					claimAt,
					deliveryDurations[:index],
					claimDuration,
					streamSendDurations[:index],
				)
			}
			return false, errors.Join(
				fmt.Errorf("SecondBox runner control command send: %w", err),
				persistErr,
			)
		}
		streamSendDurations[index] = time.Since(streamSendStartedAt)
		delivery.DeliveredAt = server.config.Now()
		deliveryDurations[index] = time.Since(deliveryStartedAt)
	}
	if err := server.config.StateStore.MarkCommandsDelivered(
		ctx,
		deliveries,
		connectionID,
	); err != nil {
		return false, err
	}
	logCommandDeliveries(
		runnerID, deliveries, claimAt, deliveryDurations,
		claimDuration, streamSendDurations,
	)
	if int64(len(deliveries)) < server.config.CommandBatchSize {
		return false, server.sendClaimedRelayFrame(
			ctx, stream, session, runnerID, connectionID,
		)
	}
	return true, nil
}

func logCommandDeliveries(
	runnerID string,
	deliveries []CommandDelivery,
	claimedAt time.Time,
	deliveryDurations []time.Duration,
	claimDuration time.Duration,
	streamSendDurations []time.Duration,
) {
	for index, delivery := range deliveries {
		slog.Info(
			"SecondBox runner control command delivered",
			"runnerId", runnerID,
			"commandId", delivery.ID,
			"kind", delivery.Kind,
			"batchSize", len(deliveries),
			"queueMs", commandQueueDuration(delivery.CreatedAt, claimedAt).Milliseconds(),
			"deliveryMs", deliveryDurations[index].Milliseconds(),
			"claimMs", claimDuration.Milliseconds(),
			"streamSendMs", streamSendDurations[index].Milliseconds(),
		)
	}
}

func commandQueueDuration(createdAt, claimedAt time.Time) time.Duration {
	if createdAt.IsZero() {
		return 0
	}
	return max(claimedAt.Sub(createdAt), 0)
}

func durableRunnerEvent(kind EventKind) bool {
	switch kind {
	case EventAssignment, EventFence, EventDrain, EventEvidence,
		EventInstanceTerminal, EventLocalWorkspace:
		return true
	default:
		return false
	}
}

func runnerEventSubtype(message *runnerv1.RunnerToControlPlane) string {
	switch {
	case message.GetAssignmentAck() != nil:
		return "assignment_ack"
	case message.GetAssignmentProgress() != nil:
		return "assignment_progress"
	case message.GetAssignmentResult() != nil:
		return "assignment_result"
	case message.GetFenceResult() != nil:
		return "fence_result"
	case message.GetDrainState() != nil:
		return "drain_state"
	case message.GetEvidence() != nil:
		return "evidence"
	case message.GetInstanceTerminal() != nil:
		return "instance_terminal"
	case message.GetLocalWorkspaceResult() != nil:
		return "local_workspace_result"
	default:
		return "unknown"
	}
}

func runnerObservedToPersistDuration(
	message *runnerv1.RunnerToControlPlane,
	persistedAt time.Time,
) time.Duration {
	progress := message.GetAssignmentProgress()
	if progress == nil || progress.ObservedAtUnixNs == 0 {
		return 0
	}
	observedAt := time.Unix(0, int64(progress.ObservedAtUnixNs))
	return max(persistedAt.Sub(observedAt), 0)
}

func (server *Server) persistEvent(ctx context.Context, event Event, receivedAt time.Time) error {
	switch event.Kind {
	case EventDuplicate:
		return nil
	case EventRegistration:
		_, err := server.config.StateStore.RecordRegistration(ctx, event.Registration, receivedAt)
		return err
	case EventHeartbeat:
		_, err := server.config.StateStore.RecordHeartbeat(ctx, event.Heartbeat, receivedAt)
		return err
	case EventAssignment, EventFence, EventDrain, EventEvidence, EventInstanceTerminal,
		EventLocalWorkspace:
		return errors.New("SecondBox runner durable event bypassed the persistence batch")
	case EventExec, EventPty, EventFile, EventPort:
		if server.config.FrameRelay == nil {
			return errors.New("SecondBox runner control data-plane relay is not configured")
		}
		_, err := server.config.FrameRelay.PersistInboundFrame(ctx, InboundRelayFrame{
			RunnerID:     event.RunnerID,
			ConnectionID: event.ConnectionID,
			Message:      event.Message,
		}, receivedAt)
		return err
	default:
		return fmt.Errorf("SecondBox runner control received unexpected event %q", event.Kind)
	}
}

func (server *Server) sendClaimedRelayFrame(
	ctx context.Context,
	stream controlPlaneFrameSender,
	session *Session,
	runnerID string,
	connectionID string,
) error {
	if server.config.FrameRelay == nil {
		return nil
	}
	delivery, found, err := server.config.FrameRelay.ClaimOutboundFrame(
		ctx, runnerID, connectionID, server.config.Now(),
	)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if delivery.ID == "" || delivery.Message == nil {
		return errors.New("SecondBox runner relay returned an incomplete outbound delivery")
	}
	if err := session.ValidateOutboundRelayFrame(delivery.Message); err != nil {
		return fmt.Errorf("SecondBox runner relay outbound frame %q: %w", delivery.ID, err)
	}
	if err := stream.Send(delivery.Message); err != nil {
		return fmt.Errorf("SecondBox runner relay send frame %q: %w", delivery.ID, err)
	}
	if err := server.config.FrameRelay.MarkOutboundFrameDelivered(
		ctx, delivery.ID, connectionID, delivery.ClaimAttempt, server.config.Now(),
	); err != nil {
		return fmt.Errorf("SecondBox runner relay mark frame %q delivered: %w", delivery.ID, err)
	}
	return nil
}

func (server *Server) peerIdentity(ctx context.Context) (RunnerIdentity, error) {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok {
		return RunnerIdentity{}, errors.New("SecondBox runner control peer identity is absent")
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) != 1 {
		return RunnerIdentity{}, errors.New("SecondBox runner control peer is not mutually authenticated")
	}
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return RunnerIdentity{}, ErrRunnerCredentialInvalid
	}
	credentials := incoming.Get(runnerCredentialMetadata)
	if len(credentials) != 1 || credentials[0] == "" {
		return RunnerIdentity{}, ErrRunnerCredentialInvalid
	}
	return server.config.CredentialVerifier.VerifyClientCertificate(
		ctx, tlsInfo.State.PeerCertificates[0], credentials[0],
	)
}
