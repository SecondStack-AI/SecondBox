package executionimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func TestBundleVerificationRepeatsOnlyWhenBundleMetadataChanges(t *testing.T) {
	manager := &Manager{}
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
	manager := &Manager{}
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
	manager := &Manager{}
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
