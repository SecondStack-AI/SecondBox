package conformance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

var (
	ErrReorderedMessage   = errors.New("SecondBox conformance reordered runner message")
	ErrInactiveConnection = errors.New("SecondBox conformance inactive runner connection")
	conformanceFenceID    atomic.Uint64
)

type Outcome string

const (
	OutcomeRegistration Outcome = "registration"
	OutcomeHeartbeat    Outcome = "heartbeat"
	OutcomeAssignment   Outcome = "assignment"
	OutcomeDuplicate    Outcome = "duplicate"
)

// Boundary exposes the durable control-plane behavior qualified by the reusable suite.
type Boundary interface {
	Connect(
		context.Context,
		string,
		string,
		*runnerv1.RunnerToControlPlane,
	) (*runnerv1.ControlPlaneToRunner, error)
	Receive(
		context.Context,
		string,
		*runnerv1.RunnerToControlPlane,
		time.Time,
	) (Outcome, error)
	SeedAssignment(context.Context, *runnerv1.AssignmentFence, time.Time) error
	ExpireRunner(context.Context, string, time.Time) error
	Snapshot(context.Context, string) (Snapshot, error)
}

// BoundaryFactory creates isolated durable state for one conformance case.
type BoundaryFactory func(*testing.T, time.Time) Boundary

// Snapshot is bounded durable evidence used by cross-implementation assertions.
type Snapshot struct {
	RunnerState        string
	DrainPhase         string
	VCPUCount          int64
	MemoryBytes        int64
	DiskBytes          int64
	Instances          int64
	Operations         int64
	ActiveConnectionID string
	AssignmentState    string
	MayReassign        bool
}

// FakeRunner produces canonical runner frames with stable identity and connection-local ordering.
type FakeRunner struct {
	RunnerID     string
	PoolName     string
	ConnectionID string
	sequence     uint64
	lastFrame    *runnerv1.RunnerToControlPlane
}

func NewFakeRunner(runnerID, poolName, connectionID string) *FakeRunner {
	return &FakeRunner{
		RunnerID: runnerID, PoolName: poolName, ConnectionID: connectionID,
	}
}

func (runner *FakeRunner) Hello(minimum, maximum uint32) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Hello{
			Hello: &runnerv1.RunnerHello{
				RunnerId:        runner.RunnerID,
				ConnectionNonce: []byte("01234567890123456789012345678901"),
				SupportedVersions: &runnerv1.ProtocolVersionRange{
					Minimum: minimum, Maximum: maximum,
				},
				MandatoryFeatures: []runnerv1.RunnerFeature{
					runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
				},
			},
		},
	}
}

func (runner *FakeRunner) Registration() *runnerv1.RunnerToControlPlane {
	sequence := runner.nextSequence()
	return runner.remember(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Registration{
			Registration: &runnerv1.RunnerRegistration{
				MessageId: runner.messageID("registration", sequence), Sequence: sequence,
				RunnerId: runner.RunnerID, ConnectionId: runner.ConnectionID,
				RunnerPoolId: runner.PoolName, SoftwareVersion: "1.0.0",
				ProtocolVersion: 3,
				BackendKind:     runnerv1.ComputeBackendKind_COMPUTE_BACKEND_KIND_FIRECRACKER,
				Capabilities: &runnerv1.RunnerCapabilities{
					Architecture: "amd64", KernelRelease: "6.12.0",
					ComputeBackendVersion: "1.16.1",
					HypervisorReady:       true, IsolationReady: true, ResourceLimitsReady: true,
					NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
					DataPlaneReady: true,
					GuestProtocolGenerations: &runnerv1.ProtocolVersionRange{
						Minimum: 1, Maximum: 1,
					},
				},
				Allocatable: &runnerv1.Capacity{
					VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
					Instances: 8, Operations: 32,
				},
				Reserved:                       &runnerv1.Capacity{},
				Materializations:               fixtureMaterializations(),
				StartupTiming:                  &runnerv1.StartupTiming{},
				DataPlaneAdvertisedAddress:     "10.0.0.5:7443",
				DataPlaneCertificateSpkiSha256: strings.Repeat("a", 64),
			},
		},
	})
}

func (runner *FakeRunner) Heartbeat(phase runnerv1.DrainPhase) *runnerv1.RunnerToControlPlane {
	sequence := runner.nextSequence()
	return runner.remember(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Heartbeat{
			Heartbeat: &runnerv1.RunnerHeartbeat{
				MessageId: runner.messageID("heartbeat", sequence), Sequence: sequence,
				RunnerId: runner.RunnerID, ConnectionId: runner.ConnectionID,
				ObservedAtUnixMs: 1,
				Allocatable: &runnerv1.Capacity{
					VcpuCount: 8, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
					Instances: 8, Operations: 32,
				},
				Reserved: &runnerv1.Capacity{}, DrainPhase: phase,
				StartupTiming:              &runnerv1.StartupTiming{},
				DataPlaneAdvertisedAddress: "10.0.0.5:7443",
			},
		},
	})
}

