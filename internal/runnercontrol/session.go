// Package runnercontrol authenticates and validates runner-initiated protocol sessions.
package runnercontrol

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/protobuf/proto"
)

var (
	ErrHelloRequired        = errors.New("SecondBox runner control Hello is required")
	ErrRegistrationRequired = errors.New("SecondBox runner control Registration is required")
	ErrSequenceReordered    = errors.New("SecondBox runner control sequence is reordered")
	ErrRunnerPrerequisites  = errors.New("SecondBox runner prerequisites failed")
	ErrRunnerMessage        = errors.New("SecondBox runner control message is invalid")
)

type EventKind string

const (
	EventWelcome          EventKind = "welcome"
	EventRegistration     EventKind = "registration"
	EventHeartbeat        EventKind = "heartbeat"
	EventAssignment       EventKind = "assignment"
	EventFence            EventKind = "fence"
	EventDrain            EventKind = "drain"
	EventEvidence         EventKind = "evidence"
	EventLocalWorkspace   EventKind = "local_workspace"
	EventExec             EventKind = "exec"
	EventFile             EventKind = "file"
	EventPort             EventKind = "port"
	EventInstanceTerminal EventKind = "instance_terminal"
	EventDuplicate        EventKind = "duplicate"
	EventRejection        EventKind = "rejection"
)

// VersionRange is the control plane's supported runner protocol window.
type VersionRange struct {
	Minimum uint32
	Maximum uint32
}

// SessionConfig binds certificate identity to one protocol connection.
type SessionConfig struct {
	AuthenticatedRunnerID string
	SupportedVersions     VersionRange
	EnabledFeatures       []runnerv1.RunnerFeature
	HeartbeatInterval     time.Duration
	ConnectionID          string
}

// Event is one validated runner message or one negotiation response.
type Event struct {
	Kind         EventKind
	RunnerID     string
	ConnectionID string
	Response     *runnerv1.ControlPlaneToRunner
	Registration *runnerv1.RunnerRegistration
	Heartbeat    *runnerv1.RunnerHeartbeat
	Message      *runnerv1.RunnerToControlPlane
}

func (event Event) GetWelcome() *runnerv1.RunnerWelcome {
	if event.Response == nil {
		return nil
	}
	return event.Response.GetWelcome()
}

func (event Event) GetRejection() *runnerv1.ProtocolRejection {
	if event.Response == nil {
		return nil
	}
	return event.Response.GetRejection()
}

// Session enforces negotiation, registration, identity, connection, and ordering.
type Session struct {
	config          SessionConfig
	helloAccepted   bool
	registered      bool
	selectedVersion uint32
	lastSequence    uint64
	messageIDs      map[string]struct{}
	enabledFeatures map[runnerv1.RunnerFeature]bool
	relayStreams    map[string]relayStreamState
	outboundStreams map[string]relayStreamState
}

// NewSession constructs the state machine after mTLS client verification.
func NewSession(config SessionConfig) *Session {
	return &Session{
		config:          config,
		messageIDs:      make(map[string]struct{}),
		enabledFeatures: featureSet(config.EnabledFeatures),
		relayStreams:    make(map[string]relayStreamState),
		outboundStreams: make(map[string]relayStreamState),
	}
}

type relayStreamState struct {
	sequence uint64
	payload  []byte
}

