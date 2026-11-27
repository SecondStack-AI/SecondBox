//go:build linux

package gvisor

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func TestValidateConfigNetworkCapacity(t *testing.T) {
	// Asset loading follows capacity validation. An absent manifest lets this
	// test distinguish accepted capacity from a rejected bound without runsc.
	root := t.TempDir()
	config := Config{
		RunscPath: root + "/runsc", AgentPath: root + "/agent", FlatRootPath: root + "/flat",
		MaterializationPath: root + "/absent.json", RuntimeDir: "/run/secondbox-capacity-test",
		MaterializationDigest: "sha256:" + strings.Repeat("a", 64),
		WorkspaceRoot:         root + "/store", SelfExecutable: root + "/runner",
		ExecutionImageCacheRoot: root + "/images", ExecutionImageVerificationRoot: root + "/records",
		ExecutionImageFetcherSocket: root + "/fetcher.sock", WorkspaceStore: &workspacestore.Store{},
		MaximumVCPUs: 1, MaximumMemoryBytes: 1 << 30, MaximumDiskBytes: 1 << 30, MaximumOperations: 1,
		NetworkPolicy: networkpolicy.RunnerConfig{
			DNSUpstream:    netip.MustParseAddrPort("127.0.0.1:53"),
			CompileOptions: networkpolicy.CompileOptions{MaximumPins: 8, MaximumTTL: time.Minute},
		},
	}
	for _, capacity := range []uint32{1, 63, 64, 65, 256, 257, 999, 1000, 1024, 1025, ^uint32(0)} {
		config.MaximumInstances = capacity
		_, err := validateConfig(config)
		if capacity <= 1024 {
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("capacity %d did not reach asset loading: %v", capacity, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "at most 1024 concurrent Instances") {
			t.Errorf("capacity %d: %v", capacity, err)
		}
	}
	config.MaximumInstances = 1024
	config.NetworkProfile = 16
	if _, err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "network profile must be below 16") {
		t.Fatalf("profile 16: %v", err)
	}
}

// TestValidateRuntimeDirRefusesDestructivePaths proves the runtime directory
// - whose startup reconciliation removes every child - can never point at the
// filesystem root, a symlinked path, a Workspace, flat-root, or execution image cache overlap, or a
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
	imageCache := filepath.Join(root, "images")
	for name, runtimeDir := range map[string]string{
		"filesystem root":         "/",
		"first level":             "/run",
		"workspace root itself":   workspaceRoot,
		"workspace root ancestor": root,
		"workspace descendant":    filepath.Join(workspaceRoot, "runtime"),
		"flat root descendant":    filepath.Join(flatRoot, "runtime"),
		"image cache itself":      imageCache,
		"image cache descendant":  filepath.Join(imageCache, "runtime"),
		"socket path overflow":    filepath.Join(root, strings.Repeat("r", maximumRuntimeDirLength)),
	} {
		if err := validateRuntimeDir(runtimeDir, workspaceRoot, flatRoot, imageCache); err == nil {
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
	if err := validateRuntimeDir(linked, workspaceRoot, flatRoot, imageCache); err == nil {
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
	if err := validateRuntimeDir(filepath.Join(aliasedWorkspace, "runtime"), workspaceLink, flatRoot, imageCache); err == nil {
		t.Error("runtime directory beneath a symlink-aliased workspace root was accepted")
	}

	valid := filepath.Join(root, "runtime")
	if err := os.Mkdir(valid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeDir(valid, workspaceRoot, flatRoot, imageCache); err != nil {
		t.Errorf("disjoint runtime directory rejected: %v", err)
	}
	if err := validateRuntimeDir(filepath.Join(root, "absent"), workspaceRoot, flatRoot, imageCache); err != nil {
		t.Errorf("not-yet-created runtime directory rejected: %v", err)
	}
	if err := validateRuntimeDir(filepath.Join(linked, "absent", "runtime"), workspaceRoot, flatRoot, imageCache); err == nil {
		t.Error("nonexistent runtime directory beneath a symlinked ancestor was accepted")
	}
}
