// Package runnercontrol owns the outbound, versioned control-plane connection.
package runnercontrol

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var immutableManifestDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const (
	runnerReconnectInitialDelay = 250 * time.Millisecond
	runnerReconnectMaximumDelay = 30 * time.Second
)

// ErrRunnerProtocolNegotiation identifies a connection rejected before registration.
var ErrRunnerProtocolNegotiation = errors.New("SecondBox runner protocol negotiation failed")

// RunnerProtocolConfig contains stable runner identity and its supported wire window.
type RunnerProtocolConfig struct {
	RunnerID          string
	RunnerPoolID      string
	SoftwareVersion   string
	ProtocolMinimum   uint32
	ProtocolMaximum   uint32
	MandatoryFeatures []runnerprotocol.RunnerFeature
}

// BackendReadiness is verified local capability and capacity evidence.
type BackendReadiness struct {
	Architecture      string
	Capacity          *runnerprotocol.Capacity
	Reserved          *runnerprotocol.Capacity
	Capabilities      *runnerprotocol.RunnerCapabilities
	ArtifactCache     []*runnerprotocol.ArtifactCacheEvidence
	ReadinessFailures []runnerprotocol.RunnerReadinessFailure
}

// BackendInstance is the provider-private identity of a ready compute instance.
type BackendInstance struct {
	BackendKind      string
	BackendReference string
}

// BackendInstanceTerminal is bounded post-ready runtime evidence. It cannot
// release assignment authority or workspace materialization.
type BackendInstanceTerminal struct {
	Fence          *runnerprotocol.AssignmentFence
	Correlation    *runnerprotocol.Correlation
	Reason         runnerprotocol.InstanceObservedTerminationReason
	ObservedAt     time.Time
	EvidenceDigest string
}

// FenceEvidence proves whether the assigned instance is stopped.
type FenceEvidence struct {
	Result                    runnerprotocol.FenceResultKind
	TerminationEvidenceDigest string
}

// CheckpointEvidence is runner-produced immutable workspace metadata.
type CheckpointEvidence struct {
	SHA256                    string
	SizeBytes                 uint64
	Compatibility             map[string]string
	TerminationEvidenceDigest string
}

// CheckpointBackend freezes and streams one exactly fenced workspace.
type CheckpointBackend interface {
	CreateCheckpoint(
		context.Context,
		*runnerprotocol.CheckpointCommand,
		func([]byte) error,
	) (CheckpointEvidence, error)
}

// RestoreBackend ingests verified provider-neutral checkpoint bytes before assignment.
type RestoreBackend interface {
	BeginRestore(context.Context, *runnerprotocol.RestoreBegin) error
	WriteRestoreChunk(context.Context, *runnerprotocol.RestoreChunk) error
}

// AssignmentBackend accepts only immutable, profile-resolved assignments.
type AssignmentBackend interface {
	Readiness(context.Context) (BackendReadiness, error)
	ValidateAssignment(context.Context, *runnerprotocol.AssignmentCommand) error
	StartAssignment(
		context.Context,
		*runnerprotocol.AssignmentCommand,
		func(runnerprotocol.AssignmentProgressStage) error,
	) (BackendInstance, error)
	FenceAssignment(context.Context, *runnerprotocol.FenceCommand) (FenceEvidence, error)
}

type evidenceAwareBackend interface {
	SetRunnerEvidenceSink(runnerevidence.Sink, string)
}

type instanceTerminalBackend interface {
	InstanceTerminals() <-chan BackendInstanceTerminal
	MarkAssignmentReady(*runnerprotocol.AssignmentFence) error
}

// RunnerProtocolStream is the generated bidirectional gRPC stream surface.
type RunnerProtocolStream interface {
	Send(*runnerprotocol.RunnerToControlPlane) error
	Recv() (*runnerprotocol.ControlPlaneToRunner, error)
}

type receivedControlPlaneFrame struct {
	message *runnerprotocol.ControlPlaneToRunner
	err     error
}

type runnerRestoreOperation struct {
	fence           *runnerprotocol.AssignmentFence
	correlation     *runnerprotocol.Correlation
	checkpointID    string
	storageObjectID string
	terminal        bool
	terminalFrame   []byte
}

// RunnerProtocolConnector establishes one mutually authenticated outbound stream.
type RunnerProtocolConnector interface {
	Connect(context.Context) (RunnerProtocolStream, error)
	Close() error
}

// RunnerProtocolService binds the versioned stream to one compute backend.
type RunnerProtocolService struct {
	config            RunnerProtocolConfig
	backend           AssignmentBackend
	dataPlaneBackend  DataPlaneBackend
	portBackend       PortBackend
	checkpointBackend CheckpointBackend
	restoreBackend    RestoreBackend
	terminalBackend   instanceTerminalBackend
	instanceTerminals <-chan BackendInstanceTerminal
	connector         RunnerProtocolConnector
	sequence          atomic.Uint64
	sendMu            sync.Mutex
	stateMu           sync.Mutex
	drain             runnerprotocol.DrainPhase
	active            map[string]*runnerprotocol.ActiveAssignmentSummary
	operationMu       sync.Mutex
	execOperations    map[string]*runnerExecOperation
	fileOperations    map[string]*runnerFileOperation
	portOperations    map[string]*runnerPortOperation
	execTerminalOrder []string
	fileTerminalOrder []string
	portTerminalOrder []string
	restoreOperations map[string]*runnerRestoreOperation
	evidence          runnerevidence.Sink
	correlations      map[string]*runnerprotocol.Correlation
}

