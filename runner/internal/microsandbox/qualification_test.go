package microsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerconformance "github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol/conformance"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
	"google.golang.org/protobuf/proto"
)

const qualificationWorkspaceBytes = int64(64 << 20)

type qualificationEvidenceSink struct {
	mu      sync.Mutex
	records []runnerevidence.Record
}

func (sink *qualificationEvidenceSink) Emit(_ context.Context, record runnerevidence.Record) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.records = append(sink.records, record)
	return nil
}

func (sink *qualificationEvidenceSink) snapshot() []runnerevidence.Record {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]runnerevidence.Record(nil), sink.records...)
}

func TestQualifiedBackendBootsAgentAndWorkspace(t *testing.T) {
	buildEnvironment := ""
	firmwareName := ""
	switch runtime.GOOS {
	case "linux":
		buildEnvironment = "SECONDBOX_MICROSANDBOX_LINUX_BUILD"
		firmwareName = "libkrunfw.so.5.6.1"
	case "darwin":
		buildEnvironment = "SECONDBOX_MICROSANDBOX_MACOS_BUILD"
		firmwareName = "libkrunfw.5.dylib"
	default:
		t.Skip("Microsandbox qualification supports Linux and Darwin")
	}
	buildRoot := os.Getenv(buildEnvironment)
	if buildRoot == "" {
		t.Skipf("%s is not set", buildEnvironment)
	}
	buildRoot, err := filepath.Abs(buildRoot)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(buildRoot, "runtime", "bin", "secondbox-microsandbox-helper")
	agentd := filepath.Join(buildRoot, "runtime", "bin", "agentd")
	firmware := filepath.Join(buildRoot, "runtime", "lib", firmwareName)
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
		FormatterKind:                workspacestore.FormatterMicrosandboxHelper,
		MicrosandboxHelperExecutable: helper,
	})
	if err != nil {
		t.Fatalf("initialize qualified WorkspaceStore: %v", err)
	}
	manifest := materialization.Manifest{
		SchemaVersion: materialization.SchemaVersion,
		Key: materialization.Key{
			BackendKind: materialization.BackendMicrosandbox, GuestArchitecture: runtime.GOARCH,
			RuntimeManifestDigest:   digestText("qualification-runtime"),
			ToolchainManifestDigest: digestText("qualification-toolchain"),
		},
		SourceOCIManifestDigest: digestText("alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"),
		FlatRootDigest:          digestFlatRoot(t, rootfs),
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
	evidenceSink := &qualificationEvidenceSink{}
	backend.SetRunnerEvidenceSink(evidenceSink, "qualification-runner")
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
			Architecture: runtime.GOARCH, StartupMode: "cold_boot",
			RequiredCapabilities: []string{"cleanup", "kvm", "local-workspace", "microsandbox", "storage"},
			MaximumOperationMs:   60_000, MaximumOutputBytes: 8 << 20,
		},
		Assets: []*runnerprotocol.AssetReference{
			{ArtifactId: "runtime", ManifestDigest: manifest.Key.RuntimeManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 6},
			{ArtifactId: "toolchain", ManifestDigest: manifest.Key.ToolchainManifestDigest, Architecture: runtime.GOARCH, GuestProtocolGeneration: 6},
		},
		DeadlineUnixMs: uint64(time.Now().Add(3 * time.Minute).UnixMilli()),
		Correlation:    &runnerprotocol.Correlation{RequestId: "qualification-request", OperationId: "qualification-operation", LeaseId: "qualification-lease"},
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
	freshMissing, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:        &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/definitely/missing"}}},
		DeadlineUnixMs: uint64(time.Now().Add(2 * time.Second).UnixMilli()), OutputLimitBytes: 1024,
	})
	if err != nil || freshMissing.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_SPAWN_FAILED {
		t.Fatalf("qualified fresh spawn failure = %#v, %v", freshMissing, err)
	}
	execResult, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf qualified-stdout; printf qualified-stderr >&2; exit 7"}}},
		Cwd:     "/workspace", OutputLimitBytes: 1024,
	})
	if err != nil || !bytes.Equal(execResult.Stdout, []byte("qualified-stdout")) || !bytes.Equal(execResult.Stderr, []byte("qualified-stderr")) || execResult.Terminal.GetExitCode() != 7 {
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
		ExpectedChecksum: "sha256:103597c5abb6113da596c18e9d1da69364eafe00a2bfaa8b12e53c44bd6b0429",
	}, []byte{0, 1, 2, 254, 255})
	if err != nil || writeResult.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("qualified File write = %#v, %v", writeResult, err)
	}
	readResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_READ, WorkspaceRelativePath: "qualification/data.bin", ExpectedSize: 5,
	}, nil)
	if err != nil || !bytes.Equal(readResult.Content, []byte{0, 1, 2, 254, 255}) {
		t.Fatalf("qualified binary File read = %#v, %v", readResult, err)
	}
	if readResult.Metadata.GetChecksum() != "sha256:103597c5abb6113da596c18e9d1da69364eafe00a2bfaa8b12e53c44bd6b0429" {
		t.Fatalf("qualified File checksum = %#v", readResult.Metadata)
	}
	statResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_STAT, WorkspaceRelativePath: "qualification/data.bin",
	}, nil)
	if err != nil || statResult.Metadata.GetSize() != 5 || statResult.Metadata.GetKind() != runnerprotocol.FileKind_FILE_KIND_FILE {
		t.Fatalf("qualified File stat = %#v, %v", statResult, err)
	}
	listResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_LIST, WorkspaceRelativePath: "qualification",
	}, nil)
	if err != nil || len(listResult.Metadata.GetDirectChildren()) != 1 || listResult.Metadata.GetDirectChildren()[0] != "data.bin" || listResult.Metadata.GetDirectChildEntries()[0].GetPath() != "qualification/data.bin" {
		t.Fatalf("qualified File list = %#v, %v", listResult, err)
	}
	existsResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_EXISTS, WorkspaceRelativePath: "qualification/data.bin",
	}, nil)
	if err != nil || !existsResult.Metadata.GetExists() {
		t.Fatalf("qualified File exists = %#v, %v", existsResult, err)
	}
	missingResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_EXISTS, WorkspaceRelativePath: "qualification/missing",
	}, nil)
	if err != nil || missingResult.Metadata.GetExists() || missingResult.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("qualified missing File exists = %#v, %v", missingResult, err)
	}
	recursiveMkdir, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_MKDIR, WorkspaceRelativePath: "qualification/nested/child", Recursive: true,
	}, nil)
	if err != nil || recursiveMkdir.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("qualified recursive File mkdir = %#v, %v", recursiveMkdir, err)
	}
	var streamed bytes.Buffer
	streamControls := make(chan runnercontrol.ExecControl, 1)
	streamControls <- runnercontrol.ExecControl{Credit: 1024}
	close(streamControls)
	streamTerminal, err := backend.ExecuteStreaming(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf streamed"}}},
		Cwd:     "/workspace", Streaming: true, OutputLimitBytes: 1024,
	}, streamControls, func(_ runnerprotocol.ExecOutputChannel, data []byte) error {
		_, err := streamed.Write(data)
		return err
	})
	if err != nil || streamed.String() != "streamed" || streamTerminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("qualified streaming Exec = %q %#v, %v", streamed.String(), streamTerminal, err)
	}
	stdinControls := make(chan runnercontrol.ExecControl, 3)
	stdinControls <- runnercontrol.ExecControl{Input: &runnerprotocol.ExecInput{Data: []byte("interactive-stdin")}}
	stdinControls <- runnercontrol.ExecControl{Input: &runnerprotocol.ExecInput{EndOfInput: true}}
	stdinControls <- runnercontrol.ExecControl{Credit: 1024}
	close(stdinControls)
	streamed.Reset()
	stdinTerminal, err := backend.ExecuteStreaming(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/cat"}}},
		Cwd:     "/workspace", Streaming: true, OutputLimitBytes: 1024,
	}, stdinControls, func(_ runnerprotocol.ExecOutputChannel, data []byte) error {
		_, err := streamed.Write(data)
		return err
	})
	if err != nil || streamed.String() != "interactive-stdin" || stdinTerminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("qualified streaming stdin = %q %#v, %v", streamed.String(), stdinTerminal, err)
	}
	creditControls := make(chan runnercontrol.ExecControl, 3)
	creditControls <- runnercontrol.ExecControl{Input: &runnerprotocol.ExecInput{EndOfInput: true}}
	creditOutput := make(chan []byte, 1)
	creditResult := make(chan error, 1)
	go func() {
		terminal, executeErr := backend.ExecuteStreaming(t.Context(), fence, &runnerprotocol.ExecOpen{
			Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf credit-gated"}}},
			Cwd:     "/workspace", Streaming: true, OutputLimitBytes: 1024,
		}, creditControls, func(_ runnerprotocol.ExecOutputChannel, data []byte) error {
			creditOutput <- bytes.Clone(data)
			return nil
		})
		if executeErr == nil && terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
			executeErr = fmt.Errorf("unexpected credit terminal: %s", terminal.GetKind())
		}
		creditResult <- executeErr
	}()
	select {
	case output := <-creditOutput:
		t.Fatalf("qualified streaming output bypassed credit: %q", output)
	case <-time.After(100 * time.Millisecond):
	}
	creditControls <- runnercontrol.ExecControl{Credit: 1024}
	close(creditControls)
	if output := <-creditOutput; string(output) != "credit-gated" {
		t.Fatalf("qualified credit-gated output = %q", output)
	}
	if err := <-creditResult; err != nil {
		t.Fatal(err)
	}
	exhausted, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf 0123456789"}}},
		Cwd:     "/workspace", OutputLimitBytes: 5,
	})
	if err != nil || string(exhausted.Stdout) != "01234" || exhausted.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
		t.Fatalf("qualified output exhaustion = %#v, %v", exhausted, err)
	}
	cancelCtx, cancelExec := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancelExec() }()
	cancelled, err := backend.ExecuteBuffered(cancelCtx, fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sleep", "5"}}},
		Cwd:     "/workspace", OutputLimitBytes: 1024,
	})
	if err != nil || cancelled.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED {
		t.Fatalf("qualified Exec cancellation = %#v, %v", cancelled, err)
	}
	deadline, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sleep", "5"}}},
		Cwd:     "/workspace", DeadlineUnixMs: uint64(time.Now().Add(100 * time.Millisecond).UnixMilli()), OutputLimitBytes: 1024,
	})
	if err != nil || deadline.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED {
		t.Fatalf("qualified Exec deadline = %#v, %v", deadline, err)
	}
	signalled, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "kill -TERM $$"}}},
		Cwd:     "/workspace", OutputLimitBytes: 1024,
	})
	if err != nil || signalled.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || signalled.Terminal.GetSignal() != 15 {
		t.Fatalf("qualified Exec signal = %#v, %v", signalled, err)
	}
	runnerconformance.RunDataPlane(t, runnerconformance.DataPlaneFixture{
		Backend: backend, PTY: backend, Port: backend, Fence: fence,
	})
	ptyControls := make(chan runnercontrol.PTYControl, 2)
	ptyControls <- runnercontrol.PTYControl{Rows: 40, Columns: 100}
	ptyControls <- runnercontrol.PTYControl{Credit: 1024}
	close(ptyControls)
	var ptyOutput bytes.Buffer
	ptyTerminal, err := backend.ExecutePTY(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf pty-ready"}}},
		Cwd:     "/workspace", Streaming: true, AllocatePty: true, PtyRows: 24, PtyColumns: 80, OutputLimitBytes: 1024,
	}, ptyControls, func(data []byte) error { _, err := ptyOutput.Write(data); return err })
	if err != nil || ptyOutput.String() != "pty-ready" || ptyTerminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("qualified PTY = %q %#v, %v", ptyOutput.String(), ptyTerminal, err)
	}
	portServer, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "nc -l -p 23456 -e /bin/cat </dev/null >/dev/null 2>&1 & sleep 0.1"}}},
		Cwd:     "/workspace", OutputLimitBytes: 1024,
	})
	if err != nil || portServer.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("start qualified guest Port server = %#v, %v", portServer, err)
	}
	port, err := backend.OpenPort(t.Context(), fence, &runnerprotocol.PortOpen{Protocol: "tcp", GuestPort: 23456, IdleTimeoutMs: 30_000})
	if err != nil {
		t.Fatalf("qualified Port open: %v", err)
	}
	portContent := []byte{0, 1, 2, 254, 255}
	if err := port.Write(t.Context(), portContent); err != nil {
		t.Fatalf("qualified Port write: %v", err)
	}
	portResponse, err := port.Read(t.Context(), len(portContent))
	if err != nil || !bytes.Equal(portResponse, portContent) {
		t.Fatalf("qualified Port response = %v, %v", portResponse, err)
	}
	if err := port.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("qualified Port close: %v", err)
	}

	httpServer, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "nc -l -p 23457 -e /bin/cat </dev/null >/dev/null 2>&1 & sleep 0.1"}}},
		Cwd:     "/workspace", OutputLimitBytes: 1024,
	})
	if err != nil || httpServer.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED {
		t.Fatalf("start qualified HTTP relay server = %#v, %v", httpServer, err)
	}
	httpPort, err := backend.OpenPort(t.Context(), fence, &runnerprotocol.PortOpen{Protocol: "http", GuestPort: 23457, IdleTimeoutMs: 30_000})
	if err != nil {
		t.Fatalf("qualified HTTP relay open: %v", err)
	}
	httpProbe := []byte("GET / HTTP/1.0\r\n\r\n")
	if err := httpPort.Write(t.Context(), httpProbe); err != nil {
		t.Fatalf("qualified HTTP relay write: %v", err)
	}
	httpEcho, err := httpPort.Read(t.Context(), len(httpProbe))
	if err != nil || len(httpEcho) == 0 {
		t.Fatalf("qualified HTTP relay response = %v, %v", httpEcho, err)
	}
	if err := httpPort.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("qualified HTTP relay close: %v", err)
	}
	postPort, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Shell{Shell: "printf post-port"}, DeadlineUnixMs: uint64(time.Now().Add(2 * time.Second).UnixMilli()), OutputLimitBytes: 1024,
	})
	if err != nil || string(postPort.Stdout) != "post-port" {
		t.Fatalf("qualified post-Port operation = %#v, %v", postPort, err)
	}
	removeResult, err := backend.ExecuteFile(t.Context(), fence, &runnerprotocol.FileOpen{
		Operation: runnerprotocol.FileOperation_FILE_OPERATION_REMOVE, WorkspaceRelativePath: "qualification", Recursive: true,
	}, nil)
	if err != nil || removeResult.Terminal.GetKind() != runnerprotocol.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED {
		t.Fatalf("qualified File remove = %#v, %v", removeResult, err)
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
		OperationId: "qualification-network-create", WorkspaceId: "qualification-network-workspace",
		FencingToken:         []byte("qualification-network-store-fence"),
		LogicalCapacityBytes: uint64(qualificationWorkspaceBytes),
	}); err != nil {
		t.Fatalf("create network-policy Workspace: %v", err)
	}
	networkAssignment := proto.Clone(assignment).(*runnerprotocol.AssignmentCommand)
	networkAssignment.WorkspaceId = "qualification-network-workspace"
	networkAssignment.Correlation = &runnerprotocol.Correlation{RequestId: "qualification-network-request", OperationId: "qualification-network-operation", LeaseId: "qualification-network-lease"}
	networkAssignment.Fence = &runnerprotocol.AssignmentFence{
		AssignmentId: "qualification-network-assignment", SandboxId: "qualification-network-sandbox",
		InstanceId: "qualification-network-instance", SandboxGeneration: 1,
		FencingToken: []byte("qualification-network-fence-000"),
	}
	networkAssignment.NetworkPolicy = &runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{{
			Target:   &runnerprotocol.NetworkDestination_Domain{Domain: "example.com"},
			Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_HTTP,
			Port:     80,
		}},
	}
	if _, err := backend.StartAssignment(t.Context(), networkAssignment, func(runnerprotocol.AssignmentProgressStage) error { return nil }); err != nil {
		t.Fatalf("start exact network-policy Instance: %v", err)
	}
	if err := backend.MarkAssignmentReady(networkAssignment.Fence); err != nil {
		t.Fatal(err)
	}
	allowedNetwork, err := backend.ExecuteBuffered(t.Context(), networkAssignment.Fence, &runnerprotocol.ExecOpen{
		Command:        &runnerprotocol.ExecOpen_Shell{Shell: "wget -q -T 10 -O - http://example.com/ | grep -q 'Example Domain'"},
		DeadlineUnixMs: uint64(time.Now().Add(15 * time.Second).UnixMilli()), OutputLimitBytes: 4096,
	})
	if err != nil || allowedNetwork.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || allowedNetwork.Terminal.GetExitCode() != 0 {
		t.Fatalf("qualified exact-domain allow = %#v, %v", allowedNetwork, err)
	}
	blockedMetadata, err := backend.ExecuteBuffered(t.Context(), networkAssignment.Fence, &runnerprotocol.ExecOpen{
		Command:        &runnerprotocol.ExecOpen_Shell{Shell: "wget -q -T 2 -O /dev/null http://169.254.169.254/"},
		DeadlineUnixMs: uint64(time.Now().Add(5 * time.Second).UnixMilli()), OutputLimitBytes: 4096,
	})
	if err != nil || blockedMetadata.Terminal.GetKind() != runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED || blockedMetadata.Terminal.GetExitCode() == 0 {
		t.Fatalf("qualified metadata block = %#v, %v", blockedMetadata, err)
	}
	if _, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{Fence: networkAssignment.Fence, DeadlineUnixMs: uint64(time.Now().Add(20 * time.Second).UnixMilli())}); err != nil {
		t.Fatalf("fence network-policy Instance: %v", err)
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
	records := evidenceSink.snapshot()
	if len(records) < 6 {
		t.Fatalf("qualified lifecycle evidence count = %d: %#v", len(records), records)
	}
	foundUnexpected := false
	for _, record := range records {
		if record.BackendKind != "microsandbox" || record.HostPlatform != microsandboxHostPlatform() || record.HelperPID <= 0 || record.Materialization != manifestDigest || record.StreamID == "" {
			t.Fatalf("incomplete qualified lifecycle evidence: %#v", record)
		}
		if record.Stage == "unexpected_exit" {
			foundUnexpected = record.HelperReason != "" && strings.HasPrefix(record.StderrDigest, "sha256:") && strings.HasPrefix(record.EventTailDigest, "sha256:")
		}
	}
	if !foundUnexpected {
		t.Fatalf("unexpected-exit evidence missing bounded termination digests: %#v", records)
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

func digestFlatRoot(t *testing.T, path string) string {
	t.Helper()
	digest, err := materialization.DigestFlatRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
