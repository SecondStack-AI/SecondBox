//go:build linux

package gvisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
)

func TestPrepareFlatRootMaterializesMinimalRootAndIsDigestStable(t *testing.T) {
	root := t.TempDir()
	if err := PrepareFlatRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFlatRoot(root); err != nil {
		t.Fatal(err)
	}
	for _, target := range requiredFlatRootTargets {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(target.path)))
		if err != nil {
			t.Fatalf("inspect %s: %v", target.path, err)
		}
		if target.directory != info.IsDir() || (!target.directory && !info.Mode().IsRegular()) {
			t.Fatalf("%s has mode %s", target.path, info.Mode())
		}
	}
	first, err := materialization.DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareFlatRoot(root); err != nil {
		t.Fatal(err)
	}
	second, err := materialization.DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent preparation changed digest: %s != %s", first, second)
	}
}

func TestPrepareFlatRootPreservesExistingValidTargets(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workspace"), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secondbox-guest-agent"), []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareFlatRoot(root); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(root, "secondbox-guest-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "placeholder" {
		t.Fatalf("existing placeholder changed to %q", payload)
	}
	info, err := os.Stat(filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o711 {
		t.Fatalf("existing directory mode changed to %o", info.Mode().Perm())
	}
}

func TestPrepareFlatRootRejectsConflictsAndSymlinks(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(string) error
		want    string
	}{
		{name: "directory is file", want: "/workspace", prepare: func(root string) error {
			return os.WriteFile(filepath.Join(root, "workspace"), nil, 0o600)
		}},
		{name: "file is directory", want: "/secondbox-guest-agent", prepare: func(root string) error {
			return os.Mkdir(filepath.Join(root, "secondbox-guest-agent"), 0o700)
		}},
		{name: "target symlink", want: "symlink", prepare: func(root string) error {
			return os.Symlink("elsewhere", filepath.Join(root, "workspace"))
		}},
		{name: "parent symlink", want: "symlink", prepare: func(root string) error {
			if err := os.Mkdir(filepath.Join(root, "real-etc"), 0o700); err != nil {
				return err
			}
			return os.Symlink("real-etc", filepath.Join(root, "etc"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := test.prepare(root); err != nil {
				t.Fatal(err)
			}
			err := PrepareFlatRoot(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateFlatRootDoesNotRepairMissingTargets(t *testing.T) {
	root := t.TempDir()
	err := ValidateFlatRoot(root)
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("validation error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "secondbox-guest-agent")); !os.IsNotExist(statErr) {
		t.Fatalf("validation mutated the flat root: %v", statErr)
	}
}

func TestPrepareFlatRootPreflightsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareFlatRoot(root); err == nil || !strings.Contains(err.Error(), "/tmp") {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "secondbox-guest-agent")); !os.IsNotExist(err) {
		t.Fatalf("preflight failure partially mutated the root: %v", err)
	}
}

func TestPrepareFlatRootRejectsFilesystemRootAndSymlinkedAncestors(t *testing.T) {
	if err := PrepareFlatRoot(string(filepath.Separator)); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("filesystem root error = %v", err)
	}

	parent := t.TempDir()
	resolvedParent := filepath.Join(parent, "resolved")
	resolvedRoot := filepath.Join(resolvedParent, "rootfs")
	if err := os.MkdirAll(resolvedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lexicalParent := filepath.Join(parent, "lexical")
	if err := os.Symlink(resolvedParent, lexicalParent); err != nil {
		t.Fatal(err)
	}
	lexicalRoot := filepath.Join(lexicalParent, "rootfs")
	if err := PrepareFlatRoot(lexicalRoot); err == nil || !strings.Contains(err.Error(), "symlink ancestors") {
		t.Fatalf("symlink ancestor error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(resolvedRoot, "secondbox-guest-agent")); !os.IsNotExist(err) {
		t.Fatalf("symlinked root was mutated: %v", err)
	}
}
