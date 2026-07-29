package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsTailIsBoundedAndRejectsSymbolicLinks(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "control-plane.jsonl")
	if err := os.WriteFile(logPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runLogsCommand(
		context.Background(), "tail",
		[]string{"--path", logPath, "--bytes", "4"}, &output,
	); err != nil {
		t.Fatal(err)
	}
	if output.String() != "6789" {
		t.Fatalf("bounded tail = %q, want %q", output.String(), "6789")
	}

	linkPath := filepath.Join(t.TempDir(), "control-plane-link.jsonl")
	if err := os.Symlink(logPath, linkPath); err != nil {
		t.Fatal(err)
	}
	err := runLogsCommand(
		context.Background(), "tail",
		[]string{"--path", linkPath, "--bytes", "4"}, &output,
	)
	if err == nil || !strings.Contains(err.Error(), "non-symbolic-link regular file") {
		t.Fatalf("symbolic-link error = %v", err)
	}
}
