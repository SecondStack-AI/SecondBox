package microvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"agentcy/internal/sandboxbroker"

	"golang.org/x/sys/unix"
)

const workspaceDurabilitySchemaVersion = "1"

type workspaceFilesystemEntry struct {
	Path            string `json:"path"`
	Kind            string `json:"kind"`
	Mode            uint32 `json:"mode"`
	UID             uint32 `json:"uid"`
	GID             uint32 `json:"gid"`
	SizeBytes       int64  `json:"sizeBytes"`
	ModTimeUnixNano int64  `json:"modTimeUnixNano"`
	SHA256          string `json:"sha256,omitempty"`
	LinkTarget      string `json:"linkTarget,omitempty"`
}

type workspaceArtifactManifest struct {
	SchemaVersion         string                     `json:"schemaVersion"`
	Kind                  string                     `json:"kind"`
	Workspace             sandboxbroker.WorkspaceRef `json:"workspace"`
	SourceGeneration      int64                      `json:"sourceGeneration"`
	TerminalTurnID        string                     `json:"terminalTurnId,omitempty"`
	TerminalStatus        string                     `json:"terminalStatus,omitempty"`
	ContentManifestSHA256 string                     `json:"contentManifestSha256"`
	ImageSHA256           string                     `json:"imageSha256"`
	ImageSizeBytes        int64                      `json:"imageSizeBytes"`
}

type workspaceTerminalMarker struct {
	SchemaVersion string                                `json:"schemaVersion"`
	Commit        sandboxbroker.TerminalWorkspaceCommit `json:"commit"`
}

type workspaceGenerationBaseMarker struct {
	SchemaVersion    string                                `json:"schemaVersion"`
	Workspace        sandboxbroker.WorkspaceRef            `json:"workspace"`
	SourceGeneration int64                                 `json:"sourceGeneration"`
	Evidence         sandboxbroker.WorkspaceGenerationBase `json:"evidence"`
}

func (b *SandboxBrokerBackend) CaptureWorkspaceBaseline(ctx context.Context, identity sandboxbroker.WorkspaceIdentity, policy sandboxbroker.LeasePolicy) (sandboxbroker.WorkspaceContentEvidence, error) {
	if strings.EqualFold(b.manager.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		return sandboxbroker.WorkspaceContentEvidence{}, errors.New("dm-thin workspace logical versioning requires the privileged thin snapshot gate")
	}
	runtime, err := b.runtimeFor(identity, policy)
	if err != nil {
		return sandboxbroker.WorkspaceContentEvidence{}, err
	}
	workspacePath, err := b.manager.prepareWorkspaceSized(ctx, runtime.agentID, runtime.compartmentID, runtime.startOpts.SandboxPolicy.WorkspaceSizeMiB)
	if err != nil {
		return sandboxbroker.WorkspaceContentEvidence{}, err
	}
	contentSHA, err := workspaceFilesystemManifestSHA256(ctx, workspacePath)
	if err != nil {
		return sandboxbroker.WorkspaceContentEvidence{}, err
	}
	base, err := b.ensureWorkspaceGenerationBase(ctx, identity, workspacePath, contentSHA)
	if err != nil {
		return sandboxbroker.WorkspaceContentEvidence{}, err
	}
	return sandboxbroker.WorkspaceContentEvidence{
		Present: true, ManifestSHA256: contentSHA,
		GenerationBase: base,
	}, nil
}

