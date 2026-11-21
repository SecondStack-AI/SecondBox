package executionimage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func TestBundleVerificationRepeatsOnlyWhenBundleMetadataChanges(t *testing.T) {
	manager := &Manager{verificationRoot: privateRecordRoot(t)}
	directory := writeBundleFixture(t)
	verifications := 0
	verify := func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		verifications++
		return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "rootfs", Path: filepath.Join(directory, "rootfs.ext4")}}, nil
	}
	for range 4 {
		artifacts, err := manager.memoizedBundleVerification(directory, verify)
		if err != nil {
			t.Fatal(err)
		}
		if len(artifacts) != 1 || artifacts[0].Label != "rootfs" {
			t.Fatalf("memoized artifacts = %+v", artifacts)
		}
	}
	if verifications != 1 {
		t.Fatalf("unchanged bundle was verified %d times", verifications)
	}
	if err := os.WriteFile(filepath.Join(directory, "rootfs.ext4"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.memoizedBundleVerification(directory, verify); err != nil {
		t.Fatal(err)
	}
	if verifications != 2 {
		t.Fatalf("replaced rootfs produced %d verifications", verifications)
	}
}

func TestBundleVerificationRejectsBundleChangedDuringVerification(t *testing.T) {
	manager := &Manager{verificationRoot: privateRecordRoot(t)}
	directory := writeBundleFixture(t)
	_, err := manager.memoizedBundleVerification(directory, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		return nil, os.WriteFile(filepath.Join(directory, "kernel"), []byte("substituted"), 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "changed during bundle verification") {
		t.Fatalf("substitution during verification error = %v", err)
	}
	verifications := 0
	if _, err := manager.memoizedBundleVerification(directory, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		verifications++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if verifications != 1 {
		t.Fatalf("rejected verification was remembered, verifications = %d", verifications)
	}
}

func TestBundleVerificationForgetsUnreachableBundles(t *testing.T) {
	manager := &Manager{verificationRoot: privateRecordRoot(t)}
	removed := writeBundleFixture(t)
	verify := func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) { return nil, nil }
	if _, err := manager.memoizedBundleVerification(removed, verify); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(removed); err != nil {
		t.Fatal(err)
	}
	retained := writeBundleFixture(t)
	if _, err := manager.memoizedBundleVerification(retained, verify); err != nil {
		t.Fatal(err)
	}
	if _, remembered := manager.rememberedBundleVerification(removed); remembered {
		t.Fatal("removed bundle directory retained its verification")
	}
	if _, remembered := manager.rememberedBundleVerification(retained); !remembered {
		t.Fatal("present bundle directory lost its verification")
	}
}

// A restarted Runner is a new Manager over the same cache root.
func TestBundleVerificationRecordSurvivesRunnerRestart(t *testing.T) {
	records := privateRecordRoot(t)
	directory := writeBundleFixture(t)
	verifications := 0
	verify := func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		verifications++
		return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "rootfs", Path: filepath.Join(directory, "rootfs.ext4")}}, nil
	}
	if _, err := (&Manager{verificationRoot: records}).memoizedBundleVerification(directory, verify); err != nil {
		t.Fatal(err)
	}
	restarted := &Manager{verificationRoot: records}
	artifacts, err := restarted.memoizedBundleVerification(directory, verify)
	if err != nil {
		t.Fatal(err)
	}
	if verifications != 1 || len(artifacts) != 1 || artifacts[0].Label != "rootfs" {
		t.Fatalf("restarted Runner verifications = %d, artifacts = %+v", verifications, artifacts)
	}
	if err := os.WriteFile(filepath.Join(directory, "shared.img"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{verificationRoot: records}).memoizedBundleVerification(directory, verify); err != nil {
		t.Fatal(err)
	}
	if verifications != 2 {
		t.Fatalf("bundle replaced while the Runner was down produced %d verifications", verifications)
	}
}

