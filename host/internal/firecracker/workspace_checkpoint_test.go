package microvm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	managedagents "agent-manager/contracts/managed-agents/v1/gen/go/managedagents"
	"agent-manager/internal/config"
	"agent-manager/internal/sandboxbroker"
)

func newWorkspaceCheckpointBackend(t *testing.T) (*SandboxBrokerBackend, sandboxbroker.WorkspaceIdentity, sandboxbroker.LeasePolicy) {
	t.Helper()
	for _, command := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skip(command + " unavailable")
		}
	}
	manager := &Manager{cfg: &config.Config{
		MicroVMWorkspaceBackend: "ext4", MicroVMWorkspaceDir: t.TempDir(), MicroVMWorkspaceSizeMiB: 8,
		MicroVMVCPUs: 2, MicroVMMemoryMiB: 256,
	}}
	backend, err := NewSandboxBrokerBackend(manager)
	if err != nil {
		t.Fatal(err)
	}
	identity := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectAgent, SubjectID: "agent-workspace", CompartmentID: "source"},
		Generation:   1,
	}
	policy := sandboxbroker.LeasePolicy{
		Resource: managedagents.ResourcePolicy{CpuMillis: 1000, MemoryBytes: 128 << 20, DiskBytes: 8 << 20, ProcessLimit: 64},
		Mount:    managedagents.MountPolicy{WorkspaceWritable: true},
	}
	return backend, identity, policy
}

func mutateWorkspaceImage(t *testing.T, backend *SandboxBrokerBackend, identity sandboxbroker.WorkspaceIdentity, name, content string) {
	t.Helper()
	workspacePath, err := backend.workspacePath(identity)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("debugfs", "-w", "-R", "write "+source+" /"+name, workspacePath).CombinedOutput()
	if err != nil {
		t.Fatalf("mutate workspace image: %v: %s", err, output)
	}
}

