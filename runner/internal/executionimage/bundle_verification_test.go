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
	directory, publicKeyPath := writeBundleFixture(t)
	verifications := 0
	verify := func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		verifications++
		return []runtimemanager.VerifiedExecutionImageArtifact{{Label: "rootfs", Path: filepath.Join(directory, "rootfs.ext4")}}, nil
	}
	for range 4 {
		artifacts, err := manager.memoizedBundleVerification(directory, publicKeyPath, verify)
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
	if _, err := manager.memoizedBundleVerification(directory, publicKeyPath, verify); err != nil {
		t.Fatal(err)
	}
	if verifications != 2 {
		t.Fatalf("replaced rootfs produced %d verifications", verifications)
	}
	if err := os.WriteFile(publicKeyPath, []byte("replacement key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.memoizedBundleVerification(directory, publicKeyPath, verify); err != nil {
		t.Fatal(err)
	}
	if verifications != 3 {
		t.Fatalf("replaced signing key produced %d verifications", verifications)
	}
}

func TestBundleVerificationRejectsBundleChangedDuringVerification(t *testing.T) {
	manager := &Manager{}
	directory, publicKeyPath := writeBundleFixture(t)
	_, err := manager.memoizedBundleVerification(directory, publicKeyPath, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
		return nil, os.WriteFile(filepath.Join(directory, "kernel"), []byte("substituted"), 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "changed during signature verification") {
		t.Fatalf("substitution during verification error = %v", err)
	}
	verifications := 0
	if _, err := manager.memoizedBundleVerification(directory, publicKeyPath, func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) {
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
	removed, publicKeyPath := writeBundleFixture(t)
	verify := func() ([]runtimemanager.VerifiedExecutionImageArtifact, error) { return nil, nil }
	if _, err := manager.memoizedBundleVerification(removed, publicKeyPath, verify); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(removed); err != nil {
		t.Fatal(err)
	}
	retained, retainedKeyPath := writeBundleFixture(t)
	if _, err := manager.memoizedBundleVerification(retained, retainedKeyPath, verify); err != nil {
		t.Fatal(err)
	}
	if _, remembered := manager.rememberedBundleVerification(removed); remembered {
		t.Fatal("removed bundle directory retained its verification")
	}
	if _, remembered := manager.rememberedBundleVerification(retained); !remembered {
		t.Fatal("present bundle directory lost its verification")
	}
}

// writeBundleFixture writes every file a signed bundle verification reads. The
// content is irrelevant here; only its filesystem identity is under test.
func writeBundleFixture(t *testing.T) (directory string, publicKeyPath string) {
	t.Helper()
	directory = t.TempDir()
	for _, name := range config.SignedArtifactFileNames {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("fixture:"+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	publicKeyPath = filepath.Join(t.TempDir(), "execution-image-public.pem")
	if err := os.WriteFile(publicKeyPath, []byte("fixture:public key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory, publicKeyPath
}
