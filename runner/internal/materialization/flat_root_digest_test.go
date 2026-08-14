package materialization

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDigestFlatRootCoversContentMetadataLinksAndXattrs(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "payload")
	if err := os.WriteFile(file, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "current")
	if err := os.Symlink("payload", link); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lsetxattr(file, "user.secondbox.digest-test", []byte{0, 1, 2, 255}, 0); err != nil {
		t.Fatal(err)
	}
	original, err := DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if original != again {
		t.Fatalf("stable flat-root digest changed: %s != %s", original, again)
	}

	assertChangesDigest := func(name string, mutate func()) {
		t.Helper()
		mutate()
		changed, err := DigestFlatRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		if changed == original {
			t.Fatalf("%s did not change flat-root digest", name)
		}
	}
	assertChangesDigest("file bytes", func() {
		if err := os.WriteFile(file, []byte("two"), 0o640); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(file, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	original, err = DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	assertChangesDigest("mode", func() {
		if err := os.Chmod(file, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	original, err = DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	assertChangesDigest("xattr", func() {
		if err := unix.Lsetxattr(file, "user.secondbox.digest-test", []byte("changed"), 0); err != nil {
			t.Fatal(err)
		}
	})
	if err := unix.Lsetxattr(file, "user.secondbox.digest-test", []byte{0, 1, 2, 255}, 0); err != nil {
		t.Fatal(err)
	}
	original, err = DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	assertChangesDigest("symlink target", func() {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("missing", link); err != nil {
			t.Fatal(err)
		}
	})
}
