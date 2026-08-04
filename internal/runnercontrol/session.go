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

// MaximumBufferedExecBytes bounds the single Exec completion carried by the
// Runner control connection independently from streamed File messages.
const MaximumBufferedExecBytes int64 = 64 << 20

var (
	ErrHelloRequired        = errors.New("SecondBox runner control Hello is required")
	ErrRegistrationRequired = errors.New("SecondBox runner control Registration is required")
	ErrSequenceReordered    = errors.New("SecondBox runner control sequence is reordered")
	ErrRunnerPrerequisites  = errors.New("SecondBox runner prerequisites failed")
	ErrRunnerMessage        = errors.New("SecondBox runner control message is invalid")
)

type EventKind string

const (
	EventWelcome           EventKind = "welcome"
	EventRegistration      EventKind = "registration"
	EventHeartbeat         EventKind = "heartbeat"
	EventAssignment        EventKind = "assignment"
	EventFence             EventKind = "fence"
	EventDrain             EventKind = "drain"
	EventEvidence          EventKind = "evidence"
	EventLocalWorkspace    EventKind = "local_workspace"
	EventExec              EventKind = "exec"
	EventPty               EventKind = "pty"
	EventFile              EventKind = "file"
	EventPort              EventKind = "port"
	EventPortDirect        EventKind = "port_direct"
	EventDataPlaneDirect   EventKind = "data_plane_direct"
	EventWorkspaceTransfer EventKind = "workspace_transfer"
	EventInstanceTerminal  EventKind = "instance_terminal"
	EventDuplicate         EventKind = "duplicate"
	EventRejection         EventKind = "rejection"
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
	config                  SessionConfig
	helloAccepted           bool
	registered              bool
	selectedVersion         uint32
	lastSequence            uint64
	messageIDs              map[string]struct{}
	enabledFeatures         map[runnerv1.RunnerFeature]bool
	inboundDataPlaneStreams map[string]dataPlaneStreamState
	outboundStreams         map[string]dataPlaneStreamState
}

// NewSession constructs the state machine after mTLS client verification.
func NewSession(config SessionConfig) *Session {
	return &Session{
		config:                  config,
		messageIDs:              make(map[string]struct{}),
		enabledFeatures:         featureSet(config.EnabledFeatures),
		inboundDataPlaneStreams: make(map[string]dataPlaneStreamState),
		outboundStreams:         make(map[string]dataPlaneStreamState),
	}
}