func (b *SandboxBrokerBackend) CommitTerminalWorkspace(ctx context.Context, identity sandboxbroker.WorkspaceIdentity, request sandboxbroker.TerminalWorkspaceRequest, baseline sandboxbroker.WorkspaceContentEvidence) (sandboxbroker.TerminalWorkspaceCommit, error) {
	if strings.EqualFold(b.manager.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		return sandboxbroker.TerminalWorkspaceCommit{}, errors.New("dm-thin workspace logical versioning requires the privileged thin snapshot gate")
	}
	markerPath := b.workspaceTerminalMarkerPath(identity, request.TurnID)
	if commit, err := b.loadWorkspaceTerminalMarker(markerPath, identity, request); err == nil {
		return commit, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return sandboxbroker.TerminalWorkspaceCommit{}, err
	}

	workspacePath, err := b.workspacePath(identity)
	if err != nil {
		return sandboxbroker.TerminalWorkspaceCommit{}, err
	}
	current := sandboxbroker.WorkspaceContentEvidence{ManifestSHA256: sandboxbroker.EmptyWorkspaceManifestSHA256}
	if _, err := os.Stat(workspacePath); err == nil {
		current.Present = true
		current.ManifestSHA256, err = workspaceFilesystemManifestSHA256(ctx, workspacePath)
		if err != nil {
			return sandboxbroker.TerminalWorkspaceCommit{}, err
		}
		current.GenerationBase = baseline.GenerationBase
		if current.GenerationBase.Ref == "" {
			base, baseErr := b.ensureWorkspaceGenerationBase(ctx, identity, workspacePath, current.ManifestSHA256)
			if baseErr != nil {
				return sandboxbroker.TerminalWorkspaceCommit{}, baseErr
			}
			current.GenerationBase = base
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return sandboxbroker.TerminalWorkspaceCommit{}, fmt.Errorf("inspect terminal workspace image: %w", err)
	}
	dirty := current.Present != baseline.Present || current.ManifestSHA256 != baseline.ManifestSHA256
	commit := sandboxbroker.TerminalWorkspaceCommit{
		Workspace: request.Workspace, SourceGeneration: identity.Generation,
		TurnID: request.TurnID, Status: request.Status,
		WorkspaceContentEvidence: current, Dirty: dirty,
	}
	if dirty {
		if !current.Present {
			return sandboxbroker.TerminalWorkspaceCommit{}, errors.New("workspace removal cannot be checkpointed as an ext4 image")
		}
		artifact, err := b.createWorkspaceArtifact(ctx, "turn_checkpoint", identity, workspacePath, current.ManifestSHA256, request.TurnID, request.Status)
		if err != nil {
			return sandboxbroker.TerminalWorkspaceCommit{}, err
		}
		commit.CheckpointRef = artifact.ref
		commit.CheckpointSHA256 = artifact.imageSHA256
		commit.CheckpointSizeBytes = artifact.imageSizeBytes
		commit.CheckpointManifestSHA256 = artifact.manifestSHA256
	}
	if err := writeWorkspaceJSONAtomically(markerPath, workspaceTerminalMarker{SchemaVersion: workspaceDurabilitySchemaVersion, Commit: commit}); err != nil {
		return sandboxbroker.TerminalWorkspaceCommit{}, fmt.Errorf("write terminal workspace marker: %w", err)
	}
	return commit, nil
}

func (b *SandboxBrokerBackend) MaterializeWorkspaceVersion(ctx context.Context, target sandboxbroker.WorkspaceIdentity, attachment sandboxbroker.WorkspaceVersionAttachment) error {
	if strings.EqualFold(b.manager.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		return errors.New("dm-thin workspace fork materialization requires the privileged thin snapshot gate")
	}
	if len(attachment.MutationLineage) != 0 {
		return errors.New("workspace mutation lineage exceeds the retained-checkpoint policy")
	}
	targetPath, err := b.workspacePath(target)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(targetPath); err == nil {
		return errors.New("fork target workspace already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !attachment.Present {
		return nil
	}
	ref := attachment.CheckpointRef
	imageSHA := attachment.CheckpointSHA256
	imageSize := attachment.CheckpointSizeBytes
	manifestSHA := attachment.CheckpointManifestSHA256
	wantKind := "turn_checkpoint"
	if ref == "" {
		ref = attachment.GenerationBase.Ref
		imageSHA = attachment.GenerationBase.SHA256
		imageSize = attachment.GenerationBase.SizeBytes
		manifestSHA = attachment.GenerationBase.ManifestSHA256
		wantKind = "generation_base"
	}
	manifest, sourcePath, err := b.verifyWorkspaceArtifact(ref, manifestSHA, imageSHA, imageSize)
	if err != nil {
		return err
	}
	if manifest.Kind != wantKind || manifest.Workspace != attachment.SourceWorkspace || manifest.SourceGeneration != attachment.SourceGeneration || manifest.ContentManifestSHA256 != attachment.ManifestSHA256 {
		return fmt.Errorf("%w: workspace artifact binding does not match requested logical version", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	if wantKind == "turn_checkpoint" && (manifest.TerminalTurnID != attachment.CheckpointTerminalTurnID || manifest.TerminalStatus != attachment.CheckpointTerminalStatus) {
		return fmt.Errorf("%w: terminal checkpoint binding does not match requested logical version", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	if err := copyWorkspaceImageAtomically(sourcePath, targetPath, imageSHA, imageSize); err != nil {
		return err
	}
	contentSHA, err := workspaceFilesystemManifestSHA256(ctx, targetPath)
	if err != nil {
		return err
	}
	if contentSHA != attachment.ManifestSHA256 {
		return fmt.Errorf("%w: materialized workspace content manifest mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	return nil
}

type workspaceArtifactEvidence struct {
	ref            string
	imageSHA256    string
	imageSizeBytes int64
	manifestSHA256 string
}

func (b *SandboxBrokerBackend) ensureWorkspaceGenerationBase(ctx context.Context, identity sandboxbroker.WorkspaceIdentity, workspacePath, contentSHA string) (sandboxbroker.WorkspaceGenerationBase, error) {
	markerPath := b.workspaceGenerationBaseMarkerPath(identity)
	if marker, err := b.loadWorkspaceGenerationBaseMarker(markerPath, identity); err == nil {
		return marker.Evidence, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return sandboxbroker.WorkspaceGenerationBase{}, err
	}
	artifact, err := b.createWorkspaceArtifact(ctx, "generation_base", identity, workspacePath, contentSHA, "", "")
	if err != nil {
		return sandboxbroker.WorkspaceGenerationBase{}, err
	}
	evidence := sandboxbroker.WorkspaceGenerationBase{
		Ref: artifact.ref, SHA256: artifact.imageSHA256, SizeBytes: artifact.imageSizeBytes, ManifestSHA256: artifact.manifestSHA256,
	}
	marker := workspaceGenerationBaseMarker{
		SchemaVersion: workspaceDurabilitySchemaVersion, Workspace: identity.WorkspaceRef,
		SourceGeneration: identity.Generation, Evidence: evidence,
	}
	if err := writeWorkspaceJSONAtomically(markerPath, marker); err != nil {
		return sandboxbroker.WorkspaceGenerationBase{}, fmt.Errorf("write workspace generation base marker: %w", err)
	}
	return evidence, nil
}

func (b *SandboxBrokerBackend) loadWorkspaceGenerationBaseMarker(path string, identity sandboxbroker.WorkspaceIdentity) (workspaceGenerationBaseMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return workspaceGenerationBaseMarker{}, err
	}
	var marker workspaceGenerationBaseMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return workspaceGenerationBaseMarker{}, fmt.Errorf("%w: decode workspace generation base marker: %v", sandboxbroker.ErrWorkspaceCheckpointCorrupt, err)
	}
	if marker.SchemaVersion != workspaceDurabilitySchemaVersion || marker.Workspace != identity.WorkspaceRef || marker.SourceGeneration != identity.Generation {
		return workspaceGenerationBaseMarker{}, fmt.Errorf("%w: workspace generation base marker binding mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	manifest, _, err := b.verifyWorkspaceArtifact(marker.Evidence.Ref, marker.Evidence.ManifestSHA256, marker.Evidence.SHA256, marker.Evidence.SizeBytes)
	if err != nil {
		return workspaceGenerationBaseMarker{}, err
	}
	if manifest.Kind != "generation_base" || manifest.Workspace != identity.WorkspaceRef || manifest.SourceGeneration != identity.Generation {
		return workspaceGenerationBaseMarker{}, fmt.Errorf("%w: workspace generation base artifact binding mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	return marker, nil
}

func (b *SandboxBrokerBackend) createWorkspaceArtifact(ctx context.Context, kind string, identity sandboxbroker.WorkspaceIdentity, sourcePath, contentSHA, turnID, status string) (workspaceArtifactEvidence, error) {
	imageSHA, err := fileSHA256(sourcePath)
	if err != nil {
		return workspaceArtifactEvidence{}, fmt.Errorf("hash workspace image: %w", err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return workspaceArtifactEvidence{}, err
	}
	manifest := workspaceArtifactManifest{
		SchemaVersion: workspaceDurabilitySchemaVersion, Kind: kind, Workspace: identity.WorkspaceRef,
		SourceGeneration: identity.Generation, TerminalTurnID: turnID, TerminalStatus: status,
		ContentManifestSHA256: contentSHA, ImageSHA256: imageSHA, ImageSizeBytes: info.Size(),
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return workspaceArtifactEvidence{}, err
	}
	manifestDigest := sha256.Sum256(manifestJSON)
	manifestSHA := hex.EncodeToString(manifestDigest[:])
	namespaceDigest := sha256.Sum256([]byte(identity.NamespaceKey()))
	ref := filepath.Join(kind+"s", hex.EncodeToString(namespaceDigest[:16]), manifestSHA)
	dir, err := b.workspaceArtifactPath(ref)
	if err != nil {
		return workspaceArtifactEvidence{}, err
	}
	if _, err := os.Stat(dir); err == nil {
		if _, _, verifyErr := b.verifyWorkspaceArtifact(ref, manifestSHA, imageSHA, info.Size()); verifyErr != nil {
			return workspaceArtifactEvidence{}, verifyErr
		}
		return workspaceArtifactEvidence{ref: ref, imageSHA256: imageSHA, imageSizeBytes: info.Size(), manifestSHA256: manifestSHA}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return workspaceArtifactEvidence{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return workspaceArtifactEvidence{}, err
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(dir), ".workspace-artifact-*")
	if err != nil {
		return workspaceArtifactEvidence{}, err
	}
	defer os.RemoveAll(tempDir)
	if err := copyWorkspaceImageAtomically(sourcePath, filepath.Join(tempDir, workspaceName), imageSHA, info.Size()); err != nil {
		return workspaceArtifactEvidence{}, err
	}
	if err := writeWorkspaceFileDurably(filepath.Join(tempDir, "manifest.json"), append(manifestJSON, '\n')); err != nil {
		return workspaceArtifactEvidence{}, err
	}
	if err := syncDirectory(tempDir); err != nil {
		return workspaceArtifactEvidence{}, err
	}
	if err := os.Rename(tempDir, dir); err != nil {
		return workspaceArtifactEvidence{}, err
	}
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return workspaceArtifactEvidence{}, err
	}
	return workspaceArtifactEvidence{ref: ref, imageSHA256: imageSHA, imageSizeBytes: info.Size(), manifestSHA256: manifestSHA}, nil
}

func (b *SandboxBrokerBackend) workspacePath(identity sandboxbroker.WorkspaceIdentity) (string, error) {
	runtime, err := sandboxBrokerRuntimeFor(identity, sandboxbroker.LeasePolicy{})
	if err != nil {
		return "", err
	}
	return filepath.Join(b.manager.cfg.MicroVMWorkspaceDir, runtime.agentID, runtime.compartmentID+"."+workspaceName), nil
}

func (b *SandboxBrokerBackend) workspaceArtifactRoot() string {
	return filepath.Join(b.manager.cfg.MicroVMWorkspaceDir, ".workspace-durability")
}

func (b *SandboxBrokerBackend) workspaceArtifactPath(ref string) (string, error) {
	ref = filepath.Clean(strings.TrimSpace(ref))
	if ref == "." || filepath.IsAbs(ref) || strings.HasPrefix(ref, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid workspace artifact reference")
	}
	root := b.workspaceArtifactRoot()
	path := filepath.Join(root, ref)
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", errors.New("workspace artifact reference escapes durability root")
	}
	return path, nil
}

func (b *SandboxBrokerBackend) workspaceTerminalMarkerPath(identity sandboxbroker.WorkspaceIdentity, turnID string) string {
	namespaceDigest := sha256.Sum256([]byte(identity.NamespaceKey()))
	turnDigest := sha256.Sum256([]byte(strings.TrimSpace(turnID)))
	return filepath.Join(b.workspaceArtifactRoot(), "terminals", hex.EncodeToString(namespaceDigest[:16]), fmt.Sprintf("generation-%d", identity.Generation), hex.EncodeToString(turnDigest[:])+".json")
}

func (b *SandboxBrokerBackend) workspaceGenerationBaseMarkerPath(identity sandboxbroker.WorkspaceIdentity) string {
	namespaceDigest := sha256.Sum256([]byte(identity.NamespaceKey()))
	return filepath.Join(b.workspaceArtifactRoot(), "generation_base_heads", hex.EncodeToString(namespaceDigest[:16]), fmt.Sprintf("generation-%d.json", identity.Generation))
}

func (b *SandboxBrokerBackend) loadWorkspaceTerminalMarker(path string, identity sandboxbroker.WorkspaceIdentity, request sandboxbroker.TerminalWorkspaceRequest) (sandboxbroker.TerminalWorkspaceCommit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sandboxbroker.TerminalWorkspaceCommit{}, err
	}
	var marker workspaceTerminalMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return sandboxbroker.TerminalWorkspaceCommit{}, fmt.Errorf("%w: decode terminal workspace marker: %v", sandboxbroker.ErrWorkspaceCheckpointCorrupt, err)
	}
	commit := marker.Commit
	if marker.SchemaVersion != workspaceDurabilitySchemaVersion || commit.Workspace != request.Workspace || commit.SourceGeneration != identity.Generation || commit.TurnID != request.TurnID || commit.Status != request.Status {
		return sandboxbroker.TerminalWorkspaceCommit{}, fmt.Errorf("%w: terminal workspace marker binding mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	if commit.Present {
		baseManifest, _, err := b.verifyWorkspaceArtifact(commit.GenerationBase.Ref, commit.GenerationBase.ManifestSHA256, commit.GenerationBase.SHA256, commit.GenerationBase.SizeBytes)
		if err != nil {
			return sandboxbroker.TerminalWorkspaceCommit{}, err
		}
		if baseManifest.Kind != "generation_base" || baseManifest.Workspace != identity.WorkspaceRef || baseManifest.SourceGeneration != identity.Generation {
			return sandboxbroker.TerminalWorkspaceCommit{}, fmt.Errorf("%w: terminal workspace generation base binding mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
		}
	} else if commit.GenerationBase.Ref != "" {
		return sandboxbroker.TerminalWorkspaceCommit{}, fmt.Errorf("%w: absent terminal workspace carries generation base evidence", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	if commit.Dirty {
		checkpointManifest, _, err := b.verifyWorkspaceArtifact(commit.CheckpointRef, commit.CheckpointManifestSHA256, commit.CheckpointSHA256, commit.CheckpointSizeBytes)
		if err != nil {
			return sandboxbroker.TerminalWorkspaceCommit{}, err
		}
		if checkpointManifest.Kind != "turn_checkpoint" || checkpointManifest.Workspace != identity.WorkspaceRef || checkpointManifest.SourceGeneration != identity.Generation || checkpointManifest.TerminalTurnID != commit.TurnID || checkpointManifest.TerminalStatus != commit.Status || checkpointManifest.ContentManifestSHA256 != commit.ManifestSHA256 {
			return sandboxbroker.TerminalWorkspaceCommit{}, fmt.Errorf("%w: terminal workspace checkpoint binding mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
		}
	}
	return commit, nil
}

func (b *SandboxBrokerBackend) verifyWorkspaceArtifact(ref, wantManifestSHA, wantImageSHA string, wantSize int64) (workspaceArtifactManifest, string, error) {
	dir, err := b.workspaceArtifactPath(ref)
	if err != nil {
		return workspaceArtifactManifest{}, "", err
	}
	manifestData, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return workspaceArtifactManifest{}, "", fmt.Errorf("%w: read workspace artifact manifest: %v", sandboxbroker.ErrWorkspaceCheckpointCorrupt, err)
	}
	manifestData = []byte(strings.TrimSpace(string(manifestData)))
	digest := sha256.Sum256(manifestData)
	if hex.EncodeToString(digest[:]) != wantManifestSHA {
		return workspaceArtifactManifest{}, "", fmt.Errorf("%w: workspace artifact manifest hash mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	var manifest workspaceArtifactManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return workspaceArtifactManifest{}, "", fmt.Errorf("%w: decode workspace artifact manifest: %v", sandboxbroker.ErrWorkspaceCheckpointCorrupt, err)
	}
	imagePath := filepath.Join(dir, workspaceName)
	info, err := os.Stat(imagePath)
	if err != nil {
		return workspaceArtifactManifest{}, "", fmt.Errorf("%w: stat workspace artifact image: %v", sandboxbroker.ErrWorkspaceCheckpointCorrupt, err)
	}
	imageSHA, err := fileSHA256(imagePath)
	if err != nil {
		return workspaceArtifactManifest{}, "", fmt.Errorf("%w: hash workspace artifact image: %v", sandboxbroker.ErrWorkspaceCheckpointCorrupt, err)
	}
	if manifest.SchemaVersion != workspaceDurabilitySchemaVersion || manifest.ImageSHA256 != wantImageSHA || manifest.ImageSizeBytes != wantSize || imageSHA != wantImageSHA || info.Size() != wantSize {
		return workspaceArtifactManifest{}, "", fmt.Errorf("%w: workspace artifact image evidence mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	return manifest, imagePath, nil
}

func workspaceFilesystemManifestSHA256(ctx context.Context, imagePath string) (string, error) {
	dumpRoot, err := os.MkdirTemp("", "agentcy-workspace-manifest-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dumpRoot)
	if output, err := exec.CommandContext(ctx, "debugfs", "-R", "rdump / "+dumpRoot, imagePath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract ext4 workspace manifest: %w: %s", err, strings.TrimSpace(string(output)))
	}
	entries := make([]workspaceFilesystemEntry, 0)
	err = filepath.WalkDir(dumpRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dumpRoot {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dumpRoot, path)
		if err != nil {
			return err
		}
		item := workspaceFilesystemEntry{Path: filepath.ToSlash(rel), Mode: uint32(info.Mode()), SizeBytes: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			item.UID, item.GID = stat.Uid, stat.Gid
		}
		switch {
		case info.IsDir():
			item.Kind = "directory"
		case info.Mode().IsRegular():
			item.Kind = "file"
			item.SHA256, err = fileSHA256(path)
			if err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			item.Kind = "symlink"
			item.LinkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported workspace filesystem entry %s", item.Path)
		}
		entries = append(entries, item)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func copyWorkspaceImageAtomically(sourcePath, targetPath, wantSHA string, wantSize int64) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(targetPath), ".workspace-image-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		temp.Close()
		return err
	}
	copyErr := copySparseWorkspaceImage(temp, source, wantSize)
	closeSourceErr := source.Close()
	if copyErr != nil || closeSourceErr != nil {
		temp.Close()
		return errors.Join(copyErr, closeSourceErr)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return err
	}
	gotSHA, err := fileSHA256(tempPath)
	if err != nil {
		return err
	}
	if info.Size() != wantSize || gotSHA != wantSHA {
		return fmt.Errorf("%w: copied workspace image evidence mismatch", sandboxbroker.ErrWorkspaceCheckpointCorrupt)
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(targetPath))
}

func copySparseWorkspaceImage(target, source *os.File, size int64) error {
	if err := target.Truncate(size); err != nil {
		return err
	}
	for offset := int64(0); offset < size; {
		dataOffset, err := unix.Seek(int(source.Fd()), offset, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locate workspace image data extent: %w", err)
		}
		holeOffset, err := unix.Seek(int(source.Fd()), dataOffset, unix.SEEK_HOLE)
		if errors.Is(err, unix.ENXIO) {
			holeOffset = size
		} else if err != nil {
			return fmt.Errorf("locate workspace image hole: %w", err)
		}
		if holeOffset > size {
			holeOffset = size
		}
		if holeOffset <= dataOffset {
			return errors.New("workspace image reported a non-positive data extent")
		}
		if _, err := source.Seek(dataOffset, io.SeekStart); err != nil {
			return err
		}
		if _, err := target.Seek(dataOffset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(target, source, holeOffset-dataOffset); err != nil {
			return err
		}
		offset = holeOffset
	}
	return nil
}

func writeWorkspaceJSONAtomically(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".workspace-json-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func writeWorkspaceFileDurably(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
