package executionimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// VerifyLocal admits only local verified bundles. It performs no registry I/O.
func (manager *Manager) VerifyLocal(ctx context.Context, image *runnerprotocol.ExecutionImage, progress func(runnerprotocol.AssignmentProgressStage) error) (PreparedImage, error) {
	if image == nil {
		if manager.fixedDirectory == "" {
			return PreparedImage{}, errors.New("SecondBox execution image verifier has no fixed bundle")
		}
		// Persisted pre-selection Sandboxes retain their immutable Profile assets.
		artifacts, err := manager.verifiedBundleArtifacts(ctx, manager.fixedDirectory)
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
	prepared, err := manager.verifyCachedDigest(ctx, digest, manager.lockDigest)
	if err != nil {
		return PreparedImage{}, err
	}
	if err := progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY); err != nil {
		prepared.Release()
		return PreparedImage{}, err
	}
	prepared.RequestedReference = image.Reference
	return prepared, nil
}

// VerifyPrepared verifies a digest the fetcher has just prepared, so the
// Runner's own bundle verification happens during preparation instead of
// inside the first start of that image.
func (manager *Manager) VerifyPrepared(ctx context.Context, digest string, progress func(runnerprotocol.AssignmentProgressStage) error) error {
	if !imageDigestPattern.MatchString(digest) {
		return errors.New("SecondBox prepared execution image digest is invalid")
	}
	prepared, err := manager.verifyCachedDigest(ctx, digest, manager.lockDigest)
	if err != nil {
		return err
	}
	prepared.Release()
	return progress(runnerprotocol.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_ARTIFACT_VERIFY)
}

// verifyCachedDigest holds the digest lock, which keeps the bundle from
// eviction, until the returned image is released.
func (manager *Manager) verifyCachedDigest(ctx context.Context, digest string, lockDigest func(context.Context, string) (*os.File, error)) (PreparedImage, error) {
	lock, err := lockDigest(ctx, digest)
	if err != nil {
		return PreparedImage{}, err
	}
	directory := filepath.Join(manager.cacheRoot, strings.TrimPrefix(digest, "sha256:"))
	artifacts, err := manager.verifiedBundleArtifacts(ctx, directory)
	if err != nil {
		_ = lock.Close()
		return PreparedImage{}, err
	}
	var once sync.Once
	return PreparedImage{Directory: directory, ResolvedDigest: digest, Artifacts: artifacts, Release: func() { once.Do(func() { _ = lock.Close() }) }}, nil
}