// Accept validates one runner frame without granting any durable authority in memory.
func (session *Session) Accept(message *runnerv1.RunnerToControlPlane) (Event, error) {
	if message == nil {
		return Event{}, ErrRunnerMessage
	}
	if !session.helloAccepted {
		hello := message.GetHello()
		if hello == nil {
			return Event{}, ErrHelloRequired
		}
		return session.acceptHello(hello)
	}
	if message.GetHello() != nil {
		return Event{}, fmt.Errorf("%w: Hello may appear only once", ErrRunnerMessage)
	}
	if message.GetExec() != nil || message.GetFile() != nil || message.GetPort() != nil {
		if !session.registered {
			return Event{}, ErrRegistrationRequired
		}
		return session.acceptRunnerRelayFrame(message)
	}
	messageID, sequence, err := runnerEnvelope(message)
	if err != nil {
		return Event{}, err
	}
	if _, duplicate := session.messageIDs[messageID]; duplicate {
		return Event{
			Kind: EventDuplicate, RunnerID: session.config.AuthenticatedRunnerID,
			ConnectionID: session.config.ConnectionID, Message: message,
		}, nil
	}
	if sequence <= session.lastSequence {
		return Event{}, ErrSequenceReordered
	}
	if !session.registered {
		registration := message.GetRegistration()
		if registration == nil {
			return Event{}, ErrRegistrationRequired
		}
		if err := session.validateRegistration(registration); err != nil {
			return Event{}, err
		}
		session.record(messageID, sequence)
		session.registered = true
		return Event{
			Kind: EventRegistration, RunnerID: session.config.AuthenticatedRunnerID,
			ConnectionID: session.config.ConnectionID,
			Registration: registration, Message: message,
		}, nil
	}
	if registration := message.GetRegistration(); registration != nil {
		return Event{}, fmt.Errorf("%w: Registration may appear only once per connection", ErrRunnerMessage)
	}
	if heartbeat := message.GetHeartbeat(); heartbeat != nil {
		if heartbeat.RunnerId != session.config.AuthenticatedRunnerID ||
			heartbeat.ConnectionId != session.config.ConnectionID ||
			heartbeat.Allocatable == nil ||
			heartbeat.Reserved == nil ||
			heartbeat.DrainPhase == runnerv1.DrainPhase_DRAIN_PHASE_UNSPECIFIED {
			return Event{}, fmt.Errorf("%w: Heartbeat identity, capacity, or drain evidence is incomplete", ErrRunnerMessage)
		}
		session.record(messageID, sequence)
		return Event{
			Kind: EventHeartbeat, RunnerID: session.config.AuthenticatedRunnerID,
			ConnectionID: session.config.ConnectionID, Heartbeat: heartbeat, Message: message,
		}, nil
	}
	kind := classifyRunnerMessage(message)
	if kind == "" {
		return Event{}, ErrRunnerMessage
	}
	session.record(messageID, sequence)
	return Event{
		Kind: kind, RunnerID: session.config.AuthenticatedRunnerID,
		ConnectionID: session.config.ConnectionID, Message: message,
	}, nil
}

func (session *Session) acceptRunnerRelayFrame(
	message *runnerv1.RunnerToControlPlane,
) (Event, error) {
	var (
		kind     EventKind
		key      string
		sequence uint64
		payload  proto.Message
	)
	switch {
	case message.GetExec() != nil:
		frame := message.GetExec()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING] {
			return Event{}, fmt.Errorf("%w: Exec streaming feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetOutput() == nil && frame.GetTerminal() == nil {
			return Event{}, fmt.Errorf("%w: runner Exec payload is not an output or terminal frame", ErrRunnerMessage)
		}
		if err := validateRelayIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return Event{}, err
		}
		kind = EventExec
		key = relayStreamKey("exec", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetFile() != nil:
		frame := message.GetFile()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING] {
			return Event{}, fmt.Errorf("%w: File streaming feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetChunk() == nil && frame.GetMetadata() == nil && frame.GetTerminal() == nil {
			return Event{}, fmt.Errorf("%w: runner File payload is not a chunk, metadata, or terminal frame", ErrRunnerMessage)
		}
		if err := validateRelayIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return Event{}, err
		}
		kind = EventFile
		key = relayStreamKey("file", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetPort() != nil:
		frame := message.GetPort()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY] {
			return Event{}, fmt.Errorf("%w: Port proxy feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetBytes() == nil && frame.GetCredit() == nil && frame.GetTerminal() == nil {
			return Event{}, fmt.Errorf("%w: runner Port payload is not bytes, credit, or terminal", ErrRunnerMessage)
		}
		if err := validateRelayIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return Event{}, err
		}
		kind = EventPort
		key = relayStreamKey("port", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("%w: encode relay frame: %v", ErrRunnerMessage, err)
	}
	duplicate, err := acceptRelaySequence(session.relayStreams, key, sequence, encoded)
	if err != nil {
		return Event{}, err
	}
	if duplicate {
		return Event{
			Kind: EventDuplicate, RunnerID: session.config.AuthenticatedRunnerID,
			ConnectionID: session.config.ConnectionID, Message: message,
		}, nil
	}
	return Event{
		Kind: kind, RunnerID: session.config.AuthenticatedRunnerID,
		ConnectionID: session.config.ConnectionID, Message: message,
	}, nil
}

