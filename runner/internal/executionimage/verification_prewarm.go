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
	directories, err := manager.cachedBundleDirectoriesByRecency()
	if err != nil {
		return err
	}
	verified, failed := 0, 0
	for _, directory := range directories {
		digest := "sha256:" + filepath.Base(directory)
		prepared, err := manager.verifyCachedDigest(ctx, digest, manager.lockExistingDigest)
		if err == nil {
			prepared.Release()
			verified++
			continue
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if errors.Is(err, os.ErrNotExist) {
			// The fetcher evicted the bundle, and with it its lock file, after
			// it was listed.
			continue
		}
		failed++
		slog.Error("SecondBox execution image prewarm verification failed", "digest", digest, "error", err)
	}
	slog.Info("SecondBox execution image prewarm verification completed",
		"verified", verified, "failed", failed, "durationMs", time.Since(started).Milliseconds())
	return nil
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