// A record root the Runner does not hold privately is refused rather than
// trusted.
func TestBundleVerificationRecordsRefuseSharedOrLinkedRoots(t *testing.T) {
	directory := writeBundleFixture(t)
	verify := func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "rootfs", Path: filepath.Join(directory, "rootfs.ext4")}}, nil
	}
	shared := filepath.Join(t.TempDir(), "records")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{verificationRoot: shared}).memoizedBundleVerification(directory, verify); err == nil || !strings.Contains(err.Error(), "owner-only access") {
		t.Fatalf("shared record root error = %v", err)
	}
	linked := filepath.Join(t.TempDir(), "records")
	if err := os.Symlink(privateRecordRoot(t), linked); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Manager{verificationRoot: linked}).memoizedBundleVerification(directory, verify); err == nil || !strings.Contains(err.Error(), "verification record root open failed") {
		t.Fatalf("linked record root error = %v", err)
	}
}

func TestVerificationRootMustStayOutsideTheCache(t *testing.T) {
	cacheRoot := t.TempDir()
	for _, root := range []string{cacheRoot, filepath.Join(cacheRoot, "records"), filepath.Dir(cacheRoot), "relative/records"} {
		if err := validateVerificationRoot(cacheRoot, root); err == nil {
			t.Fatalf("record root %q was accepted for cache %q", root, cacheRoot)
		}
	}
	if err := validateVerificationRoot(cacheRoot, filepath.Join(t.TempDir(), "records")); err != nil {
		t.Fatal(err)
	}
}

func TestBundleVerificationRecordRejectsAnotherBundle(t *testing.T) {
	records := privateRecordRoot(t)
	recorded := writeBundleFixture(t)
	manager := &Manager{verificationRoot: records}
	if _, err := manager.memoizedBundleVerification(recorded, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "rootfs", Path: filepath.Join(recorded, "rootfs.ext4")}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	other := writeBundleFixture(t)
	if err := os.Rename(filepath.Join(records, verificationRecordName(recorded)), filepath.Join(records, verificationRecordName(other))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.loadVerificationRecord(other); err == nil || !strings.Contains(err.Error(), "names another bundle directory") {
		t.Fatalf("substituted record error = %v", err)
	}
}

func TestBundleVerificationRecordsPruneRemovedBundles(t *testing.T) {
	records := privateRecordRoot(t)
	verifyKernel := func(directory string) func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		return func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
			return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "kernel", Path: filepath.Join(directory, "kernel")}}, nil
		}
	}
	removed := writeBundleFixture(t)
	if _, err := (&Manager{verificationRoot: records}).memoizedBundleVerification(removed, verifyKernel(removed)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(removed); err != nil {
		t.Fatal(err)
	}
	retained := writeBundleFixture(t)
	if _, err := (&Manager{verificationRoot: records}).memoizedBundleVerification(retained, verifyKernel(retained)); err != nil {
		t.Fatal(err)
	}
	names, err := os.ReadDir(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].Name() != verificationRecordName(retained) {
		t.Fatalf("records after pruning = %v", names)
	}
}

// Starts of the fixed bundle take no digest lock, and starts of different
// digests prune the shared record root, so record access must serialize.
func TestBundleVerificationRecordsTolerateConcurrentWriters(t *testing.T) {
	manager := &Manager{verificationRoot: privateRecordRoot(t)}
	shared := writeBundleFixture(t)
	others := make([]string, 8)
	for index := range others {
		others[index] = writeBundleFixture(t)
	}
	evicted := writeBundleFixture(t)
	if _, err := manager.memoizedBundleVerification(evicted, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "kernel", Path: filepath.Join(evicted, "kernel")}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(evicted); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errs := make(chan error, 2*len(others))
	for _, directory := range append(others, slices.Repeat([]string{shared}, len(others))...) {
		group.Go(func() {
			_, err := manager.memoizedBundleVerification(directory, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
				return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "kernel", Path: filepath.Join(directory, "kernel")}}, nil
			})
			errs <- err
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// privateRecordRoot is a record root the Runner creates on first use.
func privateRecordRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "verifications")
}

// writeBundleFixture writes every file a bundle verification reads. The
// content is irrelevant here; only its filesystem identity is under test.
func writeBundleFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, name := range config.BundleArtifactFileNames {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("fixture:"+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}
