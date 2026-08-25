package main

import (
	"os"
	"path/filepath"
	"testing"
)

func runWithArguments(t *testing.T, arguments ...string) int {
	t.Helper()
	original := os.Args
	defer func() { os.Args = original }()
	os.Args = append([]string{"guest"}, arguments...)
	return run()
}

func TestRunRejectsUsageErrors(t *testing.T) {
	if got := runWithArguments(t); got != 2 {
		t.Errorf("no arguments exit = %d, want 2", got)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	if got := runWithArguments(t, "unknown", marker); got != 2 {
		t.Errorf("unknown mode exit = %d, want 2", got)
	}
}

func TestRunHelloWritesMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	if got := runWithArguments(t, "hello", marker); got != 0 {
		t.Fatalf("hello exit = %d, want 0", got)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if string(content) != "secondbox-gvisor-probe-guest hello\n" {
		t.Errorf("marker content = %q", content)
	}
}

func TestRunFailPropagatesExitCode(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	if got := runWithArguments(t, "fail", marker); got != failExitCode {
		t.Errorf("fail exit = %d, want %d", got, failExitCode)
	}
}

func TestRunReportsUnwritableMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "absent", "marker")
	if got := runWithArguments(t, "hello", marker); got != 1 {
		t.Errorf("unwritable marker exit = %d, want 1", got)
	}
}
