package standardresources

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
)

func TestRecordedBundleAcceptsImmutablePrefixAfterPolicyAppends(t *testing.T) {
	documents, err := Documents("sha256:"+strings.Repeat("a", 64), v030RuntimeBundleDigest, v030ToolchainBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	document := documents[0]
	document.Profile.Revisions = document.Profile.Revisions[:1]
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeDocument(content); err == nil || !strings.Contains(err.Error(), "lineage") {
		t.Fatalf("current-policy decoder accepted recorded prefix: %v", err)
	}
	recorded, err := DecodeRecordedDocument(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded.Profile.Revisions) != 1 || recorded.Profile.Revisions[0].SpecDigest != document.Profile.Revisions[0].SpecDigest {
		t.Fatalf("recorded lineage = %#v", recorded.Profile.Revisions)
	}
}

func TestRecordedBundleAcceptsV04ArtifactRetentionProfile(t *testing.T) {
	// This is the first agent-compartment Profile revision published by v0.4.7.
	// Its digest includes artifactRetentionSeconds, a field intentionally removed
	// from the current public Profile contract in v0.5.0.
	content := []byte(`{
  "schemaVersion":"secondbox.standard-bundle/v2",
  "name":"agent-compartment",
  "architecture":"amd64",
  "runnerPoolSelector":"standard-amd64",
  "logicalGateway":"agent-gateway.secondbox.internal",
  "signedManifestDigest":"sha256:ced70a4475c251d297cabe77331f0680b23e162d318d94841d308ed7ec554332",
  "runtimeBundleDigest":"sha256:9279ca3f8bc3eac4adcd1953926a33fc42da99641d60af042eea12eb12ba0335",
  "toolchainBundleDigest":"sha256:cd859a7b0ef9849cc842c8b9c4d0b3b21340e50bed1ac712126585a9fa5553b4",
  "profile":{"name":"agent-compartment","revisions":[{
    "number":1,
    "specDigest":"sha256:ea4f38d9276ed6c7c519bc3ce677a035e8549434af8d00aae890815e9e3b2a08",
    "spec":{
      "pool":"standard-amd64",
      "architecture":"amd64",
      "runtimeBundleDigest":"sha256:9279ca3f8bc3eac4adcd1953926a33fc42da99641d60af042eea12eb12ba0335",
      "toolchainBundleDigest":"sha256:cd859a7b0ef9849cc842c8b9c4d0b3b21340e50bed1ac712126585a9fa5553b4",
      "resources":{"cpuMillis":1000,"memoryBytes":1073741824,"workspaceBytes":2147483648,"processLimit":64,"concurrentOperations":4},
      "startup":{"mode":"cold_boot"},
      "lifecycle":{"initialState":"running","drainGraceSeconds":10,"idleSeconds":60,"maximumDurationSeconds":900,"leaseSeconds":60},
      "retention":{"snapshotLimit":0,"snapshotRetentionSeconds":3600,"artifactRetentionSeconds":86400},
      "execution":{"maximumDeadlineMilliseconds":120000,"maximumBufferedOutputBytes":1048576,"streamWindowBytes":65536,"maximumTransferBytes":268435456,"terminalDetachSeconds":0,"dataPlaneTransport":"proxied"},
      "network":{"mode":"allow_list","destinations":[{"protocol":"https","domain":"agent-gateway.secondbox.internal","port":443}]},
      "ports":[]
    }
  }]},
  "parameterSchema":{}
}`)
	if _, err := DecodeDocument(content); err == nil || !strings.Contains(err.Error(), "artifactRetentionSeconds") {
		t.Fatalf("current decoder accepted historical Profile wire shape: %v", err)
	}
	recorded, err := DecodeRecordedDocument(content)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorded.Profile.Revisions[0].SpecDigest; got != "sha256:ea4f38d9276ed6c7c519bc3ce677a035e8549434af8d00aae890815e9e3b2a08" {
		t.Fatalf("recorded v0.4.7 Profile digest = %q", got)
	}
	unknown := []byte(strings.Replace(string(content), "artifactRetentionSeconds", "unknownRetentionSeconds", 1))
	if _, err := DecodeRecordedDocument(unknown); err == nil {
		t.Fatal("recorded decoder accepted an unknown historical Profile field")
	}
}

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

func TestAgentCompartmentPreservesV031RevisionIdentity(t *testing.T) {
	profile, err := ProfileLineage(AgentCompartment, v030RuntimeBundleDigest, v030ToolchainBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := profile.Revisions[0].SpecDigest, "sha256:837ec1f0810f9cc10d3ec760fd385cb90db894d6446f09f97a00c310449d618f"; got != want {
		t.Fatalf("v0.3.1 revision 1 digest = %q, want %q", got, want)
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
	if agent.Revisions[0].SpecDigest != "sha256:837ec1f0810f9cc10d3ec760fd385cb90db894d6446f09f97a00c310449d618f" {
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
