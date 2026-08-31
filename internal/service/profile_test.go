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
