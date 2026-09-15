package executionimage

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

func TestVerifiedArtifactsRetainIdentityAcrossPublication(t *testing.T) {
	staging := t.TempDir()
	for _, name := range []string{"kernel", "rootfs.ext4", "shared.img"} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifacts := make([]runtimemanager.VerifiedExecutionImageArtifact, 0, 3)
	for _, artifact := range []struct{ label, name string }{{"kernel", "kernel"}, {"rootfs", "rootfs.ext4"}, {"shared image", "shared.img"}} {
		identity, err := runtimemanager.CaptureVerifiedExecutionImageArtifact(artifact.label, filepath.Join(staging, artifact.name))
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, identity)
	}
	published := filepath.Join(filepath.Dir(staging), "published")
	if err := os.Rename(staging, published); err != nil {
		t.Fatal(err)
	}
	if _, err := relocateVerifiedArtifacts(published, artifacts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(published, "kernel"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := relocateVerifiedArtifacts(published, artifacts); err == nil || !strings.Contains(err.Error(), "changed during cache publication") {
		t.Fatalf("replacement identity error = %v", err)
	}
}

func TestStoragePressureReclaimsOnlyUnusedImages(t *testing.T) {
	manager := &Manager{cacheRoot: t.TempDir(), pins: make(map[string]int)}
	for _, letter := range []string{"a", "b", "c"} {
		directory := filepath.Join(manager.cacheRoot, strings.Repeat(letter, 64))
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if letter == "b" {
			if err := retainPreparedDirectory(directory, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := manager.reclaimUnusedImages("registry.example/agent@sha256:" + strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	for _, letter := range []string{"a", "b", "c"} {
		_, err := os.Stat(filepath.Join(manager.cacheRoot, strings.Repeat(letter, 64)))
		if letter == "a" && !errors.Is(err, os.ErrNotExist) || letter != "a" && err != nil {
			t.Fatalf("cache entry %s after reclamation: %v", letter, err)
		}
	}
}

func TestPreparationCapacityBytesRejectsOverflow(t *testing.T) {
	if got, err := preparationCapacityBytes(8<<30, 16<<30); err != nil || got != 32<<30 {
		t.Fatalf("preparation capacity = %d, %v", got, err)
	}
	if _, err := preparationCapacityBytes(int64(^uint64(0)>>1), 2); err == nil {
		t.Fatal("overflowing preparation capacity was accepted")
	}
}

func TestPreparationCapacityEvictsBeforeRetryingPressureAdmission(t *testing.T) {
	root := t.TempDir()
	oldest := filepath.Join(root, strings.Repeat("a", 64))
	if err := os.Mkdir(oldest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldest, "rootfs.ext4"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{cacheRoot: root, pins: make(map[string]int)}
	reservations := 0
	release, err := manager.reservePreparationCapacity(
		t.Context(),
		filepath.Join(root, strings.Repeat("b", 64)),
		1024,
		func(context.Context, uint64) (func() error, error) {
			reservations++
			if _, statErr := os.Stat(oldest); statErr == nil {
				return nil, ErrCapacityAdmissionDenied
			}
			return func() error { return nil }, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if release == nil || reservations != 2 {
		t.Fatalf("capacity reservation release=%v attempts=%d", release != nil, reservations)
	}
	if _, err := os.Stat(oldest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pressure eviction retained oldest cache entry: %v", err)
	}
}

func TestOuterDockerArchiveAcceptsContainedLayerLink(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "image.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for _, header := range []*tar.Header{
		{Name: "blob.tar", Mode: 0o400, Size: 4, Typeflag: tar.TypeReg},
		{Name: "layer/layer.tar", Linkname: "../blob.tar", Typeflag: tar.TypeSymlink},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(writer, "data"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "extracted")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := extractTarFile(t.Context(), archive, target, "", &byteBudget{maximum: 1024}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(target, "layer", "layer.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "data" {
		t.Fatalf("hard-linked layer content = %q", content)
	}
}

func TestLayerWhiteoutsAndReplacementProduceFinalBundle(t *testing.T) {
	target := t.TempDir()
	first := filepath.Join(t.TempDir(), "first.tar")
	writeTestTar(t, first, []*tar.Header{
		{Name: selectedBundleDirectory + "/keep", Mode: 0o600, Size: 3, Typeflag: tar.TypeReg},
		{Name: selectedBundleDirectory + "/remove", Mode: 0o600, Size: 3, Typeflag: tar.TypeReg},
	}, []string{"old", "old"})
	second := filepath.Join(t.TempDir(), "second.tar")
	writeTestTar(t, second, []*tar.Header{
		{Name: selectedBundleDirectory + "/.wh.remove", Typeflag: tar.TypeChar},
		{Name: selectedBundleDirectory + "/keep", Mode: 0o600, Size: 3, Typeflag: tar.TypeReg},
	}, []string{"", "new"})
	budget := &byteBudget{maximum: 1024}
	if err := extractTarFile(t.Context(), first, target, selectedBundleDirectory+"/", budget); err != nil {
		t.Fatal(err)
	}
	if err := applyLayerWhiteouts(t.Context(), second, target, selectedBundleDirectory+"/"); err != nil {
		t.Fatal(err)
	}
	if err := extractTarFile(t.Context(), second, target, selectedBundleDirectory+"/", budget); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "keep")); err != nil || string(data) != "new" {
		t.Fatalf("replacement = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(target, "remove")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("whiteout target remains: %v", err)
	}
}

func TestDigestLockStopsAtContextDeadline(t *testing.T) {
	manager := &Manager{cacheRoot: t.TempDir()}
	digest := "sha256:" + strings.Repeat("a", 64)
	first, err := manager.lockDigest(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.lockDigest(ctx, digest); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended digest lock = %v, want deadline", err)
	}
}

func TestExtractionBudgetRejectsOversizedEntry(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "oversized.tar")
	writeTestTar(t, archive, []*tar.Header{{Name: "large", Mode: 0o600, Size: 4, Typeflag: tar.TypeReg}}, []string{"data"})
	err := extractTarFile(t.Context(), archive, t.TempDir(), "", &byteBudget{maximum: 3})
	if err == nil || !strings.Contains(err.Error(), "exceeds 3 bytes") {
		t.Fatalf("oversized extraction = %v", err)
	}
}

func writeTestTar(t *testing.T, path string, headers []*tar.Header, bodies []string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for index, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if bodies[index] != "" {
			if _, err := io.WriteString(writer, bodies[index]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(writer.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOuterDockerArchiveRejectsEscapingLayerLink(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "image.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	if err := writer.WriteHeader(&tar.Header{
		Name: "layer/layer.tar", Linkname: "../../outside.tar", Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "extracted")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := extractTarFile(t.Context(), archive, target, "", &byteBudget{maximum: 1024}); err == nil || !strings.Contains(err.Error(), "link target is unsafe") {
		t.Fatalf("escaping link error = %v", err)
	}
}

func TestOperationResolutionIsDurableAndStable(t *testing.T) {
	root := t.TempDir()
	counter := filepath.Join(root, "counter")
	skopeo := filepath.Join(root, "skopeo")
	script := "#!/bin/sh\ncount=0\n[ ! -f '" + counter + "' ] || count=$(cat '" + counter + "')\ncount=$((count + 1))\nprintf '%s' \"$count\" > '" + counter + "'\nprintf '%s\\n' 'sha256:" + strings.Repeat("a", 64) + "'\n"
	if err := os.WriteFile(skopeo, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{cacheRoot: root, skopeoPath: skopeo}
	reference := "registry.example/agents/coding:stable"
	first, err := manager.resolveOperationDigest(context.Background(), "operation-one", reference, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.resolveOperationDigest(context.Background(), "operation-one", reference, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("resolved digests = %q, %q", first, second)
	}
	if calls, err := os.ReadFile(counter); err != nil || string(calls) != "1" {
		t.Fatalf("registry resolution calls = %q, error = %v", calls, err)
	}
}

func TestOperationResolutionRejectsReferenceSubstitution(t *testing.T) {
	root := t.TempDir()
	skopeo := filepath.Join(root, "skopeo")
	if err := os.WriteFile(skopeo, []byte("#!/bin/sh\nprintf '%s\\n' 'sha256:"+strings.Repeat("b", 64)+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{cacheRoot: root, skopeoPath: skopeo}
	if _, err := manager.resolveOperationDigest(context.Background(), "operation-one", "registry.example/agents/a:stable", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.resolveOperationDigest(context.Background(), "operation-one", "registry.example/agents/b:stable", "", nil); err == nil || !strings.Contains(err.Error(), "persisted execution image resolution") {
		t.Fatalf("reference substitution error = %v", err)
	}
}

func TestRegistryCertificateArgsRequireExplicitCA(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{registryCertificates: root}
	if args := manager.registryCertificateArgs("registry.example:5443", "--cert-dir"); args != nil {
		t.Fatalf("certificate arguments without CA = %#v", args)
	}
	directory := filepath.Join(root, "registry.example:5443")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ca.crt"), []byte("test CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspectWant := []string{"--cert-dir", directory}
	if args := manager.registryCertificateArgs("registry.example:5443", "--cert-dir"); !slices.Equal(args, inspectWant) {
		t.Fatalf("inspect certificate arguments = %#v, want %#v", args, inspectWant)
	}
	copyWant := []string{"--src-cert-dir", directory}
	if args := manager.registryCertificateArgs("registry.example:5443", "--src-cert-dir"); !slices.Equal(args, copyWant) {
		t.Fatalf("copy certificate arguments = %#v, want %#v", args, copyWant)
	}
}
