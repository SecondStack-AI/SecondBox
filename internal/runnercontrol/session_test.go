package runnercontrol

import (
	"errors"
	"fmt"
	"strings"
	"sync"
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

func TestSessionBoundsMessageIDDedupeAndDefersOldReplayToDurableState(t *testing.T) {
	session := negotiatedSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(2); sequence <= maxSessionMessageIDs+2; sequence++ {
		if _, err := session.Accept(heartbeatFrame(
			"runner-1", "connection-1", fmt.Sprintf("heartbeat-%d", sequence), sequence,
		)); err != nil {
			t.Fatalf("message %d: %v", sequence, err)
		}
	}
	if got := len(session.messageIDs); got != maxSessionMessageIDs {
		t.Fatalf("message ID window size = %d, want %d", got, maxSessionMessageIDs)
	}
	if _, found := session.messageIDs["heartbeat-2"]; found {
		t.Fatal("oldest message ID remained in the bounded window")
	}
	if _, found := session.messageIDs[fmt.Sprintf("heartbeat-%d", maxSessionMessageIDs+2)]; !found {
		t.Fatal("newest message ID is absent from the bounded window")
	}

	oldReplay, err := session.Accept(heartbeatFrame(
		"runner-1", "connection-1", "heartbeat-2", 2,
	))
	if err != nil || oldReplay.Kind != EventHeartbeat {
		t.Fatalf("old replay event = %#v, %v; want durable Heartbeat validation", oldReplay, err)
	}
	if got := session.lastSequence; got != maxSessionMessageIDs+2 {
		t.Fatalf("last sequence after old replay = %d, want %d", got, maxSessionMessageIDs+2)
	}
}

func TestSessionRejectsRegistrationWithFailedPrerequisitesAndVersionSkew(t *testing.T) {
	session := negotiatedSession(t)
	registration := registrationFrame("runner-1", "connection-1", 1)
	registration.GetRegistration().ReadinessFailures = []runnerv1.RunnerReadinessFailure{
		runnerv1.RunnerReadinessFailure_RUNNER_READINESS_FAILURE_HYPERVISOR,
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

func TestSessionRejectsInvalidOrDuplicateAdvertisedEgressContexts(t *testing.T) {
	for _, test := range []struct {
		name     string
		contexts []string
	}{
		{name: "invalid", contexts: []string{"Tenant-Blue"}},
		{name: "duplicate", contexts: []string{"tenant-blue", "tenant-blue"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := negotiatedSession(t)
			registration := registrationFrame("runner-1", "connection-1", 1)
			registration.GetRegistration().SupportedEgressContexts = test.contexts
			if _, err := session.Accept(registration); !errors.Is(err, ErrRunnerPrerequisites) {
				t.Fatalf("registration error = %v, want ErrRunnerPrerequisites", err)
			}
		})
	}
}

func TestSessionRejectsPortableCheckpointOnlyRunner(t *testing.T) {
	session := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-old",
		SupportedVersions:     VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures: []runnerv1.RunnerFeature{
			runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
			runnerv1.RunnerFeature_RUNNER_FEATURE_LOCAL_WORKSPACE,
		},
		HeartbeatInterval: 10 * time.Second,
		ConnectionID:      "connection-old",
	})
	oldRunner := helloFrame("runner-old", 1, 1)
	oldRunner.GetHello().MandatoryFeatures = []runnerv1.RunnerFeature{
		runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		runnerv1.RunnerFeature(6), // reserved former portable-checkpoint feature
	}
	response, err := session.Accept(oldRunner)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection().GetKind() !=
		runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_FEATURE_UNSUPPORTED {
		t.Fatalf("portable-checkpoint runner rejection = %#v", response.GetRejection())
	}
	if response.GetRejection().GetSafeDetail() !=
		"runner does not implement the mandatory local-workspace protocol" {
		t.Fatalf("portable-checkpoint runner detail = %q", response.GetRejection().GetSafeDetail())
	}
}

func TestSessionValidatesRunnerDataPlaneFeatureFenceSequenceAndDuplicates(t *testing.T) {
	session := negotiatedDataPlaneSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	fence := dataPlaneTestFence()
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

func TestSessionReleasesTerminalDataPlaneState(t *testing.T) {
	session := negotiatedDataPlaneSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	fence := dataPlaneTestFence()
	if err := session.ValidateOutboundDataPlaneFrame(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
			Fence: fence, OperationId: "operation-terminal", StreamId: "stream-terminal", Sequence: 1,
			Payload: &runnerv1.ExecFrame_Open{Open: &runnerv1.ExecOpen{
				Command: &runnerv1.ExecOpen_Shell{Shell: "true"},
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	terminal := runnerExecFrame(fence, "operation-terminal", "stream-terminal", 1, &runnerv1.ExecFrame_Terminal{
		Terminal: &runnerv1.ExecTerminal{Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED},
	})
	if _, err := session.Accept(terminal); err != nil {
		t.Fatal(err)
	}
	if len(session.inboundDataPlaneStreams) != 0 || len(session.outboundStreams) != 0 {
		t.Fatalf(
			"terminal stream state retained inbound=%d outbound=%d",
			len(session.inboundDataPlaneStreams), len(session.outboundStreams),
		)
	}
	ptyInboundKey := dataPlaneStreamKey("pty", fence, "operation-pty-terminal", "stream-pty-terminal")
	ptyExecKey := dataPlaneStreamKey("exec", fence, "operation-pty-terminal", "stream-pty-terminal")
	session.inboundDataPlaneStreams[ptyInboundKey] = dataPlaneStreamState{sequence: 1}
	session.outboundStreams[ptyInboundKey] = dataPlaneStreamState{sequence: 2}
	session.outboundStreams[ptyExecKey] = dataPlaneStreamState{sequence: 1}
	session.releaseTerminalDataPlaneState(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
			Fence: fence, OperationId: "operation-pty-terminal", StreamId: "stream-pty-terminal",
			Payload: &runnerv1.PtyFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
				Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
			}},
		}},
	}, ptyInboundKey)
	if len(session.inboundDataPlaneStreams) != 0 || len(session.outboundStreams) != 0 {
		t.Fatalf(
			"PTY terminal stream state retained inbound=%d outbound=%d",
			len(session.inboundDataPlaneStreams), len(session.outboundStreams),
		)
	}
}

