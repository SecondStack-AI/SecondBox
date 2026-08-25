//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	runnerconformance "github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol/conformance"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
	"google.golang.org/protobuf/proto"
)

const backendQualificationWorkspaceBytes = int64(256 << 20)

type qualificationEvidenceSink struct {
	mu      sync.Mutex
	records []runnerevidence.Record
}

func (sink *qualificationEvidenceSink) Emit(_ context.Context, record runnerevidence.Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	sink.mu.Lock()
	sink.records = append(sink.records, record)
	sink.mu.Unlock()
	return nil
}

// qualificationBuild resolves the SECONDBOX_GVISOR_BUILD root: bin/runsc,
// bin/secondbox-guest-agent, and rootfs/. The suite skips without it and is
// driven by the test-gvisor recipe, which fails clearly when prerequisites
// are absent.
func qualificationBuild(t *testing.T) (runsc, agent, rootfs string) {
	t.Helper()
	buildRoot := os.Getenv("SECONDBOX_GVISOR_BUILD")
	if buildRoot == "" {
		t.Skip("SECONDBOX_GVISOR_BUILD is not set")
	}
	if os.Geteuid() != 0 {
		t.Fatal("SECONDBOX_GVISOR_BUILD is set but the suite is not running as root")
	}
	absolute, err := filepath.Abs(buildRoot)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(absolute, "bin", "runsc"),
		filepath.Join(absolute, "bin", "secondbox-guest-agent"),
		filepath.Join(absolute, "rootfs")
}

type qualificationFixture struct {
	backend  *AssignmentBackend
	fence    *runnerprotocol.AssignmentFence
	command  *runnerprotocol.AssignmentCommand
	evidence *qualificationEvidenceSink
}

func newQualificationFixture(t *testing.T, suffix string) qualificationFixture {
	return newQualificationFixtureWithDNS(t, suffix, "")
}

func newQualificationFixtureWithDNS(t *testing.T, suffix, dnsUpstream string) qualificationFixture {
	t.Helper()
	runsc, agent, rootfs := qualificationBuild(t)

	qualificationRoot := t.TempDir()
	if parent := os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM"); parent != "" {
		var err error
		qualificationRoot, err = os.MkdirTemp(parent, ".secondbox-gvisor-qualification-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(qualificationRoot) })
	}
	store, err := workspacestore.New(t.Context(), workspacestore.Config{
		Root:                  filepath.Join(qualificationRoot, "store"),
		TemplateCapacityBytes: backendQualificationWorkspaceBytes,
		FormatterKind:         workspacestore.FormatterMke2fs,
	})
	if err != nil {
		t.Fatalf("initialize qualified WorkspaceStore: %v", err)
	}

	manifest := materialization.Manifest{
		SchemaVersion: materialization.SchemaVersion,
		Key: materialization.Key{
			BackendKind: materialization.BackendGVisor, GuestArchitecture: runtime.GOARCH,
			RuntimeManifestDigest:   qualificationDigestText("gvisor-qualification-runtime"),
			ToolchainManifestDigest: qualificationDigestText("gvisor-qualification-toolchain"),
		},
		SourceOCIManifestDigest: qualificationDigestText("alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"),
		FlatRootDigest:          qualificationDigestFlatRoot(t, rootfs),
		LaunchArtifacts: []materialization.LaunchArtifact{
			{ID: agentArtifactID, SHA256: qualificationDigestFile(t, agent)},
			{ID: runscArtifactID, SHA256: qualificationDigestFile(t, runsc)},
		},
		AgentProtocolGeneration: 1,
		AgentFeatures:           []string{"exec-streaming", "file-streaming", "port-proxy", "pty"},
		BackendBuildID:          "secondbox-gvisor-qualification",
		HelperBuildID:           "runsc-release-20260817.0",
	}
	manifestDigest, err := manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "materialization.json")
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	selfExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Production runtime directories are short operator paths (/run/...);
	// sun_path bounds make a deep test temp directory unusable here.
	runtimeDir, err := os.MkdirTemp("/run", "sbxgv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	backend, err := NewAssignmentBackend(Config{
		RunscPath: runsc, AgentPath: agent, FlatRootPath: rootfs,
		MaterializationPath: manifestPath, MaterializationDigest: manifestDigest,
		RuntimeDir: runtimeDir, WorkspaceRoot: filepath.Join(qualificationRoot, "store"),
		SelfExecutable: selfExecutable, DNSUpstream: dnsUpstream,
		MaximumVCPUs: 2, MaximumMemoryBytes: 1 << 30,
		MaximumDiskBytes: uint64(backendQualificationWorkspaceBytes),
		MaximumInstances: 1, MaximumOperations: 8, WorkspaceStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := &qualificationEvidenceSink{}
	backend.SetRunnerEvidenceSink(evidence, "gvisor-qualification-runner")

	workspaceID := "gvisor-workspace-" + suffix
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), &runnerprotocol.LocalWorkspaceCommand{
		Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		OperationId: "gvisor-create-" + suffix, WorkspaceId: workspaceID,
		FencingToken:         []byte("gvisor-workspace-fence-000000000"),
		LogicalCapacityBytes: uint64(backendQualificationWorkspaceBytes),
	}); err != nil {
		t.Fatalf("create qualified Workspace through protocol adapter: %v", err)
	}
	if _, err := backend.Readiness(t.Context()); err != nil {
		t.Fatalf("qualified readiness: %v", err)
	}
	fence := &runnerprotocol.AssignmentFence{
		AssignmentId: "gvisor-assignment-" + suffix, SandboxId: "gvisor-sandbox-" + suffix,
		InstanceId: "gvisor-instance-" + suffix, SandboxGeneration: 1,
		FencingToken: []byte("gvisor-assignment-fence-00000000"),
	}
	command := &runnerprotocol.AssignmentCommand{
		MessageId: "gvisor-message-" + suffix, Sequence: 1, Fence: fence,
		ProfileRevisionId: "gvisor-profile", WorkspaceId: workspaceID,
		Requirements: &runnerprotocol.ProfileRequirements{
			VcpuCount: 1, MemoryBytes: 256 << 20, DiskBytes: uint64(backendQualificationWorkspaceBytes),
			Architecture: runtime.GOARCH, StartupMode: "cold_boot",
			RequiredCapabilities: []string{"cleanup", "gvisor", "local-workspace", "storage"},
			MaximumOperationMs:   60_000, MaximumOutputBytes: 8 << 20,
		},
		Assets: []*runnerprotocol.AssetReference{
			{ArtifactId: "runtime", ManifestDigest: manifest.Key.RuntimeManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 1},
			{ArtifactId: "toolchain", ManifestDigest: manifest.Key.ToolchainManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 1},
		},
		DeadlineUnixMs: uint64(time.Now().Add(3 * time.Minute).UnixMilli()),
		Correlation:    &runnerprotocol.Correlation{RequestId: "gvisor-request", OperationId: "gvisor-operation", LeaseId: "gvisor-lease"},
		NetworkPolicy:  &runnerprotocol.NetworkPolicy{Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL},
	}
	return qualificationFixture{backend: backend, fence: fence, command: command, evidence: evidence}
}

