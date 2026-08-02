package store

import (
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestQuotaWouldExceedEverySubjectLimit(t *testing.T) {
	base := contracts.QuotaLimits{
		MaxSandboxes: 10, MaxActiveInstances: 10, MaxCPUMillis: 10,
		MaxMemoryBytes: 10, MaxArtifactBytes: 10, MaxSnapshots: 10,
		MaxArtifacts: 10, MaxPortSessions: 10, MaxConcurrentOperations: 10,
	}
	tests := map[string]struct {
		usage           quotaUsage
		requestedCPU    int64
		requestedMemory int64
		requestedActive int64
	}{
		"sandboxes":             {usage: quotaUsage{sandboxes: 10}},
		"active instances":      {usage: quotaUsage{activeInstances: 10}, requestedActive: 1},
		"CPU":                   {usage: quotaUsage{cpuMillis: 10}, requestedCPU: 1},
		"memory":                {usage: quotaUsage{memoryBytes: 10}, requestedMemory: 1},
		"Artifact bytes":        {usage: quotaUsage{artifactBytes: 11}},
		"snapshots":             {usage: quotaUsage{snapshots: 11}},
		"artifacts":             {usage: quotaUsage{artifacts: 11}},
		"port sessions":         {usage: quotaUsage{portSessions: 11}},
		"concurrent operations": {usage: quotaUsage{concurrentOperations: 11}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if !quotaWouldExceed(
				base, test.usage, test.requestedCPU, test.requestedMemory, test.requestedActive,
			) {
				t.Fatalf("%s limit was not exceeded", name)
			}
		})
	}
	if quotaWouldExceed(base, quotaUsage{}, 1, 1, 1) {
		t.Fatal("in-limit reservation was rejected")
	}
}
