package executionimage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage/executionimagetest"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func noPrewarmProgress(runnerprotocol.AssignmentProgressStage) error { return nil }

// publishCachedBundle writes a verified bundle and its digest lock file, as a
// fetcher publication leaves them.
func publishCachedBundle(t *testing.T, cacheRoot, digest string) string {
	t.Helper()
	directory := filepath.Join(cacheRoot, strings.TrimPrefix(digest, "sha256:"))
	executionimagetest.WriteBundle(t, directory, nil)
	if err := os.MkdirAll(filepath.Join(cacheRoot, ".locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheRoot, ".locks", strings.TrimPrefix(digest, "sha256:")+".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// Prewarming records every valid cached bundle, so the first start after a
// Runner restart reuses the record, and a corrupt bundle does not stop it.
func TestPrewarmVerificationsRecordsValidBundlesAndReportsCorruptOnes(t *testing.T) {
	cacheRoot := t.TempDir()
	valid := []string{"sha256:" + strings.Repeat("a1", 32), "sha256:" + strings.Repeat("b2", 32)}
	for _, digest := range valid {
		publishCachedBundle(t, cacheRoot, digest)
	}
	corrupt := "sha256:" + strings.Repeat("c3", 32)
	corruptDirectory := publishCachedBundle(t, cacheRoot, corrupt)
	if err := os.WriteFile(filepath.Join(corruptDirectory, "rootfs.ext4"), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	records := privateRecordRoot(t)
	if err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).PrewarmVerifications(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := &Manager{cacheRoot: cacheRoot, verificationRoot: records}
	for _, digest := range valid {
		directory := filepath.Join(cacheRoot, strings.TrimPrefix(digest, "sha256:"))
		if _, stored, err := restarted.loadVerificationRecord(directory); err != nil || !stored {
			t.Fatalf("prewarmed %s record stored = %v, error = %v", digest, stored, err)
		}
	}
	if _, stored, err := restarted.loadVerificationRecord(corruptDirectory); err != nil || stored {
		t.Fatalf("corrupt bundle record stored = %v, error = %v", stored, err)
	}
}

// A bundle whose lock file is gone was evicted after it was listed; prewarm
// must not create a root-owned lock file the fetcher could not open later.
func TestPrewarmVerificationsCreatesNoLockFiles(t *testing.T) {
	cacheRoot := t.TempDir()
	digest := "sha256:" + strings.Repeat("a7", 32)
	directory := publishCachedBundle(t, cacheRoot, digest)
	lockPath := filepath.Join(cacheRoot, ".locks", strings.TrimPrefix(digest, "sha256:")+".lock")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	records := privateRecordRoot(t)
	if err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).PrewarmVerifications(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prewarm recreated the digest lock file: %v", err)
	}
	if _, stored, err := (&Manager{verificationRoot: records}).loadVerificationRecord(directory); err != nil || stored {
		t.Fatalf("unlocked bundle record stored = %v, error = %v", stored, err)
	}
}

func TestPrewarmVerificationsOrdersBundlesByRecentUse(t *testing.T) {
	cacheRoot := t.TempDir()
	older := filepath.Join(cacheRoot, strings.Repeat("d4", 32))
	newer := filepath.Join(cacheRoot, strings.Repeat("e5", 32))
	for _, directory := range []string{older, newer} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(cacheRoot, ".locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(older, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}
	directories, err := (&Manager{cacheRoot: cacheRoot}).cachedBundleDirectoriesByRecency()
	if err != nil {
		t.Fatal(err)
	}
	if len(directories) != 2 || directories[0] != newer || directories[1] != older {
		t.Fatalf("prewarm order = %v", directories)
	}
}

// Prewarming reports a remaining bundle that lacks a file instead of treating
// it as evicted.
func TestPrewarmVerificationsReportsABundleMissingAFile(t *testing.T) {
	cacheRoot := t.TempDir()
	incomplete := publishCachedBundle(t, cacheRoot, "sha256:"+strings.Repeat("d8", 32))
	if err := os.Remove(filepath.Join(incomplete, "kernel")); err != nil {
		t.Fatal(err)
	}
	verified, failures, err := (&Manager{cacheRoot: cacheRoot, verificationRoot: privateRecordRoot(t)}).verifyCachedBundles(t.Context(), true)
	if err != nil || verified != 0 || len(failures) != 1 || !strings.Contains(failures[0].Error(), strings.Repeat("d8", 32)) {
		t.Fatalf("verified = %d, failures = %v, error = %v", verified, failures, err)
	}
}

// A worker image build records every cached bundle and fails rather than ship
// a corrupt bundle or an empty cache.
func TestRecordCachedBundleVerificationsFailsOnCorruptOrEmptyCaches(t *testing.T) {
	cacheRoot := t.TempDir()
	if _, err := (&Manager{cacheRoot: cacheRoot, verificationRoot: privateRecordRoot(t)}).RecordCachedBundleVerifications(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "holds no bundle") {
		t.Fatalf("empty cache error = %v", err)
	}
	valid := publishCachedBundle(t, cacheRoot, "sha256:"+strings.Repeat("a8", 32))
	records := privateRecordRoot(t)
	if verified, err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).RecordCachedBundleVerifications(t.Context()); err != nil || verified != 1 {
		t.Fatalf("valid cache verified = %d, error = %v", verified, err)
	}
	if _, stored, err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).loadVerificationRecord(valid); err != nil || !stored {
		t.Fatalf("recorded bundle stored = %v, error = %v", stored, err)
	}
	corrupt := publishCachedBundle(t, cacheRoot, "sha256:"+strings.Repeat("b9", 32))
	if err := os.WriteFile(filepath.Join(corrupt, "rootfs.ext4"), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).RecordCachedBundleVerifications(t.Context()); err == nil ||
		!strings.Contains(err.Error(), strings.Repeat("b9", 32)) {
		t.Fatalf("corrupt cache error = %v", err)
	}
	if err := os.RemoveAll(corrupt); err != nil {
		t.Fatal(err)
	}
	incomplete := publishCachedBundle(t, cacheRoot, "sha256:"+strings.Repeat("c7", 32))
	if err := os.Remove(filepath.Join(incomplete, "kernel")); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).RecordCachedBundleVerifications(t.Context()); err == nil ||
		!strings.Contains(err.Error(), strings.Repeat("c7", 32)) {
		t.Fatalf("incomplete cache error = %v", err)
	}
}

// The Runner verifies a freshly prepared digest during preparation, so its
// first start finds a recorded verification.
func TestVerifyPreparedRecordsTheBundle(t *testing.T) {
	cacheRoot := t.TempDir()
	digest := "sha256:" + strings.Repeat("f6", 32)
	directory := filepath.Join(cacheRoot, strings.TrimPrefix(digest, "sha256:"))
	executionimagetest.WriteBundle(t, directory, nil)
	records := privateRecordRoot(t)
	if err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).VerifyPrepared(t.Context(), digest, noPrewarmProgress); err != nil {
		t.Fatal(err)
	}
	if _, stored, err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).loadVerificationRecord(directory); err != nil || !stored {
		t.Fatalf("prepared bundle record stored = %v, error = %v", stored, err)
	}
	if err := (&Manager{cacheRoot: cacheRoot, verificationRoot: records}).VerifyPrepared(t.Context(), "registry.example/agent:v1", noPrewarmProgress); err == nil {
		t.Fatal("prepared image without a digest was verified")
	}
}