// ValidateOutboundRelayFrame gates a claimed durable frame against negotiated
// features and connection-local stream ordering before transport mutation.
func (session *Session) ValidateOutboundRelayFrame(message *runnerv1.ControlPlaneToRunner) error {
	if !session.helloAccepted || !session.registered || message == nil {
		return ErrRegistrationRequired
	}
	var (
		key      string
		sequence uint64
		payload  proto.Message
	)
	switch {
	case message.GetExec() != nil:
		frame := message.GetExec()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING] {
			return fmt.Errorf("%w: Exec streaming feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetOpen() == nil && frame.GetCredit() == nil && frame.GetCancel() == nil {
			return fmt.Errorf("%w: control-plane Exec payload is not an open, credit, or cancel frame", ErrRunnerMessage)
		}
		if err := validateRelayIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return err
		}
		key = relayStreamKey("exec", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetFile() != nil:
		frame := message.GetFile()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING] {
			return fmt.Errorf("%w: File streaming feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetOpen() == nil && frame.GetChunk() == nil && frame.GetCredit() == nil && frame.GetCancel() == nil {
			return fmt.Errorf("%w: control-plane File payload is not an open, chunk, credit, or cancel frame", ErrRunnerMessage)
		}
		if err := validateRelayIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return err
		}
		key = relayStreamKey("file", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetPort() != nil:
		frame := message.GetPort()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY] {
			return fmt.Errorf("%w: Port proxy feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetOpen() == nil && frame.GetBytes() == nil &&
			frame.GetCredit() == nil && frame.GetCancel() == nil {
			return fmt.Errorf("%w: control-plane Port payload is not open, bytes, credit, or cancel", ErrRunnerMessage)
		}
		if err := validateRelayIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return err
		}
		key = relayStreamKey("port", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	default:
		return fmt.Errorf("%w: outbound relay frame is not Exec, File, or Port", ErrRunnerMessage)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: encode outbound relay frame: %v", ErrRunnerMessage, err)
	}
	_, err = acceptRelaySequence(session.outboundStreams, key, sequence, encoded)
	return err
}

func acceptRelaySequence(
	streams map[string]relayStreamState,
	key string,
	sequence uint64,
	encoded []byte,
) (bool, error) {
	previous := streams[key]
	if sequence == previous.sequence && sequence != 0 && bytes.Equal(encoded, previous.payload) {
		return true, nil
	}
	// A reconnected protocol session can first observe a durable stream after
	// sequence one. PostgreSQL remains authoritative for cross-connection gaps.
	if previous.sequence != 0 && sequence != previous.sequence+1 {
		return false, ErrSequenceReordered
	}
	streams[key] = relayStreamState{
		sequence: sequence,
		payload:  bytes.Clone(encoded),
	}
	return false, nil
}

func validateRelayIdentity(
	fence *runnerv1.AssignmentFence,
	operationID string,
	streamID string,
	sequence uint64,
) error {
	if fence == nil ||
		strings.TrimSpace(fence.AssignmentId) == "" ||
		strings.TrimSpace(fence.SandboxId) == "" ||
		strings.TrimSpace(fence.InstanceId) == "" ||
		fence.SandboxGeneration == 0 ||
		len(fence.FencingToken) == 0 ||
		strings.TrimSpace(operationID) == "" ||
		strings.TrimSpace(streamID) == "" ||
		sequence == 0 {
		return fmt.Errorf("%w: relay frame fencing, operation, stream, or sequence identity is incomplete", ErrRunnerMessage)
	}
	return nil
}

func relayStreamKey(
	kind string,
	fence *runnerv1.AssignmentFence,
	operationID string,
	streamID string,
) string {
	return strings.Join([]string{
		kind,
		fence.AssignmentId,
		fence.SandboxId,
		fence.InstanceId,
		fmt.Sprintf("%d", fence.SandboxGeneration),
		string(fence.FencingToken),
		operationID,
		streamID,
	}, "\x00")
}

func (session *Session) acceptHello(hello *runnerv1.RunnerHello) (Event, error) {
	if hello.RunnerId != session.config.AuthenticatedRunnerID {
		return session.rejection(
			runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_IDENTITY_MISMATCH,
			"runner certificate identity does not match Hello",
		), nil
	}
	if len(hello.ConnectionNonce) < 32 || hello.SupportedVersions == nil ||
		hello.SupportedVersions.Minimum == 0 ||
		hello.SupportedVersions.Minimum > hello.SupportedVersions.Maximum {
		return session.rejection(
			runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_INVALID_HELLO,
			"runner Hello is incomplete",
		), nil
	}
	minimum := max(hello.SupportedVersions.Minimum, session.config.SupportedVersions.Minimum)
	maximum := min(hello.SupportedVersions.Maximum, session.config.SupportedVersions.Maximum)
	if minimum > maximum {
		return session.rejection(
			runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_VERSION_UNSUPPORTED,
			"runner protocol version is unsupported",
		), nil
	}
	enabled := featureSet(session.config.EnabledFeatures)
	mandatory := featureSet(hello.MandatoryFeatures)
	if enabled[runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE] &&
		!mandatory[runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE] {
		return session.rejection(
			runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_FEATURE_UNSUPPORTED,
			"runner does not implement the mandatory local-workspace protocol",
		), nil
	}
	for _, feature := range hello.MandatoryFeatures {
		if !enabled[feature] {
			return session.rejection(
				runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_FEATURE_UNSUPPORTED,
				"runner mandatory feature is unsupported",
			), nil
		}
	}
	session.helloAccepted = true
	session.selectedVersion = maximum
	return Event{
		Kind: EventWelcome,
		Response: &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Welcome{
				Welcome: &runnerv1.RunnerWelcome{
					ConnectionId: session.config.ConnectionID, SelectedVersion: maximum,
					EnabledFeatures:     append([]runnerv1.RunnerFeature(nil), session.config.EnabledFeatures...),
					HeartbeatIntervalMs: uint64(session.config.HeartbeatInterval.Milliseconds()),
				},
			},
		},
	}, nil
}

