package runnercontrol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestRunnerProtocolServiceReconnectsAndReadvertisesActiveAssignments(t *testing.T) {
	assignment := resolvedAssignmentCommand()
	assignment.Requirements.RequiresTenantEgressContext = true
	assignment.EgressContext = "tenant-blue"
	firstStream := &recordingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			runnerWelcomeFrame("connection-1"),
			{
				Message: &runnerprotocol.ControlPlaneToRunner_Assignment{
					Assignment: assignment,
				},
			},
		},
	}
	secondStream := &blockingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			runnerWelcomeFrame("connection-2"),
		},
		heartbeats: make(chan *runnerprotocol.RunnerHeartbeat, 1),
	}
	connector := &sequenceProtocolConnector{
		streams: []RunnerProtocolStream{firstStream, secondStream},
	}
	backend := &recordingAssignmentBackend{
		readiness: BackendReadiness{
			Capacity:     &runnerprotocol.Capacity{},
			Reserved:     &runnerprotocol.Capacity{},
			Capabilities: &runnerprotocol.RunnerCapabilities{},
		},
		instance: BackendInstance{
			BackendKind:      "firecracker",
			BackendReference: "fc-instance-1",
		},
		startupCount: 4,
		startupP95:   75 * time.Millisecond,
	}
	config := testRunnerConfig()
	config.SupportedEgressContexts = []string{assignment.EgressContext}
	service, err := NewRunnerProtocolService(config, backend, connector)
	if err != nil {
		t.Fatal(err)
	}

	runContext, cancelRun := context.WithCancel(t.Context())
	runResult := make(chan error, 1)
	go func() {
		runResult <- service.Run(runContext)
	}()

	select {
	case heartbeat := <-secondStream.heartbeats:
		if heartbeat.ConnectionId != "connection-2" {
			t.Fatalf("reconnect heartbeat connection = %q, want connection-2", heartbeat.ConnectionId)
		}
		if len(heartbeat.ActiveAssignments) != 1 ||
			!proto.Equal(
				heartbeat.ActiveAssignments[0],
				&runnerprotocol.ActiveAssignmentSummary{
					AssignmentId:      assignment.Fence.AssignmentId,
					SandboxId:         assignment.Fence.SandboxId,
					InstanceId:        assignment.Fence.InstanceId,
					SandboxGeneration: assignment.Fence.SandboxGeneration,
					FencingToken:      assignment.Fence.FencingToken,
					EgressContext:     assignment.EgressContext,
				},
			) {
			t.Fatalf(
				"reconnect active assignments = %#v, want retained assignment %q",
				heartbeat.ActiveAssignments,
				assignment.Fence.AssignmentId,
			)
		}
		if heartbeat.StartupTiming == nil ||
			heartbeat.StartupTiming.SampleCount != 4 ||
			heartbeat.StartupTiming.P95Milliseconds != 75 {
			t.Fatalf("reconnect startup timing = %#v", heartbeat.StartupTiming)
		}
	case runErr := <-runResult:
		t.Fatalf("Run() stopped after transient stream loss: %v", runErr)
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect did not re-advertise the active assignment")
	}

	if got := backend.startCalls.Load(); got != 1 {
		t.Fatalf("backend assignment starts = %d, want exactly one", got)
	}
	if got := backend.fenceCalls.Load(); got != 0 {
		t.Fatalf("backend assignment fences = %d, want none during reconnect", got)
	}
	if got := connector.connectCalls.Load(); got != 2 {
		t.Fatalf("control-plane connections = %d, want two", got)
	}
	if got := connector.closeCalls.Load(); got != 1 {
		t.Fatalf("closed control-plane sessions before cancellation = %d, want one", got)
	}

	cancelRun()
	if runErr := <-runResult; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run() cancellation error = %v, want context canceled", runErr)
	}
	if got := connector.closeCalls.Load(); got != 2 {
		t.Fatalf("closed control-plane sessions after cancellation = %d, want two", got)
	}
}

func TestRunnerProtocolServiceStartupRecoveryRequiresConfiguredContext(t *testing.T) {
	recovered := &runnerprotocol.ActiveAssignmentSummary{
		AssignmentId: "assignment-recovered", SandboxId: "sandbox-recovered",
		InstanceId: "instance-recovered", SandboxGeneration: 7,
		FencingToken: []byte("recovered-assignment-fence"), EgressContext: "tenant-blue",
	}
	backend := &recoveredRecordingAssignmentBackend{
		recordingAssignmentBackend: recordingAssignmentBackend{},
		recovered:                  []*runnerprotocol.ActiveAssignmentSummary{recovered},
	}
	config := testRunnerConfig()
	if _, err := NewRunnerProtocolService(config, backend, staticProtocolConnector{}); err == nil ||
		!strings.Contains(err.Error(), "startup recovery refuses") ||
		!strings.Contains(err.Error(), "never substitute") {
		t.Fatalf("missing recovered-context error = %v", err)
	}

	config.SupportedEgressContexts = []string{"tenant-blue"}
	service, err := NewRunnerProtocolService(config, backend, staticProtocolConnector{})
	if err != nil {
		t.Fatal(err)
	}
	if active := service.activeAssignments(); len(active) != 1 || !proto.Equal(active[0], recovered) {
		t.Fatalf("recovered active assignments = %#v", active)
	}
}