// NewRunnerProtocolService validates immutable identity before creating the composition root.
func NewRunnerProtocolService(
	config RunnerProtocolConfig,
	backend AssignmentBackend,
	connector RunnerProtocolConnector,
) (*RunnerProtocolService, error) {
	config.RunnerID = strings.TrimSpace(config.RunnerID)
	config.RunnerPoolID = strings.TrimSpace(config.RunnerPoolID)
	config.SoftwareVersion = strings.TrimSpace(config.SoftwareVersion)
	if config.RunnerID == "" || config.RunnerPoolID == "" || config.SoftwareVersion == "" {
		return nil, fmt.Errorf("SecondBox runner protocol config requires runner, pool, and software identity")
	}
	if config.ProtocolMinimum == 0 ||
		config.ProtocolMaximum == 0 ||
		config.ProtocolMinimum > config.ProtocolMaximum {
		return nil, fmt.Errorf("SecondBox runner protocol config has an invalid supported-version window")
	}
	if backend == nil {
		return nil, fmt.Errorf("SecondBox runner protocol assignment backend is required")
	}
	if connector == nil {
		return nil, fmt.Errorf("SecondBox runner protocol connector is required")
	}
	dataPlaneBackend, implementsDataPlane := backend.(DataPlaneBackend)
	_, implementsPTY := backend.(PTYDataPlaneBackend)
	portBackend, implementsPort := backend.(PortBackend)
	checkpointBackend, implementsCheckpoint := backend.(CheckpointBackend)
	restoreBackend, implementsRestore := backend.(RestoreBackend)
	terminalBackend, implementsTerminal := backend.(instanceTerminalBackend)
	for _, feature := range config.MandatoryFeatures {
		if (feature == runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING ||
			feature == runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING) &&
			!implementsDataPlane {
			return nil, fmt.Errorf("SecondBox runner data-plane features require a data-plane backend")
		}
		if feature == runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY && !implementsPTY {
			return nil, fmt.Errorf("SecondBox runner PTY feature requires a PTY backend")
		}
		if feature == runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY && !implementsPort {
			return nil, fmt.Errorf("SecondBox runner Port proxy feature requires a Port backend")
		}
		if feature == runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT &&
			(!implementsCheckpoint || !implementsRestore) {
			return nil, fmt.Errorf("SecondBox runner checkpoint feature requires checkpoint create and restore backends")
		}
	}
	service := &RunnerProtocolService{
		config:            config,
		backend:           backend,
		dataPlaneBackend:  dataPlaneBackend,
		portBackend:       portBackend,
		checkpointBackend: checkpointBackend,
		restoreBackend:    restoreBackend,
		terminalBackend:   terminalBackend,
		connector:         connector,
		drain:             runnerprotocol.DrainPhase_DRAIN_PHASE_ACTIVE,
		active:            make(map[string]*runnerprotocol.ActiveAssignmentSummary),
		execOperations:    make(map[string]*runnerExecOperation),
		fileOperations:    make(map[string]*runnerFileOperation),
		portOperations:    make(map[string]*runnerPortOperation),
		restoreOperations: make(map[string]*runnerRestoreOperation),
		evidence:          runnerevidence.SlogSink{},
		correlations:      make(map[string]*runnerprotocol.Correlation),
	}
	if implementsTerminal {
		service.instanceTerminals = terminalBackend.InstanceTerminals()
		if service.instanceTerminals == nil {
			return nil, fmt.Errorf("SecondBox runner terminal-event backend returned a nil source")
		}
	}
	if evidenceBackend, ok := backend.(evidenceAwareBackend); ok {
		evidenceBackend.SetRunnerEvidenceSink(service.evidence, config.RunnerID)
	}
	return service, nil
}

// SetEvidenceSink replaces the fixed-shape evidence destination for tests and
// alternate Runner-local durable sinks.
func (s *RunnerProtocolService) SetEvidenceSink(sink runnerevidence.Sink) {
	if sink == nil {
		return
	}
	s.evidence = sink
	if evidenceBackend, ok := s.backend.(evidenceAwareBackend); ok {
		evidenceBackend.SetRunnerEvidenceSink(sink, s.config.RunnerID)
	}
}

// Run preserves Runner-owned Instances while reconnecting transient control-plane sessions.
func (s *RunnerProtocolService) Run(ctx context.Context) error {
	reconnectDelay := runnerReconnectInitialDelay
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sessionEstablished, sessionErr := s.runProtocolSession(ctx)
		sessionErr = errors.Join(sessionErr, s.connector.Close())
		if err := ctx.Err(); err != nil {
			return err
		}
		if isTerminalRunnerProtocolError(sessionErr) {
			return sessionErr
		}
		if sessionErr == nil {
			sessionErr = errors.New("SecondBox runner protocol session ended without an error")
		}
		if sessionEstablished {
			reconnectDelay = runnerReconnectInitialDelay
		}
		slog.Warn(
			"SecondBox runner protocol session lost; reconnecting",
			"error", sessionErr,
			"retryDelay", reconnectDelay,
		)
		if err := waitRunnerProtocolReconnect(ctx, reconnectDelay); err != nil {
			return err
		}
		reconnectDelay = nextRunnerProtocolReconnectDelay(reconnectDelay)
	}
}

func (s *RunnerProtocolService) runProtocolSession(ctx context.Context) (bool, error) {
	stream, err := s.connector.Connect(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner protocol connect: %w", err)
	}

	connectionNonce := make([]byte, 32)
	if _, err := rand.Read(connectionNonce); err != nil {
		return false, fmt.Errorf("SecondBox runner protocol connection nonce: %w", err)
	}
	if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Hello{
			Hello: &runnerprotocol.RunnerHello{
				RunnerId:        s.config.RunnerID,
				ConnectionNonce: connectionNonce,
				SupportedVersions: &runnerprotocol.ProtocolVersionRange{
					Minimum: s.config.ProtocolMinimum,
					Maximum: s.config.ProtocolMaximum,
				},
				RequestedFeatures: append([]runnerprotocol.RunnerFeature(nil), s.config.MandatoryFeatures...),
				MandatoryFeatures: append([]runnerprotocol.RunnerFeature(nil), s.config.MandatoryFeatures...),
			},
		},
	}); err != nil {
		return false, fmt.Errorf("SecondBox runner protocol send hello: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return false, fmt.Errorf("SecondBox runner protocol receive welcome: %w", err)
	}
	if rejection := first.GetRejection(); rejection != nil {
		return false, fmt.Errorf("%w: %s", ErrRunnerProtocolNegotiation, rejection.GetSafeDetail())
	}
	welcome := first.GetWelcome()
	if err := s.validateWelcome(welcome); err != nil {
		return false, err
	}

	readiness, err := s.backend.Readiness(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner readiness failed: %w", err)
	}
	if err := s.sendRegistration(
		stream,
		welcome.ConnectionId,
		welcome.SelectedVersion,
		readiness,
	); err != nil {
		return false, err
	}
	if err := s.sendHeartbeat(stream, welcome.ConnectionId, readiness); err != nil {
		return true, err
	}
	return true, s.consumeCommands(ctx, stream, welcome, readiness)
}