func (session *Session) validateRegistration(registration *runnerv1.RunnerRegistration) error {
	if registration.RunnerId != session.config.AuthenticatedRunnerID ||
		registration.ConnectionId != session.config.ConnectionID ||
		strings.TrimSpace(registration.RunnerPoolId) == "" ||
		strings.TrimSpace(registration.SoftwareVersion) == "" ||
		registration.ProtocolVersion != session.selectedVersion ||
		registration.Capabilities == nil ||
		registration.Allocatable == nil ||
		registration.Reserved == nil {
		return fmt.Errorf("%w: Registration identity, version, or capacity evidence is incomplete", ErrRunnerMessage)
	}
	if len(registration.ReadinessFailures) != 0 ||
		!registration.Capabilities.KvmReady ||
		!registration.Capabilities.JailerReady ||
		!registration.Capabilities.CgroupReady ||
		!registration.Capabilities.NetworkPolicyReady ||
		!registration.Capabilities.StorageReady ||
		!registration.Capabilities.CleanupReady ||
		registration.Capabilities.GuestProtocolGenerations == nil ||
		registration.Capabilities.GuestProtocolGenerations.Minimum == 0 ||
		registration.Capabilities.GuestProtocolGenerations.Minimum >
			registration.Capabilities.GuestProtocolGenerations.Maximum {
		return ErrRunnerPrerequisites
	}
	return nil
}

