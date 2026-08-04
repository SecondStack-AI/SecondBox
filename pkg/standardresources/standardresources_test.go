package standardresources

import (
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
)

func TestStandardProfilesHaveFixedArchitectureCapabilitiesAndGatewayBounds(t *testing.T) {
	digest := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	agent, err := ProfileLineage(AgentCompartment, digest)
	if err != nil {
		t.Fatal(err)
	}
	coding, err := ProfileLineage(DurableCoding, digest)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []resourceapply.Profile{agent, coding} {
		if profile.Name != AgentCompartment && profile.Name != DurableCoding {
			t.Fatalf("unexpected Profile %q", profile.Name)
		}
		if len(profile.Revisions) != 1 || profile.Revisions[0].Number != 1 {
			t.Fatalf("lineage = %#v", profile.Revisions)
		}
		actual, err := resourceapply.SpecDigest(profile.Revisions[0].Spec)
		if err != nil || actual != profile.Revisions[0].SpecDigest {
			t.Fatalf("identity = %s, %v", actual, err)
		}
		if profile.Revisions[0].Spec.Architecture != ArchitectureAMD64 || profile.Revisions[0].Spec.Network.Mode != "allow_list" || len(profile.Revisions[0].Spec.Network.Destinations) != 1 {
			t.Fatalf("standard spec = %#v", profile.Revisions[0].Spec)
		}
	}
	if agent.Revisions[0].Spec.Network.Destinations[0].Domain != AgentGateway || len(agent.Revisions[0].Spec.Ports) != 0 || agent.Revisions[0].Spec.Retention.SnapshotLimit != 0 {
		t.Fatalf("agent-compartment is over-capable: %#v", agent.Revisions[0].Spec)
	}
	if coding.Revisions[0].Spec.Network.Destinations[0].Domain != PlatformGateway || len(coding.Revisions[0].Spec.Ports) == 0 || coding.Revisions[0].Spec.Retention.SnapshotLimit == 0 {
		t.Fatalf("durable-coding lacks durable capabilities: %#v", coding.Revisions[0].Spec)
	}
}

func TestProfileLineageRejectsUnknownBundle(t *testing.T) {
	if _, err := ProfileLineage("unknown", "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"); err == nil {
		t.Fatal("expected unknown bundle failure")
	}
}