func isTerminalRunnerProtocolError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRunnerProtocolNegotiation) {
		return true
	}
	switch status.Code(err) {
	case codes.Unauthenticated,
		codes.PermissionDenied,
		codes.InvalidArgument,
		codes.FailedPrecondition,
		codes.Unimplemented:
		return true
	default:
		return false
	}
}

func waitRunnerProtocolReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextRunnerProtocolReconnectDelay(current time.Duration) time.Duration {
	if current >= runnerReconnectMaximumDelay/2 {
		return runnerReconnectMaximumDelay
	}
	return current * 2
}

func (s *RunnerProtocolService) validateWelcome(welcome *runnerprotocol.RunnerWelcome) error {
	if welcome == nil {
		return fmt.Errorf("%w: first control-plane frame was not Welcome", ErrRunnerProtocolNegotiation)
	}
	if strings.TrimSpace(welcome.ConnectionId) == "" ||
		welcome.SelectedVersion < s.config.ProtocolMinimum ||
		welcome.SelectedVersion > s.config.ProtocolMaximum ||
		welcome.HeartbeatIntervalMs == 0 {
		return fmt.Errorf("%w: Welcome selected invalid connection parameters", ErrRunnerProtocolNegotiation)
	}
	enabled := make(map[runnerprotocol.RunnerFeature]bool, len(welcome.EnabledFeatures))
	requested := make(map[runnerprotocol.RunnerFeature]bool, len(s.config.MandatoryFeatures))
	for _, feature := range s.config.MandatoryFeatures {
		requested[feature] = true
	}
	for _, feature := range welcome.EnabledFeatures {
		if !requested[feature] {
			return fmt.Errorf("%w: control plane enabled unrequested feature %s", ErrRunnerProtocolNegotiation, feature)
		}
		enabled[feature] = true
	}
	for _, feature := range s.config.MandatoryFeatures {
		if !enabled[feature] {
			return fmt.Errorf("%w: mandatory feature %s was not enabled", ErrRunnerProtocolNegotiation, feature)
		}
	}
	return nil
}

func (s *RunnerProtocolService) sendRegistration(
	stream RunnerProtocolStream,
	connectionID string,
	selectedProtocolVersion uint32,
	readiness BackendReadiness,
) error {
	sequence := s.nextSequence()
	registration := &runnerprotocol.RunnerRegistration{
		MessageId:         s.messageID(sequence),
		Sequence:          sequence,
		RunnerId:          s.config.RunnerID,
		ConnectionId:      connectionID,
		RunnerPoolId:      s.config.RunnerPoolID,
		SoftwareVersion:   s.config.SoftwareVersion,
		ProtocolVersion:   selectedProtocolVersion,
		Capabilities:      readiness.Capabilities,
		Allocatable:       readiness.Capacity,
		Reserved:          readiness.Reserved,
		ArtifactCache:     readiness.ArtifactCache,
		ReadinessFailures: readiness.ReadinessFailures,
	}
	if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Registration{Registration: registration},
	}); err != nil {
		return fmt.Errorf("SecondBox runner protocol send registration: %w", err)
	}
	return nil
}

