package executionimage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PrewarmVerifications verifies every cached bundle, most recently used
// first, after a Runner start. A bundle with a valid verification record costs
// only metadata checks; one without it is hashed here instead of inside the
// first Sandbox start that selects it. A start of the same digest waits on the
// digest lock and then reuses this verification.
//
// A bundle that fails verification stays in the cache and fails every start
// that selects it, as it would without prewarming, so the failure is reported
// and the remaining bundles are still verified.
func (manager *Manager) PrewarmVerifications(ctx context.Context) error {
	started := time.Now()
	verified, failures, err := manager.verifyCachedBundles(ctx)
	if err != nil {
		return err
	}
	for _, failure := range failures {
		slog.Error("SecondBox execution image prewarm verification failed", "error", failure)
	}
	slog.Info("SecondBox execution image prewarm verification completed",
		"verified", verified, "failed", len(failures), "durationMs", time.Since(started).Milliseconds())
	return nil
}

// RecordCachedBundleVerifications verifies and records every cached bundle
// before any Runner starts on this cache, as a worker image build does. Unlike
// prewarming it fails when a bundle fails or the cache holds none, because the
// built image would otherwise ship unverified bundles.
func (manager *Manager) RecordCachedBundleVerifications(ctx context.Context) (int, error) {
	verified, failures, err := manager.verifyCachedBundles(ctx)
	if err != nil {
		return verified, err
	}
	if len(failures) > 0 {
		return verified, errors.Join(failures...)
	}
	if verified == 0 {
		return 0, errors.New("SecondBox execution image cache holds no bundle to record")
	}
	return verified, nil
}

func (manager *Manager) verifyCachedBundles(ctx context.Context) (int, []error, error) {
	directories, err := manager.cachedBundleDirectoriesByRecency()
	if err != nil {
		return 0, nil, err
	}
	verified := 0
	var failures []error
	for _, directory := range directories {
		digest := "sha256:" + filepath.Base(directory)
		prepared, err := manager.verifyCachedDigest(ctx, digest, manager.lockExistingDigest)
		if err == nil {
			prepared.Release()
			verified++
			continue
		}
		if ctx.Err() != nil {
			return verified, failures, context.Cause(ctx)
		}
		if errors.Is(err, os.ErrNotExist) {
			// The fetcher evicted the bundle, and with it its lock file, after
			// it was listed.
			continue
		}
		failures = append(failures, fmt.Errorf("SecondBox execution image %s verification failed: %w", digest, err))
	}
	return verified, failures, nil
}

func (manager *Manager) cachedBundleDirectoriesByRecency() ([]string, error) {
	entries, err := os.ReadDir(manager.cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image cache read failed: %w", err)
	}
	type candidate struct {
		path     string
		modified time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() || !cacheDirectoryPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("SecondBox execution image cache entry inspection failed: %w", err)
		}
		candidates = append(candidates, candidate{path: filepath.Join(manager.cacheRoot, entry.Name()), modified: info.ModTime()})
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].modified.After(candidates[right].modified)
	})
	directories := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		directories = append(directories, candidate.path)
	}
	return directories, nil
}
