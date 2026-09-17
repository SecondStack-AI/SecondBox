package firecracker

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionImageCacheMustShareTheRunDirFilesystem(t *testing.T) {
	cacheRoot := t.TempDir()
	if err := validateExecutionImageCacheFilesystem(cacheRoot, t.TempDir()); err != nil {
		t.Fatalf("one filesystem was rejected: %v", err)
	}
	// /dev is its own filesystem on every host that runs a Runner.
	err := validateExecutionImageCacheFilesystem(cacheRoot, "/dev")
	if err == nil || !strings.Contains(err.Error(), "are on different filesystems") {
		t.Fatalf("separate filesystems error = %v", err)
	}
	err = validateExecutionImageCacheFilesystem(filepath.Join(cacheRoot, "absent"), cacheRoot)
	if err == nil || !strings.Contains(err.Error(), "SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT") {
		t.Fatalf("absent cache root error = %v", err)
	}
}