type dataPlaneStreamState struct {
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
	if message.GetExec() != nil || message.GetPty() != nil ||
		message.GetFile() != nil || message.GetPort() != nil ||
		message.GetWorkspaceTransfer() != nil {
		if !session.registered {
			return Event{}, ErrRegistrationRequired
		}
		return session.acceptRunnerDataPlaneFrame(message)
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
			heartbeat.StartupTiming == nil ||
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

func (session *Session) acceptRunnerDataPlaneFrame(
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
		if frame.GetOutput() == nil && frame.GetTerminal() == nil && frame.GetBufferedResult() == nil {
			return Event{}, fmt.Errorf("%w: runner Exec payload is not output or a terminal result", ErrRunnerMessage)
		}
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return Event{}, err
		}
		kind = EventExec
		key = dataPlaneStreamKey("exec", frame.Fence, frame.OperationId, frame.StreamId)
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
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return Event{}, err
		}
		kind = EventFile
		key = dataPlaneStreamKey("file", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetPty() != nil:
		frame := message.GetPty()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_PTY] {
			return Event{}, fmt.Errorf("%w: PTY feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetOutput() == nil && frame.GetTerminal() == nil && frame.GetAttachResult() == nil {
			return Event{}, fmt.Errorf("%w: runner PTY payload is not output, attach result, or terminal", ErrRunnerMessage)
		}
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return Event{}, err
		}
		if frame.GetAttachResult() != nil {
			if frame.GetAttachResult().Kind == runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_UNSPECIFIED ||
				frame.GetAttachResult().AfterSequence < -1 {
				return Event{}, fmt.Errorf("%w: runner PTY attach result is incomplete", ErrRunnerMessage)
			}
			if frame.GetAttachResult().Kind == runnerv1.PtyAttachResultKind_PTY_ATTACH_RESULT_KIND_ATTACHED {
				session.inboundDataPlaneStreams[dataPlaneStreamKey(
					"pty", frame.Fence, frame.OperationId, frame.StreamId,
				)] = dataPlaneStreamState{sequence: uint64(frame.GetAttachResult().AfterSequence + 1)}
			}
			return Event{
				Kind: EventPty, RunnerID: session.config.AuthenticatedRunnerID,
				ConnectionID: session.config.ConnectionID, Message: message,
			}, nil
		}
		kind = EventPty
		key = dataPlaneStreamKey("pty", frame.Fence, frame.OperationId, frame.StreamId)
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
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return Event{}, err
		}
		kind = EventPort
		key = dataPlaneStreamKey("port", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetWorkspaceTransfer() != nil:
		frame := message.GetWorkspaceTransfer()
		if session.selectedVersion < 2 {
			return Event{}, fmt.Errorf("%w: Workspace relocation requires protocol generation 2", ErrRunnerMessage)
		}
		if strings.TrimSpace(frame.OperationId) == "" ||
			strings.TrimSpace(frame.SandboxId) == "" ||
			strings.TrimSpace(frame.WorkspaceId) == "" ||
			frame.Generation == 0 || frame.Sequence == 0 {
			return Event{}, fmt.Errorf("%w: Workspace transfer identity is incomplete", ErrRunnerMessage)
		}
		switch {
		case frame.GetOpen() != nil:
			if frame.GetOpen().LogicalCapacityBytes == 0 || len(frame.GetOpen().FencingToken) == 0 {
				return Event{}, fmt.Errorf("%w: Workspace transfer open authority is incomplete", ErrRunnerMessage)
			}
		case frame.GetChunk() != nil:
			if len(frame.GetChunk().Data) == 0 || len(frame.GetChunk().Data) > 64<<10 {
				return Event{}, fmt.Errorf("%w: Workspace transfer chunk exceeds its bound", ErrRunnerMessage)
			}
		case frame.GetCredit() != nil:
			if frame.GetCredit().ByteCount == 0 || frame.GetCredit().ByteCount > 1<<20 {
				return Event{}, fmt.Errorf("%w: Workspace transfer credit exceeds its bound", ErrRunnerMessage)
			}
		case frame.GetCommit() != nil:
			if frame.GetCommit().SizeBytes == 0 || !strings.HasPrefix(frame.GetCommit().Sha256, "sha256:") {
				return Event{}, fmt.Errorf("%w: Workspace transfer commit is incomplete", ErrRunnerMessage)
			}
		case frame.GetResult() != nil:
			if frame.GetResult().Terminal == runnerv1.WorkspaceTransferTerminalKind_WORKSPACE_TRANSFER_TERMINAL_KIND_UNSPECIFIED {
				return Event{}, fmt.Errorf("%w: Workspace transfer result is incomplete", ErrRunnerMessage)
			}
		case frame.GetCancel() != nil:
			if strings.TrimSpace(frame.GetCancel().SafeDetail) == "" {
				return Event{}, fmt.Errorf("%w: Workspace transfer cancellation is incomplete", ErrRunnerMessage)
			}
		default:
			return Event{}, fmt.Errorf("%w: Workspace transfer payload is absent", ErrRunnerMessage)
		}
		kind = EventWorkspaceTransfer
		key = strings.Join([]string{
			"workspace-transfer", frame.OperationId, frame.SandboxId,
			frame.WorkspaceId, fmt.Sprintf("%d", frame.Generation),
		}, "\x00")
		sequence = frame.Sequence
		payload = frame
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("%w: encode data-plane frame: %v", ErrRunnerMessage, err)
	}
	duplicate, err := acceptDataPlaneSequence(session.inboundDataPlaneStreams, key, sequence, encoded)
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

