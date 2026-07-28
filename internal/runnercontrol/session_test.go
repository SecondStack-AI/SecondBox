package runnercontrol

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/protobuf/proto"
)

func TestSessionRequiresCertificateIdentityHelloAndRegistrationOrder(t *testing.T) {
	session := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1",
		SupportedVersions:     VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures:       []runnerv1.RunnerFeature{runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE},
		HeartbeatInterval:     10 * time.Second,
		ConnectionID:          "connection-1",
	})
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); !errors.Is(err, ErrHelloRequired) {
		t.Fatalf("registration before Hello error = %v, want ErrHelloRequired", err)
	}
	rejection, err := session.Accept(helloFrame("other-runner", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if rejection.GetRejection().GetKind() != runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_IDENTITY_MISMATCH {
		t.Fatalf("identity rejection = %#v", rejection.GetRejection())
	}
}

func TestSessionDeduplicatesAndRejectsReorderedRunnerEvidence(t *testing.T) {
	session := negotiatedSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	heartbeat := heartbeatFrame("runner-1", "connection-1", "heartbeat-2", 2)
	event, err := session.Accept(heartbeat)
	if err != nil || event.Kind != EventHeartbeat {
		t.Fatalf("heartbeat event = %#v, %v", event, err)
	}
	duplicate, err := session.Accept(heartbeat)
	if err != nil || duplicate.Kind != EventDuplicate {
		t.Fatalf("duplicate event = %#v, %v", duplicate, err)
	}
	if _, err := session.Accept(heartbeatFrame("runner-1", "connection-1", "heartbeat-reordered", 1)); !errors.Is(err, ErrSequenceReordered) {
		t.Fatalf("reordered heartbeat error = %v, want ErrSequenceReordered", err)
	}
}

func TestSessionRejectsRegistrationWithFailedPrerequisitesAndVersionSkew(t *testing.T) {
	session := negotiatedSession(t)
	registration := registrationFrame("runner-1", "connection-1", 1)
	registration.GetRegistration().ReadinessFailures = []runnerv1.RunnerReadinessFailure{
		runnerv1.RunnerReadinessFailure_RUNNER_READINESS_FAILURE_KVM,
	}
	if _, err := session.Accept(registration); !errors.Is(err, ErrRunnerPrerequisites) {
		t.Fatalf("failed prerequisite registration error = %v, want ErrRunnerPrerequisites", err)
	}

	unsupported := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1",
		SupportedVersions:     VersionRange{Minimum: 2, Maximum: 2},
		HeartbeatInterval:     10 * time.Second,
		ConnectionID:          "connection-unsupported",
	})
	response, err := unsupported.Accept(helloFrame("runner-1", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection().GetKind() != runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_VERSION_UNSUPPORTED {
		t.Fatalf("version rejection = %#v", response.GetRejection())
	}
}

func TestSessionValidatesRunnerRelayFeatureFenceSequenceAndDuplicates(t *testing.T) {
	session := negotiatedRelaySession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	fence := relayTestFence()
	first := runnerExecFrame(fence, "operation-1", "stream-1", 1, &runnerv1.ExecFrame_Output{
		Output: &runnerv1.ExecOutput{
			Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
			Data:    []byte("first"),
		},
	})
	event, err := session.Accept(first)
	if err != nil || event.Kind != EventExec {
		t.Fatalf("first Exec event = %#v, %v", event, err)
	}
	duplicate, err := session.Accept(proto.Clone(first).(*runnerv1.RunnerToControlPlane))
	if err != nil || duplicate.Kind != EventDuplicate {
		t.Fatalf("duplicate Exec event = %#v, %v", duplicate, err)
	}
	conflict := runnerExecFrame(fence, "operation-1", "stream-1", 1, &runnerv1.ExecFrame_Output{
		Output: &runnerv1.ExecOutput{
			Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
			Data:    []byte("different"),
		},
	})
	if _, err := session.Accept(conflict); !errors.Is(err, ErrSequenceReordered) {
		t.Fatalf("conflicting duplicate error = %v, want ErrSequenceReordered", err)
	}
	gap := runnerExecFrame(fence, "operation-1", "stream-1", 3, &runnerv1.ExecFrame_Terminal{
		Terminal: &runnerv1.ExecTerminal{Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED},
	})
	if _, err := session.Accept(gap); !errors.Is(err, ErrSequenceReordered) {
		t.Fatalf("gapped Exec error = %v, want ErrSequenceReordered", err)
	}
	incomplete := proto.Clone(fence).(*runnerv1.AssignmentFence)
	incomplete.SandboxGeneration = 0
	if _, err := session.Accept(runnerFileFrame(incomplete, "operation-2", "stream-2", 1)); !errors.Is(err, ErrRunnerMessage) {
		t.Fatalf("incomplete generation frame error = %v, want ErrRunnerMessage", err)
	}
}

func TestSessionRejectsRelayFramesWithoutNegotiatedFeature(t *testing.T) {
	session := negotiatedSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Accept(runnerFileFrame(relayTestFence(), "operation-1", "stream-1", 1)); !errors.Is(err, ErrRunnerMessage) {
		t.Fatalf("unnegotiated File frame error = %v, want ErrRunnerMessage", err)
	}
}

func TestSessionClassifiesDurableInstanceTerminalEnvelope(t *testing.T) {
	session := negotiatedSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	frame := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_InstanceTerminal{
			InstanceTerminal: &runnerv1.InstanceTerminal{
				MessageId:                 "terminal-2",
				Sequence:                  2,
				Fence:                     relayTestFence(),
				Reason:                    runnerv1.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_GUEST_SHUTDOWN,
				ObservedAtUnixMs:          1,
				TerminationEvidenceDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Correlation: &runnerv1.Correlation{
					RequestId: "request-1", OperationId: "operation-1", LeaseId: "lease-1",
					SandboxId: "sandbox-1", InstanceId: "instance-1", SandboxGeneration: 3,
					AssignmentId: "assignment-1", RunnerId: "runner-1",
				},
			},
		},
	}
	event, err := session.Accept(frame)
	if err != nil || event.Kind != EventInstanceTerminal {
		t.Fatalf("instance terminal event = %#v, %v", event, err)
	}
}

