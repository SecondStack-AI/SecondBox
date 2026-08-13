//go:build linux

package microsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
	"google.golang.org/protobuf/proto"
)

const qualificationWorkspaceBytes = int64(64 << 20)

func TestQualifiedLinuxBackendBootsAgentAndWorkspaceOnKVM(t *testing.T) {
	buildRoot := os.Getenv("SECONDBOX_MICROSANDBOX_LINUX_BUILD")
	if buildRoot == "" {
		t.Skip("SECONDBOX_MICROSANDBOX_LINUX_BUILD is not set")
	}
	buildRoot, err := filepath.Abs(buildRoot)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(buildRoot, "cargo-target", "debug", "secondbox-microsandbox-helper")
	agentd := filepath.Join(buildRoot, "runtime", "bin", "agentd")
	firmware := filepath.Join(buildRoot, "runtime", "lib", "libkrunfw.so.5.6.1")
	rootfs := filepath.Join(buildRoot, "rootfs")

	qualificationRoot := t.TempDir()
	if parent := os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM"); parent != "" {
		qualificationRoot, err = os.MkdirTemp(parent, ".secondbox-microsandbox-qualification-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(qualificationRoot) })
	}
	storeRoot := filepath.Join(qualificationRoot, "store")
	store, err := workspacestore.New(t.Context(), workspacestore.Config{
		Root:                         storeRoot,
		TemplateCapacityBytes:        qualificationWorkspaceBytes,
		MicrosandboxHelperExecutable: helper,
	})
	if err != nil {
		t.Fatalf("initialize qualified WorkspaceStore: %v", err)
	}
	manifest := materialization.Manifest{
		SchemaVersion: materialization.SchemaVersion,
		Key: materialization.Key{
			BackendKind: materialization.BackendMicrosandbox, GuestArchitecture: "amd64",
			RuntimeManifestDigest:   digestText("qualification-runtime"),
			ToolchainManifestDigest: digestText("qualification-toolchain"),
		},
		SourceOCIManifestDigest: digestText("alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"),
		FlatRootDigest:          digestText(rootfs),
		LaunchArtifacts: []materialization.LaunchArtifact{
			{ID: "agentd", SHA256: digestFile(t, agentd)},
			{ID: "helper", SHA256: digestFile(t, helper)},
			{ID: "libkrunfw", SHA256: digestFile(t, firmware)},
		},
		AgentProtocolGeneration: 6,
		AgentFeatures:           []string{"exec-streaming", "file-streaming", "pty", "tcp"},
		BackendBuildID:          "microsandbox-0.6.8-msb_krun-0.1.30",
		HelperBuildID:           "secondbox-microsandbox-helper-0.1.0",
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
	backend, err := NewAssignmentBackend(Config{
		HelperExecutable: helper, LibkrunfwPath: firmware, AgentdPath: agentd,
		FlatRootPath: rootfs, MaterializationPath: manifestPath,
		MaterializationDigest: manifestDigest, MaximumVCPUs: 2,
		MaximumMemoryBytes: 512 << 20, MaximumDiskBytes: uint64(qualificationWorkspaceBytes),
		MaximumInstances: 1, MaximumOperations: 8, WorkspaceStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ExecuteLocalWorkspace(t.Context(), &runnerprotocol.LocalWorkspaceCommand{
		Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		OperationId: "qualification-create", WorkspaceId: "qualification-workspace",
		FencingToken:         []byte("qualification-workspace-fence-0000"),
		LogicalCapacityBytes: uint64(qualificationWorkspaceBytes),
	}); err != nil {
		t.Fatalf("create qualified Workspace through protocol adapter: %v", err)
	}
	if _, err := backend.Readiness(t.Context()); err != nil {
		t.Fatalf("qualified readiness: %v", err)
	}
	fence := &runnerprotocol.AssignmentFence{
		AssignmentId: "qualification-assignment", SandboxId: "qualification-sandbox",
		InstanceId: "qualification-instance", SandboxGeneration: 1,
		FencingToken: []byte("qualification-assignment-fence-00"),
	}
	assignment := &runnerprotocol.AssignmentCommand{
		MessageId: "qualification-message", Sequence: 1, Fence: fence,
		ProfileRevisionId: "qualification-profile", WorkspaceId: "qualification-workspace",
		Requirements: &runnerprotocol.ProfileRequirements{
			VcpuCount: 1, MemoryBytes: 256 << 20, DiskBytes: uint64(qualificationWorkspaceBytes),
			Architecture: "amd64", StartupMode: "cold_boot",
			RequiredCapabilities: []string{"cleanup", "kvm", "local-workspace", "microsandbox", "storage"},
			MaximumOperationMs:   60_000, MaximumOutputBytes: 8 << 20,
		},
		Assets: []*runnerprotocol.AssetReference{
			{ArtifactId: "runtime", ManifestDigest: manifest.Key.RuntimeManifestDigest, Architecture: "amd64", GuestProtocolGeneration: 6},
			{ArtifactId: "toolchain", ManifestDigest: manifest.Key.ToolchainManifestDigest, Architecture: "amd64", GuestProtocolGeneration: 6},
		},
		DeadlineUnixMs: uint64(time.Now().Add(3 * time.Minute).UnixMilli()),
		Correlation:    &runnerprotocol.Correlation{RequestId: "qualification-request"},
		NetworkPolicy:  &runnerprotocol.NetworkPolicy{Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL},
	}
	instance, err := backend.StartAssignment(t.Context(), assignment, func(runnerprotocol.AssignmentProgressStage) error { return nil })
	if err != nil {
		t.Fatalf("boot qualified Microsandbox Instance: %v", err)
	}
	if instance.BackendKind != "microsandbox" || instance.BackendReference == "" {
		t.Fatalf("unexpected qualified backend Instance: %#v", instance)
	}
	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatal(err)
	}
	execResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf linux-stdout; printf linux-stderr >&2; exit 7"}}},
		Cwd:     "/workspace", OutputLimitBytes: 1024,
	})
	if err != nil || !bytes.Equal(execResult.Stdout, []byte("linux-stdout")) || !bytes.Equal(execResult.Stderr, []byte("linux-stderr")) || execResult.Terminal.GetExitCode() != 7 {
		t.Fatalf("qualified buffered Exec = %#v, %v", execResult, err)
	}
	mkdirResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_MKDIR, WorkspaceRelativePath: "qualification",
	}, nil)
	if err != nil || mkdirResult.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("qualified File mkdir = %#v, %v", mkdirResult, err)
	}
	writeResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "qualification/data.bin", ExpectedSize: 5,
	}, []byte{0, 1, 2, 254, 255})
	if err != nil || writeResult.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("qualified File write = %#v, %v", writeResult, err)
	}
	readResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: "qualification/data.bin",
	}, nil)
	if err != nil || !bytes.Equal(readResult.Content, []byte{0, 1, 2, 254, 255}) {
		t.Fatalf("qualified binary File read = %#v, %v", readResult, err)
	}
	var streamed bytes.Buffer
	streamTerminal, err := backend.ExecuteStreaming(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf streamed"}}},
		Cwd:     "/workspace", Streaming: true, OutputLimitBytes: 1024,
	}, make(chan runnercontrol.ExecControl), func(_ runnerprotocol.ExecOutputChannel, data []byte) error {
		_, err := streamed.Write(data)
		return err
	})
	if err != nil || streamed.String() != "streamed" || streamTerminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("qualified streaming Exec = %q %#v, %v", streamed.String(), streamTerminal, err)
	}
	overCapacity := proto.Clone(assignment).(*runnerprotocol.AssignmentCommand)
	overCapacity.Fence = &runnerprotocol.AssignmentFence{
		AssignmentId: "qualification-capacity-assignment", SandboxId: "qualification-capacity-sandbox",
		InstanceId: "qualification-capacity-instance", SandboxGeneration: 1,
		FencingToken: []byte("qualification-capacity-fence-000"),
	}
	overCapacity.WorkspaceId = "qualification-capacity-workspace"
	if _, err := backend.StartAssignment(t.Context(), overCapacity, func(runnerprotocol.AssignmentProgressStage) error { return nil }); err == nil {
		t.Fatal("second concurrent Instance exceeded exact local capacity")
	}
	shutdownContext, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	evidence, err := backend.FenceAssignment(shutdownContext, &runnerprotocol.FenceCommand{
		Fence: fence, DeadlineUnixMs: uint64(time.Now().Add(20 * time.Second).UnixMilli()),
	})
	if err != nil {
		t.Fatalf("fence qualified Microsandbox Instance: %v", err)
	}
	if evidence.Result != runnerprotocol.FenceResultKind_FENCE_RESULT_KIND_STOPPED || evidence.TerminationEvidenceDigest == "" {
		t.Fatalf("unexpected qualified fence evidence: %#v", evidence)
	}

	if _, err := backend.ExecuteLocalWorkspace(t.Context(), &runnerprotocol.LocalWorkspaceCommand{
		Kind:        runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_CREATE,
		OperationId: "qualification-terminal-create", WorkspaceId: "qualification-terminal-workspace",
		FencingToken:         []byte("qualification-terminal-store-fence"),
		LogicalCapacityBytes: uint64(qualificationWorkspaceBytes),
	}); err != nil {
		t.Fatalf("create terminal-event Workspace: %v", err)
	}
	terminalAssignment := proto.Clone(assignment).(*runnerprotocol.AssignmentCommand)
	terminalAssignment.WorkspaceId = "qualification-terminal-workspace"
	terminalAssignment.Fence = &runnerprotocol.AssignmentFence{
		AssignmentId: "qualification-terminal-assignment", SandboxId: "qualification-terminal-sandbox",
		InstanceId: "qualification-terminal-instance", SandboxGeneration: 1,
		FencingToken: []byte("qualification-terminal-fence-00"),
	}
	if _, err := backend.StartAssignment(t.Context(), terminalAssignment, func(runnerprotocol.AssignmentProgressStage) error { return nil }); err != nil {
		t.Fatalf("start terminal-event Instance: %v", err)
	}
	if err := backend.MarkAssignmentReady(terminalAssignment.Fence); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	terminalProcess := backend.assignments[terminalAssignment.Fence.AssignmentId].process
	backend.mu.Unlock()
	if err := terminalProcess.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-backend.InstanceTerminals():
		if !sameFence(terminal.Fence, terminalAssignment.Fence) || terminal.EvidenceDigest == "" {
			t.Fatalf("unexpected helper-exit terminal: %#v", terminal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unexpected helper exit did not publish a terminal")
	}
	select {
	case duplicate := <-backend.InstanceTerminals():
		t.Fatalf("unexpected duplicate helper-exit terminal: %#v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
	terminalFenceContext, terminalFenceCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer terminalFenceCancel()
	if _, err := backend.FenceAssignment(terminalFenceContext, &runnerprotocol.FenceCommand{
		Fence:          terminalAssignment.Fence,
		DeadlineUnixMs: uint64(time.Now().Add(4 * time.Second).UnixMilli()),
	}); err != nil {
		t.Fatalf("release unexpectedly exited Instance: %v", err)
	}
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
