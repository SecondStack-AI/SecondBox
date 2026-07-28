package main

import (
	"path/filepath"
	"testing"
)

func TestValidateRunnerExecutionIdentityAllowsUnprivilegedProtocolProbeOnly(t *testing.T) {
	for _, test := range []struct {
		name        string
		healthcheck bool
		uid         int
		wantErr     bool
	}{
		{name: "root runner", uid: 0},
		{name: "root healthcheck", healthcheck: true, uid: 0},
		{name: "unprivileged healthcheck", healthcheck: true, uid: 1234},
		{name: "unprivileged runner", uid: 1234, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateRunnerExecutionIdentity(test.healthcheck, test.uid)
			if (err != nil) != test.wantErr {
				t.Fatalf(
					"validateRunnerExecutionIdentity(%t, %d) error = %v, wantErr %t",
					test.healthcheck,
					test.uid,
					err,
					test.wantErr,
				)
			}
		})
	}
}

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
