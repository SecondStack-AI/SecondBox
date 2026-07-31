package runnercontrol

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// concurrentAssignmentBackend blocks every start until released, so the number
// of assignments inside StartAssignment at once is directly observable.
type concurrentAssignmentBackend struct {
	recordingAssignmentBackend
	entered     chan struct{}
	release     chan struct{}
	inside      atomic.Int32
	maxTogether atomic.Int32
}

type concurrentWorkspaceCreateBackend struct {
	recordingAssignmentBackend
	entered     chan struct{}
	release     chan struct{}
	inspected   chan struct{}
	inside      atomic.Int32
	maxTogether atomic.Int32
}

func (backend *concurrentWorkspaceCreateBackend) ExecuteLocalWorkspace(
	ctx context.Context,
	command *runnerprotocol.LocalWorkspaceCommand,
) (LocalWorkspaceEvidence, error) {
	if command.Kind !=
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE {
		backend.inspected <- struct{}{}
		return LocalWorkspaceEvidence{}, nil
	}
	current := backend.inside.Add(1)
	for {
		observed := backend.maxTogether.Load()
		if current <= observed || backend.maxTogether.CompareAndSwap(observed, current) {
			break
		}
	}
	backend.entered <- struct{}{}
	select {
	case <-backend.release:
	case <-ctx.Done():
		backend.inside.Add(-1)
		return LocalWorkspaceEvidence{}, ctx.Err()
	}
	backend.inside.Add(-1)
	return LocalWorkspaceEvidence{
		Generation: 1, LogicalCapacity: 16 << 20,
	}, nil
}

func (backend *concurrentAssignmentBackend) StartAssignment(
	_ context.Context,
	_ *runnerprotocol.AssignmentCommand,
	_ func(runnerprotocol.AssignmentProgressStage) error,
) (BackendInstance, error) {
	current := backend.inside.Add(1)
	for {
		observed := backend.maxTogether.Load()
		if current <= observed || backend.maxTogether.CompareAndSwap(observed, current) {
			break
		}
	}
	backend.entered <- struct{}{}
	<-backend.release
	backend.inside.Add(-1)
	return BackendInstance{BackendKind: "firecracker", BackendReference: "fc-instance"}, nil
}

func assignmentFrameAt(sequence int) *runnerprotocol.ControlPlaneToRunner {
	assignment := resolvedAssignmentCommand()
	assignment.MessageId = fmt.Sprintf("message-%d", sequence)
	assignment.Sequence = uint64(sequence)
	assignment.Fence.AssignmentId = fmt.Sprintf("assignment-%d", sequence)
	assignment.Fence.SandboxId = fmt.Sprintf("sandbox-%d", sequence)
	assignment.Fence.InstanceId = fmt.Sprintf("instance-%d", sequence)
	return &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Assignment{Assignment: assignment},
	}
}

func workspaceFrameAt(
	sequence int,
	kind runnerprotocol.LocalWorkspaceCommandKind,
) *runnerprotocol.ControlPlaneToRunner {
	command := localWorkspaceReconnectCommand(kind)
	command.MessageId = fmt.Sprintf("workspace-message-%d", sequence)
	command.Sequence = uint64(sequence)
	command.OperationId = fmt.Sprintf("workspace-operation-%d", sequence)
	command.EffectId = fmt.Sprintf("workspace-effect-%d", sequence)
	command.SandboxId = fmt.Sprintf("workspace-sandbox-%d", sequence)
	command.WorkspaceId = fmt.Sprintf("workspace-%d", sequence)
	command.Correlation.OperationId = command.OperationId
	command.Correlation.SandboxId = command.SandboxId
	return &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_LocalWorkspace{
			LocalWorkspace: command,
		},
	}
}

