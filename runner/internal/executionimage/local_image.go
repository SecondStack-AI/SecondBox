package executionimage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// VerifyLocal admits only local signed bytes. It performs no registry I/O.
func (manager *Manager) VerifyLocal(ctx context.Context, image *runnerprotocol.ExecutionImage, progress func(runnerprotocol.AssignmentProgressStage) error) (PreparedImage, error) {
	if image == nil {
		// Persisted pre-selection Sandboxes retain their immutable Profile assets.
		fixed := &Manager{publicKeyPath: manager.fixedPublicKeyPath, publicKeySHA256: manager.fixedPublicKeySHA256}
		artifacts, err := fixed.verifyAndCaptureArtifacts(ctx, manager.fixedDirectory)
		if err != nil {
			return PreparedImage{}, err
		}
		if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
			return PreparedImage{}, err
		}
		return PreparedImage{Directory: manager.fixedDirectory, Artifacts: artifacts, Release: func() {}}, nil
	}
	_, digest, found := strings.Cut(image.Reference, "@")
	if !found || !imageDigestPattern.MatchString(digest) {
		return PreparedImage{}, errors.New("SecondBox assignment execution image must be a resolved digest")
	}
	lock, err := manager.lockDigest(ctx, digest)
	if err != nil {
		return PreparedImage{}, err
	}
	directory := filepath.Join(manager.cacheRoot, strings.TrimPrefix(digest, "sha256:"))
	artifacts, err := manager.verifyAndCaptureArtifacts(ctx, directory)
	if err != nil {
		_ = lock.Close()
		return PreparedImage{}, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
		_ = lock.Close()
		return PreparedImage{}, err
	}
	var once sync.Once
	return PreparedImage{Directory: directory, RequestedReference: image.Reference, ResolvedDigest: digest, Artifacts: artifacts, Release: func() { once.Do(func() { _ = lock.Close() }) }}, nil
}
