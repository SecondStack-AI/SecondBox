//go:build linux

package gvisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateRuntimeDirRefusesDestructivePaths proves the runtime directory
// - whose startup reconciliation removes every child - can never point at the
// filesystem root, a symlinked path, a Workspace or flat-root overlap, or a
// path long enough to push per-Instance socket paths past the Unix limit.
func TestValidateRuntimeDirRefusesDestructivePaths(t *testing.T) {
	// The socket-path bound makes long paths invalid by design, so the test
	// works under a deliberately short root rather than t.TempDir().
	root, err := os.MkdirTemp("/tmp", "gvr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	workspaceRoot := filepath.Join(root, "workspaces")
	flatRoot := filepath.Join(root, "flat")
	for name, runtimeDir := range map[string]string{
		"filesystem root":         "/",
		"first level":             "/run",
		"workspace root itself":   workspaceRoot,
		"workspace root ancestor": root,
		"workspace descendant":    filepath.Join(workspaceRoot, "runtime"),
		"flat root descendant":    filepath.Join(flatRoot, "runtime"),
		"socket path overflow":    filepath.Join(root, strings.Repeat("r", maximumRuntimeDirLength)),
	} {
		if err := validateRuntimeDir(runtimeDir, workspaceRoot, flatRoot); err == nil {
			t.Errorf("%s runtime directory %q was accepted", name, runtimeDir)
		}
	}

	linked := filepath.Join(root, "linked")
	if err := os.Symlink(filepath.Join(root, "real"), linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeDir(linked, workspaceRoot, flatRoot); err == nil {
		t.Error("symlinked runtime directory was accepted")
	}

	aliasedWorkspace := filepath.Join(root, "real-workspaces")
	if err := os.Mkdir(aliasedWorkspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceLink := filepath.Join(root, "workspace-link")
	if err := os.Symlink(aliasedWorkspace, workspaceLink); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeDir(filepath.Join(aliasedWorkspace, "runtime"), workspaceLink, flatRoot); err == nil {
		t.Error("runtime directory beneath a symlink-aliased workspace root was accepted")
	}

	valid := filepath.Join(root, "runtime")
	if err := os.Mkdir(valid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeDir(valid, workspaceRoot, flatRoot); err != nil {
		t.Errorf("disjoint runtime directory rejected: %v", err)
	}
	if err := validateRuntimeDir(filepath.Join(root, "absent"), workspaceRoot, flatRoot); err != nil {
		t.Errorf("not-yet-created runtime directory rejected: %v", err)
	}
}
