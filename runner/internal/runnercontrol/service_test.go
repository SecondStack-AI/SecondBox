package runnercontrol

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

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
			Capacity:     &runnerprotocol.Capacity{VcpuMillis: 4000, MemoryBytes: 8 << 30, DiskBytes: 64 << 30, Instances: 2, Operations: 8},
			Capabilities: &runnerprotocol.RunnerCapabilities{
				Architecture:             "amd64",
				FirecrackerVersion:       "1.16.1",
				KvmReady:                 true,
				JailerReady:              true,
				CgroupReady:              true,
				NetworkPolicyReady:       true,
				StorageReady:             true,
				CleanupReady:             true,
				GuestProtocolGenerations: &runnerprotocol.ProtocolVersionRange{Minimum: 1, Maximum: 1},
			},
		},
		instance: BackendInstance{BackendKind: "firecracker", BackendReference: "fc-instance-1"},
	}
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	evidenceSink := &recordingEvidenceSink{}
	service.SetEvidenceSink(evidenceSink)

	err = service.Run(t.Context())
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
	if stream.outbound[1].GetRegistration() == nil {
		t.Fatal("registration was sent before protocol negotiation or omitted")
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

	err = service.Run(t.Context())
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
	service, err := NewRunnerProtocolService(testRunnerConfig(), backend, staticProtocolConnector{stream: stream})
	if err != nil {
		t.Fatal(err)
	}

	err = service.Run(t.Context())
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
		state.RemainingAssignments[0].AssignmentId != assignment.Fence.AssignmentId {
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
			MaximumOperationMs: 30_000,
			MaximumOutputBytes: 8 << 20,
		},
		Assets: []*runnerprotocol.SignedAssetReference{
			{
				ArtifactId:              "secondbox-rootfs-1",
				ManifestDigest:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				SignatureKeyId:          "key-1",
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

func testRunnerConfig() RunnerProtocolConfig {
	return RunnerProtocolConfig{
		RunnerID:        "runner-1",
		RunnerPoolID:    "pool-1",
		SoftwareVersion: "1.0.0",
		ProtocolMinimum: 1,
		ProtocolMaximum: 1,
		MandatoryFeatures: []runnerprotocol.RunnerFeature{
			runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		},
	}
}

type recordingProtocolStream struct {
	inbound  []*runnerprotocol.ControlPlaneToRunner
	outbound []*runnerprotocol.RunnerToControlPlane
}

func (s *recordingProtocolStream) Send(message *runnerprotocol.RunnerToControlPlane) error {
	s.outbound = append(s.outbound, message)
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

type recordingAssignmentBackend struct {
	readiness BackendReadiness
	instance  BackendInstance
	started   *runnerprotocol.AssignmentCommand
}

func (b *recordingAssignmentBackend) Readiness(context.Context) (BackendReadiness, error) {
	return b.readiness, nil
}

func (*recordingAssignmentBackend) ValidateAssignment(context.Context, *runnerprotocol.AssignmentCommand) error {
	return nil
}

func (b *recordingAssignmentBackend) StartAssignment(
	_ context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (BackendInstance, error) {
	b.started = assignment
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
		return BackendInstance{}, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY); err != nil {
		return BackendInstance{}, err
	}
	return b.instance, nil
}

func (*recordingAssignmentBackend) FenceAssignment(context.Context, *runnerprotocol.FenceCommand) (FenceEvidence, error) {
	return FenceEvidence{}, nil
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