func TestRunnerProtocolServiceHeartbeatsWhileAssignmentStartIsBlocked(t *testing.T) {
	backend := &blockingAssignmentBackend{
		recordingAssignmentBackend: recordingAssignmentBackend{
			readiness: BackendReadiness{
				Capacity:     &runnerprotocol.Capacity{},
				Reserved:     &runnerprotocol.Capacity{},
				Capabilities: &runnerprotocol.RunnerCapabilities{},
			},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		backend,
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancelRun := context.WithCancel(t.Context())
	stream := &blockingProtocolStream{
		ctx: runContext,
		inbound: []*runnerprotocol.ControlPlaneToRunner{{
			Message: &runnerprotocol.ControlPlaneToRunner_Assignment{
				Assignment: resolvedAssignmentCommand(),
			},
		}},
		heartbeats: make(chan *runnerprotocol.RunnerHeartbeat, 1),
	}
	welcome := runnerWelcomeFrame("connection-heartbeat")
	welcome.GetWelcome().HeartbeatIntervalMs = 10
	runResult := make(chan error, 1)
	go func() {
		runResult <- service.consumeCommands(
			runContext,
			stream,
			welcome.GetWelcome(),
			backend.readiness,
		)
	}()

	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("assignment start did not block")
	}
	select {
	case heartbeat := <-stream.heartbeats:
		if heartbeat.ConnectionId != "connection-heartbeat" {
			t.Fatalf("heartbeat connection = %q", heartbeat.ConnectionId)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runner heartbeat stopped while assignment start was blocked")
	}

	close(backend.release)
	cancelRun()
	if runErr := <-runResult; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("consumeCommands cancellation error = %v", runErr)
	}
}

func TestRunnerRegistrationAdvertisesStaticSupportedEgressContexts(t *testing.T) {
	config := testRunnerConfig()
	config.SupportedEgressContexts = []string{"tenant-z", "tenant-a"}
	service, err := NewRunnerProtocolService(
		config,
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := &recordingProtocolStream{}
	if err := service.sendRegistration(
		stream,
		"connection-1",
		runnerprotocol.SupportedProtocolMaximum,
		BackendReadiness{
			Capacity: &runnerprotocol.Capacity{}, Reserved: &runnerprotocol.Capacity{},
			Capabilities: &runnerprotocol.RunnerCapabilities{},
		},
	); err != nil {
		t.Fatal(err)
	}
	registration := stream.outbound[0].GetRegistration()
	if !slices.Equal(registration.SupportedEgressContexts, []string{"tenant-a", "tenant-z"}) {
		t.Fatalf("registration egress contexts = %v", registration.SupportedEgressContexts)
	}
}

func TestSequencedRunnerFramesAllocateSequenceInsideSendOrder(t *testing.T) {
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := &recordingProtocolStream{}
	firstBuilderStarted := make(chan struct{})
	releaseFirstBuilder := make(chan struct{})
	secondBuilderStarted := make(chan struct{})
	results := make(chan error, 2)

	go func() {
		results <- service.sendSequencedRunnerFrame(
			stream,
			func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
				close(firstBuilderStarted)
				<-releaseFirstBuilder
				return sequencedHeartbeat(service, sequence)
			},
		)
	}()
	<-firstBuilderStarted
	go func() {
		results <- service.sendSequencedRunnerFrame(
			stream,
			func(sequence uint64) *runnerprotocol.RunnerToControlPlane {
				close(secondBuilderStarted)
				return sequencedHeartbeat(service, sequence)
			},
		)
	}()

	select {
	case <-secondBuilderStarted:
		t.Fatal("second sequence was allocated before the first frame was sent")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirstBuilder)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(stream.outbound) != 2 {
		t.Fatalf("sent frames = %d, want 2", len(stream.outbound))
	}
	for index, message := range stream.outbound {
		heartbeat := message.GetHeartbeat()
		wantSequence := uint64(index + 1)
		if heartbeat == nil || heartbeat.Sequence != wantSequence {
			t.Fatalf("sent frame %d = %#v, want heartbeat sequence %d", index, message, wantSequence)
		}
		if heartbeat.MessageId != service.messageID(wantSequence) {
			t.Fatalf(
				"heartbeat message ID = %q, want %q",
				heartbeat.MessageId,
				service.messageID(wantSequence),
			)
		}
	}
}

func sequencedHeartbeat(
	service *RunnerProtocolService,
	sequence uint64,
) *runnerprotocol.RunnerToControlPlane {
	return &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Heartbeat{
			Heartbeat: &runnerprotocol.RunnerHeartbeat{
				MessageId: service.messageID(sequence),
				Sequence:  sequence,
			},
		},
	}
}

func TestRunnerProtocolServiceSeparatesAdmissionFromArtifactProgress(t *testing.T) {
	backend := &recordingAssignmentBackend{
		instance: BackendInstance{
			BackendKind:      "firecracker",
			BackendReference: "fc-instance-1",
		},
	}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		backend,
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := &recordingProtocolStream{}
	if err := service.handleAssignment(t.Context(), stream, resolvedAssignmentCommand()); err != nil {
		t.Fatalf("handle assignment: %v", err)
	}

	var progress []*runnerprotocol.AssignmentProgress
	for _, message := range stream.outbound {
		if observed := message.GetAssignmentProgress(); observed != nil {
			progress = append(progress, observed)
		}
	}
	want := []runnerprotocol.AssignmentProgressStage{
		runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_RUNNER_ADMISSION,
		runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY,
		runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY,
	}
	if len(progress) != len(want) {
		t.Fatalf("assignment progress count = %d, want %d", len(progress), len(want))
	}
	for index, stage := range want {
		if progress[index].Stage != stage {
			t.Fatalf("assignment progress[%d] = %s, want %s", index, progress[index].Stage, stage)
		}
	}
	if progress[0].ObservedAtUnixNs > progress[1].ObservedAtUnixNs {
		t.Fatalf(
			"runner admission boundary %d followed artifact boundary %d",
			progress[0].ObservedAtUnixNs,
			progress[1].ObservedAtUnixNs,
		)
	}
}

func TestRunnerProtocolServiceReturnsLogicalLocalWorkspaceReceipt(t *testing.T) {
	backend := &recordingLocalWorkspaceBackend{
		recordingAssignmentBackend: &recordingAssignmentBackend{},
		evidence: LocalWorkspaceEvidence{
			PreviousGeneration: 4,
			Generation:         5,
			LogicalCapacity:    16 << 20,
			ReceiptRecordedAt:  time.Unix(100, 0).UTC(),
		},
	}
	config := testRunnerConfig()
	config.MandatoryFeatures = append(
		config.MandatoryFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	)
	service, err := NewRunnerProtocolService(
		config,
		backend,
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := &recordingProtocolStream{}
	command := &runnerprotocol.LocalWorkspaceCommand{
		MessageId:          "message-local-workspace",
		Sequence:           1,
		CommandVersion:     1,
		Kind:               runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
		OperationId:        "operation-stop",
		EffectId:           "effect-stop",
		SandboxId:          "sandbox-one",
		WorkspaceId:        "workspace-one",
		ExpectedGeneration: 4,
		NextGeneration:     5,
		FencingToken:       []byte("fencing-token"),
		Correlation: &runnerprotocol.Correlation{
			RequestId:   "request-stop",
			OperationId: "operation-stop",
			SandboxId:   "sandbox-one",
			RunnerId:    "runner-1",
		},
	}
	if err := service.handleLocalWorkspace(t.Context(), stream, command); err != nil {
		t.Fatalf("handle local Workspace: %v", err)
	}
	if backend.command != command {
		t.Fatal("local Workspace command did not reach backend")
	}
	if len(stream.outbound) != 1 {
		t.Fatalf("local Workspace result frames = %d", len(stream.outbound))
	}
	result := stream.outbound[0].GetLocalWorkspaceResult()
	if result == nil ||
		result.Terminal != runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED ||
		result.EffectId != command.EffectId ||
		result.WorkspaceId != command.WorkspaceId ||
		result.PreviousGeneration != 4 ||
		result.Generation != 5 ||
		result.LogicalCapacityBytes != 16<<20 ||
		result.ReceiptRecordedAtUnixMs != uint64(time.Unix(100, 0).UnixMilli()) {
		t.Fatalf("local Workspace result = %#v", result)
	}
	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte("/var/lib"),
		[]byte("workspace.ext4"),
		[]byte("storage_object"),
	} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("local Workspace result leaked %q", forbidden)
		}
	}
}

