package service

import (
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func testBindings() BuiltInProfileBindings {
	binding := BuiltInProfileBinding{
		Pool:                  "runners-a",
		RuntimeBundleDigest:   "sha256:" + strings.Repeat("1", 64),
		ToolchainBundleDigest: "sha256:" + strings.Repeat("2", 64),
	}
	return BuiltInProfileBindings{AgentCompartment: binding, CodingEnvironment: binding}
}

func TestBuildBuiltInProfilesAppliesTheDeploymentBinding(t *testing.T) {
	profiles, err := BuildBuiltInProfiles(testBindings())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles = %d; want both built-ins", len(profiles))
	}
	for _, profile := range profiles {
		spec := profile.CurrentRevision.Spec
		if spec.Pool != "runners-a" {
			t.Errorf("%s pool = %q; want the bound pool", profile.Name, spec.Pool)
		}
		if spec.RuntimeBundleDigest != "sha256:"+strings.Repeat("1", 64) ||
			spec.ToolchainBundleDigest != "sha256:"+strings.Repeat("2", 64) {
			t.Errorf("%s digests = %+v; want the bound digests", profile.Name, spec)
		}
	}
}

// TestBuildBuiltInProfilesKeepsTheFixedPolicy proves a binding supplies only the
// deployment values and cannot alter the built-in policy itself.
func TestBuildBuiltInProfilesKeepsTheFixedPolicy(t *testing.T) {
	profiles, err := BuildBuiltInProfiles(testBindings())
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]contracts.Profile, len(profiles))
	for _, profile := range profiles {
		byName[profile.Name] = profile
	}
	agent, found := byName[BuiltInProfileAgentCompartment]
	if !found {
		t.Fatal("the agent compartment Profile must be present")
	}
	if agent.CurrentRevision.Spec.Network.Mode != "deny_all" {
		t.Errorf("agent network mode = %q; want deny_all", agent.CurrentRevision.Spec.Network.Mode)
	}
	coding, found := byName[BuiltInProfileCodingEnvironment]
	if !found {
		t.Fatal("the coding environment Profile must be present")
	}
	if len(coding.CurrentRevision.Spec.Ports) != 1 ||
		coding.CurrentRevision.Spec.Ports[0].Port != 3000 {
		t.Errorf("coding ports = %+v; want the fixed development port", coding.CurrentRevision.Spec.Ports)
	}
}

func TestBuildBuiltInProfilesRejectsAnIncompleteBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BuiltInProfileBindings)
	}{
		{"no pool", func(b *BuiltInProfileBindings) { b.AgentCompartment.Pool = "" }},
		{"no runtime digest", func(b *BuiltInProfileBindings) {
			b.AgentCompartment.RuntimeBundleDigest = ""
		}},
		{"no toolchain digest", func(b *BuiltInProfileBindings) {
			b.CodingEnvironment.ToolchainBundleDigest = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bindings := testBindings()
			test.mutate(&bindings)
			if _, err := BuildBuiltInProfiles(bindings); err == nil {
				t.Fatal("an incomplete binding must be rejected")
			}
		})
	}
}

// TestResolveBuiltInProfilesHasNoImplicitDefault proves the placeholder-bearing
// fallback is gone: a control plane must be told what its built-ins pin.
func TestResolveBuiltInProfilesHasNoImplicitDefault(t *testing.T) {
	for _, absent := range [][]contracts.Profile{nil, {}} {
		_, err := resolveBuiltInProfiles(absent)
		if err == nil || !strings.Contains(err.Error(), "must be configured explicitly") {
			t.Fatalf("error = %v; want an explicit-configuration requirement", err)
		}
	}
}

func TestResolveBuiltInProfilesRequiresBothReservedNames(t *testing.T) {
	profiles, err := BuildBuiltInProfiles(testBindings())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBuiltInProfiles(profiles[:1]); err == nil ||
		!strings.Contains(err.Error(), "is required") {
		t.Fatalf("error = %v; want the missing built-in reported", err)
	}
}