func (s *RunnerProtocolService) consumeCommands(
	ctx context.Context,
	stream RunnerProtocolStream,
	welcome *runnerprotocol.RunnerWelcome,
	readiness BackendReadiness,
) error {
	received := make(chan receivedControlPlaneFrame, 1)
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	go pumpControlPlaneFrames(connectionCtx, stream.Recv, received)
	ticker := time.NewTicker(time.Duration(welcome.HeartbeatIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	asyncErrors := make(chan error, 1)
	controlState := newControlCommandState()
	enabled := make(map[runnerprotocol.RunnerFeature]bool, len(welcome.EnabledFeatures))
	for _, feature := range welcome.EnabledFeatures {
		enabled[feature] = true
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.sendHeartbeat(stream, welcome.ConnectionId, readiness); err != nil {
				return err
			}
		case frame := <-received:
			if frame.err != nil {
				return frame.err
			}
			duplicate, err := controlState.accept(frame.message)
			if err != nil {
				return err
			}
			if duplicate {
				continue
			}
			if err := s.handleCommand(connectionCtx, stream, frame.message, enabled, asyncErrors); err != nil {
				return err
			}
		case err := <-asyncErrors:
			return err
		case terminal := <-s.instanceTerminals:
			if err := s.sendInstanceTerminal(ctx, stream, terminal); err != nil {
				return err
			}
		}
	}
}

func pumpControlPlaneFrames(
	ctx context.Context,
	recv func() (*runnerprotocol.ControlPlaneToRunner, error),
	received chan<- receivedControlPlaneFrame,
) {
	for {
		message, err := recv()
		select {
		case received <- receivedControlPlaneFrame{message: message, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

type controlCommandState struct {
	lastSequence uint64
	messages     map[string][]byte
}

func newControlCommandState() *controlCommandState {
	return &controlCommandState{messages: make(map[string][]byte)}
}

func (state *controlCommandState) accept(
	message *runnerprotocol.ControlPlaneToRunner,
) (bool, error) {
	var (
		messageID string
		sequence  uint64
	)
	switch {
	case message.GetAssignment() != nil:
		messageID = message.GetAssignment().MessageId
		sequence = message.GetAssignment().Sequence
	case message.GetFence() != nil:
		messageID = message.GetFence().MessageId
		sequence = message.GetFence().Sequence
	case message.GetDrain() != nil:
		messageID = message.GetDrain().MessageId
		sequence = message.GetDrain().Sequence
	case message.GetCheckpoint() != nil:
		messageID = message.GetCheckpoint().MessageId
		sequence = message.GetCheckpoint().Sequence
	default:
		return false, nil
	}
	if strings.TrimSpace(messageID) == "" || sequence == 0 {
		return false, fmt.Errorf("SecondBox runner control command envelope is incomplete")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return false, fmt.Errorf("SecondBox runner encode control command: %w", err)
	}
	if previous, exists := state.messages[messageID]; exists {
		if bytes.Equal(previous, encoded) {
			return true, nil
		}
		return false, fmt.Errorf("SecondBox runner control command ID was reused with different content")
	}
	if sequence <= state.lastSequence {
		return false, fmt.Errorf("SecondBox runner control command sequence is reordered")
	}
	state.messages[messageID] = bytes.Clone(encoded)
	state.lastSequence = sequence
	return false, nil
}

func (s *RunnerProtocolService) handleCommand(
	ctx context.Context,
	stream RunnerProtocolStream,
	message *runnerprotocol.ControlPlaneToRunner,
	enabled map[runnerprotocol.RunnerFeature]bool,
	asyncErrors chan<- error,
) error {
	switch {
	case message.GetAssignment() != nil:
		return s.handleAssignment(ctx, stream, message.GetAssignment())
	case message.GetFence() != nil:
		return s.handleFence(ctx, stream, message.GetFence())
	case message.GetDrain() != nil:
		return s.handleDrain(stream, message.GetDrain())
	case message.GetExec() != nil:
		return s.handleExecFrame(ctx, stream, message.GetExec(), enabled, asyncErrors)
	case message.GetPty() != nil:
		return s.handlePTYFrame(ctx, stream, message.GetPty(), enabled)
	case message.GetFile() != nil:
		return s.handleFileFrame(ctx, stream, message.GetFile(), enabled, asyncErrors)
	case message.GetPort() != nil:
		return s.handlePortFrame(ctx, stream, message.GetPort(), enabled, asyncErrors)
	case message.GetCheckpoint() != nil:
		if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT] ||
			s.checkpointBackend == nil {
			return fmt.Errorf("SecondBox runner checkpoint feature was not negotiated")
		}
		return s.handleCheckpoint(ctx, stream, message.GetCheckpoint())
	case message.GetRestoreBegin() != nil:
		if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT] ||
			s.restoreBackend == nil {
			return fmt.Errorf("SecondBox runner checkpoint restore feature was not negotiated")
		}
		return s.handleRestoreBegin(ctx, message.GetRestoreBegin())
	case message.GetRestoreChunk() != nil:
		if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT] ||
			s.restoreBackend == nil {
			return fmt.Errorf("SecondBox runner checkpoint restore feature was not negotiated")
		}
		return s.handleRestoreChunk(ctx, message.GetRestoreChunk())
	default:
		return fmt.Errorf("SecondBox runner protocol received unsupported control-plane frame")
	}
}

func (s *RunnerProtocolService) handleCheckpoint(
	ctx context.Context,
	stream RunnerProtocolStream,
	command *runnerprotocol.CheckpointCommand,
) error {
	if command == nil || command.Fence == nil || command.CheckpointId == "" ||
		command.StorageObjectId == "" || command.MaximumSizeBytes == 0 ||
		command.DeadlineUnixMs == 0 || !s.hasActiveFence(command.Fence) {
		return fmt.Errorf("SecondBox runner Checkpoint command authority is incomplete or stale")
	}
	if err := s.validateOperationCorrelation(
		command.Fence,
		command.GetCorrelation().GetOperationId(),
		command.Correlation,
	); err != nil {
		return err
	}
	deadline := time.UnixMilli(int64(command.DeadlineUnixMs))
	if !deadline.After(time.Now()) {
		return s.sendCheckpointResult(
			stream, command, CheckpointEvidence{},
			runnerprotocol.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_DEADLINE_EXCEEDED,
			"checkpoint deadline expired",
		)
	}
	checkpointContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var offset uint64
	emit := func(data []byte) error {
		if len(data) == 0 {
			return nil
		}
		if uint64(len(data)) > command.MaximumSizeBytes-offset {
			return fmt.Errorf("SecondBox runner checkpoint exceeds command size bound")
		}
		sequence := s.nextSequence()
		if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_CheckpointChunk{
				CheckpointChunk: &runnerprotocol.CheckpointChunk{
					MessageId: s.messageID(sequence), Sequence: sequence, Fence: command.Fence,
					CheckpointId: command.CheckpointId, StorageObjectId: command.StorageObjectId,
					Offset: offset, Data: append([]byte(nil), data...),
				},
			},
		}); err != nil {
			return err
		}
		offset += uint64(len(data))
		return nil
	}
	evidence, err := s.checkpointBackend.CreateCheckpoint(checkpointContext, command, emit)
	if err != nil {
		terminal := runnerprotocol.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_RUNNER_FAILED
		if errors.Is(checkpointContext.Err(), context.DeadlineExceeded) {
			terminal = runnerprotocol.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_DEADLINE_EXCEEDED
		}
		return s.sendCheckpointResult(stream, command, evidence, terminal, "runner checkpoint failed")
	}
	if evidence.SizeBytes != offset || evidence.SizeBytes > command.MaximumSizeBytes ||
		evidence.SHA256 == "" || len(evidence.Compatibility) == 0 {
		return s.sendCheckpointResult(
			stream, command, evidence,
			runnerprotocol.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_INTEGRITY_FAILED,
			"checkpoint evidence does not match streamed bytes",
		)
	}
	sequence := s.nextSequence()
	if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_CheckpointChunk{
			CheckpointChunk: &runnerprotocol.CheckpointChunk{
				MessageId: s.messageID(sequence), Sequence: sequence, Fence: command.Fence,
				CheckpointId: command.CheckpointId, StorageObjectId: command.StorageObjectId,
				Offset: offset, EndOfObject: true,
			},
		},
	}); err != nil {
		return err
	}
	return s.sendCheckpointResult(
		stream, command, evidence,
		runnerprotocol.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_CREATED, "",
	)
}