func TestRunnerProtocolServiceReturnsReconciliationInventoryAndReceipts(t *testing.T) {
	backend := &recordingLocalWorkspaceBackend{
		recordingAssignmentBackend: &recordingAssignmentBackend{},
		evidence: LocalWorkspaceEvidence{
			Inventory: []*runnerprotocol.LocalWorkspaceInventoryItem{{
				WorkspaceId:          "workspace-one",
				Generation:           5,
				LogicalCapacityBytes: 16 << 20,
				Formatted:            true,
				ActiveWriter:         true,
			}},
			Receipts: []*runnerprotocol.LocalWorkspaceReceiptItem{{
				Kind:                 runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
				OperationId:          "operation-snapshot",
				WorkspaceId:          "workspace-one",
				SnapshotId:           "snapshot-one",
				Generation:           5,
				LogicalCapacityBytes: 16 << 20,
				ReceiptRecordedAtUnixMs: uint64(
					time.Unix(100, 0).UnixMilli(),
				),
			}},
		},
	}
	config := testRunnerConfig()
	config.MandatoryFeatures = append(
		config.MandatoryFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	)
	service, err := NewRunnerProtocolService(
		config,
		backend,
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := &recordingProtocolStream{}
	command := &runnerprotocol.LocalWorkspaceCommand{
		MessageId:      "workspace-reconcile-connection",
		Sequence:       1,
		CommandVersion: 1,
		Kind:           runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE,
		OperationId:    "workspace-reconcile-connection",
		EffectId:       "workspace-reconcile-connection",
		Correlation: &runnerprotocol.Correlation{
			RequestId:   "workspace-reconcile-connection",
			OperationId: "workspace-reconcile-connection",
			RunnerId:    "runner-1",
		},
	}
	if err := service.handleLocalWorkspace(t.Context(), stream, command); err != nil {
		t.Fatal(err)
	}
	result := findLocalWorkspaceResult(stream.outbound)
	if result == nil ||
		len(result.Inventory) != 1 ||
		!result.Inventory[0].ActiveWriter ||
		len(result.Receipts) != 1 ||
		result.Receipts[0].SnapshotId != "snapshot-one" ||
		result.Receipts[0].ReceiptRecordedAtUnixMs == 0 {
		t.Fatalf("Workspace reconciliation result = %#v", result)
	}
}

func TestRunnerProtocolReconnectReplaysDurableLocalWorkspaceResult(t *testing.T) {
	for _, kind := range allLocalWorkspaceCommandKinds() {
		t.Run(kind.String(), func(t *testing.T) {
			receiptTime := time.Date(2026, 7, 29, 23, 0, 0, 0, time.UTC)
			backend := &recordingLocalWorkspaceBackend{
				recordingAssignmentBackend: &recordingAssignmentBackend{
					readiness: BackendReadiness{
						Capacity:     &runnerprotocol.Capacity{},
						Reserved:     &runnerprotocol.Capacity{},
						Capabilities: &runnerprotocol.RunnerCapabilities{},
					},
				},
				evidence: LocalWorkspaceEvidence{
					PreviousGeneration: 4,
					Generation:         5,
					LogicalCapacity:    16 << 20,
					ReceiptRecordedAt:  receiptTime,
				},
			}
			config := testRunnerConfig()
			config.MandatoryFeatures = append(
				config.MandatoryFeatures,
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
			)
			command := localWorkspaceReconnectCommand(kind)
			first := &recordingProtocolStream{
				inbound: []*runnerprotocol.ControlPlaneToRunner{
					localWorkspaceWelcomeFrame("connection-1"),
					{Message: &runnerprotocol.ControlPlaneToRunner_LocalWorkspace{
						LocalWorkspace: proto.Clone(command).(*runnerprotocol.LocalWorkspaceCommand),
					}},
				},
				sendError: func(message *runnerprotocol.RunnerToControlPlane) error {
					if message.GetLocalWorkspaceResult() != nil {
						return io.ErrUnexpectedEOF
					}
					return nil
				},
			}
			secondCommand := proto.Clone(command).(*runnerprotocol.LocalWorkspaceCommand)
			secondCommand.Sequence = 1
			second := &recordingProtocolStream{
				inbound: []*runnerprotocol.ControlPlaneToRunner{
					localWorkspaceWelcomeFrame("connection-2"),
					{Message: &runnerprotocol.ControlPlaneToRunner_LocalWorkspace{
						LocalWorkspace: secondCommand,
					}},
				},
			}
			connector := &sequenceProtocolConnector{streams: []RunnerProtocolStream{first, second}}
			service, err := NewRunnerProtocolService(config, backend, connector)
			if err != nil {
				t.Fatal(err)
			}
			if established, err := service.runProtocolSession(t.Context()); !established ||
				!errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("first session = established %t, error %v", established, err)
			}
			if established, err := service.runProtocolSession(t.Context()); !established ||
				!errors.Is(err, io.EOF) {
				t.Fatalf("second session = established %t, error %v", established, err)
			}
			if backend.localCalls.Load() != 2 {
				t.Fatalf("local Workspace executions = %d, want receipt replay", backend.localCalls.Load())
			}
			firstResult := findLocalWorkspaceResult(first.outbound)
			secondResult := findLocalWorkspaceResult(second.outbound)
			if firstResult == nil || secondResult == nil {
				t.Fatalf("replayed results = first %#v, second %#v", firstResult, secondResult)
			}
			firstResult.MessageId, secondResult.MessageId = "", ""
			firstResult.Sequence, secondResult.Sequence = 0, 0
			if !proto.Equal(firstResult, secondResult) {
				t.Fatalf("replayed local Workspace result changed:\nfirst  %#v\nsecond %#v", firstResult, secondResult)
			}
		})
	}
}

func TestRunnerProtocolReconnectRetriesLocalWorkspaceWithoutReceipt(t *testing.T) {
	for _, kind := range allLocalWorkspaceCommandKinds() {
		t.Run(kind.String(), func(t *testing.T) {
			backend := &recordingLocalWorkspaceBackend{
				recordingAssignmentBackend: &recordingAssignmentBackend{
					readiness: BackendReadiness{
						Capacity:     &runnerprotocol.Capacity{},
						Reserved:     &runnerprotocol.Capacity{},
						Capabilities: &runnerprotocol.RunnerCapabilities{},
					},
				},
				err: errors.New("interrupted before durable receipt"),
			}
			config := testRunnerConfig()
			config.MandatoryFeatures = append(
				config.MandatoryFeatures,
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
			)
			command := localWorkspaceReconnectCommand(kind)
			first := &recordingProtocolStream{
				inbound: []*runnerprotocol.ControlPlaneToRunner{
					localWorkspaceWelcomeFrame("connection-1"),
					{Message: &runnerprotocol.ControlPlaneToRunner_LocalWorkspace{
						LocalWorkspace: proto.Clone(command).(*runnerprotocol.LocalWorkspaceCommand),
					}},
				},
				sendError: func(message *runnerprotocol.RunnerToControlPlane) error {
					if message.GetLocalWorkspaceResult() != nil {
						return io.ErrUnexpectedEOF
					}
					return nil
				},
			}
			second := &recordingProtocolStream{
				inbound: []*runnerprotocol.ControlPlaneToRunner{
					localWorkspaceWelcomeFrame("connection-2"),
					{Message: &runnerprotocol.ControlPlaneToRunner_LocalWorkspace{
						LocalWorkspace: proto.Clone(command).(*runnerprotocol.LocalWorkspaceCommand),
					}},
				},
			}
			connector := &sequenceProtocolConnector{streams: []RunnerProtocolStream{first, second}}
			service, err := NewRunnerProtocolService(config, backend, connector)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.runProtocolSession(t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("pre-receipt session error = %v", err)
			}
			firstResult := findLocalWorkspaceResult(first.outbound)
			if firstResult == nil ||
				firstResult.Terminal !=
					runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED ||
				firstResult.ReceiptRecordedAtUnixMs != 0 {
				t.Fatalf("pre-receipt result = %#v", firstResult)
			}
			backend.err = nil
			backend.evidence = LocalWorkspaceEvidence{
				PreviousGeneration: 4, Generation: 5, LogicalCapacity: 16 << 20,
				ReceiptRecordedAt: time.Date(2026, 7, 29, 23, 30, 0, 0, time.UTC),
			}
			if _, err := service.runProtocolSession(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("retried session error = %v", err)
			}
			secondResult := findLocalWorkspaceResult(second.outbound)
			if secondResult == nil ||
				secondResult.Terminal !=
					runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED ||
				secondResult.ReceiptRecordedAtUnixMs == 0 {
				t.Fatalf("retried result = %#v", secondResult)
			}
		})
	}
}