// ValidateOutboundDataPlaneFrame gates an outbound data-plane frame against negotiated
// features and connection-local stream ordering before transport mutation.
func (session *Session) ValidateOutboundDataPlaneFrame(message *runnerv1.ControlPlaneToRunner) error {
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
		if frame.GetOpen() == nil && frame.GetInput() == nil &&
			frame.GetCredit() == nil && frame.GetCancel() == nil {
			return fmt.Errorf("%w: control-plane Exec payload is not open, input, credit, or cancel", ErrRunnerMessage)
		}
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return err
		}
		key = dataPlaneStreamKey("exec", frame.Fence, frame.OperationId, frame.StreamId)
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
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return err
		}
		key = dataPlaneStreamKey("file", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetPty() != nil:
		frame := message.GetPty()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_PTY] {
			return fmt.Errorf("%w: PTY feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetInput() == nil && frame.GetResize() == nil &&
			frame.GetAttach() == nil && frame.GetDetach() == nil &&
			frame.GetCredit() == nil {
			return fmt.Errorf("%w: control-plane PTY payload is not input, resize, attach, detach, or credit", ErrRunnerMessage)
		}
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return err
		}
		if frame.GetAttach() != nil || frame.GetDetach() != nil {
			return nil
		}
		// A Terminal begins with an ExecOpen and continues with PTY frames; both
		// use one operation sequence and therefore one connection-local key.
		key = dataPlaneStreamKey("exec", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	case message.GetPort() != nil:
		frame := message.GetPort()
		if !session.enabledFeatures[runnerv1.RunnerFeature_RUNNER_FEATURE_PORT_PROXY] {
			return fmt.Errorf("%w: Port proxy feature was not negotiated", ErrRunnerMessage)
		}
		if frame.GetOpen() == nil && frame.GetDirectOpen() == nil &&
			frame.GetBytes() == nil &&
			frame.GetCredit() == nil && frame.GetCancel() == nil {
			return fmt.Errorf("%w: control-plane Port payload is not open, direct open, bytes, credit, or cancel", ErrRunnerMessage)
		}
		if err := validateDataPlaneFrameIdentity(frame.Fence, frame.OperationId, frame.StreamId, frame.Sequence); err != nil {
			return err
		}
		key = dataPlaneStreamKey("port", frame.Fence, frame.OperationId, frame.StreamId)
		sequence = frame.Sequence
		payload = frame
	default:
		return fmt.Errorf("%w: outbound data-plane frame is not Exec, File, or Port", ErrRunnerMessage)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: encode outbound data-plane frame: %v", ErrRunnerMessage, err)
	}
	_, err = acceptDataPlaneSequence(session.outboundStreams, key, sequence, encoded)
	return err
}

func acceptDataPlaneSequence(
	streams map[string]dataPlaneStreamState,
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
	streams[key] = dataPlaneStreamState{
		sequence: sequence,
		payload:  bytes.Clone(encoded),
	}
	return false, nil
}

func validateDataPlaneFrameIdentity(
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
		return fmt.Errorf("%w: data-plane frame fencing, operation, stream, or sequence identity is incomplete", ErrRunnerMessage)
	}
	return nil
}

func dataPlaneStreamKey(
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
		registration.Reserved == nil ||
		registration.StartupTiming == nil {
		return fmt.Errorf("%w: Registration identity, version, or capacity evidence is incomplete", ErrRunnerMessage)
	}
	if len(registration.ReadinessFailures) != 0 ||
		!registration.Capabilities.KvmReady ||
		!registration.Capabilities.JailerReady ||
		!registration.Capabilities.CgroupReady ||
		!registration.Capabilities.NetworkPolicyReady ||
		!registration.Capabilities.StorageReady ||
		!registration.Capabilities.CleanupReady ||
		!registration.Capabilities.DataPlaneReady ||
		strings.TrimSpace(registration.DataPlaneAdvertisedAddress) == "" ||
		len(registration.DataPlaneCertificateSpkiSha256) != 64 ||
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
	case message.GetPortDirectConsume() != nil:
		return validateEnvelope(message.GetPortDirectConsume().MessageId, message.GetPortDirectConsume().Sequence)
	case message.GetDataPlaneDirectConsume() != nil:
		return validateEnvelope(message.GetDataPlaneDirectConsume().MessageId, message.GetDataPlaneDirectConsume().Sequence)
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
	case message.GetPortDirectConsume() != nil:
		return EventPortDirect
	case message.GetDataPlaneDirectConsume() != nil:
		return EventDataPlaneDirect
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