func (s *RunnerProtocolService) sendCheckpointResult(
	stream RunnerProtocolStream,
	command *runnerprotocol.CheckpointCommand,
	evidence CheckpointEvidence,
	terminal runnerprotocol.CheckpointTerminalKind,
	safeDetail string,
) error {
	if err := s.emitEvidence(
		context.Background(),
		runnerevidence.EventCheckpointTerminal,
		command.Fence,
		command.Correlation,
		command.GetCorrelation().GetOperationId(),
		terminal.String(),
		terminalOutcome(terminal.String()),
	); err != nil {
		return err
	}
	sequence := s.nextSequence()
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_CheckpointResult{
			CheckpointResult: &runnerprotocol.CheckpointResult{
				MessageId: s.messageID(sequence), Sequence: sequence, Fence: command.Fence,
				CheckpointId: command.CheckpointId, StorageObjectId: command.StorageObjectId,
				Terminal: terminal, Sha256: evidence.SHA256, SizeBytes: evidence.SizeBytes,
				Compatibility:             evidence.Compatibility,
				TerminationEvidenceDigest: evidence.TerminationEvidenceDigest,
				SafeDetail:                safeDetail, Correlation: command.Correlation,
			},
		},
	})
}

func (s *RunnerProtocolService) handleRestoreBegin(
	ctx context.Context,
	begin *runnerprotocol.RestoreBegin,
) error {
	if begin == nil || begin.Fence == nil || begin.CheckpointId == "" ||
		begin.StorageObjectId == "" || begin.Sha256 == "" || begin.SizeBytes == 0 ||
		begin.DeadlineUnixMs == 0 {
		return fmt.Errorf("SecondBox runner Restore begin authority is incomplete")
	}
	operationID := begin.GetCorrelation().GetOperationId()
	if err := s.validateOperationCorrelation(begin.Fence, operationID, begin.Correlation); err != nil {
		return err
	}
	key := runnerRestoreOperationKey(begin.Fence, begin.CheckpointId)
	s.operationMu.Lock()
	existing := s.restoreOperations[key]
	if existing != nil {
		matches := sameRunnerFence(existing.fence, begin.Fence) &&
			existing.storageObjectID == begin.StorageObjectId &&
			proto.Equal(existing.correlation, begin.Correlation)
		s.operationMu.Unlock()
		if !matches {
			return fmt.Errorf("SecondBox runner Restore begin conflicts with retained operation state")
		}
		return nil
	}
	s.operationMu.Unlock()
	if err := s.restoreBackend.BeginRestore(ctx, begin); err != nil {
		evidenceErr := s.emitEvidence(
			context.Background(),
			runnerevidence.EventRestoreTerminal,
			begin.Fence,
			begin.Correlation,
			operationID,
			"begin_failed",
			"failed",
		)
		return errors.Join(err, evidenceErr)
	}
	s.operationMu.Lock()
	s.restoreOperations[key] = &runnerRestoreOperation{
		fence: cloneRunnerFence(begin.Fence), correlation: cloneRunnerCorrelation(begin.Correlation),
		checkpointID: begin.CheckpointId, storageObjectID: begin.StorageObjectId,
	}
	s.operationMu.Unlock()
	return nil
}

func (s *RunnerProtocolService) handleRestoreChunk(
	ctx context.Context,
	chunk *runnerprotocol.RestoreChunk,
) error {
	if chunk == nil || chunk.Fence == nil || chunk.CheckpointId == "" ||
		chunk.StorageObjectId == "" {
		return fmt.Errorf("SecondBox runner Restore chunk authority is incomplete")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("SecondBox runner Restore chunk encoding: %w", err)
	}
	key := runnerRestoreOperationKey(chunk.Fence, chunk.CheckpointId)
	s.operationMu.Lock()
	state := s.restoreOperations[key]
	if state == nil ||
		!sameRunnerFence(state.fence, chunk.Fence) ||
		state.storageObjectID != chunk.StorageObjectId {
		s.operationMu.Unlock()
		return fmt.Errorf("SecondBox runner Restore chunk correlation is missing or stale")
	}
	if state.terminal {
		duplicate := bytes.Equal(state.terminalFrame, encoded)
		s.operationMu.Unlock()
		if duplicate {
			return nil
		}
		return fmt.Errorf("SecondBox runner Restore chunk follows terminal state")
	}
	s.operationMu.Unlock()
	if err := s.restoreBackend.WriteRestoreChunk(ctx, chunk); err != nil {
		evidenceErr := s.emitEvidence(
			context.Background(),
			runnerevidence.EventRestoreTerminal,
			state.fence,
			state.correlation,
			state.correlation.OperationId,
			"restore_failed",
			"failed",
		)
		s.operationMu.Lock()
		state.terminal = true
		state.terminalFrame = bytes.Clone(encoded)
		s.operationMu.Unlock()
		return errors.Join(err, evidenceErr)
	}
	if !chunk.EndOfObject {
		return nil
	}
	if err := s.emitEvidence(
		context.Background(),
		runnerevidence.EventRestoreTerminal,
		state.fence,
		state.correlation,
		state.correlation.OperationId,
		"restored",
		"completed",
	); err != nil {
		return err
	}
	s.operationMu.Lock()
	state.terminal = true
	state.terminalFrame = bytes.Clone(encoded)
	s.operationMu.Unlock()
	return nil
}

func runnerRestoreOperationKey(
	fence *runnerprotocol.AssignmentFence,
	checkpointID string,
) string {
	return strings.Join([]string{
		fence.AssignmentId,
		fmt.Sprintf("%d", fence.SandboxGeneration),
		checkpointID,
	}, "\x00")
}