func (runner *FakeRunner) AssignmentReady(
	fence *runnerv1.AssignmentFence,
) *runnerv1.RunnerToControlPlane {
	sequence := runner.nextSequence()
	return runner.remember(&runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId: runner.messageID("assignment-result", sequence),
				Sequence:  sequence, Fence: fence,
				Terminal:    runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind: "firecracker", BackendReference: "fc-conformance",
				Correlation: runner.assignmentCorrelation(fence),
			},
		},
	})
}

func (runner *FakeRunner) ReplayLast() (*runnerv1.RunnerToControlPlane, error) {
	if runner.lastFrame == nil {
		return nil, errors.New("SecondBox fake runner has no frame to replay")
	}
	return runner.lastFrame, nil
}

func (runner *FakeRunner) ReorderedAssignmentReady(
	fence *runnerv1.AssignmentFence,
	sequence uint64,
) *runnerv1.RunnerToControlPlane {
	return &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId: runner.messageID("reordered-result", sequence),
				Sequence:  sequence, Fence: fence,
				Terminal:    runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind: "firecracker", BackendReference: "fc-reordered",
				Correlation: runner.assignmentCorrelation(fence),
			},
		},
	}
}

func (runner *FakeRunner) assignmentCorrelation(
	fence *runnerv1.AssignmentFence,
) *runnerv1.Correlation {
	return &runnerv1.Correlation{
		RequestId: "request-conformance", OperationId: "operation-conformance",
		SandboxId: fence.GetSandboxId(), InstanceId: fence.GetInstanceId(),
		SandboxGeneration: fence.GetSandboxGeneration(),
		AssignmentId:      fence.GetAssignmentId(), RunnerId: runner.RunnerID,
	}
}

func (runner *FakeRunner) Reconnect(connectionID string) {
	runner.ConnectionID = connectionID
	runner.sequence = 0
	runner.lastFrame = nil
}

func (runner *FakeRunner) nextSequence() uint64 {
	runner.sequence++
	return runner.sequence
}

func (runner *FakeRunner) remember(
	frame *runnerv1.RunnerToControlPlane,
) *runnerv1.RunnerToControlPlane {
	runner.lastFrame = frame
	return frame
}

func (runner *FakeRunner) messageID(kind string, sequence uint64) string {
	return fmt.Sprintf("%s-%s-%d", runner.ConnectionID, kind, sequence)
}

