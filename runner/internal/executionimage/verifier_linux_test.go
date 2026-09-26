//go:build linux

package executionimage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage/executionimagetest"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"golang.org/x/sys/unix"
)

const verifierTestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func noVerifierProgress(runnerprotocol.AssignmentProgressStage) error { return nil }

// reflinkCacheRoot returns a cache root on the explicit reflink qualification
// filesystem, the same prerequisite the WorkspaceStore reflink tests use.
func reflinkCacheRoot(t *testing.T) string {
	t.Helper()
	parent := os.Getenv("SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM")
	if parent == "" {
		t.Skip("real reflink qualification filesystem must be explicit")
	}
	root, err := os.MkdirTemp(parent, "secondbox-execution-image-cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func publishCachedBundle(t *testing.T, cacheRoot string, rootfs []byte) *executionimagetest.Publisher {
	t.Helper()
	publisher := executionimagetest.NewPublisher(t)
	directory := filepath.Join(cacheRoot, strings.TrimPrefix(verifierTestDigest, "sha256:"))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "rootfs.ext4"), rootfs, 0o600); err != nil {
		t.Fatal(err)
	}
	publisher.WriteBundle(t, directory, nil)
	return publisher
}

func TestVerifierRefusesUnpinnedTrustAndHasNoFixedBundle(t *testing.T) {
	publisher := executionimagetest.NewPublisher(t)
	if _, err := NewVerifier("relative/cache", publisher.PublicKeyPath, publisher.PublicKeySHA256); err == nil {
		t.Fatal("relative cache root was accepted")
	}
	if _, err := NewVerifier(t.TempDir(), publisher.PublicKeyPath, strings.Repeat("0", 64)); err == nil ||
		!strings.Contains(err.Error(), "does not match its pinned fingerprint") {
		t.Fatalf("mismatched publisher fingerprint error = %v", err)
	}
	verifier, err := NewVerifier(t.TempDir(), publisher.PublicKeyPath, publisher.PublicKeySHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyLocal(t.Context(), nil, noVerifierProgress); err == nil ||
		!strings.Contains(err.Error(), "no fixed signed bundle") {
		t.Fatalf("fixed-bundle request on a verifier error = %v", err)
	}
}

func TestCloneVerifiedRootfsRefusesAReplacedFile(t *testing.T) {
	cacheRoot := t.TempDir()
	publisher := publishCachedBundle(t, cacheRoot, []byte("verified-root"))
	verifier, err := NewVerifier(cacheRoot, publisher.PublicKeyPath, publisher.PublicKeySHA256)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := verifier.VerifyLocal(t.Context(), &runnerprotocol.ExecutionImage{Reference: "registry.example/app@" + verifierTestDigest}, noVerifierProgress)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Release()
	rootfs := filepath.Join(prepared.Directory, "rootfs.ext4")
	replacement := rootfs + ".replacement"
	if err := os.WriteFile(replacement, []byte("unverified-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, rootfs); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.CloneVerifiedRootfs(prepared); err == nil ||
		!strings.Contains(err.Error(), "changed after verification") {
		t.Fatalf("replaced rootfs clone error = %v", err)
	}
}

func TestCloneVerifiedRootfsReflinksAnUnnamedCopy(t *testing.T) {
	cacheRoot := reflinkCacheRoot(t)
	content := bytes.Repeat([]byte("selected-root-block"), 4096)
	publisher := publishCachedBundle(t, cacheRoot, content)
	verifier, err := NewVerifier(cacheRoot, publisher.PublicKeyPath, publisher.PublicKeySHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.ProbeRootfsCloning(); err != nil {
		t.Fatalf("reflink cache root failed its startup probe: %v", err)
	}
	prepared, err := verifier.VerifyLocal(context.Background(), &runnerprotocol.ExecutionImage{Reference: "registry.example/app@" + verifierTestDigest}, noVerifierProgress)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := verifier.CloneVerifiedRootfs(prepared)
	prepared.Release()
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(clone.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Nlink != 0 {
		t.Fatalf("rootfs clone has %d names; it must be unnamed", stat.Nlink)
	}
	cloned, err := io.ReadAll(io.NewSectionReader(clone, 0, stat.Size))
	if err != nil || !bytes.Equal(cloned, content) {
		t.Fatalf("rootfs clone content differs: %v", err)
	}
	if _, err := clone.WriteAt([]byte("guest-private"), 0); err != nil {
		t.Fatal(err)
	}
	cached, err := os.ReadFile(filepath.Join(prepared.Directory, "rootfs.ext4"))
	if err != nil || !bytes.Equal(cached, content) {
		t.Fatalf("writing the clone changed the verified cache entry: %v", err)
	}
}

func TestRootfsCloningProbeRejectsAFilesystemWithoutReflink(t *testing.T) {
	cacheRoot := t.TempDir()
	var filesystem unix.Statfs_t
	if err := unix.Statfs(cacheRoot, &filesystem); err != nil {
		t.Fatal(err)
	}
	if filesystem.Type == unix.BTRFS_SUPER_MAGIC || filesystem.Type == unix.XFS_SUPER_MAGIC {
		t.Skip("the temporary directory supports reflink; the refusal needs another filesystem")
	}
	publisher := executionimagetest.NewPublisher(t)
	verifier, err := NewVerifier(cacheRoot, publisher.PublicKeyPath, publisher.PublicKeySHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.ProbeRootfsCloning(); err == nil || !strings.Contains(err.Error(), "cannot reflink") {
		t.Fatalf("non-reflink probe error = %v", err)
	}
}