func TestSessionSerializesConcurrentInboundTerminalsAndOutboundControls(t *testing.T) {
	session := negotiatedDataPlaneSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	fence := dataPlaneTestFence()
	const streams = 128
	for index := range streams {
		operationID := fmt.Sprintf("operation-concurrent-%d", index)
		streamID := fmt.Sprintf("stream-concurrent-%d", index)
		if err := session.ValidateOutboundDataPlaneFrame(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: operationID, StreamId: streamID, Sequence: 1,
				Payload: &runnerv1.ExecFrame_Open{Open: &runnerv1.ExecOpen{
					Command: &runnerv1.ExecOpen_Shell{Shell: "true"},
				}},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	errors := make(chan error, streams*2)
	var group sync.WaitGroup
	for index := range streams {
		operationID := fmt.Sprintf("operation-concurrent-%d", index)
		streamID := fmt.Sprintf("stream-concurrent-%d", index)
		group.Add(2)
		go func() {
			defer group.Done()
			_, err := session.Accept(runnerExecFrame(
				fence, operationID, streamID, 1,
				&runnerv1.ExecFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
					Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED,
				}},
			))
			errors <- err
		}()
		go func() {
			defer group.Done()
			errors <- session.ValidateOutboundDataPlaneFrame(&runnerv1.ControlPlaneToRunner{
				Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
					Fence: fence, OperationId: operationID, StreamId: streamID, Sequence: 2,
					Payload: &runnerv1.ExecFrame_Credit{Credit: &runnerv1.StreamCredit{
						ByteCount: 1,
					}},
				}},
			})
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent data-plane session mutation: %v", err)
		}
	}
}

