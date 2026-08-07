package firecracker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func terminalPathRunnerCertificate(t *testing.T, runnerID string) tls.Certificate {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := url.Parse("spiffe://secondbox/runner/" + runnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute),
		NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		URIs: []*url.URL{identity},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
}

func TestParseOOMKillCounterRequiresExactKernelKey(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{
			name:  "cgroup v2 memory events",
			input: "low 0\nhigh 0\nmax 3\noom 2\noom_kill 1\noom_group_kill 0\n",
			want:  1,
		},
		{
			name:  "cgroup v1 oom control",
			input: "oom_kill_disable 0\nunder_oom 0\noom_kill 7\n",
			want:  7,
		},
		{name: "near match", input: "not_oom_kill 8\n", wantErr: true},
		{name: "duplicate", input: "oom_kill 1\noom_kill 2\n", wantErr: true},
		{name: "malformed", input: "oom_kill yes\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseOOMKillCounter([]byte(test.input))
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseOOMKillCounter() = %d, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseOOMKillCounter() = %d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestFirecrackerSuccessfulGuestExitMarkerIsStructurallyExact(t *testing.T) {
	const marker = "2026-07-28T12:34:56.123456789 [fc-instance:main] Firecracker exited successfully\n"
	if !hasExactSuccessfulGuestExitMarker(strings.NewReader(marker)) {
		t.Fatal("pinned Firecracker 1.16.1 successful-exit marker was not recognized")
	}
	for _, nearMatch := range []string{
		"Firecracker exited successfully\n",
		"2026-07-28T12:34:56.123456789 [fc-instance:main] Firecracker exited successfully extra\n",
		"2026-07-28T12:34:56.123456789 [fc-instance:main] prefix Firecracker exited successfully\n",
		"2026-07-28T12:34:56.123456789 [fc-instance:main Firecracker exited successfully\n",
	} {
		if hasExactSuccessfulGuestExitMarker(strings.NewReader(nearMatch)) {
			t.Fatalf("near-match log line was accepted: %q", nearMatch)
		}
	}
}

func TestClassifyPostReadyTerminationUsesAuthoritativeEvidence(t *testing.T) {
	tests := []struct {
		name     string
		input    terminationEvidence
		want     observedTerminationReason
		wantEmit bool
	}{
		{
			name: "oom counter delta outranks successful marker",
			input: terminationEvidence{
				ready: true, baselineOOMKills: ptrUint64(2), observedOOMKills: ptrUint64(3),
				successfulGuestExit: true,
			},
			want: observedTerminationResourceExhaustion, wantEmit: true,
		},
		{
			name: "exact successful guest exit",
			input: terminationEvidence{
				ready: true, baselineOOMKills: ptrUint64(2), observedOOMKills: ptrUint64(2),
				successfulGuestExit: true,
			},
			want: observedTerminationGuestShutdown, wantEmit: true,
		},
		{
			name: "generic exit is internal failure",
			input: terminationEvidence{
				ready: true, baselineOOMKills: ptrUint64(2), observedOOMKills: ptrUint64(2),
			},
			want: observedTerminationInternalFailure, wantEmit: true,
		},
		{
			name: "missing oom baseline is internal failure",
			input: terminationEvidence{
				ready: true, observedOOMKills: ptrUint64(3), successfulGuestExit: true,
			},
			want: observedTerminationInternalFailure, wantEmit: true,
		},
		{
			name: "counter regression is internal failure",
			input: terminationEvidence{
				ready: true, baselineOOMKills: ptrUint64(3), observedOOMKills: ptrUint64(2),
			},
			want: observedTerminationInternalFailure, wantEmit: true,
		},
		{
			name: "evidence read failure is internal failure",
			input: terminationEvidence{
				ready: true, baselineOOMKills: ptrUint64(2), evidenceErr: errors.New("read failed"),
			},
			want: observedTerminationInternalFailure, wantEmit: true,
		},
		{
			name: "startup exit is not a post-ready event",
			input: terminationEvidence{
				baselineOOMKills: ptrUint64(2), observedOOMKills: ptrUint64(2),
			},
		},
		{
			name: "explicit stop is not a natural terminal event",
			input: terminationEvidence{
				ready: true, explicitStop: true,
				baselineOOMKills: ptrUint64(2), observedOOMKills: ptrUint64(3),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, emit := classifyPostReadyTermination(test.input)
			if got != test.want || emit != test.wantEmit {
				t.Fatalf("classification = %q, %t; want %q, %t", got, emit, test.want, test.wantEmit)
			}
		})
	}
}

func ptrUint64(value uint64) *uint64 {
	return &value
}

func TestNaturalReapFlowsFromReadyManagerThroughBackendToProtocolFrame(t *testing.T) {
	for _, test := range []struct {
		name       string
		log        string
		wantReason runnerprotocol.InstanceObservedTerminationReason
	}{
		{
			name:       "exact guest shutdown",
			log:        "2026-07-28T12:34:56.123456789 [fc-terminal:main] Firecracker exited successfully\n",
			wantReason: runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_GUEST_SHUTDOWN,
		},
		{
			name:       "ambiguous disappearance",
			log:        "2026-07-28T12:34:56.123456789 [fc-terminal:main] VMM process disappeared\n",
			wantReason: runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldRead := readTerminationEvidenceFile
			readTerminationEvidenceFile = func(string) ([]byte, error) {
				return []byte("oom_kill 0\n"), nil
			}
			t.Cleanup(func() { readTerminationEvidenceFile = oldRead })

			logPath := t.TempDir() + "/firecracker.log"
			if err := os.WriteFile(logPath, []byte(test.log), 0o600); err != nil {
				t.Fatal(err)
			}
			fence := terminalPathFence()
			inst := &instance{
				id: "fc-terminal", sandboxID: fence.SandboxId,
				sandboxGeneration: fence.SandboxGeneration,
				compartmentID:     fence.InstanceId, assignmentID: fence.AssignmentId,
				requestID: "request-terminal", operationID: "operation-terminal",
				leaseID: "lease-terminal", logPath: logPath, done: make(chan struct{}),
			}
			manager := &Manager{
				cfg: &config.Config{
					MicroVMJailerCgroupVersion: 2, MicroVMJailerParentCgroup: "secondbox",
					MicroVMMaxConcurrentGlobal: 1,
				},
				instances: map[string]*instance{inst.id: inst},
				guestIPs:  map[string]string{},
			}
			backend := &AssignmentBackend{
				manager: manager, assignments: map[string]activeRunnerAssignment{},
				instanceTerminals: make(chan runnercontrol.BackendInstanceTerminal, 1),
			}
			wrapper := &terminalPathBackend{AssignmentBackend: backend, inst: inst}
			stream := newTerminalPathStream()
			stream.inbound <- terminalPathWelcome()
			stream.inbound <- &runnerprotocol.ControlPlaneToRunner{
				Message: &runnerprotocol.ControlPlaneToRunner_Assignment{
					Assignment: terminalPathAssignment(fence),
				},
			}
			connector := &terminalPathConnector{stream: stream}
			service, err := runnercontrol.NewRunnerProtocolService(
				runnercontrol.RunnerProtocolConfig{
					RunnerID: "runner-terminal", RunnerPoolID: "pool-terminal",
					SoftwareVersion: "1.0.0", ProtocolMinimum: 1, ProtocolMaximum: 1,
					MaximumConcurrentStarts:           1,
					MaximumConcurrentWorkspaceCreates: 1,
					DataPlaneListenAddress:            "127.0.0.1:0",
					DataPlaneAdvertisedAddress:        "10.0.0.5:7443",
					DataPlaneCertificate:              terminalPathRunnerCertificate(t, "runner-terminal"),
					MandatoryFeatures: []runnerprotocol.RunnerFeature{
						runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
					},
				},
				wrapper,
				connector,
			)
			if err != nil {
				t.Fatal(err)
			}
			service.SetEvidenceSink(runnerevidence.SlogSink{})
			ctx, cancel := context.WithCancel(t.Context())
			runDone := make(chan error, 1)
			go func() { runDone <- service.Run(ctx) }()
			waitForTerminalPath(t, func() bool {
				manager.mu.Lock()
				defer manager.mu.Unlock()
				return inst.ready
			})

			manager.reap(inst)
			waitForTerminalPath(t, func() bool {
				for _, outbound := range stream.snapshot() {
					terminal := outbound.GetInstanceTerminal()
					if terminal != nil {
						return terminal.Reason == test.wantReason &&
							terminal.Fence.AssignmentId == fence.AssignmentId &&
							terminal.Correlation.RequestId == "request-terminal" &&
							terminal.TerminationEvidenceDigest != ""
					}
				}
				return false
			})
			cancel()
			if err := <-runDone; !errors.Is(err, context.Canceled) {
				t.Fatalf("RunnerProtocolService.Run() = %v, want context cancellation", err)
			}
		})
	}
}

func TestPreReadyAndExplicitStopReapEmitNoInstanceTerminal(t *testing.T) {
	oldRead := readTerminationEvidenceFile
	readTerminationEvidenceFile = func(string) ([]byte, error) {
		return []byte("oom_kill 0\n"), nil
	}
	t.Cleanup(func() { readTerminationEvidenceFile = oldRead })
	for _, explicit := range []bool{false, true} {
		name := "pre-ready"
		if explicit {
			name = "explicit-stop"
		}
		t.Run(name, func(t *testing.T) {
			fence := terminalPathFence()
			logPath := t.TempDir() + "/firecracker.log"
			if err := os.WriteFile(logPath, []byte(
				"2026-07-28T12:34:56.123456789 [fc-terminal:main] Firecracker exited successfully\n",
			), 0o600); err != nil {
				t.Fatal(err)
			}
			inst := &instance{
				id: "fc-terminal", sandboxID: fence.SandboxId,
				sandboxGeneration: fence.SandboxGeneration,
				compartmentID:     fence.InstanceId, assignmentID: fence.AssignmentId,
				requestID: "request-terminal", operationID: "operation-terminal",
				leaseID: "lease-terminal", logPath: logPath, done: make(chan struct{}),
			}
			manager := &Manager{
				cfg: &config.Config{
					MicroVMJailerCgroupVersion: 2, MicroVMJailerParentCgroup: "secondbox",
				},
				instances: map[string]*instance{inst.id: inst},
				guestIPs:  map[string]string{},
				runnerID:  "runner-terminal",
			}
			terminals := make(chan InstanceTerminalObservation, 1)
			if explicit {
				if err := manager.MarkAssignmentReady(
					inst.id,
					func(_ context.Context, observation InstanceTerminalObservation) error {
						terminals <- observation
						return nil
					},
				); err != nil {
					t.Fatal(err)
				}
				if err := manager.stopInstance(t.Context(), inst, false); err != nil {
					t.Fatal(err)
				}
			} else {
				manager.reap(inst)
			}
			select {
			case terminal := <-terminals:
				t.Fatalf("unexpected terminal observation: %+v", terminal)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

type terminalPathBackend struct {
	*AssignmentBackend
	inst *instance
}

func (*terminalPathBackend) Readiness(context.Context) (runnercontrol.BackendReadiness, error) {
	return runnercontrol.BackendReadiness{
		Capacity: &runnerprotocol.Capacity{},
		Reserved: &runnerprotocol.Capacity{},
		Capabilities: &runnerprotocol.RunnerCapabilities{
			Architecture: "amd64", FirecrackerVersion: "1.16.1",
			KvmReady: true, JailerReady: true, CgroupReady: true,
			NetworkPolicyReady: true, StorageReady: true, CleanupReady: true,
			GuestProtocolGenerations: &runnerprotocol.ProtocolVersionRange{Minimum: 1, Maximum: 1},
		},
	}, nil
}

func (*terminalPathBackend) ValidateAssignment(
	context.Context,
	*runnerprotocol.AssignmentCommand,
) error {
	return nil
}

func (backend *terminalPathBackend) StartAssignment(
	_ context.Context,
	assignment *runnerprotocol.AssignmentCommand,
	progress func(runnerprotocol.AssignmentProgressStage) error,
) (runnercontrol.BackendInstance, error) {
	if err := progress(
		runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY,
	); err != nil {
		return runnercontrol.BackendInstance{}, err
	}
	correlation := assignment.GetCorrelation()
	backend.mu.Lock()
	backend.assignments[assignment.Fence.AssignmentId] = activeRunnerAssignment{
		fence:            cloneAssignmentFence(assignment.Fence),
		backendReference: backend.inst.id,
		correlation: &runnerprotocol.Correlation{
			RequestId: correlation.RequestId, OperationId: correlation.OperationId,
			LeaseId: correlation.LeaseId, SandboxId: assignment.Fence.SandboxId,
			InstanceId:        assignment.Fence.InstanceId,
			SandboxGeneration: assignment.Fence.SandboxGeneration,
			AssignmentId:      assignment.Fence.AssignmentId, RunnerId: "runner-terminal",
		},
	}
	backend.mu.Unlock()
	return runnercontrol.BackendInstance{
		BackendKind: "firecracker", BackendReference: backend.inst.id,
	}, nil
}

type terminalPathConnector struct {
	stream *terminalPathStream
}

func (connector *terminalPathConnector) Connect(
	ctx context.Context,
) (runnercontrol.RunnerProtocolStream, error) {
	connector.stream.ctx = ctx
	return connector.stream, nil
}

func (*terminalPathConnector) Close() error { return nil }

type terminalPathStream struct {
	ctx      context.Context
	inbound  chan *runnerprotocol.ControlPlaneToRunner
	mu       sync.Mutex
	outbound []*runnerprotocol.RunnerToControlPlane
}

func newTerminalPathStream() *terminalPathStream {
	return &terminalPathStream{
		inbound: make(chan *runnerprotocol.ControlPlaneToRunner, 2),
	}
}

func (stream *terminalPathStream) Send(message *runnerprotocol.RunnerToControlPlane) error {
	stream.mu.Lock()
	stream.outbound = append(stream.outbound, message)
	stream.mu.Unlock()
	return nil
}

func (stream *terminalPathStream) Recv() (*runnerprotocol.ControlPlaneToRunner, error) {
	select {
	case message := <-stream.inbound:
		return message, nil
	case <-stream.ctx.Done():
		return nil, stream.ctx.Err()
	}
}

func (stream *terminalPathStream) snapshot() []*runnerprotocol.RunnerToControlPlane {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return append([]*runnerprotocol.RunnerToControlPlane(nil), stream.outbound...)
}

func terminalPathFence() *runnerprotocol.AssignmentFence {
	return &runnerprotocol.AssignmentFence{
		AssignmentId: "assignment-terminal", SandboxId: "sandbox-terminal",
		InstanceId: "instance-terminal", SandboxGeneration: 1,
		FencingToken: []byte("01234567890123456789012345678901"),
	}
}

func terminalPathAssignment(
	fence *runnerprotocol.AssignmentFence,
) *runnerprotocol.AssignmentCommand {
	return &runnerprotocol.AssignmentCommand{
		MessageId: "assignment-command", Sequence: 1, Fence: fence,
		ProfileRevisionId: "profile-terminal",
		Requirements: &runnerprotocol.ProfileRequirements{
			VcpuCount: 1, VcpuMillis: 1000, ProcessLimit: 128, MemoryBytes: 1 << 30, DiskBytes: 10 << 30,
			Architecture: "amd64", StartupMode: "cold_boot",
			MaximumOperationMs: 60_000, MaximumOutputBytes: 1 << 20,
		},
		Assets: []*runnerprotocol.SignedAssetReference{{
			ArtifactId: "runtime", Architecture: "amd64", GuestProtocolGeneration: 1,
			ManifestDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			SignatureKeyId: "release-key",
		}},
		DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Correlation: &runnerprotocol.Correlation{
			RequestId: "request-terminal", OperationId: "operation-terminal",
			LeaseId: "lease-terminal",
		},
		NetworkPolicy: &runnerprotocol.NetworkPolicy{
			Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL,
		},
	}
}

func terminalPathWelcome() *runnerprotocol.ControlPlaneToRunner {
	return &runnerprotocol.ControlPlaneToRunner{
		Message: &runnerprotocol.ControlPlaneToRunner_Welcome{
			Welcome: &runnerprotocol.RunnerWelcome{
				ConnectionId: "connection-terminal", SelectedVersion: 1,
				EnabledFeatures: []runnerprotocol.RunnerFeature{
					runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
				},
				HeartbeatIntervalMs: 60_000,
			},
		},
	}
}

func waitForTerminalPath(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Runner terminal path")
		}
		time.Sleep(time.Millisecond)
	}
}

var _ runnercontrol.AssignmentBackend = (*terminalPathBackend)(nil)
