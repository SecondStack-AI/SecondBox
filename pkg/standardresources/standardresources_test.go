package standardresources

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
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

func TestRecordedBundleAuthenticatesLegacyCPUAndProcessFields(t *testing.T) {
	legacySpec := legacyRecordedProfileRevisionSpec{
		Pool: PoolAMD64, Architecture: ArchitectureAMD64,
		RuntimeBundleDigest: v030RuntimeBundleDigest, ToolchainBundleDigest: v030ToolchainBundleDigest,
		Resources: legacyRecordedResourcePolicy{CPUMillis: 1000, MemoryBytes: 1 << 30, WorkspaceBytes: 2 << 30, ProcessLimit: 64, ConcurrentOperations: 4},
		Startup:   secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot},
		Lifecycle: secondboxclient.LifecyclePolicy{InitialState: secondboxclient.SandboxDesiredStateRunning, DrainGraceSeconds: 10, IdleSeconds: 60, MaximumDurationSeconds: 900, LeaseSeconds: 60},
		Retention: secondboxclient.RetentionPolicy{SnapshotLimit: 0, SnapshotRetentionSeconds: 3600},
		Execution: secondboxclient.ExecutionPolicy{MaximumDeadlineMilliseconds: 120000, MaximumBufferedOutputBytes: 1 << 20, StreamWindowBytes: 64 << 10, MaximumTransferBytes: 256 << 20, DataPlaneTransport: "proxied"},
		Network:   secondboxclient.NetworkPolicy{Mode: "allow_list", Destinations: []secondboxclient.NetworkDestination{{Protocol: "https", Domain: AgentGateway, Port: 443}}},
		Ports:     []secondboxclient.PortPolicy{},
	}
	rawSpec, err := json.Marshal(legacySpec)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := recordedProfileSpecIdentity(rawSpec)
	if err != nil {
		t.Fatal(err)
	}
	document := RecordedBundleDocument{
		SchemaVersion: BundleSchemaVersion, Name: AgentCompartment, Architecture: ArchitectureAMD64,
		RunnerPoolSelector: PoolAMD64, LogicalGateway: AgentGateway,
		SignedManifestDigest: "sha256:" + strings.Repeat("a", 64), RuntimeBundleDigest: v030RuntimeBundleDigest, ToolchainBundleDigest: v030ToolchainBundleDigest,
		Profile:         RecordedProfile{Name: AgentCompartment, Revisions: []RecordedProfileRevision{{Number: 1, SpecDigest: digest, Spec: rawSpec}}},
		ParameterSchema: json.RawMessage(`{"type":"object"}`),
	}
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeDocument(content); err == nil || !strings.Contains(err.Error(), "cpuMillis") {
		t.Fatalf("current decoder accepted legacy resources: %v", err)
	}
	if _, err := DecodeRecordedDocument(content); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentsContainThreeExplicitBundlesAndNoIsolatedGatewayDependency(t *testing.T) {
	documents, err := Documents("sha256:"+strings.Repeat("c", 64), v030RuntimeBundleDigest, v030ToolchainBundleDigest)
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
	isolated, err := ProfileLineage(AgentCompartmentIsolated, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []resourceapply.Profile{agent, coding, isolated} {
		if !slices.Contains(BundleNames(), profile.Name) {
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
			if revision.Spec.Architecture != ArchitectureAMD64 {
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
	if currentAgent.Network.RequiresTenantEgressContext == nil || !*currentAgent.Network.RequiresTenantEgressContext || currentAgent.Network.Mode != "allow_list" || len(currentAgent.Network.Destinations) != 1 || currentAgent.Network.Destinations[0].Domain != AgentGateway || len(currentAgent.Ports) != 0 || currentAgent.Retention.SnapshotLimit != 0 {
		t.Fatalf("agent-compartment is over-capable: %#v", currentAgent)
	}
	if coding.Revisions[0].Spec.Network.RequiresTenantEgressContext == nil || !*coding.Revisions[0].Spec.Network.RequiresTenantEgressContext || coding.Revisions[0].Spec.Network.Mode != "allow_list" || len(coding.Revisions[0].Spec.Network.Destinations) != 1 || coding.Revisions[0].Spec.Network.Destinations[0].Domain != PlatformGateway || len(coding.Revisions[0].Spec.Ports) == 0 || coding.Revisions[0].Spec.Retention.SnapshotLimit == 0 {
		t.Fatalf("durable-coding lacks durable capabilities: %#v", coding.Revisions[0].Spec)
	}
	isolatedSpec := isolated.Revisions[0].Spec
	if isolatedSpec.Network.RequiresTenantEgressContext == nil || *isolatedSpec.Network.RequiresTenantEgressContext || isolatedSpec.Network.Mode != "deny_all" || len(isolatedSpec.Network.Destinations) != 0 || len(isolatedSpec.Ports) != 0 || isolatedSpec.Retention.SnapshotLimit != 0 || isolatedSpec.Execution.MaximumDeadlineMilliseconds != 900000 || isolatedSpec.Resources.WorkspaceBytes == 0 || isolatedSpec.Lifecycle.MaximumDurationSeconds == 0 {
		t.Fatalf("agent-compartment-isolated capability bounds = %#v", isolatedSpec)
	}
	networkAgent := currentAgent
	networkAgent.Network = isolatedSpec.Network
	if !reflect.DeepEqual(networkAgent, isolatedSpec) {
		t.Fatalf("isolated Profile changed more than network policy: agent=%#v isolated=%#v", currentAgent, isolatedSpec)
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
	if got, want := profile.Revisions[0].SpecDigest, "sha256:054dc1ce0afc837bf729c32ddbb64b532ba6a8a75793dd492d9d8698765c1e88"; got != want {
		t.Fatalf("portable revision 1 digest = %q, want %q", got, want)
	}
}

func TestAgentCompartmentIsolatedCanonicalRevisionIdentity(t *testing.T) {
	profile, err := ProfileLineage(AgentCompartmentIsolated, v030RuntimeBundleDigest, v030ToolchainBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := profile.Revisions[0].SpecDigest, "sha256:e1c26c6688bc9eb9bc80fd994904b4e020131a8e61ca5341e37fc2e4ba3632db"; got != want {
		t.Fatalf("agent-compartment-isolated revision 1 digest = %q, want %q", got, want)
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
	isolated, err := ProfileLineage(AgentCompartmentIsolated, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.Revisions) != 3 || len(coding.Revisions) != 2 || len(isolated.Revisions) != 2 {
		t.Fatalf("changed-bundle lineage = agent %#v coding %#v isolated %#v", agent.Revisions, coding.Revisions, isolated.Revisions)
	}
	if agent.Revisions[0].SpecDigest != "sha256:054dc1ce0afc837bf729c32ddbb64b532ba6a8a75793dd492d9d8698765c1e88" {
		t.Fatalf("changed bundle rewrote agent revision 1: %#v", agent.Revisions)
	}
	if isolated.Revisions[0].SpecDigest != "sha256:e1c26c6688bc9eb9bc80fd994904b4e020131a8e61ca5341e37fc2e4ba3632db" {
		t.Fatalf("changed bundle rewrote isolated revision 1: %#v", isolated.Revisions)
	}
	for _, profile := range []resourceapply.Profile{agent, coding, isolated} {
		head := profile.Revisions[len(profile.Revisions)-1].Spec
		if head.RuntimeBundleDigest != runtimeDigest || head.ToolchainBundleDigest != toolchainDigest {
			t.Fatalf("changed bundle did not reach %s head: %#v", profile.Name, head)
		}
	}
}

func TestDevelopmentProfileLineageUsesOnlySyntheticAssets(t *testing.T) {
	runtimeDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	toolchainDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, name := range BundleNames() {
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
