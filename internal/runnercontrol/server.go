package runnercontrol

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
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
	RecordEvent(context.Context, Event, time.Time) (bool, error)
	ClaimCommand(context.Context, string, string, time.Time) (CommandDelivery, bool, error)
	MarkCommandDelivered(context.Context, string, string, time.Time) error
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

// NewServer validates the control-plane runner protocol composition.
func NewServer(config ServerConfig) (*Server, error) {
	if config.CredentialVerifier == nil ||
		config.StateStore == nil ||
		config.SupportedVersions.Minimum == 0 ||
		config.SupportedVersions.Minimum > config.SupportedVersions.Maximum ||
		config.HeartbeatInterval <= 0 ||
		config.CommandPollInterval <= 0 ||
		config.Now == nil ||
		config.NewConnectionID == nil {
		return nil, errors.New("SecondBox runner control server requires credential, state, protocol, heartbeat, clock, and connection configuration")
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
	commandTicker := time.NewTicker(server.config.CommandPollInterval)
	defer commandTicker.Stop()
	registered := false
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case frame := <-received:
			if frame.err != nil {
				return fmt.Errorf("SecondBox runner control receive: %w", frame.err)
			}
			event, err := session.Accept(frame.message)
			if err != nil {
				return err
			}
			if err := server.persistEvent(stream.Context(), event); err != nil {
				return err
			}
			if event.Kind == EventRegistration {
				registered = true
			}
		case <-commandTicker.C:
			if !registered {
				continue
			}
			if err := server.sendNextOutboundFrame(
				stream.Context(), stream, session, identity.RunnerID, connectionID,
			); err != nil {
				return err
			}
		}
	}
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

func (server *Server) sendNextOutboundFrame(
	ctx context.Context,
	stream controlPlaneFrameSender,
	session *Session,
	runnerID string,
	connectionID string,
) error {
	delivery, found, err := server.config.StateStore.ClaimCommand(
		ctx, runnerID, connectionID, server.config.Now(),
	)
	if err != nil {
		return err
	}
	if found {
		if err := stream.Send(delivery.Message); err != nil {
			return fmt.Errorf("SecondBox runner control command send: %w", err)
		}
		if err := server.config.StateStore.MarkCommandDelivered(
			ctx, delivery.ID, connectionID, server.config.Now(),
		); err != nil {
			return err
		}
		return nil
	}
	return server.sendClaimedRelayFrame(ctx, stream, session, runnerID, connectionID)
}

func (server *Server) persistEvent(ctx context.Context, event Event) error {
	switch event.Kind {
	case EventDuplicate:
		return nil
	case EventRegistration:
		_, err := server.config.StateStore.RecordRegistration(ctx, event.Registration, server.config.Now())
		return err
	case EventHeartbeat:
		_, err := server.config.StateStore.RecordHeartbeat(ctx, event.Heartbeat, server.config.Now())
		return err
	case EventAssignment, EventFence, EventDrain, EventEvidence, EventInstanceTerminal,
		EventLocalWorkspace:
		_, err := server.config.StateStore.RecordEvent(ctx, event, server.config.Now())
		return err
	case EventExec, EventPty, EventFile, EventPort:
		if server.config.FrameRelay == nil {
			return errors.New("SecondBox runner control data-plane relay is not configured")
		}
		_, err := server.config.FrameRelay.PersistInboundFrame(ctx, InboundRelayFrame{
			RunnerID:     event.RunnerID,
			ConnectionID: event.ConnectionID,
			Message:      event.Message,
		}, server.config.Now())
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
