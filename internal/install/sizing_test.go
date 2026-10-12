package install

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

var agentCompartmentSizing = CapacityOverrides{SandboxProfile: "agent-compartment", ControlReserveVCPUCount: 1, ControlReserveMemoryBytes: 1 << 30}

func TestDefaultPlanRecordsNoSandboxProfile(t *testing.T) {
	plan, err := ProposePlan(plannerFacts(t), plannerInput(t, StorageExistingMount))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SandboxProfile() != "durable-coding" || strings.Contains(string(encoded), "sandboxProfile") {
		t.Fatalf("default plan sandbox Profile = %q, encoded field present = %t", plan.SandboxProfile(), strings.Contains(string(encoded), "sandboxProfile"))
	}
}

func TestSmallHostPlansAgentCompartmentCapacityOnlyWhenSelected(t *testing.T) {
	facts := plannerFacts(t)
	facts.CPUCount, facts.MemoryBytes = 3, 3<<30
	if _, err := ProposePlan(facts, plannerInput(t, StorageExistingMount)); err == nil || !strings.Contains(err.Error(), "durable-coding smoke Sandbox") {
		t.Fatalf("default sizing on a 3 vCPU / 3 GiB host = %v", err)
	}
	input := plannerInput(t, StorageExistingMount)
	input.CapacityOverrides = agentCompartmentSizing
	plan, err := ProposePlan(facts, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SandboxProfile() != "agent-compartment" || plan.Capacity.MaxSandboxes != 2 || plan.Capacity.MaxVCPUCount != 2 || plan.Capacity.MaxMemoryBytes != 2<<30 || plan.Capacity.ConcurrentOperations != 8 || plan.Capacity.ConcurrentStarts != 2 {
		t.Fatalf("agent-compartment sizing = %q %#v", plan.SandboxProfile(), plan.Capacity)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePlan(encoded)
	if err != nil || decoded.SandboxProfile() != "agent-compartment" {
		t.Fatalf("decoded sandbox Profile = %q, %v", decoded.SandboxProfile(), err)
	}
	if !strings.Contains(RenderPlanReview(plan), "Capacity: 2 agent-compartment Sandboxes") {
		t.Fatalf("review omitted the sizing Profile:\n%s", RenderPlanReview(plan))
	}
}

func TestCapacityOverridesRejectInvalidValues(t *testing.T) {
	facts := plannerFacts(t)
	facts.CPUCount, facts.MemoryBytes = 3, 3<<30
	for name, overrides := range map[string]CapacityOverrides{
		"unknown Profile":                   {SandboxProfile: "custom"},
		"negative vCPU reserve":             {SandboxProfile: "agent-compartment", ControlReserveVCPUCount: -1},
		"negative memory reserve":           {SandboxProfile: "agent-compartment", ControlReserveMemoryBytes: -1},
		"vCPU reserve leaves no Sandbox":    {SandboxProfile: "agent-compartment", ControlReserveVCPUCount: 3, ControlReserveMemoryBytes: 1 << 30},
		"memory reserve leaves no Sandbox":  {SandboxProfile: "agent-compartment", ControlReserveVCPUCount: 1, ControlReserveMemoryBytes: 2<<30 + 1},
		"default reserve leaves no Sandbox": {SandboxProfile: "agent-compartment"},
	} {
		input := plannerInput(t, StorageExistingMount)
		input.CapacityOverrides = overrides
		if _, err := ProposePlan(facts, input); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, recorded := range []string{"durable-coding", "custom"} {
		plan := validPlan(t)
		plan.SizedSandboxProfile = recorded
		if err := plan.Validate(); err == nil {
			t.Errorf("plan recording sandbox Profile %q was accepted", recorded)
		}
	}
}

func TestPreflightCapacityFollowsSandboxSizing(t *testing.T) {
	for _, test := range []struct {
		name     string
		capacity CapacityOverrides
		want     FindingClass
	}{{"default", CapacityOverrides{}, FindingNeedsAction}, {"agent-compartment", agentCompartmentSizing, FindingPass}} {
		probes := qualifiedProbes()
		probes.Capacity = test.capacity
		probes.CPUCount = 2
		probes.Filesystem.(*fakeFilesystem).files["/proc/meminfo"] = "MemTotal: 2097152 kB\n"
		facts, err := Preflight(context.Background(), probes)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"cpu", "memory"} {
			if finding := findingByID(t, facts, id); finding.Class != test.want {
				t.Errorf("%s %s finding = %#v", test.name, id, finding)
			}
		}
	}
	probes := qualifiedProbes()
	probes.Capacity = CapacityOverrides{SandboxProfile: "custom"}
	if _, err := Preflight(context.Background(), probes); err == nil {
		t.Fatal("preflight accepted an unknown sandbox Profile")
	}
}

func TestSmokeProfilesBootIsolatedThenEgressContextProfile(t *testing.T) {
	for profile, want := range map[string][]string{
		"durable-coding":             {"agent-compartment-isolated", "durable-coding"},
		"agent-compartment":          {"agent-compartment-isolated", "agent-compartment"},
		"agent-compartment-isolated": {"agent-compartment-isolated", "agent-compartment"},
	} {
		if got := SmokeProfiles(profile); !slices.Equal(got, want) {
			t.Errorf("%s smoke Profiles = %v, want %v", profile, got, want)
		}
	}
}
