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
// release assignment authority or the runner-local Workspace attachment.
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

// LocalWorkspaceEvidence is bounded logical receipt or inventory evidence. It
// cannot carry a host path or workspace image bytes.
type LocalWorkspaceEvidence struct {
	PreviousGeneration uint64
	Generation         uint64
	LogicalCapacity    uint64
	ReceiptRecordedAt  time.Time
	Inventory          []*runnerprotocol.LocalWorkspaceInventoryItem
	Receipts           []*runnerprotocol.LocalWorkspaceReceiptItem
}

// LocalWorkspaceBackend executes versioned runner-local storage commands.
type LocalWorkspaceBackend interface {
	ExecuteLocalWorkspace(
		context.Context,
		*runnerprotocol.LocalWorkspaceCommand,
	) (LocalWorkspaceEvidence, error)
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

type startupTimingBackend interface {
	StartupTiming() (uint64, time.Duration)
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
	workspaceBackend  LocalWorkspaceBackend
	terminalBackend   instanceTerminalBackend
	instanceTerminals <-chan BackendInstanceTerminal
	connector         RunnerProtocolConnector
	sequence          uint64
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
	workspaceBackend, implementsWorkspace := backend.(LocalWorkspaceBackend)
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
		if feature == runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE &&
			!implementsWorkspace {
			return nil, fmt.Errorf("SecondBox runner local-workspace feature requires a WorkspaceStore backend")
		}
	}
	service := &RunnerProtocolService{
		config:           config,
		backend:          backend,
		dataPlaneBackend: dataPlaneBackend,
		portBackend:      portBackend,
		workspaceBackend: workspaceBackend,
		terminalBackend:  terminalBackend,
		connector:        connector,
		drain:            runnerprotocol.DrainPhase_DRAIN_PHASE_ACTIVE,
		active:           make(map[string]*runnerprotocol.ActiveAssignmentSummary),
		execOperations:   make(map[string]*runnerExecOperation),
		fileOperations:   make(map[string]*runnerFileOperation),
		portOperations:   make(map[string]*runnerPortOperation),
		evidence:         runnerevidence.SlogSink{},
		correlations:     make(map[string]*runnerprotocol.Correlation),
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
	if err := s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_Registration{
					Registration: &runnerprotocol.RunnerRegistration{
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
						StartupTiming:     s.startupTiming(),
					},
				},
			}
		},
	); err != nil {
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
	// Assignment starts run off the receive loop, so the runner admits as many
	// concurrent assignments as it advertises capacity for. Handling them inline
	// admitted exactly one at a time: thirty-two concurrent assignments entered
	// the backend 298-437 ms apart, one full microVM start each, and the
	// reported admission stage measured that queue wait rather than any work.
	//
	// The wait is registered before the cancel so teardown cancels the
	// connection first and only then waits for in-flight starts to observe it.
	var assignmentsInFlight sync.WaitGroup
	defer assignmentsInFlight.Wait()
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	assignmentSlots := make(chan struct{}, concurrentAssignmentLimit(readiness))
	go pumpControlPlaneFrames(connectionCtx, stream.Recv, received)
	asyncErrors := make(chan error, 1)
	go s.sendHeartbeats(
		connectionCtx,
		stream,
		welcome.ConnectionId,
		readiness,
		time.Duration(welcome.HeartbeatIntervalMs)*time.Millisecond,
		asyncErrors,
	)
	controlState := newControlCommandState()
	enabled := make(map[runnerprotocol.RunnerFeature]bool, len(welcome.EnabledFeatures))
	for _, feature := range welcome.EnabledFeatures {
		enabled[feature] = true
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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
			// Sequence acceptance above already ran in receive order, so a
			// concurrent start cannot reorder the control command stream.
			if assignment := frame.message.GetAssignment(); assignment != nil {
				select {
				case assignmentSlots <- struct{}{}:
				case <-connectionCtx.Done():
					return connectionCtx.Err()
				}
				assignmentsInFlight.Add(1)
				go func() {
					defer assignmentsInFlight.Done()
					defer func() { <-assignmentSlots }()
					if err := s.handleAssignment(connectionCtx, stream, assignment); err != nil {
						select {
						case asyncErrors <- err:
						default:
						}
					}
				}()
				continue
			}
			// Fence and drain decide whether compute may keep running, so they
			// stay ordered behind any assignment start already in flight.
			if frame.message.GetFence() != nil || frame.message.GetDrain() != nil {
				assignmentsInFlight.Wait()
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

func (s *RunnerProtocolService) sendHeartbeats(
	ctx context.Context,
	stream RunnerProtocolStream,
	connectionID string,
	readiness BackendReadiness,
	interval time.Duration,
	asyncErrors chan<- error,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.sendHeartbeat(stream, connectionID, readiness); err != nil {
				select {
				case asyncErrors <- err:
				case <-ctx.Done():
				}
				return
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
	case message.GetLocalWorkspace() != nil:
		messageID = message.GetLocalWorkspace().MessageId
		sequence = message.GetLocalWorkspace().Sequence
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
	case message.GetLocalWorkspace() != nil:
		if !enabled[runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE] ||
			s.workspaceBackend == nil {
			return fmt.Errorf("SecondBox runner local-workspace feature was not negotiated")
		}
		return s.handleLocalWorkspace(ctx, stream, message.GetLocalWorkspace())
	default:
		return fmt.Errorf("SecondBox runner protocol received unsupported control-plane frame")
	}
}

func (s *RunnerProtocolService) handleLocalWorkspace(
	ctx context.Context,
	stream RunnerProtocolStream,
	command *runnerprotocol.LocalWorkspaceCommand,
) error {
	if command == nil ||
		command.CommandVersion != 1 ||
		command.Kind == runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_UNSPECIFIED ||
		strings.TrimSpace(command.EffectId) == "" {
		return fmt.Errorf("SecondBox runner local-workspace command is incomplete")
	}
	if command.Kind != runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE &&
		(strings.TrimSpace(command.SandboxId) == "" ||
			strings.TrimSpace(command.WorkspaceId) == "") {
		return fmt.Errorf("SecondBox runner local-workspace identity is incomplete")
	}
	switch command.Kind {
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT:
		if len(command.FencingToken) == 0 {
			return fmt.Errorf("SecondBox runner local-workspace fencing token is required")
		}
	}
	var (
		evidence     LocalWorkspaceEvidence
		executionErr error
	)
	executionStartedAt := time.Now()
	if command.Correlation == nil || command.Correlation.RunnerId != s.config.RunnerID {
		executionErr = localWorkspaceCommandError{
			terminal: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_WRONG_HOME_RUNNER,
		}
	} else {
		evidence, executionErr = s.workspaceBackend.ExecuteLocalWorkspace(ctx, command)
	}
	terminal := localWorkspaceTerminal(executionErr)
	slog.Info(
		"SecondBox runner local Workspace command completed",
		"kind", command.Kind.String(),
		"operationId", command.OperationId,
		"sandboxId", command.SandboxId,
		"workspaceId", command.WorkspaceId,
		"terminal", terminal.String(),
		"executionMs", time.Since(executionStartedAt).Milliseconds(),
	)
	result := &runnerprotocol.LocalWorkspaceResult{
		CommandVersion:       command.CommandVersion,
		Kind:                 command.Kind,
		Terminal:             terminal,
		OperationId:          command.OperationId,
		EffectId:             command.EffectId,
		SandboxId:            command.SandboxId,
		WorkspaceId:          command.WorkspaceId,
		SnapshotId:           command.SnapshotId,
		PreviousGeneration:   evidence.PreviousGeneration,
		Generation:           evidence.Generation,
		LogicalCapacityBytes: evidence.LogicalCapacity,
		Inventory:            evidence.Inventory,
		Receipts:             evidence.Receipts,
		Correlation:          cloneRunnerCorrelation(command.Correlation),
	}
	if !evidence.ReceiptRecordedAt.IsZero() {
		result.ReceiptRecordedAtUnixMs = uint64(evidence.ReceiptRecordedAt.UTC().UnixMilli())
	}
	if executionErr != nil {
		slog.Warn(
			"SecondBox runner local-workspace command failed",
			"kind", command.Kind.String(),
			"terminal", terminal.String(),
			"error", executionErr,
		)
		result.SafeDetail = localWorkspaceSafeDetail(terminal)
	}
	return s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			result.MessageId = s.messageID(sequence)
			result.Sequence = sequence
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_LocalWorkspaceResult{
					LocalWorkspaceResult: result,
				},
			}
		},
	)
}