// TestQualifiedGVisorBackendBootsAgentAndWorkspace proves the complete
// vertical slice on a real host: sandbox boot with the loop-mounted
// Workspace, the negotiated agent session, the complete data plane through
// the shared conformance suite, allow-list rejection, and fencing.
func TestQualifiedGVisorBackendBootsAgentAndWorkspace(t *testing.T) {
	fixture := newQualificationFixture(t, "primary")
	backend, fence := fixture.backend, fixture.fence
	t.Cleanup(func() { _ = backend.Shutdown(context.Background()) })

	// An exact allow-list compiles and validates; a rule with no exact
	// representation (an unsupported protocol) stays rejected.
	allowList := proto.Clone(fixture.command).(*runnerprotocol.AssignmentCommand)
	allowList.NetworkPolicy = &runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{{
			Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTPS, Port: 443,
			Target: &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"},
		}},
	}
	if err := backend.ValidateAssignment(t.Context(), allowList); err != nil {
		t.Fatalf("exact allow-list was rejected: %v", err)
	}
	malformed := proto.Clone(allowList).(*runnerprotocol.AssignmentCommand)
	malformed.NetworkPolicy.Destinations[0].Protocol = runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_UNSPECIFIED
	if err := backend.ValidateAssignment(t.Context(), malformed); err == nil {
		t.Fatal("policy without an exact representation was accepted")
	}

	instance, err := backend.StartAssignment(t.Context(), fixture.command,
		func(runnerprotocol.AssignmentProgressStage) error { return nil })
	if err != nil {
		t.Fatalf("boot qualified gVisor Instance: %v", err)
	}
	if instance.BackendKind != "gvisor" || instance.BackendReference == "" {
		t.Fatalf("unexpected qualified backend Instance: %#v", instance)
	}
	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatal(err)
	}

	execResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{
			Argument: []string{"/bin/sh", "-c", "printf qualified-stdout; printf qualified-stderr >&2; exit 7"},
		}},
		Cwd: ".", OutputLimitBytes: 1024,
	})
	if err != nil || !bytes.Equal(execResult.Stdout, []byte("qualified-stdout")) ||
		!bytes.Equal(execResult.Stderr, []byte("qualified-stderr")) || execResult.Terminal.GetExitCode() != 7 {
		t.Fatalf("qualified buffered Exec = %#v, %v", execResult, err)
	}
	writeResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "data.bin",
		ExpectedSize:     5,
		ExpectedChecksum: "sha256:103597c5abb6113da596c18e9d1da69364eafe00a2bfaa8b12e53c44bd6b0429",
	}, []byte{0, 1, 2, 254, 255})
	if err != nil || writeResult.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("qualified File write = %#v, %v", writeResult, err)
	}
	readResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: "data.bin", ExpectedSize: 5,
	}, nil)
	if err != nil || !bytes.Equal(readResult.Content, []byte{0, 1, 2, 254, 255}) {
		t.Fatalf("qualified binary File read = %#v, %v", readResult, err)
	}

	runnerconformance.RunDataPlane(t, runnerconformance.DataPlaneFixture{
		Backend: backend, PTY: backend, Port: backend, Fence: fence,
	})

	staleFence := &runnerprotocol.AssignmentFence{
		AssignmentId: fence.AssignmentId, SandboxId: fence.SandboxId,
		InstanceId: fence.InstanceId, SandboxGeneration: fence.SandboxGeneration + 1,
		FencingToken: fence.FencingToken,
	}
	if _, err := backend.ExecuteBuffered(t.Context(), staleFence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/true"}}},
		OutputLimitBytes: 1024,
	}); err == nil {
		t.Fatal("stale generation reached the guest")
	}

	fenceEvidence, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{
		Fence: fence, DeadlineUnixMs: uint64(time.Now().Add(15 * time.Second).UnixMilli()),
	})
	if err != nil || fenceEvidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED {
		t.Fatalf("qualified fence = %#v, %v", fenceEvidence, err)
	}
	if _, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/true"}}},
		OutputLimitBytes: 1024,
	}); err == nil {
		t.Fatal("fenced assignment still accepted operations")
	}
	repeat, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{
		Fence: fence, DeadlineUnixMs: uint64(time.Now().Add(5 * time.Second).UnixMilli()),
	})
	if err != nil || repeat.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_ALREADY_STOPPED {
		t.Fatalf("repeated fence = %#v, %v", repeat, err)
	}
	if count, _ := backend.StartupTiming(); count != 1 {
		t.Fatalf("startup samples = %d, want 1", count)
	}
}