func (s *RunnerProtocolService) handleAssignment(
	ctx context.Context,
	stream RunnerProtocolStream,
	assignment *runnerprotocol.AssignmentCommand,
) error {
	if s.drainPhase() != runnerprotocol.DrainPhase_DRAIN_PHASE_ACTIVE {
		return s.sendAssignmentAck(
			stream,
			assignment,
			runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_DRAINING,
			"runner is draining",
		)
	}
	if err := validateResolvedAssignment(assignment); err != nil {
		return s.sendAssignmentAck(
			stream,
			assignment,
			runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_INCOMPATIBLE_PROFILE,
			err.Error(),
		)
	}
	if err := s.backend.ValidateAssignment(ctx, assignment); err != nil {
		return s.sendAssignmentAck(
			stream,
			assignment,
			runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_PREREQUISITE,
			err.Error(),
		)
	}
	if err := s.sendAssignmentAck(
		stream,
		assignment,
		runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED,
		"",
	); err != nil {
		return err
	}
	progress := func(stage runnerprotocol.AssignmentProgressStage) error {
		sequence := s.nextSequence()
		return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
			Message: &runnerprotocol.RunnerToControlPlane_AssignmentProgress{
				AssignmentProgress: &runnerprotocol.AssignmentProgress{
					MessageId:        s.messageID(sequence),
					Sequence:         sequence,
					Fence:            assignment.Fence,
					Stage:            stage,
					ObservedAtUnixMs: uint64(time.Now().UnixMilli()),
					Correlation:      s.assignmentCorrelation(assignment),
				},
			},
		})
	}
	instance, err := s.backend.StartAssignment(ctx, assignment, progress)
	terminal := runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY
	safeDetail := ""
	if err != nil {
		terminal = runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_RUNNER_FAILED
		safeDetail = err.Error()
	} else {
		s.recordActiveAssignment(assignment.Fence, instance.BackendReference)
		s.recordAssignmentCorrelation(assignment)
	}
	if evidenceErr := s.emitEvidence(
		ctx,
		runnerevidence.EventAssignmentTerminal,
		assignment.Fence,
		s.assignmentCorrelation(assignment),
		"",
		terminal.String(),
		terminalOutcome(terminal.String()),
	); evidenceErr != nil {
		return evidenceErr
	}
	sequence := s.nextSequence()
	if err := s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerprotocol.AssignmentResult{
				MessageId:        s.messageID(sequence),
				Sequence:         sequence,
				Fence:            assignment.Fence,
				Terminal:         terminal,
				BackendKind:      instance.BackendKind,
				BackendReference: instance.BackendReference,
				SafeDetail:       safeDetail,
				Correlation:      s.assignmentCorrelation(assignment),
			},
		},
	}); err != nil {
		return err
	}
	if terminal == runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY &&
		s.terminalBackend != nil {
		if err := s.terminalBackend.MarkAssignmentReady(assignment.Fence); err != nil {
			return fmt.Errorf("SecondBox runner establish terminal evidence baseline: %w", err)
		}
	}
	return nil
}

func (s *RunnerProtocolService) sendInstanceTerminal(
	ctx context.Context,
	stream RunnerProtocolStream,
	terminal BackendInstanceTerminal,
) error {
	if !s.hasActiveFence(terminal.Fence) ||
		terminal.Correlation == nil ||
		terminal.Correlation.RequestId == "" ||
		terminal.Correlation.OperationId == "" ||
		terminal.Correlation.SandboxId != terminal.Fence.GetSandboxId() ||
		terminal.Correlation.InstanceId != terminal.Fence.GetInstanceId() ||
		terminal.Correlation.SandboxGeneration != terminal.Fence.GetSandboxGeneration() ||
		terminal.Correlation.AssignmentId != terminal.Fence.GetAssignmentId() ||
		terminal.Correlation.RunnerId != s.config.RunnerID ||
		terminal.ObservedAt.IsZero() ||
		!immutableManifestDigest.MatchString(terminal.EvidenceDigest) ||
		!validObservedTerminationReason(terminal.Reason) {
		return errors.New("SecondBox Runner instance terminal evidence is incomplete or stale")
	}
	authoritative := s.correlationForAssignment(terminal.Fence.AssignmentId)
	if !proto.Equal(authoritative, terminal.Correlation) {
		return errors.New("SecondBox Runner instance terminal correlation changed after assignment")
	}
	if err := s.emitEvidence(
		ctx,
		runnerevidence.EventInstanceTerminal,
		terminal.Fence,
		terminal.Correlation,
		"",
		terminal.Reason.String(),
		"observed",
	); err != nil {
		return err
	}
	sequence := s.nextSequence()
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_InstanceTerminal{
			InstanceTerminal: &runnerprotocol.InstanceTerminal{
				MessageId:                 s.messageID(sequence),
				Sequence:                  sequence,
				Fence:                     terminal.Fence,
				Reason:                    terminal.Reason,
				ObservedAtUnixMs:          uint64(terminal.ObservedAt.UTC().UnixMilli()),
				TerminationEvidenceDigest: terminal.EvidenceDigest,
				Correlation:               terminal.Correlation,
			},
		},
	})
}

func validObservedTerminationReason(
	reason runnerprotocol.InstanceObservedTerminationReason,
) bool {
	switch reason {
	case runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_GUEST_SHUTDOWN,
		runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_RESOURCE_EXHAUSTION,
		runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE:
		return true
	default:
		return false
	}
}

func (s *RunnerProtocolService) sendAssignmentAck(
	stream RunnerProtocolStream,
	assignment *runnerprotocol.AssignmentCommand,
	decision runnerprotocol.AssignmentDecision,
	safeDetail string,
) error {
	if decision != runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED {
		if err := s.emitEvidence(
			context.Background(),
			runnerevidence.EventAssignmentTerminal,
			assignment.GetFence(),
			s.assignmentCorrelation(assignment),
			"",
			decision.String(),
			"rejected",
		); err != nil {
			return err
		}
	}
	sequence := s.nextSequence()
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_AssignmentAck{
			AssignmentAck: &runnerprotocol.AssignmentAck{
				MessageId:  s.messageID(sequence),
				Sequence:   sequence,
				Fence:      assignment.GetFence(),
				Decision:   decision,
				SafeDetail: safeDetail,
			},
		},
	})
}

func (s *RunnerProtocolService) handleFence(
	ctx context.Context,
	stream RunnerProtocolStream,
	command *runnerprotocol.FenceCommand,
) error {
	if command == nil || command.Fence == nil {
		return fmt.Errorf("SecondBox runner Fence command authority is incomplete")
	}
	operationID := command.GetCorrelation().GetOperationId()
	if err := s.validateOperationCorrelation(command.Fence, operationID, command.Correlation); err != nil {
		return err
	}
	correlation := cloneRunnerCorrelation(command.Correlation)
	evidence, err := s.backend.FenceAssignment(ctx, command)
	if err != nil {
		evidence.Result = runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_FAILED
	} else if command != nil && command.Fence != nil {
		s.removeActiveAssignment(command.Fence.AssignmentId)
	}
	sequence := s.nextSequence()
	result := &runnerprotocol.FenceResult{
		MessageId:                 s.messageID(sequence),
		Sequence:                  sequence,
		Fence:                     command.GetFence(),
		Result:                    evidence.Result,
		TerminationEvidenceDigest: evidence.TerminationEvidenceDigest,
		Correlation:               cloneRunnerCorrelation(correlation),
	}
	if err != nil {
		result.SafeDetail = err.Error()
	}
	if evidenceErr := s.emitEvidence(
		ctx,
		runnerevidence.EventFenceTerminal,
		command.GetFence(),
		correlation,
		operationID,
		result.Result.String(),
		terminalOutcome(result.Result.String()),
	); evidenceErr != nil {
		return evidenceErr
	}
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_FenceResult{FenceResult: result},
	})
}

