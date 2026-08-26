//go:build linux

package main

import "testing"

func TestLinuxRunnerExecutionIdentityAllowsUnprivilegedProtocolProbeOnly(t *testing.T) {
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
