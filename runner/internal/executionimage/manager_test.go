package executionimage

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
	if err := extractTarFile(archive, target, ""); err != nil {
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
	if err := extractTarFile(archive, target, ""); err == nil || !strings.Contains(err.Error(), "link target is unsafe") {
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