func (s *RunnerProtocolService) handleDrain(
	stream RunnerProtocolStream,
	command *runnerprotocol.DrainCommand,
) error {
	remaining := s.activeAssignments()
	phase := runnerprotocol.DrainPhase_DRAIN_PHASE_DRAINING
	if len(remaining) == 0 {
		phase = runnerprotocol.DrainPhase_DRAIN_PHASE_DRAINED
	}
	s.stateMu.Lock()
	s.drain = phase
	s.stateMu.Unlock()
	sequence := s.nextSequence()
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_DrainState{
			DrainState: &runnerprotocol.DrainState{
				MessageId:            s.messageID(sequence),
				Sequence:             sequence,
				Phase:                phase,
				RemainingAssignments: remaining,
			},
		},
	})
}

func (s *RunnerProtocolService) sendHeartbeat(
	stream RunnerProtocolStream,
	connectionID string,
	readiness BackendReadiness,
) error {
	sequence := s.nextSequence()
	return s.sendRunnerFrame(stream, &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Heartbeat{
			Heartbeat: &runnerprotocol.RunnerHeartbeat{
				MessageId:         s.messageID(sequence),
				Sequence:          sequence,
				RunnerId:          s.config.RunnerID,
				ConnectionId:      connectionID,
				ObservedAtUnixMs:  uint64(time.Now().UnixMilli()),
				Allocatable:       readiness.Capacity,
				Reserved:          readiness.Reserved,
				ActiveAssignments: s.activeAssignments(),
				DrainPhase:        s.drainPhase(),
			},
		},
	})
}

func (s *RunnerProtocolService) recordActiveAssignment(
	fence *runnerprotocol.AssignmentFence,
	backendReference string,
) {
	if fence == nil {
		return
	}
	s.stateMu.Lock()
	s.active[fence.AssignmentId] = &runnerprotocol.ActiveAssignmentSummary{
		AssignmentId:      fence.AssignmentId,
		SandboxId:         fence.SandboxId,
		InstanceId:        fence.InstanceId,
		SandboxGeneration: fence.SandboxGeneration,
		FencingToken:      append([]byte(nil), fence.FencingToken...),
	}
	s.stateMu.Unlock()
}

func (s *RunnerProtocolService) removeActiveAssignment(assignmentID string) {
	s.stateMu.Lock()
	delete(s.active, assignmentID)
	delete(s.correlations, assignmentID)
	if s.drain == runnerprotocol.DrainPhase_DRAIN_PHASE_DRAINING && len(s.active) == 0 {
		s.drain = runnerprotocol.DrainPhase_DRAIN_PHASE_DRAINED
	}
	s.stateMu.Unlock()
}

func (s *RunnerProtocolService) activeAssignments() []*runnerprotocol.ActiveAssignmentSummary {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	assignments := make([]*runnerprotocol.ActiveAssignmentSummary, 0, len(s.active))
	for _, active := range s.active {
		assignments = append(assignments, &runnerprotocol.ActiveAssignmentSummary{
			AssignmentId:       active.AssignmentId,
			SandboxId:          active.SandboxId,
			InstanceId:         active.InstanceId,
			SandboxGeneration:  active.SandboxGeneration,
			FencingToken:       append([]byte(nil), active.FencingToken...),
			ActiveOperationIds: append([]string(nil), active.ActiveOperationIds...),
		})
	}
	return assignments
}

func (s *RunnerProtocolService) hasActiveFence(fence *runnerprotocol.AssignmentFence) bool {
	if fence == nil {
		return false
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	active := s.active[fence.AssignmentId]
	return active != nil &&
		active.SandboxId == fence.SandboxId &&
		active.InstanceId == fence.InstanceId &&
		active.SandboxGeneration == fence.SandboxGeneration &&
		bytes.Equal(active.FencingToken, fence.FencingToken)
}

func (s *RunnerProtocolService) setActiveOperation(
	assignmentID string,
	operationID string,
	active bool,
) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	assignment := s.active[assignmentID]
	if assignment == nil {
		return
	}
	index := -1
	for candidate, existing := range assignment.ActiveOperationIds {
		if existing == operationID {
			index = candidate
			break
		}
	}
	if active && index == -1 {
		assignment.ActiveOperationIds = append(assignment.ActiveOperationIds, operationID)
	}
	if !active && index != -1 {
		assignment.ActiveOperationIds = append(
			assignment.ActiveOperationIds[:index],
			assignment.ActiveOperationIds[index+1:]...,
		)
	}
}

func (s *RunnerProtocolService) drainPhase() runnerprotocol.DrainPhase {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.drain
}

func (s *RunnerProtocolService) assignmentCorrelation(
	assignment *runnerprotocol.AssignmentCommand,
) *runnerprotocol.Correlation {
	if assignment == nil {
		return &runnerprotocol.Correlation{RunnerId: s.config.RunnerID}
	}
	correlation := assignment.GetCorrelation()
	if correlation == nil {
		correlation = &runnerprotocol.Correlation{}
	} else {
		correlation = &runnerprotocol.Correlation{
			RequestId:   correlation.RequestId,
			OperationId: correlation.OperationId,
			LeaseId:     correlation.LeaseId,
		}
	}
	if fence := assignment.GetFence(); fence != nil {
		correlation.SandboxId = fence.SandboxId
		correlation.InstanceId = fence.InstanceId
		correlation.SandboxGeneration = fence.SandboxGeneration
		correlation.AssignmentId = fence.AssignmentId
	}
	correlation.RunnerId = s.config.RunnerID
	return correlation
}

func (s *RunnerProtocolService) recordAssignmentCorrelation(assignment *runnerprotocol.AssignmentCommand) {
	if assignment == nil || assignment.Fence == nil {
		return
	}
	s.stateMu.Lock()
	s.correlations[assignment.Fence.AssignmentId] = s.assignmentCorrelation(assignment)
	s.stateMu.Unlock()
}

