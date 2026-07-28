package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func shortUnixSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sb-uds-")
	if err != nil {
		t.Fatalf("create short Unix socket directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove short Unix socket directory: %v", err)
		}
	})
	path := filepath.Join(dir, name)
	if err := checkUnixSocketPath("test", path, "test fixture"); err != nil {
		t.Fatal(err)
	}
	return path
}
