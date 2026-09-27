package standardresources

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestDocumentRejectsLineageThatDiffersFromPolicy(t *testing.T) {
	documents, err := Documents()
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
		t.Fatalf("decoder accepted a lineage that differs from policy: %v", err)
	}
}

// TestPublishedLineageConvergesOnceBundleDigestsAreRemoved proves an upgraded
// deployment keeps its installed Profile history. Migration 0031 removes the
// retired bundle digests from every recorded spec; what remains must be the
// current lineage revision for revision, so resource apply sees an unaltered
// prefix instead of a rewritten history.
func TestPublishedLineageConvergesOnceBundleDigestsAreRemoved(t *testing.T) {
	for _, name := range BundleNames() {
		content, err := os.ReadFile(filepath.Join("testdata", "v0.18.1", name+".standard-bundle.json"))
		if err != nil {
			t.Fatal(err)
		}
		var published struct {
			Profile struct {
				Revisions []struct {
					Number int64                      `json:"number"`
					Spec   map[string]json.RawMessage `json:"spec"`
				} `json:"revisions"`
			} `json:"profile"`
		}
		if err := json.Unmarshal(content, &published); err != nil {
			t.Fatal(err)
		}
		current, err := ProfileLineage(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(published.Profile.Revisions) != len(current.Revisions) {
			t.Fatalf("%s published %d revisions, current lineage has %d", name, len(published.Profile.Revisions), len(current.Revisions))
		}
		for index, revision := range published.Profile.Revisions {
			if _, pinned := revision.Spec["runtimeBundleDigest"]; !pinned {
				t.Fatalf("%s revision %d fixture lacks the retired bundle digest", name, revision.Number)
			}
			delete(revision.Spec, "runtimeBundleDigest")
			delete(revision.Spec, "toolchainBundleDigest")
			stripped, err := json.Marshal(revision.Spec)
			if err != nil {
				t.Fatal(err)
			}
			var spec secondboxclient.ProfileRevisionSpec
			decoder := json.NewDecoder(bytes.NewReader(stripped))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&spec); err != nil {
				t.Fatalf("%s revision %d: %v", name, revision.Number, err)
			}
			digest, err := resourceapply.SpecDigest(spec)
			if err != nil {
				t.Fatal(err)
			}
			if revision.Number != current.Revisions[index].Number || digest != current.Revisions[index].SpecDigest {
				t.Fatalf("%s installed revision %d does not converge on the current lineage", name, revision.Number)
			}
		}
	}
}