func localWorkspaceSafeDetail(terminal runnerprotocol.LocalWorkspaceTerminalKind) string {
	switch terminal {
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_LOCAL_DATA_ABSENT:
		return "local workspace data is absent"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_ACTIVE_WRITER:
		return "workspace has an active writer"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_GENERATION:
		return "workspace generation is stale"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STALE_FENCE:
		return "workspace fencing authority is stale"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STORAGE_INCOMPATIBLE:
		return "local workspace storage is incompatible"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_INSUFFICIENT_SPACE:
		return "local workspace storage has insufficient space"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CORRUPT_RECEIPT:
		return "local workspace receipt is corrupt"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_CONFLICTING_REPLAY:
		return "local workspace operation conflicts with its durable receipt"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_WRONG_HOME_RUNNER:
		return "workspace is not owned by this runner"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SNAPSHOT_IN_USE:
		return "snapshot is in use"
	case runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RESTORE_PENDING:
		return "workspace restore is pending"
	default:
		return "local workspace operation failed"
	}
}

type localWorkspaceTerminalError interface {
	LocalWorkspaceTerminal() runnerprotocol.LocalWorkspaceTerminalKind
}

type localWorkspaceCommandError struct {
	terminal runnerprotocol.LocalWorkspaceTerminalKind
}

func (failure localWorkspaceCommandError) Error() string {
	return localWorkspaceSafeDetail(failure.terminal)
}

func (failure localWorkspaceCommandError) LocalWorkspaceTerminal() runnerprotocol.LocalWorkspaceTerminalKind {
	return failure.terminal
}

