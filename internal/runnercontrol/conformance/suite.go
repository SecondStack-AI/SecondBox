// Package conformance provides reusable runner protocol state-machine qualification.
package conformance

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
)

// RunSessionSuite qualifies negotiation, registration, duplicate, reorder, and reconnect behavior.
func RunSessionSuite(
	t *testing.T,
	factory func(connectionID string) *runnercontrol.Session,
) {
	t.Helper()
	t.Run("registration_duplicate_reorder", func(t *testing.T) {
		session := factory("connection-1")
		welcome, err := session.Accept(hello("runner-1", 1, 1))
		if err != nil || welcome.GetWelcome() == nil {
			t.Fatalf("Welcome = %#v, %v", welcome, err)
		}
		registration := registered("runner-1", "connection-1", "registration-1", 1)
		if event, err := session.Accept(registration); err != nil || event.Kind != runnercontrol.EventRegistration {
			t.Fatalf("Registration = %#v, %v", event, err)
		}
		heartbeat := activeHeartbeat("runner-1", "connection-1", "heartbeat-2", 2)
		if event, err := session.Accept(heartbeat); err != nil || event.Kind != runnercontrol.EventHeartbeat {
			t.Fatalf("Heartbeat = %#v, %v", event, err)
		}
		if event, err := session.Accept(heartbeat); err != nil || event.Kind != runnercontrol.EventDuplicate {
			t.Fatalf("duplicate Heartbeat = %#v, %v", event, err)
		}
		if _, err := session.Accept(activeHeartbeat("runner-1", "connection-1", "old", 1)); !errors.Is(err, runnercontrol.ErrSequenceReordered) {
			t.Fatalf("reordered Heartbeat error = %v", err)
		}
	})
	t.Run("reconnect_resets_connection_sequence_not_identity", func(t *testing.T) {
		session := factory("connection-2")
		if welcome, err := session.Accept(hello("runner-1", 1, 1)); err != nil || welcome.GetWelcome() == nil {
			t.Fatalf("reconnect Welcome = %#v, %v", welcome, err)
		}
		if event, err := session.Accept(registered("runner-1", "connection-2", "registration-new", 1)); err != nil || event.Kind != runnercontrol.EventRegistration {
			t.Fatalf("reconnect Registration = %#v, %v", event, err)
		}
	})
	t.Run("unsupported_version", func(t *testing.T) {
		session := factory("connection-version")
		rejection, err := session.Accept(hello("runner-1", 2, 3))
		if err != nil {
			t.Fatal(err)
		}
		if rejection.GetRejection().GetKind() != runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_VERSION_UNSUPPORTED {
			t.Fatalf("version rejection = %#v", rejection.GetRejection())
		}
	})
}

// DefaultSessionFactory returns the v1 evidence-enabled control-plane session.
func DefaultSessionFactory(connectionID string) *runnercontrol.Session {
	return runnercontrol.NewSession(runnercontrol.SessionConfig{
		AuthenticatedRunnerID: "runner-1",
		SupportedVersions:     runnercontrol.VersionRange{Minimum: 1, Maximum: 1},
		EnabledFeatures: []runnerv1.RunnerFeature{
			runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
		},
		HeartbeatInterval: 10 * time.Second,
		ConnectionID:      connectionID,
	})
}

func hello(runnerID string, minimum, maximum uint32) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Hello{
			Hello: &runnerv1.RunnerHello{
				RunnerId: runnerID, ConnectionNonce: []byte("01234567890123456789012345678901"),
				SupportedVersions: &runnerv1.ProtocolVersionRange{Minimum: minimum, Maximum: maximum},
				MandatoryFeatures: []runnerv1.RunnerFeature{
					runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
				},
			},
		},
	}
}

func registered(
	runnerID string,
	connectionID string,
	messageID string,
	sequence uint64,
) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Registration{
			Registration: &runnerv1.RunnerRegistration{
				MessageId: messageID, Sequence: sequence, RunnerId: runnerID,
				ConnectionId: connectionID, RunnerPoolId: "general",
				SoftwareVersion: "1.0.0", ProtocolVersion: 1,
				Capabilities: &runnerv1.RunnerCapabilities{
					Architecture: "amd64", FirecrackerVersion: "1.16.1",
					KvmReady: true, JailerReady: true, CgroupReady: true,
					NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
					GuestProtocolGenerations: &runnerv1.ProtocolVersionRange{Minimum: 1, Maximum: 1},
				},
				Allocatable: &runnerv1.Capacity{
					VcpuMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
					Instances: 8, Operations: 32,
				},
				Reserved:      &runnerv1.Capacity{},
				StartupTiming: &runnerv1.StartupTiming{},
			},
		},
	}
}

func activeHeartbeat(
	runnerID string,
	connectionID string,
	messageID string,
	sequence uint64,
) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Heartbeat{
			Heartbeat: &runnerv1.RunnerHeartbeat{
				MessageId: messageID, Sequence: sequence, RunnerId: runnerID,
				ConnectionId: connectionID, ObservedAtUnixMs: 1,
				Allocatable: &runnerv1.Capacity{
					VcpuMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
					Instances: 8, Operations: 32,
				},
				Reserved:      &runnerv1.Capacity{},
				DrainPhase:    runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE,
				StartupTiming: &runnerv1.StartupTiming{},
			},
		},
	}
}