func allLocalWorkspaceCommandKinds() []runnerprotocol.LocalWorkspaceCommandKind {
	return []runnerprotocol.LocalWorkspaceCommandKind{
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_INSPECT,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_DELETE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_CREATE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_SWAP,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_ABORT,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE,
		runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CLONE_FROM_SNAPSHOT,
	}
}

func localWorkspaceReconnectCommand(
	kind runnerprotocol.LocalWorkspaceCommandKind,
) *runnerprotocol.LocalWorkspaceCommand {
	return &runnerprotocol.LocalWorkspaceCommand{
		MessageId: "command-" + kind.String(), Sequence: 1, CommandVersion: 1,
		Kind:        kind,
		OperationId: "operation-local", EffectId: "effect-local",
		SandboxId: "sandbox-one", WorkspaceId: "workspace-one", SnapshotId: "snapshot-one",
		ExpectedGeneration: 4, NextGeneration: 5, LogicalCapacityBytes: 16 << 20,
		FencingToken: []byte("01234567890123456789012345678901"),
		Correlation: &runnerprotocol.Correlation{
			OperationId: "operation-local", SandboxId: "sandbox-one", RunnerId: "runner-1",
		},
	}
}

func TestRunnerProtocolServiceRedactsLocalWorkspaceFailureDetails(t *testing.T) {
	backend := &recordingLocalWorkspaceBackend{
		recordingAssignmentBackend: &recordingAssignmentBackend{},
		err: typedLocalWorkspaceTestError{
			terminal: runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_STORAGE_INCOMPATIBLE,
			detail:   "reflink failed for /var/lib/secondbox/private/workspace.ext4",
		},
	}
	config := testRunnerConfig()
	config.MandatoryFeatures = append(
		config.MandatoryFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	)
	service, err := NewRunnerProtocolService(
		config, backend, staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := &recordingProtocolStream{}
	if err := service.handleLocalWorkspace(t.Context(), stream, &runnerprotocol.LocalWorkspaceCommand{
		CommandVersion: 1,
		Kind:           runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		EffectId:       "effect-create", SandboxId: "sandbox-one", WorkspaceId: "workspace-one",
		FencingToken: []byte("fencing-token"),
		Correlation:  &runnerprotocol.Correlation{RunnerId: "runner-1"},
	}); err != nil {
		t.Fatal(err)
	}
	result := stream.outbound[0].GetLocalWorkspaceResult()
	if result.SafeDetail != "local workspace storage is incompatible" ||
		strings.Contains(result.SafeDetail, "/var/lib") ||
		strings.Contains(result.SafeDetail, "ext4") {
		t.Fatalf("local Workspace safe detail = %q", result.SafeDetail)
	}
}

func TestRunnerProtocolServiceRejectsLocalWorkspaceForAnotherHomeRunner(t *testing.T) {
	backend := &recordingLocalWorkspaceBackend{
		recordingAssignmentBackend: &recordingAssignmentBackend{},
	}
	config := testRunnerConfig()
	config.MandatoryFeatures = append(
		config.MandatoryFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	)
	service, err := NewRunnerProtocolService(
		config, backend, staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := &recordingProtocolStream{}
	if err := service.handleLocalWorkspace(t.Context(), stream, &runnerprotocol.LocalWorkspaceCommand{
		CommandVersion: 1,
		Kind:           runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		EffectId:       "effect-create", SandboxId: "sandbox-one", WorkspaceId: "workspace-one",
		FencingToken: []byte("fencing-token"),
		Correlation:  &runnerprotocol.Correlation{RunnerId: "runner-other"},
	}); err != nil {
		t.Fatal(err)
	}
	if backend.command != nil {
		t.Fatal("wrong-home local Workspace command reached the backend")
	}
	result := stream.outbound[0].GetLocalWorkspaceResult()
	if result.Terminal != runnerprotocol.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_WRONG_HOME_RUNNER ||
		result.SafeDetail != "workspace is not owned by this runner" {
		t.Fatalf("wrong-home local Workspace result = %#v", result)
	}
}

type typedLocalWorkspaceTestError struct {
	terminal runnerprotocol.LocalWorkspaceTerminalKind
	detail   string
}

func (failure typedLocalWorkspaceTestError) Error() string {
	return failure.detail
}

func (failure typedLocalWorkspaceTestError) LocalWorkspaceTerminal() runnerprotocol.LocalWorkspaceTerminalKind {
	return failure.terminal
}

func TestRunnerProtocolServiceNegotiatesBeforeProfileResolvedAssignment(t *testing.T) {
	assignment := resolvedAssignmentCommand()
	stream := &recordingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			{
				Message: &runnerprotocol.ControlPlaneToRunner_Welcome{
					Welcome: &runnerprotocol.RunnerWelcome{
						ConnectionId:    "connection-1",
						SelectedVersion: 1,
						EnabledFeatures: []runnerprotocol.RunnerFeature{
							runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
						},
						HeartbeatIntervalMs: 60_000,
					},
				},
			},
			{
				Message: &runnerprotocol.ControlPlaneToRunner_Assignment{
					Assignment: assignment,
				},
			},
		},
	}
	backend := &recordingAssignmentBackend{
		readiness: BackendReadiness{
			Architecture: "amd64",
			Capacity:     &runnerprotocol.Capacity{VcpuCount: 4, MemoryBytes: 8 << 30, DiskBytes: 64 << 30, Instances: 2, Operations: 8},
			Capabilities: &runnerprotocol.RunnerCapabilities{
				Architecture:             "amd64",
				ComputeBackendVersion:    "1.16.1",
				HypervisorReady:          true,
				IsolationReady:           true,
				ResourceLimitsReady:      true,
				NetworkPolicyReady:       true,
				StorageReady:             true,
				CleanupReady:             true,
				GuestProtocolGenerations: &runnerprotocol.ProtocolVersionRange{Minimum: 1, Maximum: 1},
			},
		},
		instance:     BackendInstance{BackendKind: "firecracker", BackendReference: "fc-instance-1"},
		startupCount: 3,
		startupP95:   40 * time.Millisecond,
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	evidenceSink := &recordingEvidenceSink{}
	service.SetEvidenceSink(evidenceSink)

	_, err = service.runProtocolSession(t.Context())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run() error = %v, want stream EOF", err)
	}
	if backend.started != assignment {
		t.Fatal("profile-resolved assignment was not passed to the compute backend")
	}
	if len(stream.outbound) < 6 {
		t.Fatalf("outbound messages = %d, want hello, registration, ack, progress, and result evidence", len(stream.outbound))
	}
	if stream.outbound[0].GetHello() == nil {
		t.Fatal("first outbound runner message was not Hello")
	}
	registration := stream.outbound[1].GetRegistration()
	if registration == nil {
		t.Fatal("registration was sent before protocol negotiation or omitted")
	}
	if registration.StartupTiming == nil ||
		registration.StartupTiming.SampleCount != 3 ||
		registration.StartupTiming.P95Milliseconds != 40 {
		t.Fatalf("registration startup timing = %#v", registration.StartupTiming)
	}
	ack := findAssignmentAck(stream.outbound)
	if ack == nil || ack.Decision != runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED {
		t.Fatalf("assignment ack = %#v, want accepted", ack)
	}
	result := findAssignmentResult(stream.outbound)
	if result == nil || result.Terminal != runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY {
		t.Fatalf("assignment result = %#v, want ready", result)
	}
	if result.Correlation == nil ||
		result.Correlation.AssignmentId != assignment.Fence.AssignmentId ||
		result.Correlation.SandboxGeneration != assignment.Fence.SandboxGeneration ||
		result.Correlation.RunnerId != "runner-1" {
		t.Fatalf("assignment result lacks structured correlation: %#v", result.Correlation)
	}
	records := evidenceSink.snapshot()
	if len(records) != 1 ||
		records[0].Event != runnerevidence.EventAssignmentTerminal ||
		records[0].RequestID != "request-1" ||
		records[0].OperationID != "operation-1" ||
		records[0].LeaseID != "lease-1" ||
		records[0].RunnerID != "runner-1" {
		t.Fatalf("assignment evidence = %+v", records)
	}
}

