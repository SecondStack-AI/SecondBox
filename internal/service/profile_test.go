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
			VCPUCount: 1, MemoryBytes: 64 << 20, WorkspaceBytes: 1 << 20, ConcurrentOperations: 1,
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

func TestValidateProfileResourceCeiling(t *testing.T) {
	pointer := func(value int64) *int64 { return &value }
	for _, test := range []struct {
		name    string
		ceiling contracts.ProfileResourceCeiling
		resume  bool
		valid   bool
	}{
		{"absent", nil, false, true},
		{"null axes", contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": nil}, false, true},
		{"equal bounds", contracts.ProfileResourceCeiling{"vcpuCount": pointer(1), "memoryBytes": pointer(64 << 20), "workspaceBytes": pointer(1 << 20)}, false, true},
		{"higher bounds", contracts.ProfileResourceCeiling{"vcpuCount": pointer(2), "memoryBytes": pointer(128 << 20), "workspaceBytes": pointer(2 << 20)}, false, true},
		{"below cpu", contracts.ProfileResourceCeiling{"vcpuCount": pointer(0), "memoryBytes": nil, "workspaceBytes": nil}, false, false},
		{"below memory", contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": pointer(0), "workspaceBytes": nil}, false, false},
		{"below disk", contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": pointer(0)}, false, false},
		{"empty", contracts.ProfileResourceCeiling{}, false, false},
		{"missing cpu", contracts.ProfileResourceCeiling{"memoryBytes": nil, "workspaceBytes": nil}, false, false},
		{"missing memory", contracts.ProfileResourceCeiling{"vcpuCount": nil, "workspaceBytes": nil}, false, false},
		{"missing disk", contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil}, false, false},
		{"unknown axis", contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "disk": nil}, false, false},
		{"extra axis", contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": nil, "disk": nil}, false, false},
		{"resume absent", nil, true, true},
		{"resume ceiling", contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": nil}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := validProfileRevisionSpecForValidation()
			spec.ResourceCeiling = test.ceiling
			if test.resume {
				spec.Startup.Mode = contracts.StartupModeSnapshotResume
			}
			if err := validateProfileRevisionSpec(spec); (err == nil) != test.valid {
				t.Fatalf("validation = %v; valid=%t", err, test.valid)
			}
		})
	}
}

func TestValidateProfileResourcesRequireWholeMiB(t *testing.T) {
	for _, axis := range []string{"memoryBytes", "workspaceBytes"} {
		for _, ceiling := range []bool{false, true} {
			spec := validProfileRevisionSpecForValidation()
			if ceiling {
				bound := int64(1<<30 + 1)
				spec.ResourceCeiling = contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": nil}
				spec.ResourceCeiling[axis] = &bound
			} else if axis == "memoryBytes" {
				spec.Resources.MemoryBytes++
			} else {
				spec.Resources.WorkspaceBytes++
			}
			if err := validateProfileRevisionSpec(spec); err == nil || !strings.Contains(err.Error(), axis+" must use whole MiB") {
				t.Fatalf("axis=%s ceiling=%t error=%v", axis, ceiling, err)
			}
		}
	}
}