func localWorkspaceTerminal(err error) runnerprotocol.LocalWorkspaceTerminalKind {
	if err == nil {
		return runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
	}
	var typed localWorkspaceTerminalError
	if errors.As(err, &typed) {
		return typed.LocalWorkspaceTerminal()
	}
	return runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED
}

func (s *RunnerProtocolService) handleAssignment(
	ctx context.Context,
	stream RunnerProtocolStream,
	assignment *runnerprotocol.AssignmentCommand,
) error {
	admissionObservedAt := time.Now()
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
	if err := s.sendAssignmentProgress(
		stream,
		assignment,
		runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_RUNNER_ADMISSION,
		admissionObservedAt,
	); err != nil {
		return err
	}
	progress := func(stage runnerprotocol.AssignmentProgressStage) error {
		return s.sendAssignmentProgress(stream, assignment, stage, time.Now())
	}
	instance, err := s.backend.StartAssignment(ctx, assignment, progress)
	terminal := runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY
	safeDetail := ""
	if err != nil {
		slog.Warn(
			"SecondBox runner assignment start failed",
			"assignmentId", assignment.Fence.AssignmentId,
			"sandboxId", assignment.Fence.SandboxId,
			"error", err,
		)
		terminal = runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_RUNNER_FAILED
		safeDetail = "runner failed to start assignment"
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
	if err := s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
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
			}
		},
	); err != nil {
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

func (s *RunnerProtocolService) sendAssignmentProgress(
	stream RunnerProtocolStream,
	assignment *runnerprotocol.AssignmentCommand,
	stage runnerprotocol.AssignmentProgressStage,
	observedAt time.Time,
) error {
	return s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_AssignmentProgress{
					AssignmentProgress: &runnerprotocol.AssignmentProgress{
						MessageId:        s.messageID(sequence),
						Sequence:         sequence,
						Fence:            assignment.Fence,
						Stage:            stage,
						ObservedAtUnixMs: uint64(observedAt.UnixMilli()),
						Correlation:      s.assignmentCorrelation(assignment),
						ObservedAtUnixNs: uint64(observedAt.UnixNano()),
					},
				},
			}
		},
	)
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
	return s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
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
			}
		},
	)
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
		slog.Warn(
			"SecondBox runner assignment rejected",
			"decision", decision.String(),
			"safeDetail", safeDetail,
		)
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
	return s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_AssignmentAck{
					AssignmentAck: &runnerprotocol.AssignmentAck{
						MessageId:  s.messageID(sequence),
						Sequence:   sequence,
						Fence:      assignment.GetFence(),
						Decision:   decision,
						SafeDetail: safeDetail,
					},
				},
			}
		},
	)
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
	result := &runnerprotocol.FenceResult{
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
	return s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			result.MessageId = s.messageID(sequence)
			result.Sequence = sequence
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_FenceResult{FenceResult: result},
			}
		},
	)
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
	return s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
				Message: &runnerprotocol.RunnerToControlPlane_DrainState{
					DrainState: &runnerprotocol.DrainState{
						MessageId:            s.messageID(sequence),
						Sequence:             sequence,
						Phase:                phase,
						RemainingAssignments: remaining,
					},
				},
			}
		},
	)
}

func (s *RunnerProtocolService) sendHeartbeat(
	stream RunnerProtocolStream,
	connectionID string,
	readiness BackendReadiness,
) error {
	return s.sendSequencedRunnerFrame(
		stream,
		func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
			return &runnerprotocol.RunnerToControlPlane{
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
						StartupTiming:     s.startupTiming(),
					},
				},
			}
		},
	)
}

func (s *RunnerProtocolService) startupTiming() *runnerprotocol.StartupTiming {
	backend, ok := s.backend.(startupTimingBackend)
	if !ok {
		return &runnerprotocol.StartupTiming{}
	}
	count, p95 := backend.StartupTiming()
	if p95 < 0 {
		p95 = 0
	}
	return &runnerprotocol.StartupTiming{
		SampleCount: count, P95Milliseconds: uint64(p95.Milliseconds()),
	}
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

func (s *RunnerProtocolService) messageID(sequence uint64) string {
	return fmt.Sprintf("%s-%d", s.config.RunnerID, sequence)
}

func (s *RunnerProtocolService) sendSequencedRunnerFrame(
	stream RunnerProtocolStream,
	build func(uint64) *runnerprotocol.RunnerToControlPlane,
) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.sequence++
	return stream.Send(build(s.sequence))
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

// concurrentAssignmentLimit bounds concurrent assignment starts by the instance
// capacity the runner advertises to the control plane, so the runner never
// admits more work than it just claimed it could hold.
//
// A runner that advertises no instance capacity keeps the previous behaviour of
// starting one assignment at a time rather than assuming a bound it has not
// established.
func concurrentAssignmentLimit(readiness BackendReadiness) int {
	limit := int(readiness.Capacity.GetInstances())
	if limit < 1 {
		return 1
	}
	return limit
}