func TestRunnerProtocolServiceRejectsUnresolvedAssignmentBeforeBackend(t *testing.T) {
	assignment := resolvedAssignmentCommand()
	assignment.ProfileRevisionId = ""
	stream := &recordingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			{
				Message: &runnerprotocol.ControlPlaneToRunner_Welcome{
					Welcome: &runnerprotocol.RunnerWelcome{
						ConnectionId:    "connection-1",
						SelectedVersion: 1,
						EnabledFeatures: []runnerprotocol.RunnerFeature{
							runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
						},
						HeartbeatIntervalMs: 60_000,
					},
				},
			},
			{
				Message: &runnerprotocol.ControlPlaneToRunner_Assignment{
					Assignment: assignment,
				},
			},
		},
	}
	backend := &recordingAssignmentBackend{
		readiness: BackendReadiness{
			Capacity:     &runnerprotocol.Capacity{},
			Capabilities: &runnerprotocol.RunnerCapabilities{},
		},
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: stream})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.runProtocolSession(t.Context())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run() error = %v, want stream EOF", err)
	}
	if backend.started != nil {
		t.Fatal("unresolved assignment reached the compute backend")
	}
	ack := findAssignmentAck(stream.outbound)
	if ack == nil || ack.Decision != runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_INCOMPATIBLE_PROFILE {
		t.Fatalf("assignment ack = %#v, want incompatible-profile rejection", ack)
	}
}

func TestRunnerProtocolServicePreservesBackendAssignmentClassifications(t *testing.T) {
	tests := []struct {
		name         string
		validateErr  error
		startErr     error
		wantDecision runnerprotocol.AssignmentDecision
		wantTerminal runnerprotocol.AssignmentTerminalKind
	}{
		{
			name: "validation decision",
			validateErr: classifiedAssignmentError{
				decision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_CAPACITY,
				terminal: runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_ADMISSION_FAILED,
			},
			wantDecision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_CAPACITY,
		},
		{
			name: "startup terminal",
			startErr: classifiedAssignmentError{
				decision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_PREREQUISITE,
				terminal: runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_INFRASTRUCTURE_FAILED,
			},
			wantDecision: runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_ACCEPTED,
			wantTerminal: runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_INFRASTRUCTURE_FAILED,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assignment := resolvedAssignmentCommand()
			stream := &recordingProtocolStream{inbound: []*runnerprotocol.ControlPlaneToRunner{
				runnerWelcomeFrame("connection-1"),
				{Message: &runnerprotocol.ControlPlaneToRunner_Assignment{Assignment: assignment}},
			}}
			backend := &recordingAssignmentBackend{
				readiness:   BackendReadiness{Capacity: &runnerprotocol.Capacity{}, Capabilities: &runnerprotocol.RunnerCapabilities{}},
				validateErr: test.validateErr,
				startErr:    test.startErr,
			}
			service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: stream})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.runProtocolSession(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("Run() error = %v, want stream EOF", err)
			}
			ack := findAssignmentAck(stream.outbound)
			if ack == nil || ack.Decision != test.wantDecision {
				t.Fatalf("assignment ack = %#v, want %s", ack, test.wantDecision)
			}
			result := findAssignmentResult(stream.outbound)
			if test.wantTerminal == runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_UNSPECIFIED {
				if result != nil {
					t.Fatalf("unexpected assignment result = %#v", result)
				}
			} else if result == nil || result.Terminal != test.wantTerminal {
				t.Fatalf("assignment result = %#v, want %s", result, test.wantTerminal)
			}
		})
	}
}

func TestRunnerProtocolServiceRejectsMutationBeforeWelcome(t *testing.T) {
	stream := &recordingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			{
				Message: &runnerprotocol.ControlPlaneToRunner_Assignment{
					Assignment: resolvedAssignmentCommand(),
				},
			},
		},
	}
	backend := &recordingAssignmentBackend{}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: stream})
	if err != nil {
		t.Fatal(err)
	}

	err = service.Run(t.Context())
	if err == nil || !errors.Is(err, ErrRunnerProtocolNegotiation) {
		t.Fatalf("Run() error = %v, want negotiation failure", err)
	}
	if backend.started != nil {
		t.Fatal("pre-negotiation mutation reached the compute backend")
	}
}

func TestRunnerProtocolServiceStopsOnTerminalAuthenticationFailure(t *testing.T) {
	connector := &terminalErrorProtocolConnector{
		err: status.Error(codes.Unauthenticated, "runner credential revoked"),
	}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		&recordingAssignmentBackend{},
		connector,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = service.Run(t.Context())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Run() error = %v, want unauthenticated", err)
	}
	if got := connector.connectCalls.Load(); got != 1 {
		t.Fatalf("terminal authentication connection attempts = %d, want one", got)
	}
}

func TestRunnerProtocolReconnectBackoffIsBounded(t *testing.T) {
	delay := runnerReconnectInitialDelay
	for delay < runnerReconnectMaximumDelay {
		next := nextRunnerProtocolReconnectDelay(delay)
		if next <= delay || next > runnerReconnectMaximumDelay {
			t.Fatalf(
				"next reconnect delay after %s = %s, want increasing delay bounded by %s",
				delay,
				next,
				runnerReconnectMaximumDelay,
			)
		}
		delay = next
	}
	if next := nextRunnerProtocolReconnectDelay(delay); next != runnerReconnectMaximumDelay {
		t.Fatalf(
			"reconnect delay after maximum = %s, want %s",
			next,
			runnerReconnectMaximumDelay,
		)
	}
}