func TestWorkspaceFilesystemManifestIsStableWithSymlinks(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	if _, err := backend.CaptureWorkspaceBaseline(t.Context(), identity, policy); err != nil {
		t.Fatal(err)
	}
	workspacePath, err := backend.workspacePath(identity)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.Symlink("/usr/bin/python3", filepath.Join(source, "python")); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("mkfs.ext4", "-F", "-d", source, workspacePath).CombinedOutput()
	if err != nil {
		t.Fatalf("format symlink workspace image: %v: %s", err, output)
	}
	first, err := workspaceFilesystemManifestSHA256(t.Context(), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	second, err := workspaceFilesystemManifestSHA256(t.Context(), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("symlink workspace manifest is unstable: first=%s second=%s", first, second)
	}
}

func TestExt4CleanTurnDoesNotCreateTurnCheckpoint(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	baseline, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := backend.CommitTerminalWorkspace(context.Background(), identity, sandboxbroker.TerminalWorkspaceRequest{
		Workspace: identity.WorkspaceRef, TurnID: "turn-clean", Status: sandboxbroker.WorkspaceTerminalCompleted,
	}, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if commit.Dirty || commit.CheckpointRef != "" {
		t.Fatalf("clean terminal commit = %+v", commit)
	}
	if _, err := os.Stat(filepath.Join(backend.workspaceArtifactRoot(), "turn_checkpoints")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean turn created checkpoint directory: %v", err)
	}
}

func TestExt4DirtyTurnCreatesContentVerifiedCheckpoint(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	baseline, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	mutateWorkspaceImage(t, backend, identity, "changed.txt", "preserve this mutation")
	commit, err := backend.CommitTerminalWorkspace(context.Background(), identity, sandboxbroker.TerminalWorkspaceRequest{
		Workspace: identity.WorkspaceRef, TurnID: "turn-dirty", Status: sandboxbroker.WorkspaceTerminalCompleted,
	}, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !commit.Dirty || commit.CheckpointRef == "" || commit.CheckpointSHA256 == "" || commit.CheckpointManifestSHA256 == "" {
		t.Fatalf("dirty terminal commit = %+v", commit)
	}
	if _, _, err := backend.verifyWorkspaceArtifact(commit.CheckpointRef, commit.CheckpointManifestSHA256, commit.CheckpointSHA256, commit.CheckpointSizeBytes); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceArtifactRejectsContentEvidenceFromDifferentImage(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	baseline, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	mutateWorkspaceImage(t, backend, identity, "changed-after-evidence.txt", "new logical content")
	workspacePath, err := backend.workspacePath(identity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.createWorkspaceArtifact(
		context.Background(),
		"turn_checkpoint",
		identity,
		workspacePath,
		baseline.ManifestSHA256,
		"turn-stale-content-evidence",
		sandboxbroker.WorkspaceTerminalCompleted,
	)
	if !errors.Is(err, sandboxbroker.ErrWorkspaceCheckpointCorrupt) {
		t.Fatalf("create artifact with stale content evidence error = %v", err)
	}
}

func TestExt4GenerationBaseSurvivesLaterWorkspaceMutation(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	initial, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	mutateWorkspaceImage(t, backend, identity, "later.txt", "later generation content")
	later, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	if later.ManifestSHA256 == initial.ManifestSHA256 {
		t.Fatal("later turn baseline did not observe filesystem mutation")
	}
	if later.GenerationBase != initial.GenerationBase {
		t.Fatalf("generation base changed after mutation: initial=%+v later=%+v", initial.GenerationBase, later.GenerationBase)
	}
}

func TestExt4FailedAndCancelledDirtyTurnsKeepMutations(t *testing.T) {
	for _, status := range []string{sandboxbroker.WorkspaceTerminalFailed, sandboxbroker.WorkspaceTerminalCancelled} {
		t.Run(status, func(t *testing.T) {
			backend, identity, policy := newWorkspaceCheckpointBackend(t)
			baseline, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
			if err != nil {
				t.Fatal(err)
			}
			mutateWorkspaceImage(t, backend, identity, status+".txt", "mutation survives "+status)
			commit, err := backend.CommitTerminalWorkspace(context.Background(), identity, sandboxbroker.TerminalWorkspaceRequest{
				Workspace: identity.WorkspaceRef, TurnID: "turn-" + status, Status: status,
			}, baseline)
			if err != nil {
				t.Fatal(err)
			}
			if !commit.Dirty || commit.Status != status || commit.CheckpointRef == "" {
				t.Fatalf("%s terminal commit = %+v", status, commit)
			}
		})
	}
}

func TestWorkspaceCheckpointCorruptionFailsForkMaterialization(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	baseline, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	mutateWorkspaceImage(t, backend, identity, "corrupt.txt", "checkpoint bytes")
	commit, err := backend.CommitTerminalWorkspace(context.Background(), identity, sandboxbroker.TerminalWorkspaceRequest{
		Workspace: identity.WorkspaceRef, TurnID: "turn-corrupt", Status: sandboxbroker.WorkspaceTerminalFailed,
	}, baseline)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir, err := backend.workspaceArtifactPath(commit.CheckpointRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, workspaceName), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectAgent, SubjectID: identity.SubjectID, CompartmentID: "fork-corrupt"}, Generation: 1,
	}
	err = backend.MaterializeWorkspaceVersion(context.Background(), target, workspaceAttachment(identity, commit, 1))
	if !errors.Is(err, sandboxbroker.ErrWorkspaceCheckpointCorrupt) {
		t.Fatalf("materialize corrupt checkpoint error = %v", err)
	}
}

func TestWorkspaceRecoveryDoesNotDependOnGoldenSnapshot(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	baseline, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	mutateWorkspaceImage(t, backend, identity, "recovery.txt", "canonical filesystem state")
	commit, err := backend.CommitTerminalWorkspace(context.Background(), identity, sandboxbroker.TerminalWorkspaceRequest{
		Workspace: identity.WorkspaceRef, TurnID: "turn-recovery", Status: sandboxbroker.WorkspaceTerminalCancelled,
	}, baseline)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "vmstate.snap")
	if err := os.WriteFile(snapshot, []byte("acceleration cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapshot); err != nil {
		t.Fatal(err)
	}
	target := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectAgent, SubjectID: identity.SubjectID, CompartmentID: "fork-recovery"}, Generation: 1,
	}
	if err := backend.MaterializeWorkspaceVersion(context.Background(), target, workspaceAttachment(identity, commit, 7)); err != nil {
		t.Fatal(err)
	}
	targetPath, err := backend.workspacePath(target)
	if err != nil {
		t.Fatal(err)
	}
	content, ok, err := debugfsReadFile(context.Background(), targetPath, "/recovery.txt")
	if err != nil || !ok || string(content) != "canonical filesystem state" {
		t.Fatalf("recovered workspace content = %q, ok=%v, err=%v", content, ok, err)
	}
}

func TestCleanLogicalVersionMaterializesRetainedDirtyCheckpoint(t *testing.T) {
	backend, identity, policy := newWorkspaceCheckpointBackend(t)
	baseline, err := backend.CaptureWorkspaceBaseline(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	mutateWorkspaceImage(t, backend, identity, "retained.txt", "retained dirty checkpoint")
	commit, err := backend.CommitTerminalWorkspace(context.Background(), identity, sandboxbroker.TerminalWorkspaceRequest{
		Workspace: identity.WorkspaceRef, TurnID: "turn-dirty-source", Status: sandboxbroker.WorkspaceTerminalFailed,
	}, baseline)
	if err != nil {
		t.Fatal(err)
	}
	attachment := workspaceAttachment(identity, commit, 1)
	attachment.LogicalVersion = 2
	attachment.TerminalTurnID = "turn-clean-selected"
	attachment.TerminalStatus = sandboxbroker.WorkspaceTerminalCancelled
	target := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectAgent, SubjectID: identity.SubjectID, CompartmentID: "fork-clean"}, Generation: 1,
	}
	if err := backend.MaterializeWorkspaceVersion(context.Background(), target, attachment); err != nil {
		t.Fatal(err)
	}
	targetPath, err := backend.workspacePath(target)
	if err != nil {
		t.Fatal(err)
	}
	content, ok, err := debugfsReadFile(context.Background(), targetPath, "/retained.txt")
	if err != nil || !ok || string(content) != "retained dirty checkpoint" {
		t.Fatalf("materialized retained checkpoint content = %q, ok=%v, err=%v", content, ok, err)
	}
}

func workspaceAttachment(identity sandboxbroker.WorkspaceIdentity, commit sandboxbroker.TerminalWorkspaceCommit, logicalVersion int64) sandboxbroker.WorkspaceVersionAttachment {
	return sandboxbroker.WorkspaceVersionAttachment{
		SourceWorkspace: identity.WorkspaceRef, LogicalVersion: logicalVersion, SourceGeneration: identity.Generation,
		TerminalTurnID: commit.TurnID, TerminalStatus: commit.Status, WorkspaceContentEvidence: commit.WorkspaceContentEvidence,
		CheckpointRef: commit.CheckpointRef, CheckpointSHA256: commit.CheckpointSHA256,
		CheckpointSizeBytes: commit.CheckpointSizeBytes, CheckpointManifestSHA256: commit.CheckpointManifestSHA256,
		CheckpointLogicalVersion: logicalVersion, CheckpointTerminalTurnID: commit.TurnID, CheckpointTerminalStatus: commit.Status,
	}
}