func negotiatedSession(t *testing.T) *Session {
	t.Helper()
	session := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1",
		SupportedVersions:     VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures:       []runnerv1.RunnerFeature{runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE},
		HeartbeatInterval:     10 * time.Second,
		ConnectionID:          "connection-1",
	})
	if response, err := session.Accept(helloFrame("runner-1", 1, 1)); err != nil || response.GetWelcome() == nil {
		t.Fatalf("Hello response = %#v, %v", response, err)
	}
	return session
}

func negotiatedRelaySession(t *testing.T) *Session {
	t.Helper()
	session := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1",
		SupportedVersions:     VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures: []runnerv1.RunnerFeature{
			runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
			runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
			runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING,
		},
		HeartbeatInterval: 10 * time.Second,
		ConnectionID:      "connection-1",
	})
	if response, err := session.Accept(helloRelayFrame("runner-1")); err != nil || response.GetWelcome() == nil {
		t.Fatalf("Hello response = %#v, %v", response, err)
	}
	return session
}

func helloFrame(runnerID string, minimum, maximum uint32) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Hello{
			Hello: &runnerv1.RunnerHello{
				RunnerId: runnerID, ConnectionNonce: []byte("01234567890123456789012345678901"),
				SupportedVersions: &runnerv1.ProtocolVersionRange{Minimum: minimum, Maximum: maximum},
				MandatoryFeatures: []runnerv1.RunnerFeature{runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE},
			},
		},
	}
}

func helloRelayFrame(runnerID string) *runnerv1.RunnerToControlPlane {
	frame := helloFrame(runnerID, 1, 1)
	frame.GetHello().MandatoryFeatures = []runnerv1.RunnerFeature{
		runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
		runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING,
	}
	return frame
}

func registrationFrame(runnerID, connectionID string, sequence uint64) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Registration{
			Registration: &runnerv1.RunnerRegistration{
				MessageId: "registration", Sequence: sequence, RunnerId: runnerID,
				ConnectionId: connectionID, RunnerPoolId: "general", SoftwareVersion: "1.0.0",
				ProtocolVersion: 1,
				Capabilities: &runnerv1.RunnerCapabilities{
					Architecture: "amd64", FirecrackerVersion: "1.16.1",
					KvmReady: true, JailerReady: true, CgroupReady: true,
					NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
					GuestProtocolGenerations: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
				},
				Allocatable: &runnerv1.Capacity{VcpuMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8},
				Reserved:    &runnerv1.Capacity{},
			},
		},
	}
}

func heartbeatFrame(runnerID, connectionID, messageID string, sequence uint64) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Heartbeat{
			Heartbeat: &runnerv1.RunnerHeartbeat{
				MessageId: messageID, Sequence: sequence, RunnerId: runnerID,
				ConnectionId: connectionID, ObservedAtUnixMs: 1,
				Allocatable: &runnerv1.Capacity{VcpuMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8},
				Reserved:    &runnerv1.Capacity{}, DrainPhase: runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
			},
		},
	}
}

func relayTestFence() *runnerv1.AssignmentFence {
	return &runnerv1.AssignmentFence{
		AssignmentId:      "assignment-1",
		SandboxId:         "sandbox-1",
		InstanceId:        "instance-1",
		SandboxGeneration: 3,
		FencingToken:      []byte("opaque-fence-token"),
	}
}

func runnerExecFrame(
	fence *runnerv1.AssignmentFence,
	operationID string,
	streamID string,
	sequence uint64,
	payload any,
) *runnerv1.RunnerToControlPlane {
	frame := &runnerv1.ExecFrame{
		Fence: fence, OperationId: operationID, StreamId: streamID, Sequence: sequence,
	}
	switch typed := payload.(type) {
	case *runnerv1.ExecFrame_Output:
		frame.Payload = typed
	case *runnerv1.ExecFrame_Terminal:
		frame.Payload = typed
	default:
		panic("unsupported Exec test payload")
	}
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Exec{Exec: frame},
	}
}

func runnerFileFrame(
	fence *runnerv1.AssignmentFence,
	operationID string,
	streamID string,
	sequence uint64,
) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_File{File: &runnerv1.FileFrame{
			Fence: fence, OperationId: operationID, StreamId: streamID, Sequence: sequence,
			Payload: &runnerv1.FileFrame_Terminal{
				Terminal: &runnerv1.FileTerminal{Kind: runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED},
			},
		}},
	}
}