// TestQualifiedGVisorBackendConformance runs the shared assignment-backend
// contract suite against the real backend with a real runsc.
func TestQualifiedGVisorBackendConformance(t *testing.T) {
	qualificationBuild(t)
	sequence := 0
	runnerconformance.Run(t, func(t *testing.T) runnerconformance.Fixture {
		sequence++
		fixture := newQualificationFixture(t, fmt.Sprintf("conformance-%d", sequence))
		t.Cleanup(func() { _ = fixture.backend.Shutdown(context.Background()) })
		return runnerconformance.Fixture{
			Backend:    fixture.backend,
			Assignment: fixture.command,
			AdvanceWorkspace: func(ctx context.Context, workspaceID string, expected, next uint64) error {
				_, err := fixture.backend.ExecuteLocalWorkspace(ctx, &runnerprotocol.LocalWorkspaceCommand{
					Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ADVANCE_GENERATION,
					OperationId: fmt.Sprintf("gvisor-advance-%d-%d", sequence, next), WorkspaceId: workspaceID,
					FencingToken:       []byte("gvisor-workspace-fence-000000000"),
					ExpectedGeneration: expected, NextGeneration: next,
				})
				return err
			},
		}
	})
}

// TestQualifiedGVisorSupervisorLossDeliversOneTerminal proves an unexpected
// post-ready compute exit becomes exactly one provider-neutral terminal.
func TestQualifiedGVisorSupervisorLossDeliversOneTerminal(t *testing.T) {
	fixture := newQualificationFixture(t, "terminal")
	backend, fence := fixture.backend, fixture.fence
	t.Cleanup(func() { _ = backend.Shutdown(context.Background()) })
	if _, err := backend.StartAssignment(t.Context(), fixture.command,
		func(runnerprotocol.AssignmentProgressStage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	active := backend.assignments[fence.AssignmentId]
	backend.mu.Unlock()
	if err := syscallKillGroup(active.handles.Command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-backend.InstanceTerminals():
		if terminal.Reason != runnerprotocol.InstanceObservedTerminationReason_INSTANCE_OBSERVED_TERMINATION_REASON_INTERNAL_FAILURE ||
			!sameFence(terminal.Fence, fence) || terminal.EvidenceDigest == "" {
			t.Fatalf("unexpected instance terminal: %#v", terminal)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no instance terminal after supervisor loss")
	}
	select {
	case terminal := <-backend.InstanceTerminals():
		t.Fatalf("second instance terminal delivered: %#v", terminal)
	case <-time.After(500 * time.Millisecond):
	}
}

func qualificationDigestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func qualificationDigestFile(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func qualificationDigestFlatRoot(t *testing.T, path string) string {
	t.Helper()
	digest, err := materialization.DigestFlatRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
