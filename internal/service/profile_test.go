package service

import (
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestValidateProfileRevisionSpecRequiresExplicitTenantEgressContextPolicy(t *testing.T) {
	spec := validProfileRevisionSpecForValidation()
	spec.Network.RequiresTenantEgressContext = nil
	if err := validateProfileRevisionSpec(spec); err == nil || !strings.Contains(err.Error(), "requiresTenantEgressContext") {
		t.Fatalf("omitted requiresTenantEgressContext error = %v", err)
	}

	spec.Network.RequiresTenantEgressContext = new(bool)
	if err := validateProfileRevisionSpec(spec); err != nil {
		t.Fatalf("explicit false requiresTenantEgressContext rejected: %v", err)
	}
}

func TestValidateProfileAttributedExecutionPolicy(t *testing.T) {
	for _, test := range []struct {
		name            string
		gateway         string
		connections     int64
		requiresContext bool
		valid           bool
	}{
		{"valid", "agent-runner-gateway", 32, true, true},
		{"missing context", "agent-runner-gateway", 32, false, false},
		{"missing gateway", "", 32, true, false},
		{"gateway IP", "127.0.0.1", 32, true, false},
		{"gateway URL", "https://gateway.example", 32, true, false},
		{"gateway wildcard", "*.example", 32, true, false},
		{"noncanonical gateway", "Gateway.example.", 32, true, false},
		{"missing connection bound", "gateway.example", 0, true, false},
		{"excess connection bound", "gateway.example", 4097, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := validProfileRevisionSpecForValidation()
			spec.Network.RequiresTenantEgressContext = &test.requiresContext
			spec.AttributedExecution = &contracts.AttributedExecutionPolicy{
				Gateway: test.gateway, MaximumConnections: test.connections,
			}
			if err := validateProfileRevisionSpec(spec); (err == nil) != test.valid {
				t.Fatalf("profile validation = %v; valid = %v", err, test.valid)
			}
		})
	}
}

func validProfileRevisionSpecForValidation() contracts.ProfileRevisionSpec {
	return contracts.ProfileRevisionSpec{
		Pool:                  "pool",
		Architecture:          "amd64",
		RuntimeBundleDigest:   "sha256:" + strings.Repeat("a", 64),
		ToolchainBundleDigest: "sha256:" + strings.Repeat("b", 64),
		Resources: contracts.ResourcePolicy{
			VCPUCount: 1, MemoryBytes: 1, WorkspaceBytes: 1, ConcurrentOperations: 1,
		},
		Startup: contracts.StartupPolicy{Mode: contracts.StartupModeColdBoot},
		Lifecycle: contracts.LifecyclePolicy{
			InitialState: contracts.SandboxDesiredStateStopped, DrainGraceSeconds: 1,
			IdleSeconds: 1, MaximumDurationSeconds: 1, LeaseSeconds: 1,
		},
		Retention: contracts.RetentionPolicy{SnapshotRetentionSeconds: 1},
		Execution: contracts.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 1, MaximumBufferedOutputBytes: 1,
			StreamWindowBytes: 4096, MaximumTransferBytes: 1,
			DataPlaneTransport: contracts.DataPlaneTransportProxied,
		},
		Network: contracts.NetworkPolicy{
			Mode: "deny_all", Destinations: []contracts.NetworkDestination{},
			RequiresTenantEgressContext: new(bool),
		},
		Ports: []contracts.PortPolicy{},
	}
}