func TestDocumentsContainThreeExplicitBundlesAndNoIsolatedGatewayDependency(t *testing.T) {
	documents, err := Documents()
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != len(BundleNames()) {
		t.Fatalf("standard documents = %#v", documents)
	}
	for index, name := range BundleNames() {
		if documents[index].Name != name {
			t.Fatalf("standard document order = %#v", documents)
		}
	}
	isolated := documents[len(documents)-1]
	if isolated.LogicalGateway != "" || isolated.Profile.Name != AgentCompartmentIsolated {
		t.Fatalf("isolated standard document = %#v", isolated)
	}
	encoded, err := json.Marshal(isolated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeDocument(encoded); err != nil {
		t.Fatalf("isolated standard document decode: %v", err)
	}
}

func TestStandardProfilesHaveFixedArchitectureCapabilitiesAndGatewayBounds(t *testing.T) {
	agent, err := ProfileLineage(AgentCompartment)
	if err != nil {
		t.Fatal(err)
	}
	coding, err := ProfileLineage(DurableCoding)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := ProfileLineage(AgentCompartmentIsolated)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []resourceapply.Profile{agent, coding, isolated} {
		if !slices.Contains(BundleNames(), profile.Name) {
			t.Fatalf("unexpected Profile %q", profile.Name)
		}
		wantRevisions := 2
		if profile.Name == AgentCompartment {
			wantRevisions = 6
		}
		if profile.Name == AgentCompartmentIsolated {
			wantRevisions = 3
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
			if revision.Spec.Architecture != ArchitectureAMD64 {
				t.Fatalf("standard spec = %#v", revision.Spec)
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
	wantAttributed := &secondboxclient.AttributedExecutionPolicy{Gateway: AgentGateway, MaximumConnections: 128}
	if !reflect.DeepEqual(currentAgent.AttributedExecution, wantAttributed) {
		t.Fatalf("agent-compartment attributed policy = %#v", currentAgent.AttributedExecution)
	}
	previousAgent = agent.Revisions[1].Spec
	previousAgent.AttributedExecution = wantAttributed
	previousAgent.AttributedExecutionCeiling = secondboxclient.AttributedExecutionConnectionLimits{MaximumConnections: 4096}
	previousAgent.Lifecycle.MaximumDurationSeconds = secondboxclient.Unlimited
	previousAgent.LifecycleCeiling = &secondboxclient.SandboxLifecycleLimits{IdleSeconds: secondboxclient.Unlimited, MaximumDurationSeconds: secondboxclient.Unlimited}
	if !reflect.DeepEqual(previousAgent, currentAgent) {
		t.Fatalf("agent-compartment current revision changed more than attributed permission and lifecycle policy: %#v", agent.Revisions)
	}
	if currentAgent.Network.RequiresTenantEgressContext == nil || !*currentAgent.Network.RequiresTenantEgressContext || currentAgent.Network.Mode != "allow_list" || len(currentAgent.Network.Destinations) != 1 || currentAgent.Network.Destinations[0].Domain != AgentGateway || len(currentAgent.Ports) != 0 || currentAgent.Retention.SnapshotLimit != 0 {
		t.Fatalf("agent-compartment is over-capable: %#v", currentAgent)
	}
	if coding.Revisions[0].Spec.Network.RequiresTenantEgressContext == nil || !*coding.Revisions[0].Spec.Network.RequiresTenantEgressContext || coding.Revisions[0].Spec.Network.Mode != "allow_list" || len(coding.Revisions[0].Spec.Network.Destinations) != 1 || coding.Revisions[0].Spec.Network.Destinations[0].Domain != PlatformGateway || len(coding.Revisions[0].Spec.Ports) == 0 || coding.Revisions[0].Spec.Retention.SnapshotLimit == 0 {
		t.Fatalf("durable-coding lacks durable capabilities: %#v", coding.Revisions[0].Spec)
	}
	isolatedSpec := isolated.Revisions[len(isolated.Revisions)-1].Spec
	if isolatedSpec.Network.RequiresTenantEgressContext == nil || *isolatedSpec.Network.RequiresTenantEgressContext || isolatedSpec.Network.Mode != "deny_all" || len(isolatedSpec.Network.Destinations) != 0 || len(isolatedSpec.Ports) != 0 || isolatedSpec.Retention.SnapshotLimit != 0 || isolatedSpec.Execution.MaximumDeadlineMilliseconds != 900000 || isolatedSpec.Resources.WorkspaceBytes == 0 || isolatedSpec.Lifecycle.MaximumDurationSeconds == 0 {
		t.Fatalf("agent-compartment-isolated capability bounds = %#v", isolatedSpec)
	}
	networkAgent := currentAgent
	networkAgent.Network = isolatedSpec.Network
	networkAgent.AttributedExecution = nil
	networkAgent.AttributedExecutionCeiling = secondboxclient.AttributedExecutionConnectionLimits{}
	if !reflect.DeepEqual(networkAgent, isolatedSpec) {
		t.Fatalf("isolated Profile changed more than network policy: agent=%#v isolated=%#v", currentAgent, isolatedSpec)
	}
}

func TestProfileLineageRejectsUnknownBundle(t *testing.T) {
	if _, err := ProfileLineage("unknown"); err == nil {
		t.Fatal("expected unknown bundle failure")
	}
}

func TestAgentCompartmentPinsPortableResourceRevisionIdentity(t *testing.T) {
	profile, err := ProfileLineage(AgentCompartment)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := profile.Revisions[0].SpecDigest, "sha256:9abf6803df496257362d7c4003088de1a17302d3fc6d149632bcf53686f37752"; got != want {
		t.Fatalf("portable revision 1 digest = %q, want %q", got, want)
	}
}

func TestAgentCompartmentIsolatedCanonicalRevisionIdentity(t *testing.T) {
	profile, err := ProfileLineage(AgentCompartmentIsolated)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := profile.Revisions[0].SpecDigest, "sha256:7d3273dc7bf997e28034509d2a1c002b5340039363acbbeb539b745c7947375a"; got != want {
		t.Fatalf("agent-compartment-isolated revision 1 digest = %q, want %q", got, want)
	}
}

// TestProfileLineageKeepsRetiredBundleRevisionNumbers pins the revision that
// once moved each bundle off the v0.3.0 execution assets. It now repeats its
// predecessor, and removing it would renumber every installed lineage.
func TestProfileLineageKeepsRetiredBundleRevisionNumbers(t *testing.T) {
	for name, repeated := range map[string]int{AgentCompartment: 2, DurableCoding: 1, AgentCompartmentIsolated: 1} {
		profile, err := ProfileLineage(name)
		if err != nil {
			t.Fatal(err)
		}
		if profile.Revisions[repeated].SpecDigest != profile.Revisions[repeated-1].SpecDigest {
			t.Fatalf("%s revision %d no longer repeats its predecessor", name, repeated+1)
		}
	}
}

func TestDevelopmentProfileLineageOmitsPublishedHistory(t *testing.T) {
	for _, name := range BundleNames() {
		profile, err := DevelopmentProfileLineage(name)
		if err != nil {
			t.Fatal(err)
		}
		wantRevisions := 1
		if name == AgentCompartment {
			wantRevisions = 4
		}
		if name == AgentCompartmentIsolated {
			wantRevisions = 2
		}
		if len(profile.Revisions) != wantRevisions || profile.Revisions[0].Number != 1 {
			t.Fatalf("development %s lineage = %#v", name, profile.Revisions)
		}
		spec := profile.Revisions[0].Spec
		if name == AgentCompartment && !reflect.DeepEqual(spec, agentSpec(PoolAMD64, 900000)) {
			t.Fatalf("attributed permission replaced development revision 1: %#v", spec)
		}
	}
}

func TestAttributedConnectionRevisionPreservesHistoricalPrefix(t *testing.T) {
	for _, development := range []bool{false, true} {
		build := ProfileLineage
		if development {
			build = DevelopmentProfileLineage
		}
		profile, err := build(AgentCompartment)
		if err != nil {
			t.Fatal(err)
		}
		for _, revision := range profile.Revisions[:len(profile.Revisions)-1] {
			if revision.Spec.AttributedExecutionCeiling != (secondboxclient.AttributedExecutionConnectionLimits{}) {
				t.Fatal("rewrote historical ceiling")
			}
			if permission := revision.Spec.AttributedExecution; permission != nil && permission.MaximumConnections != 2 {
				t.Fatal("rewrote historical default through shared pointer")
			}
		}
		latest := profile.Revisions[len(profile.Revisions)-1].Spec
		permission := *latest.AttributedExecution
		permission.MaximumConnections = 2
		latest.AttributedExecution = &permission
		latest.AttributedExecutionCeiling = secondboxclient.AttributedExecutionConnectionLimits{}
		if !reflect.DeepEqual(latest, profile.Revisions[len(profile.Revisions)-2].Spec) {
			t.Fatal("connection revision changed unrelated policy")
		}
	}
}
