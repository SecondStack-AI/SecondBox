package main

import (
	"path/filepath"
	"testing"
)

func TestConfigureRunnerLoggingReturnsCloseFailure(t *testing.T) {
	closeLog, err := configureRunnerLogging(filepath.Join(t.TempDir(), "runner.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeLog(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := closeLog(); err == nil {
		t.Fatal("second close must surface the log file close failure")
	}
}