// RunRunnerConformanceSuite qualifies every Task 4 runner lifecycle behavior.
func RunRunnerConformanceSuite(t *testing.T, factory BoundaryFactory) {
	t.Helper()
	now := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)

	t.Run("registration_and_capacity", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		snapshot := readSnapshot(t, boundary, runner.RunnerID)
		if snapshot.RunnerState != "ready" ||
			snapshot.VCPUCount != 8 ||
			snapshot.MemoryBytes != 32<<30 ||
			snapshot.DiskBytes != 200<<30 ||
			snapshot.Instances != 8 ||
			snapshot.Operations != 32 {
			t.Fatalf("registered capacity snapshot = %#v", snapshot)
		}
	})

	t.Run("stale_heartbeat", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		if _, err := boundary.Receive(
			t.Context(), runner.ConnectionID,
			runner.Heartbeat(runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE), now,
		); err != nil {
			t.Fatal(err)
		}
		if err := boundary.ExpireRunner(
			t.Context(), runner.RunnerID, now.Add(31*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		if snapshot := readSnapshot(t, boundary, runner.RunnerID); snapshot.RunnerState != "offline" {
			t.Fatalf("stale Runner state = %q, want offline", snapshot.RunnerState)
		}
	})

	t.Run("duplicate_message", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		heartbeat := runner.Heartbeat(runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
		if _, err := boundary.Receive(t.Context(), runner.ConnectionID, heartbeat, now); err != nil {
			t.Fatal(err)
		}
		replayed, err := runner.ReplayLast()
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := boundary.Receive(t.Context(), runner.ConnectionID, replayed, now)
		if err != nil || outcome != OutcomeDuplicate {
			t.Fatalf("duplicate outcome, error = %q, %v", outcome, err)
		}
	})

	t.Run("duplicate_older_than_session_window", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		first := runner.Heartbeat(runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
		if _, err := boundary.Receive(t.Context(), runner.ConnectionID, first, now); err != nil {
			t.Fatal(err)
		}
		for range 260 {
			if _, err := boundary.Receive(
				t.Context(), runner.ConnectionID,
				runner.Heartbeat(runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE), now,
			); err != nil {
				t.Fatal(err)
			}
		}
		outcome, err := boundary.Receive(t.Context(), runner.ConnectionID, first, now)
		if err != nil || outcome != OutcomeDuplicate {
			t.Fatalf("old duplicate outcome, error = %q, %v", outcome, err)
		}
	})

	t.Run("reordered_result", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		fence := conformanceFence()
		if err := boundary.SeedAssignment(t.Context(), fence, now); err != nil {
			t.Fatal(err)
		}
		if _, err := boundary.Receive(
			t.Context(), runner.ConnectionID, runner.AssignmentReady(fence), now,
		); err != nil {
			t.Fatal(err)
		}
		reordered := runner.ReorderedAssignmentReady(fence, 1)
		if _, err := boundary.Receive(
			t.Context(), runner.ConnectionID, reordered, now,
		); !errors.Is(err, ErrReorderedMessage) {
			t.Fatalf("reordered result error = %v, want ErrReorderedMessage", err)
		}
	})

	t.Run("reconnect", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		oldConnectionID := runner.ConnectionID
		oldHeartbeat := runner.Heartbeat(runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
		runner.Reconnect("connection-2")
		connectAndRegister(t, boundary, runner, now.Add(time.Second))
		if _, err := boundary.Receive(
			t.Context(), oldConnectionID, oldHeartbeat, now.Add(time.Second),
		); !errors.Is(err, ErrInactiveConnection) {
			t.Fatalf("old connection error = %v, want ErrInactiveConnection", err)
		}
		snapshot := readSnapshot(t, boundary, runner.RunnerID)
		if snapshot.ActiveConnectionID != "connection-2" {
			t.Fatalf("active connection = %q, want connection-2", snapshot.ActiveConnectionID)
		}
	})

	t.Run("draining", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		if _, err := boundary.Receive(
			t.Context(), runner.ConnectionID,
			runner.Heartbeat(runnerv1.DrainPhase_DRAIN_PHASE_DRAINING), now,
		); err != nil {
			t.Fatal(err)
		}
		snapshot := readSnapshot(t, boundary, runner.RunnerID)
		if snapshot.RunnerState != "draining" || snapshot.DrainPhase != "draining" {
			t.Fatalf("draining snapshot = %#v", snapshot)
		}
	})

	t.Run("assignment_loss_requires_fence", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-1")
		connectAndRegister(t, boundary, runner, now)
		fence := conformanceFence()
		if err := boundary.SeedAssignment(t.Context(), fence, now); err != nil {
			t.Fatal(err)
		}
		if err := boundary.ExpireRunner(
			t.Context(), runner.RunnerID, now.Add(31*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		snapshot := readSnapshot(t, boundary, runner.RunnerID)
		if snapshot.AssignmentState != "uncertain" || snapshot.MayReassign {
			t.Fatalf("lost Assignment snapshot = %#v", snapshot)
		}
	})

	t.Run("version_rejection", func(t *testing.T) {
		boundary := factory(t, now)
		runner := NewFakeRunner("runner-conformance", "pool-conformance", "connection-version")
		response, err := boundary.Connect(
			t.Context(), runner.RunnerID, runner.ConnectionID,
			runner.Hello(runnerv1.SupportedProtocolMaximum+1, runnerv1.SupportedProtocolMaximum+2),
		)
		if err != nil {
			t.Fatal(err)
		}
		if response.GetRejection().GetKind() != runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_VERSION_UNSUPPORTED {
			t.Fatalf("version rejection = %#v", response.GetRejection())
		}
	})
}

func connectAndRegister(
	t *testing.T,
	boundary Boundary,
	runner *FakeRunner,
	now time.Time,
) {
	t.Helper()
	response, err := boundary.Connect(
		t.Context(), runner.RunnerID, runner.ConnectionID,
		runner.Hello(runnerv1.SupportedProtocolMinimum, runnerv1.SupportedProtocolMaximum),
	)
	if err != nil || response.GetWelcome() == nil {
		t.Fatalf("Welcome, error = %#v, %v", response, err)
	}
	outcome, err := boundary.Receive(
		t.Context(), runner.ConnectionID, runner.Registration(), now,
	)
	if err != nil || outcome != OutcomeRegistration {
		t.Fatalf("Registration outcome, error = %q, %v", outcome, err)
	}
}

func readSnapshot(t *testing.T, boundary Boundary, runnerID string) Snapshot {
	t.Helper()
	snapshot, err := boundary.Snapshot(t.Context(), runnerID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func conformanceFence() *runnerv1.AssignmentFence {
	suffix := strconv.FormatUint(conformanceFenceID.Add(1), 10)
	return &runnerv1.AssignmentFence{
		AssignmentId:      "assignment-conformance-" + suffix,
		SandboxId:         "sandbox-conformance-" + suffix,
		InstanceId:        "instance-conformance-" + suffix,
		SandboxGeneration: 1,
		FencingToken:      []byte("01234567890123456789012345678901"),
	}
}