func (s *RunnerProtocolService) correlationForAssignment(assignmentID string) *runnerprotocol.Correlation {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	correlation := s.correlations[assignmentID]
	if correlation == nil {
		return &runnerprotocol.Correlation{AssignmentId: assignmentID, RunnerId: s.config.RunnerID}
	}
	return proto.Clone(correlation).(*runnerprotocol.Correlation)
}

func (s *RunnerProtocolService) emitEvidence(
	ctx context.Context,
	event runnerevidence.Event,
	fence *runnerprotocol.AssignmentFence,
	correlation *runnerprotocol.Correlation,
	operationID string,
	terminalKind string,
	outcome string,
) error {
	record := runnerevidence.NewRecord(event, outcome, terminalKind, time.Now().UTC())
	if correlation != nil {
		record.RequestID = correlation.RequestId
		record.OperationID = correlation.OperationId
		record.LeaseID = correlation.LeaseId
		record.RunnerID = correlation.RunnerId
	}
	if operationID != "" {
		record.OperationID = operationID
	}
	if fence != nil {
		record.SandboxID = fence.SandboxId
		record.InstanceID = fence.InstanceId
		record.SandboxGeneration = fence.SandboxGeneration
		record.AssignmentID = fence.AssignmentId
	}
	if record.RunnerID == "" {
		record.RunnerID = s.config.RunnerID
	}
	if err := record.Validate(); err != nil {
		return err
	}
	return s.evidence.Emit(ctx, record)
}

func terminalOutcome(terminal string) string {
	if strings.Contains(terminal, "READY") ||
		strings.Contains(terminal, "EXITED") ||
		strings.Contains(terminal, "COMPLETED") ||
		strings.Contains(terminal, "STOPPED") ||
		strings.Contains(terminal, "CREATED") ||
		strings.Contains(terminal, "CLOSED") {
		return "completed"
	}
	return "failed"
}

func (s *RunnerProtocolService) nextSequence() uint64 {
	return s.sequence.Add(1)
}

func (s *RunnerProtocolService) messageID(sequence uint64) string {
	return fmt.Sprintf("%s-%d", s.config.RunnerID, sequence)
}

func (s *RunnerProtocolService) sendRunnerFrame(
	stream RunnerProtocolStream,
	message *runnerprotocol.RunnerToControlPlane,
) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return stream.Send(message)
}

func validateResolvedAssignment(assignment *runnerprotocol.AssignmentCommand) error {
	if assignment == nil || assignment.Fence == nil {
		return fmt.Errorf("SecondBox runner assignment is missing fencing identity")
	}
	if strings.TrimSpace(assignment.MessageId) == "" ||
		assignment.Sequence == 0 ||
		strings.TrimSpace(assignment.Fence.AssignmentId) == "" ||
		strings.TrimSpace(assignment.Fence.SandboxId) == "" ||
		strings.TrimSpace(assignment.Fence.InstanceId) == "" ||
		assignment.Fence.SandboxGeneration == 0 ||
		len(assignment.Fence.FencingToken) == 0 {
		return fmt.Errorf("SecondBox runner assignment has incomplete generation or fencing identity")
	}
	if strings.TrimSpace(assignment.ProfileRevisionId) == "" || assignment.Requirements == nil {
		return fmt.Errorf("SecondBox runner assignment is not profile resolved")
	}
	requirements := assignment.Requirements
	if requirements.VcpuCount == 0 ||
		requirements.MemoryBytes == 0 ||
		requirements.DiskBytes == 0 ||
		strings.TrimSpace(requirements.Architecture) == "" ||
		requirements.MaximumOperationMs == 0 ||
		requirements.MaximumOutputBytes == 0 {
		return fmt.Errorf("SecondBox runner assignment has incomplete immutable profile requirements")
	}
	if len(assignment.Assets) == 0 {
		return fmt.Errorf("SecondBox runner assignment has no signed immutable assets")
	}
	for _, asset := range assignment.Assets {
		if asset == nil ||
			strings.TrimSpace(asset.ArtifactId) == "" ||
			!immutableManifestDigest.MatchString(asset.ManifestDigest) ||
			strings.TrimSpace(asset.SignatureKeyId) == "" ||
			strings.TrimSpace(asset.Architecture) == "" ||
			asset.GuestProtocolGeneration == 0 {
			return fmt.Errorf("SecondBox runner assignment contains a mutable or unsigned asset")
		}
	}
	if err := validateResolvedNetworkPolicy(assignment.NetworkPolicy); err != nil {
		return err
	}
	return nil
}

func validateResolvedNetworkPolicy(policy *runnerprotocol.NetworkPolicy) error {
	if policy == nil || policy.Mode == runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_UNSPECIFIED {
		return fmt.Errorf("SecondBox runner assignment has no resolved network policy")
	}
	switch policy.Mode {
	case runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL:
		if len(policy.Destinations) != 0 {
			return fmt.Errorf("SecondBox runner deny-all network policy contains destinations")
		}
	case runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST:
		if len(policy.Destinations) == 0 {
			return fmt.Errorf("SecondBox runner allow-list network policy has no destinations")
		}
		for _, destination := range policy.Destinations {
			if destination == nil ||
				destination.Protocol == runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_UNSPECIFIED ||
				destination.Port == 0 ||
				destination.Port > 65535 {
				return fmt.Errorf("SecondBox runner allow-list network destination is incomplete")
			}
			switch target := destination.Target.(type) {
			case *runnerprotocol.NetworkDestination_Domain:
				if strings.TrimSpace(target.Domain) == "" {
					return fmt.Errorf("SecondBox runner allow-list domain is empty")
				}
			case *runnerprotocol.NetworkDestination_Cidr:
				if strings.TrimSpace(target.Cidr) == "" {
					return fmt.Errorf("SecondBox runner allow-list CIDR is empty")
				}
			default:
				return fmt.Errorf("SecondBox runner allow-list network destination target is absent")
			}
		}
	default:
		return fmt.Errorf("SecondBox runner assignment selects an unknown network policy mode")
	}
	return nil
}