func (session *Session) rejection(
	kind runnerv1.ProtocolRejectionKind,
	detail string,
) Event {
	return Event{
		Kind: EventRejection,
		Response: &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Rejection{
				Rejection: &runnerv1.ProtocolRejection{Kind: kind, SafeDetail: detail},
			},
		},
	}
}

func (session *Session) record(messageID string, sequence uint64) {
	session.messageIDs[messageID] = struct{}{}
	session.lastSequence = sequence
}

func runnerEnvelope(message *runnerv1.RunnerToControlPlane) (string, uint64, error) {
	switch {
	case message.GetRegistration() != nil:
		return validateEnvelope(message.GetRegistration().MessageId, message.GetRegistration().Sequence)
	case message.GetHeartbeat() != nil:
		return validateEnvelope(message.GetHeartbeat().MessageId, message.GetHeartbeat().Sequence)
	case message.GetAssignmentAck() != nil:
		return validateEnvelope(message.GetAssignmentAck().MessageId, message.GetAssignmentAck().Sequence)
	case message.GetAssignmentProgress() != nil:
		return validateEnvelope(message.GetAssignmentProgress().MessageId, message.GetAssignmentProgress().Sequence)
	case message.GetAssignmentResult() != nil:
		return validateEnvelope(message.GetAssignmentResult().MessageId, message.GetAssignmentResult().Sequence)
	case message.GetFenceResult() != nil:
		return validateEnvelope(message.GetFenceResult().MessageId, message.GetFenceResult().Sequence)
	case message.GetDrainState() != nil:
		return validateEnvelope(message.GetDrainState().MessageId, message.GetDrainState().Sequence)
	case message.GetEvidence() != nil:
		return validateEnvelope(message.GetEvidence().MessageId, message.GetEvidence().Sequence)
	case message.GetLocalWorkspaceResult() != nil:
		return validateEnvelope(message.GetLocalWorkspaceResult().MessageId, message.GetLocalWorkspaceResult().Sequence)
	case message.GetInstanceTerminal() != nil:
		return validateEnvelope(message.GetInstanceTerminal().MessageId, message.GetInstanceTerminal().Sequence)
	default:
		return "", 0, fmt.Errorf("%w: stream frame has no durable message envelope", ErrRunnerMessage)
	}
}

func validateEnvelope(messageID string, sequence uint64) (string, uint64, error) {
	if strings.TrimSpace(messageID) == "" || sequence == 0 {
		return "", 0, fmt.Errorf("%w: message ID and sequence are required", ErrRunnerMessage)
	}
	return messageID, sequence, nil
}

func classifyRunnerMessage(message *runnerv1.RunnerToControlPlane) EventKind {
	switch {
	case message.GetAssignmentAck() != nil,
		message.GetAssignmentProgress() != nil,
		message.GetAssignmentResult() != nil:
		return EventAssignment
	case message.GetFenceResult() != nil:
		return EventFence
	case message.GetDrainState() != nil:
		return EventDrain
	case message.GetEvidence() != nil:
		return EventEvidence
	case message.GetLocalWorkspaceResult() != nil:
		return EventLocalWorkspace
	case message.GetInstanceTerminal() != nil:
		return EventInstanceTerminal
	default:
		return ""
	}
}

func featureSet(features []runnerv1.RunnerFeature) map[runnerv1.RunnerFeature]bool {
	result := make(map[runnerv1.RunnerFeature]bool, len(features))
	for _, feature := range features {
		result[feature] = true
	}
	return result
}