func TestSessionRequiresProtocolTwoAndBoundedWorkspaceTransferFrames(t *testing.T) {
	session := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1",
		SupportedVersions:     VersionRange{Minimum: 1, Maximum: 2},
		EnabledFeatures:       []runnerv1.RunnerFeature{runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE},
		HeartbeatInterval:     10 * time.Second,
		ConnectionID:          "connection-1",
	})
	if response, err := session.Accept(helloFrame("runner-1", 2, 2)); err != nil || response.GetWelcome() == nil {
		t.Fatalf("Hello response = %#v, %v", response, err)
	}
	registration := registrationFrame("runner-1", "connection-1", 1)
	registration.GetRegistration().ProtocolVersion = 2
	if _, err := session.Accept(registration); err != nil {
		t.Fatal(err)
	}
	open := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_WorkspaceTransfer{
			WorkspaceTransfer: &runnerv1.WorkspaceTransferFrame{
				OperationId: "operation-relocation", SandboxId: "sandbox-1",
				WorkspaceId: "workspace-1", Generation: 3, Sequence: 1,
				Payload: &runnerv1.WorkspaceTransferFrame_Open{
					Open: &runnerv1.WorkspaceTransferOpen{
						LogicalCapacityBytes: 8 << 30,
						FencingToken:         []byte("01234567890123456789012345678901"),
					},
				},
			},
		},
	}
	event, err := session.Accept(open)
	if err != nil || event.Kind != EventWorkspaceTransfer {
		t.Fatalf("Workspace transfer open event = %#v, %v", event, err)
	}
	oversized := proto.Clone(open).(*runnerv1.RunnerToControlPlane)
	oversized.GetWorkspaceTransfer().Sequence = 2
	oversized.GetWorkspaceTransfer().Payload = &runnerv1.WorkspaceTransferFrame_Chunk{
		Chunk: &runnerv1.WorkspaceTransferChunk{Data: make([]byte, (64<<10)+1)},
	}
	if _, err := session.Accept(oversized); !errors.Is(err, ErrRunnerMessage) {
		t.Fatalf("oversized Workspace transfer chunk error = %v", err)
	}
}

func TestSessionRejectsDataPlaneFramesWithoutNegotiatedFeature(t *testing.T) {
	session := negotiatedSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Accept(runnerFileFrame(dataPlaneTestFence(), "operation-1", "stream-1", 1)); !errors.Is(err, ErrRunnerMessage) {
		t.Fatalf("unnegotiated File frame error = %v, want ErrRunnerMessage", err)
	}
}

func TestSessionAcceptsOrderedOutboundExecInput(t *testing.T) {
	session := negotiatedDataPlaneSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	fence := dataPlaneTestFence()
	for _, frame := range []*runnerv1.ExecFrame{
		{
			Fence: fence, OperationId: "operation-1", StreamId: "stream-1", Sequence: 1,
			Payload: &runnerv1.ExecFrame_Open{Open: &runnerv1.ExecOpen{
				Command: &runnerv1.ExecOpen_Shell{Shell: "cat"},
			}},
		},
		{
			Fence: fence, OperationId: "operation-1", StreamId: "stream-1", Sequence: 2,
			Payload: &runnerv1.ExecFrame_Input{Input: &runnerv1.ExecInput{
				Data: []byte("payload\n"),
			}},
		},
	} {
		if err := session.ValidateOutboundDataPlaneFrame(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: frame},
		}); err != nil {
			t.Fatalf("outbound Exec sequence %d: %v", frame.Sequence, err)
		}
	}
}