func TestRunnerProtocolServiceRequiresBackendForAdvertisedDataPlane(t *testing.T) {
	config := testRunnerConfig()
	config.MandatoryFeatures = append(
		config.MandatoryFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
	)
	if _, err := NewRunnerProtocolService(
		config,
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	); err == nil {
		t.Fatal("runner service advertised Exec without a data-plane backend")
	}
}

func TestRunnerProtocolServiceRequiresItsIdentityCertificateForDataPlaneTLS(t *testing.T) {
	for name, certificate := range map[string]tls.Certificate{
		"missing":          {},
		"foreign_identity": testRunnerCertificate("runner-foreign"),
	} {
		t.Run(name, func(t *testing.T) {
			config := testRunnerConfig()
			config.DataPlaneCertificate = certificate
			if _, err := NewRunnerProtocolService(
				config,
				&recordingAssignmentBackend{},
				staticProtocolConnector{stream: &recordingProtocolStream{}},
			); err == nil {
				t.Fatal("runner service accepted invalid data-plane TLS identity")
			}
		})
	}
}

func TestRunnerControlCommandStateDeduplicatesAndRejectsReordering(t *testing.T) {
	state := newControlCommandState()
	assignment := &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Assignment{
			Assignment: resolvedAssignmentCommand(),
		},
	}
	if duplicate, err := state.accept(assignment); err != nil || duplicate {
		t.Fatalf("first command = duplicate %t, error %v", duplicate, err)
	}
	if duplicate, err := state.accept(proto.Clone(assignment).(*runnerprotocol.ControlPlaneToRunner)); err != nil || !duplicate {
		t.Fatalf("exact duplicate command = duplicate %t, error %v", duplicate, err)
	}
	conflict := proto.Clone(assignment).(*runnerprotocol.ControlPlaneToRunner)
	conflict.GetAssignment().ProfileRevisionId = "different"
	if _, err := state.accept(conflict); err == nil {
		t.Fatal("conflicting duplicate command ID was accepted")
	}
	reordered := &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Drain{Drain: &runnerprotocol.DrainCommand{
			MessageId: "drain-reordered", Sequence: 1,
			Mode: runnerprotocol.DrainMode_DRAIN_MODE_GRACEFUL,
		}},
	}
	if _, err := state.accept(reordered); err == nil {
		t.Fatal("reordered control command was accepted")
	}
}

func TestControlPlaneReceivePumpExitsWhenOwnerCancelsBlockedDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	received := make(chan receivedControlPlaneFrame, 1)
	secondReceive := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		pumpControlPlaneFrames(ctx, func() (*runnerprotocol.ControlPlaneToRunner, error) {
			if calls.Add(1) == 2 {
				close(secondReceive)
			}
			return &runnerprotocol.ControlPlaneToRunner{}, nil
		}, received)
		close(done)
	}()
	<-secondReceive
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("control-plane receive pump leaked after owner cancellation")
	}
}

func TestResolvedAssignmentRequiresStructurallyCompleteNetworkPolicy(t *testing.T) {
	tests := map[string]*runnerprotocol.NetworkPolicy{
		"missing": nil,
		"unspecified": {
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_UNSPECIFIED,
		},
		"deny_all_with_destination": {
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL,
			Destinations: []*runnerprotocol.NetworkDestination{{
				Target:   &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"},
				Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS,
				Port:     443,
			}},
		},
		"empty_allow_list": {
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		},
		"allow_list_missing_target": {
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
			Destinations: []*runnerprotocol.NetworkDestination{{
				Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP,
				Port:     443,
			}},
		},
		"allow_list_missing_protocol": {
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
			Destinations: []*runnerprotocol.NetworkDestination{{
				Target: &runnerprotocol.NetworkDestination_Cidr{Cidr: "192.0.2.0/24"},
				Port:   443,
			}},
		},
		"allow_list_missing_port": {
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
			Destinations: []*runnerprotocol.NetworkDestination{{
				Target:   &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"},
				Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS,
			}},
		},
	}
	for name, policy := range tests {
		t.Run(name, func(t *testing.T) {
			assignment := resolvedAssignmentCommand()
			assignment.NetworkPolicy = policy
			if err := validateResolvedAssignment(assignment); err == nil {
				t.Fatal("invalid network policy was accepted")
			}
		})
	}
	assignment := resolvedAssignmentCommand()
	assignment.NetworkPolicy = &runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{
			{
				Target:   &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"},
				Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS,
				Port:     443,
			},
			{
				Target:   &runnerprotocol.NetworkDestination_Cidr{Cidr: "192.0.2.0/24"},
				Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP,
				Port:     8443,
			},
		},
	}
	if err := validateResolvedAssignment(assignment); err != nil {
		t.Fatalf("complete allow-list network policy: %v", err)
	}
}

func TestRunnerProtocolServiceDrainReportsRemainingReadyAssignment(t *testing.T) {
	assignment := resolvedAssignmentCommand()
	assignment.Requirements.RequiresTenantEgressContext = true
	assignment.EgressContext = "tenant-blue"
	stream := &recordingProtocolStream{
		inbound: []*runnerprotocol.ControlPlaneToRunner{
			{Message: &runnerprotocol.ControlPlaneToRunner_Welcome{Welcome: &runnerprotocol.RunnerWelcome{
				ConnectionId:        "connection-1",
				SelectedVersion:     1,
				EnabledFeatures:     []runnerprotocol.RunnerFeature{runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE},
				HeartbeatIntervalMs: 60_000,
			}}},
			{Message: &runnerprotocol.ControlPlaneToRunner_Assignment{Assignment: assignment}},
			{Message: &runnerprotocol.ControlPlaneToRunner_Drain{Drain: &runnerprotocol.DrainCommand{
				MessageId:      "drain-1",
				Sequence:       2,
				Mode:           runnerprotocol.DrainMode_DRAIN_MODE_GRACEFUL,
				DeadlineUnixMs: 4_102_444_800_000,
			}}},
		},
	}
	backend := &recordingAssignmentBackend{
		readiness: BackendReadiness{
			Capacity:     &runnerprotocol.Capacity{},
			Capabilities: &runnerprotocol.RunnerCapabilities{},
		},
		instance: BackendInstance{BackendKind: "firecracker", BackendReference: "fc-instance-1"},
	}
	config := testRunnerConfig()
	config.SupportedEgressContexts = []string{assignment.EgressContext}
	service, err := NewRunnerProtocolService(config, backend, staticProtocolConnector{stream: stream})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.runProtocolSession(t.Context())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run() error = %v, want stream EOF", err)
	}
	var state *runnerprotocol.DrainState
	for _, message := range stream.outbound {
		if message.GetDrainState() != nil {
			state = message.GetDrainState()
		}
	}
	if state == nil ||
		state.Phase != runnerprotocol.DrainPhase_DRAIN_PHASE_DRAINING ||
		len(state.RemainingAssignments) != 1 ||
		state.RemainingAssignments[0].AssignmentId != assignment.Fence.AssignmentId ||
		state.RemainingAssignments[0].EgressContext != assignment.EgressContext {
		t.Fatalf("drain state = %#v, want draining with the active assignment", state)
	}
}

