package standardresources

import (
	"reflect"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
)

func TestStandardProfilesHaveFixedArchitectureCapabilitiesAndGatewayBounds(t *testing.T) {
	runtimeDigest := v030RuntimeBundleDigest
	toolchainDigest := v030ToolchainBundleDigest
	agent, err := ProfileLineage(AgentCompartment, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	coding, err := ProfileLineage(DurableCoding, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []resourceapply.Profile{agent, coding} {
		if profile.Name != AgentCompartment && profile.Name != DurableCoding {
			t.Fatalf("unexpected Profile %q", profile.Name)
		}
		wantRevisions := 1
		if profile.Name == AgentCompartment {
			wantRevisions = 2
		}
		if len(profile.Revisions) != wantRevisions {
			t.Fatalf("lineage = %#v", profile.Revisions)
		}
		for index, revision := range profile.Revisions {
			if revision.Number != int64(index+1) {
				t.Fatalf("lineage = %#v", profile.Revisions)
			}
			actual, err := resourceapply.SpecDigest(revision.Spec)
			if err != nil || actual != revision.SpecDigest {
				t.Fatalf("identity = %s, %v", actual, err)
			}
			if revision.Spec.Architecture != ArchitectureAMD64 || revision.Spec.Network.Mode != "allow_list" || len(revision.Spec.Network.Destinations) != 1 {
				t.Fatalf("standard spec = %#v", revision.Spec)
			}
			if revision.Spec.RuntimeBundleDigest != runtimeDigest || revision.Spec.ToolchainBundleDigest != toolchainDigest || revision.Spec.RuntimeBundleDigest == revision.Spec.ToolchainBundleDigest {
				t.Fatalf("standard component identity = %#v", revision.Spec)
			}
		}
	}
	if agent.Revisions[0].Spec.Execution.MaximumDeadlineMilliseconds != 120000 || agent.Revisions[1].Spec.Execution.MaximumDeadlineMilliseconds != 900000 {
		t.Fatalf("agent-compartment deadlines = %d, %d", agent.Revisions[0].Spec.Execution.MaximumDeadlineMilliseconds, agent.Revisions[1].Spec.Execution.MaximumDeadlineMilliseconds)
	}
	previousAgent := agent.Revisions[0].Spec
	previousAgent.Execution.MaximumDeadlineMilliseconds = 900000
	if !reflect.DeepEqual(previousAgent, agent.Revisions[1].Spec) {
		t.Fatalf("agent-compartment revision 2 changed more than its deadline: %#v", agent.Revisions)
	}
	currentAgent := agent.Revisions[len(agent.Revisions)-1].Spec
	if currentAgent.Network.Destinations[0].Domain != AgentGateway || len(currentAgent.Ports) != 0 || currentAgent.Retention.SnapshotLimit != 0 {
		t.Fatalf("agent-compartment is over-capable: %#v", currentAgent)
	}
	if coding.Revisions[0].Spec.Network.Destinations[0].Domain != PlatformGateway || len(coding.Revisions[0].Spec.Ports) == 0 || coding.Revisions[0].Spec.Retention.SnapshotLimit == 0 {
		t.Fatalf("durable-coding lacks durable capabilities: %#v", coding.Revisions[0].Spec)
	}
}

func TestProfileLineageRejectsUnknownBundle(t *testing.T) {
	if _, err := ProfileLineage("unknown", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("expected unknown bundle failure")
	}
}

func TestAgentCompartmentPinsPortableResourceRevisionIdentity(t *testing.T) {
	profile, err := ProfileLineage(AgentCompartment, v030RuntimeBundleDigest, v030ToolchainBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := profile.Revisions[0].SpecDigest, "sha256:0724aee520173710db793ad365db28cdb6905f2a5fecb2cb785c475443f98ed4"; got != want {
		t.Fatalf("portable revision 1 digest = %q, want %q", got, want)
	}
}

func TestProfileLineageAppendsChangedBundleWithoutRewritingHistory(t *testing.T) {
	runtimeDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	toolchainDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	agent, err := ProfileLineage(AgentCompartment, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	coding, err := ProfileLineage(DurableCoding, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.Revisions) != 3 || len(coding.Revisions) != 2 {
		t.Fatalf("changed-bundle lineage = agent %#v coding %#v", agent.Revisions, coding.Revisions)
	}
	if agent.Revisions[0].SpecDigest != "sha256:0724aee520173710db793ad365db28cdb6905f2a5fecb2cb785c475443f98ed4" {
		t.Fatalf("changed bundle rewrote agent revision 1: %#v", agent.Revisions)
	}
	for _, profile := range []resourceapply.Profile{agent, coding} {
		head := profile.Revisions[len(profile.Revisions)-1].Spec
		if head.RuntimeBundleDigest != runtimeDigest || head.ToolchainBundleDigest != toolchainDigest {
			t.Fatalf("changed bundle did not reach %s head: %#v", profile.Name, head)
		}
	}
}

func TestDevelopmentProfileLineageUsesOnlySyntheticAssets(t *testing.T) {
	runtimeDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	toolchainDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, name := range []string{AgentCompartment, DurableCoding} {
		profile, err := DevelopmentProfileLineage(name, runtimeDigest, toolchainDigest)
		if err != nil {
			t.Fatal(err)
		}
		if len(profile.Revisions) != 1 || profile.Revisions[0].Number != 1 {
			t.Fatalf("development %s lineage = %#v", name, profile.Revisions)
		}
		spec := profile.Revisions[0].Spec
		if spec.RuntimeBundleDigest != runtimeDigest || spec.ToolchainBundleDigest != toolchainDigest {
			t.Fatalf("development %s assets = %#v", name, spec)
		}
	}
}