func TestSessionAcceptsOrderedTerminalFramesAcrossExecAndPtyEnvelopes(t *testing.T) {
	session := negotiatedDataPlaneSession(t)
	if _, err := session.Accept(registrationFrame("runner-1", "connection-1", 1)); err != nil {
		t.Fatal(err)
	}
	fence := dataPlaneTestFence()
	frames := []*runnerv1.ControlPlaneToRunner{
		{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: "terminal-1", StreamId: "stream-1", Sequence: 1,
				Payload: &runnerv1.ExecFrame_Open{Open: &runnerv1.ExecOpen{
					Command: &runnerv1.ExecOpen_Shell{Shell: "sh"}, AllocatePty: true,
				}},
			}},
		},
		{
			Message: &runnerv1.ControlPlaneToRunner_Pty{Pty: &runnerv1.PtyFrame{
				Fence: fence, OperationId: "terminal-1", StreamId: "stream-1", Sequence: 2,
				Payload: &runnerv1.PtyFrame_Credit{
					Credit: &runnerv1.StreamCredit{ByteCount: 1024},
				},
			}},
		},
		{
			Message: &runnerv1.ControlPlaneToRunner_Pty{Pty: &runnerv1.PtyFrame{
				Fence: fence, OperationId: "terminal-1", StreamId: "stream-1", Sequence: 3,
				Payload: &runnerv1.PtyFrame_Input{
					Input: &runnerv1.PtyInput{Data: []byte("echo ready\n")},
				},
			}},
		},
		{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: "terminal-1", StreamId: "stream-1", Sequence: 4,
				Payload: &runnerv1.ExecFrame_Cancel{
					Cancel: &runnerv1.ExecCancel{Reason: "test cancellation"},
				},
			}},
		},
	}
	for index, frame := range frames {
		if err := session.ValidateOutboundDataPlaneFrame(frame); err != nil {
			t.Fatalf("outbound Terminal frame %d: %v", index+1, err)
		}
	}

	output := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
			Fence: fence, OperationId: "terminal-1", StreamId: "stream-1", Sequence: 1,
			Payload: &runnerv1.PtyFrame_Output{Output: &runnerv1.ExecOutput{
				Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
				Data:    []byte("ready\r\n"),
			}},
		}},
	}
	event, err := session.Accept(output)
	if err != nil || event.Kind != EventPty {
		t.Fatalf("inbound Terminal event = %#v, %v", event, err)
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
				Fence:                     dataPlaneTestFence(),
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

func negotiatedDataPlaneSession(t *testing.T) *Session {
	t.Helper()
	session := NewSession(SessionConfig{
		AuthenticatedRunnerID: "runner-1",
		SupportedVersions:     VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures: []runnerv1.RunnerFeature{
			runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
			runnerv1.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
			runnerv1.RunnerFeature_RUNNER_FEATURE_PTY,
			runnerv1.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING,
		},
		HeartbeatInterval: 10 * time.Second,
		ConnectionID:      "connection-1",
	})
	if response, err := session.Accept(helloDataPlaneFrame("runner-1")); err != nil || response.GetWelcome() == nil {
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

func helloDataPlaneFrame(runnerID string) *runnerv1.RunnerToControlPlane {
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
					Architecture: "amd64", ComputeBackendVersion: "1.16.1",
					HypervisorReady: true, IsolationReady: true, ResourceLimitsReady: true,
					NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
					DataPlaneReady:           true,
					GuestProtocolGenerations: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
				},
				Allocatable:                    &runnerv1.Capacity{VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8},
				Reserved:                       &runnerv1.Capacity{},
				StartupTiming:                  &runnerv1.StartupTiming{},
				DataPlaneAdvertisedAddress:     "10.0.0.5:7443",
				DataPlaneCertificateSpkiSha256: strings.Repeat("a", 64),
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
				Allocatable: &runnerv1.Capacity{VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30, Instances: 8},
				Reserved:    &runnerv1.Capacity{}, DrainPhase: runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
				StartupTiming:              &runnerv1.StartupTiming{},
				DataPlaneAdvertisedAddress: "10.0.0.5:7443",
			},
		},
	}
}

func dataPlaneTestFence() *runnerv1.AssignmentFence {
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