// Handling assignments inline admitted exactly one at a time, so a burst queued
// behind a full microVM start each. Starts now run off the receive loop, bounded
// independently from resident instance capacity.
func TestAssignmentsAreAdmittedConcurrentlyUpToConfiguredStartLimit(t *testing.T) {
	const capacity = 4
	const maximumConcurrentStarts = 2
	backend := &concurrentAssignmentBackend{
		recordingAssignmentBackend: recordingAssignmentBackend{
			readiness: BackendReadiness{
				Capacity:     &runnerprotocol.Capacity{Instances: capacity},
				Reserved:     &runnerprotocol.Capacity{},
				Capabilities: &runnerprotocol.RunnerCapabilities{},
			},
		},
		entered: make(chan struct{}, capacity),
		release: make(chan struct{}),
	}
	protocolConfig := testRunnerConfig()
	protocolConfig.MaximumConcurrentStarts = maximumConcurrentStarts
	service, err := NewRunnerProtocolService(
		protocolConfig, backend, staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	inbound := make([]*runnerprotocol.ControlPlaneToRunner, 0, capacity)
	for sequence := 1; sequence <= capacity; sequence++ {
		inbound = append(inbound, assignmentFrameAt(sequence))
	}
	stream := &blockingProtocolStream{
		ctx: runContext, inbound: inbound,
		heartbeats: make(chan *runnerprotocol.RunnerHeartbeat, capacity*4),
	}
	welcome := runnerWelcomeFrame("connection-concurrent")
	welcome.GetWelcome().HeartbeatIntervalMs = 50
	var consumed sync.WaitGroup
	consumed.Add(1)
	go func() {
		defer consumed.Done()
		_ = service.consumeCommands(runContext, stream, welcome.GetWelcome(), backend.readiness)
	}()

	for admitted := 0; admitted < maximumConcurrentStarts; admitted++ {
		select {
		case <-backend.entered:
		case <-time.After(3 * time.Second):
			t.Fatalf(
				"only %d of %d assignments were admitted concurrently; starts are serialised",
				admitted, maximumConcurrentStarts,
			)
		}
	}
	if got := backend.maxTogether.Load(); got != maximumConcurrentStarts {
		t.Fatalf(
			"peak concurrent assignment starts = %d, want %d",
			got,
			maximumConcurrentStarts,
		)
	}
	close(backend.release)
	cancelRun()
	consumed.Wait()
}

func TestConcurrentAssignmentLimitUsesConfiguredAndResidentBounds(t *testing.T) {
	if got := concurrentAssignmentLimit(8, BackendReadiness{
		Capacity: &runnerprotocol.Capacity{Instances: 32},
	}); got != 8 {
		t.Fatalf("limit = %d, want configured 8", got)
	}
	if got := concurrentAssignmentLimit(32, BackendReadiness{
		Capacity: &runnerprotocol.Capacity{Instances: 8},
	}); got != 8 {
		t.Fatalf("limit = %d, want resident capacity 8", got)
	}
	// A runner that advertises no instance capacity must not assume one.
	if got := concurrentAssignmentLimit(8, BackendReadiness{
		Capacity: &runnerprotocol.Capacity{},
	}); got != 1 {
		t.Fatalf("limit without advertised capacity = %d, want 1", got)
	}
	if got := concurrentAssignmentLimit(8, BackendReadiness{}); got != 1 {
		t.Fatalf("limit without capacity evidence = %d, want 1", got)
	}
}

func TestWorkspaceCreatesUseBoundedConcurrencyAndPreserveMutationBarriers(t *testing.T) {
	const maximumConcurrentCreates = 2
	backend := &concurrentWorkspaceCreateBackend{
		recordingAssignmentBackend: recordingAssignmentBackend{
			readiness: BackendReadiness{
				Capacity:     &runnerprotocol.Capacity{Instances: 4},
				Reserved:     &runnerprotocol.Capacity{},
				Capabilities: &runnerprotocol.RunnerCapabilities{},
			},
		},
		entered:   make(chan struct{}, maximumConcurrentCreates),
		release:   make(chan struct{}),
		inspected: make(chan struct{}, 1),
	}
	protocolConfig := testRunnerConfig()
	protocolConfig.MaximumConcurrentWorkspaceCreates = maximumConcurrentCreates
	protocolConfig.MandatoryFeatures = append(
		protocolConfig.MandatoryFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	)
	service, err := NewRunnerProtocolService(
		protocolConfig,
		backend,
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	stream := &blockingProtocolStream{
		ctx: runContext,
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			workspaceFrameAt(
				1,
				runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
			),
			workspaceFrameAt(
				2,
				runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
			),
			workspaceFrameAt(
				3,
				runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_INSPECT,
			),
		},
		heartbeats: make(chan *runnerprotocol.RunnerHeartbeat, 8),
	}
	welcome := localWorkspaceWelcomeFrame("connection-workspace-concurrent")
	var consumed sync.WaitGroup
	consumed.Add(1)
	go func() {
		defer consumed.Done()
		_ = service.consumeCommands(
			runContext,
			stream,
			welcome.GetWelcome(),
			backend.readiness,
		)
	}()

	for admitted := 0; admitted < maximumConcurrentCreates; admitted++ {
		select {
		case <-backend.entered:
		case <-time.After(3 * time.Second):
			t.Fatalf(
				"only %d of %d Workspace creates ran concurrently",
				admitted,
				maximumConcurrentCreates,
			)
		}
	}
	select {
	case <-backend.inspected:
		t.Fatal("non-create Workspace mutation crossed the create barrier")
	case <-time.After(100 * time.Millisecond):
	}
	if got := backend.maxTogether.Load(); got != maximumConcurrentCreates {
		t.Fatalf(
			"peak concurrent Workspace creates = %d, want %d",
			got,
			maximumConcurrentCreates,
		)
	}
	close(backend.release)
	select {
	case <-backend.inspected:
	case <-time.After(3 * time.Second):
		t.Fatal("Workspace mutation barrier did not release after creates completed")
	}
	cancelRun()
	consumed.Wait()
}