func TestRunnerProtocolServiceSendsOnlyCurrentCorrelatedInstanceTerminal(t *testing.T) {
	assignment := resolvedAssignmentCommand()
	service, err := NewRunnerProtocolService(
		testRunnerConfig(),
		&recordingAssignmentBackend{},
		staticProtocolConnector{stream: &recordingProtocolStream{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.recordActiveAssignment(assignment.Fence, "fc-instance-1")
	service.recordAssignmentCorrelation(assignment)
	stream := &recordingProtocolStream{}
	correlation := service.correlationForAssignment(assignment.Fence.AssignmentId)
	terminal := BackendInstanceTerminal{
		Fence:          assignment.Fence,
		Correlation:    correlation,
		Reason:         runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_GUEST_SHUTDOWN,
		ObservedAt:     time.Now().UTC(),
		EvidenceDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := service.sendInstanceTerminal(t.Context(), stream, terminal); err != nil {
		t.Fatal(err)
	}
	if len(stream.outbound) != 1 {
		t.Fatalf("outbound messages = %d, want one", len(stream.outbound))
	}
	frame := stream.outbound[0].GetInstanceTerminal()
	if frame == nil ||
		frame.Reason != terminal.Reason ||
		frame.TerminationEvidenceDigest != terminal.EvidenceDigest ||
		!proto.Equal(frame.Correlation, correlation) {
		t.Fatalf("instance terminal frame = %#v", frame)
	}

	stale := terminal
	stale.Fence = proto.Clone(terminal.Fence).(*runnerprotocol.AssignmentFence)
	stale.Fence.SandboxGeneration++
	if err := service.sendInstanceTerminal(t.Context(), stream, stale); err == nil {
		t.Fatal("stale terminal fence was accepted")
	}
	changedCorrelation := terminal
	changedCorrelation.Correlation = proto.Clone(correlation).(*runnerprotocol.Correlation)
	changedCorrelation.Correlation.OperationId = "changed"
	if err := service.sendInstanceTerminal(t.Context(), stream, changedCorrelation); err == nil {
		t.Fatal("changed terminal correlation was accepted")
	}
}

func resolvedAssignmentCommand() *runnerprotocol.AssignmentCommand {
	return &runnerprotocol.AssignmentCommand{
		MessageId: "message-1",
		Sequence:  1,
		Fence: &runnerprotocol.AssignmentFence{
			AssignmentId:      "assignment-1",
			SandboxId:         "sandbox-1",
			InstanceId:        "instance-1",
			SandboxGeneration: 3,
			FencingToken:      []byte("opaque-fence-token"),
		},
		ProfileRevisionId: "profile-revision-1",
		Requirements: &runnerprotocol.ProfileRequirements{
			VcpuCount:          2,
			MemoryBytes:        4 << 30,
			DiskBytes:          16 << 30,
			Architecture:       "amd64",
			StartupMode:        "cold_boot",
			MaximumOperationMs: 30_000,
			MaximumOutputBytes: 8 << 20,
		},
		Assets: []*runnerprotocol.AssetReference{
			{
				ArtifactId:              "secondbox-rootfs-1",
				ManifestDigest:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Architecture:            "amd64",
				GuestProtocolGeneration: 1,
			},
		},
		DeadlineUnixMs: 4_102_444_800_000,
		Correlation: &runnerprotocol.Correlation{
			RequestId:   "request-1",
			OperationId: "operation-1",
			LeaseId:     "lease-1",
		},
		NetworkPolicy: &runnerprotocol.NetworkPolicy{
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL,
		},
	}
}

func TestRunnerRejectsAssignmentEgressContextPrerequisiteFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		contexts   []string
		mutate     func(*runnerprotocol.AssignmentCommand)
		wantDetail string
	}{
		{
			name: "missing", contexts: []string{"tenant-blue"},
			mutate: func(assignment *runnerprotocol.AssignmentCommand) {
				assignment.Requirements.RequiresTenantEgressContext = true
			},
			wantDetail: "required egress context is missing",
		},
		{
			name: "unexpected", contexts: []string{"tenant-blue"},
			mutate: func(assignment *runnerprotocol.AssignmentCommand) {
				assignment.EgressContext = "tenant-blue"
			},
			wantDetail: "unexpected egress context",
		},
		{
			name: "unsupported", contexts: []string{"tenant-green"},
			mutate: func(assignment *runnerprotocol.AssignmentCommand) {
				assignment.Requirements.RequiresTenantEgressContext = true
				assignment.EgressContext = "tenant-blue"
			},
			wantDetail: "egress context is unsupported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testRunnerConfig()
			config.SupportedEgressContexts = test.contexts
			stream := &recordingProtocolStream{}
			service, err := NewRunnerProtocolService(
				config,
				&recordingAssignmentBackend{},
				staticProtocolConnector{stream: stream},
			)
			if err != nil {
				t.Fatal(err)
			}
			assignment := resolvedAssignmentCommand()
			test.mutate(assignment)
			if err := service.handleAssignment(t.Context(), stream, assignment); err != nil {
				t.Fatal(err)
			}
			ack := stream.outbound[len(stream.outbound)-1].GetAssignmentAck()
			if ack.GetDecision() != runnerprotocol.AssignmentDecision_ASSIGNMENT_DECISION_REJECTED_PREREQUISITE ||
				!strings.Contains(ack.GetSafeDetail(), test.wantDetail) {
				t.Fatalf("assignment Ack = %#v", ack)
			}
		})
	}
}

func testRunnerConfig() RunnerProtocolConfig {
	return RunnerProtocolConfig{
		RunnerID:                          "runner-1",
		RunnerPoolID:                      "pool-1",
		SoftwareVersion:                   "1.0.0",
		ProtocolMinimum:                   1,
		ProtocolMaximum:                   1,
		MaximumConcurrentStarts:           4,
		MaximumConcurrentWorkspaceCreates: 4,
		DataPlaneListenAddress:            "127.0.0.1:0",
		DataPlaneAdvertisedAddress:        "10.0.0.5:7443",
		DataPlaneCertificate:              testRunnerCertificate("runner-1"),
		MandatoryFeatures: []runnerprotocol.RunnerFeature{
			runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		},
	}
}

func testRunnerCertificate(runnerID string) tls.Certificate {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	identity, err := url.Parse("spiffe://secondbox/runner/" + runnerID)
	if err != nil {
		panic(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute),
		NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		URIs: []*url.URL{identity},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
}

type recordingProtocolStream struct {
	inbound   []*runnerprotocol.ControlPlaneToRunner
	outbound  []*runnerprotocol.RunnerToControlPlane
	sendError func(*runnerprotocol.RunnerToControlPlane) error
}

func (s *recordingProtocolStream) Send(message *runnerprotocol.RunnerToControlPlane) error {
	s.outbound = append(s.outbound, message)
	if s.sendError != nil {
		return s.sendError(message)
	}
	return nil
}

func (s *recordingProtocolStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	if len(s.inbound) == 0 {
		return nil, io.EOF
	}
	message := s.inbound[0]
	s.inbound = s.inbound[1:]
	return message, nil
}

type staticProtocolConnector struct {
	stream RunnerProtocolStream
}

func (c staticProtocolConnector) Connect(context.Context) (RunnerProtocolStream, error) {
	return c.stream, nil
}

func (staticProtocolConnector) Close() error { return nil }

type sequenceProtocolConnector struct {
	mu           sync.Mutex
	streams      []RunnerProtocolStream
	connectCalls atomic.Uint32
	closeCalls   atomic.Uint32
}

func (c *sequenceProtocolConnector) Connect(ctx context.Context) (RunnerProtocolStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connectCalls.Add(1)
	if len(c.streams) == 0 {
		return nil, errors.New("test connector has no remaining control-plane stream")
	}
	stream := c.streams[0]
	c.streams = c.streams[1:]
	if blocking, ok := stream.(*blockingProtocolStream); ok {
		blocking.ctx = ctx
	}
	return stream, nil
}

func (c *sequenceProtocolConnector) Close() error {
	c.closeCalls.Add(1)
	return nil
}

type terminalErrorProtocolConnector struct {
	err          error
	connectCalls atomic.Uint32
}

func (c *terminalErrorProtocolConnector) Connect(context.Context) (RunnerProtocolStream, error) {
	c.connectCalls.Add(1)
	return nil, c.err
}

func (*terminalErrorProtocolConnector) Close() error { return nil }

type blockingProtocolStream struct {
	ctx        context.Context
	mu         sync.Mutex
	inbound    []*runnerprotocol.ControlPlaneToRunner
	heartbeats chan *runnerprotocol.RunnerHeartbeat
}

func (s *blockingProtocolStream) Send(message *runnerprotocol.RunnerToControlPlane) error {
	heartbeat := message.GetHeartbeat()
	if heartbeat != nil {
		s.heartbeats <- proto.Clone(heartbeat).(*runnerprotocol.RunnerHeartbeat)
	}
	return nil
}

func (s *blockingProtocolStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	s.mu.Lock()
	if len(s.inbound) != 0 {
		message := s.inbound[0]
		s.inbound = s.inbound[1:]
		s.mu.Unlock()
		return message, nil
	}
	s.mu.Unlock()
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

type recordingAssignmentBackend struct {
	readiness    BackendReadiness
	instance     BackendInstance
	started      *runnerprotocol.AssignmentCommand
	validateErr  error
	startErr     error
	startupCount uint64
	startupP95   time.Duration
	startCalls   atomic.Uint32
	fenceCalls   atomic.Uint32
}

type recoveredRecordingAssignmentBackend struct {
	recordingAssignmentBackend
	recovered []*runnerprotocol.ActiveAssignmentSummary
}

func (backend *recoveredRecordingAssignmentBackend) RecoveredAssignments() []*runnerprotocol.ActiveAssignmentSummary {
	return backend.recovered
}

type classifiedAssignmentError struct {
	decision runnerprotocol.AssignmentDecision
	terminal runnerprotocol.AssignmentTerminalKind
}

func (err classifiedAssignmentError) Error() string { return "classified assignment failure" }
func (err classifiedAssignmentError) AssignmentDecision() runnerprotocol.AssignmentDecision {
	return err.decision
}
func (err classifiedAssignmentError) AssignmentTerminal() runnerprotocol.AssignmentTerminalKind {
	return err.terminal
}

type blockingAssignmentBackend struct {
	recordingAssignmentBackend
	started chan struct{}
	release chan struct{}
}

func (backend *blockingAssignmentBackend) StartAssignment(
	ctx context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	_ func(runnerprotocol.AssignmentProgressStage) error,
) (BackendInstance, error) {
	backend.startCalls.Add(1)
	backend.recordingAssignmentBackend.started = assignment
	close(backend.started)
	select {
	case <-backend.release:
		return backend.instance, nil
	case <-ctx.Done():
		return BackendInstance{}, ctx.Err()
	}
}

func (b *recordingAssignmentBackend) StartupTiming() (uint64, time.Duration) {
	return b.startupCount, b.startupP95
}

type recordingLocalWorkspaceBackend struct {
	*recordingAssignmentBackend
	command    *runnerprotocol.LocalWorkspaceCommand
	evidence   LocalWorkspaceEvidence
	err        error
	localCalls atomic.Uint32
}

func (backend *recordingLocalWorkspaceBackend) ExecuteLocalWorkspace(
	_ context.Context,
	command *runnerprotocol.LocalWorkspaceCommand,
) (LocalWorkspaceEvidence, error) {
	backend.localCalls.Add(1)
	backend.command = command
	return backend.evidence, backend.err
}

func (b *recordingAssignmentBackend) Readiness(context.Context) (BackendReadiness, error) {
	return b.readiness, nil
}

func (b *recordingAssignmentBackend) ValidateAssignment(context.Context, *runnerprotocol.AssignmentCommand) error {
	return b.validateErr
}

func (b *recordingAssignmentBackend) StartAssignment(
	_ context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (BackendInstance, error) {
	b.startCalls.Add(1)
	b.started = assignment
	if b.startErr != nil {
		return BackendInstance{}, b.startErr
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
		return BackendInstance{}, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY); err != nil {
		return BackendInstance{}, err
	}
	return b.instance, nil
}

func (b *recordingAssignmentBackend) FenceAssignment(context.Context, *runnerprotocol.FenceCommand) (FenceEvidence, error) {
	b.fenceCalls.Add(1)
	return FenceEvidence{}, nil
}

func runnerWelcomeFrame(connectionID string) *runnerprotocol.ControlPlaneToRunner {
	return &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Welcome{
			Welcome: &runnerprotocol.RunnerWelcome{
				ConnectionId:        connectionID,
				SelectedVersion:     1,
				EnabledFeatures:     []runnerprotocol.RunnerFeature{runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE},
				HeartbeatIntervalMs: 60_000,
			},
		},
	}
}

func localWorkspaceWelcomeFrame(connectionID string) *runnerprotocol.ControlPlaneToRunner {
	welcome := runnerWelcomeFrame(connectionID)
	welcome.GetWelcome().EnabledFeatures = append(
		welcome.GetWelcome().EnabledFeatures,
		runnerprotocol.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
	)
	return welcome
}

func findLocalWorkspaceResult(
	messages []*runnerprotocol.RunnerToControlPlane,
) *runnerprotocol.LocalWorkspaceResult {
	for _, message := range messages {
		if result := message.GetLocalWorkspaceResult(); result != nil {
			return proto.Clone(result).(*runnerprotocol.LocalWorkspaceResult)
		}
	}
	return nil
}

func findAssignmentAck(messages []*runnerprotocol.RunnerToControlPlane) *runnerprotocol.AssignmentAck {
	for _, message := range messages {
		if ack := message.GetAssignmentAck(); ack != nil {
			return ack
		}
	}
	return nil
}

func findAssignmentResult(messages []*runnerprotocol.RunnerToControlPlane) *runnerprotocol.AssignmentResult {
	for _, message := range messages {
		if result := message.GetAssignmentResult(); result != nil {
			return result
		}
	}
	return nil
}
