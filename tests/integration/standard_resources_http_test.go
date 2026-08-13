package integration_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestStandardResourcesFreshUpgradeAndReplayConvergeThroughLiveControlPlane(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	handler, err := api.NewHandler(api.HandlerConfig{Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaximumDataPlaneBodyBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}

	document := liveStandardDocument(t)
	fresh, err := resourceapply.Apply(t.Context(), client, document)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Results) != 4 || fresh.Results[1].Action != resourceapply.ActionCreate || fresh.Results[2].Action != resourceapply.ActionAppend {
		t.Fatalf("fresh results = %#v", fresh.Results)
	}
	agent, err := client.GetProfile(t.Context(), standardresources.AgentCompartment)
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.Revisions) != 2 || agent.Revisions[0].Spec.Execution.MaximumDeadlineMilliseconds != 120000 || agent.CurrentRevision.Number != 2 || agent.CurrentRevision.Spec.Execution.MaximumDeadlineMilliseconds != 900000 {
		t.Fatalf("fresh agent-compartment lineage = %#v", agent)
	}

	upgraded := document
	upgraded.Profiles = append([]resourceapply.Profile(nil), document.Profiles...)
	upgraded.Profiles[1].Revisions = append([]resourceapply.ProfileRevision(nil), document.Profiles[1].Revisions...)
	second := upgraded.Profiles[1].Revisions[0].Spec
	second.Resources.VCPUCount++
	digest, err := resourceapply.SpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	upgraded.Profiles[1].Revisions = append(upgraded.Profiles[1].Revisions, resourceapply.ProfileRevision{Number: 2, SpecDigest: digest, Spec: second})
	if _, err := resourceapply.Apply(t.Context(), client, upgraded); err != nil {
		t.Fatal(err)
	}
	replayed, err := resourceapply.Apply(t.Context(), client, upgraded)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range replayed.Results {
		if result.Action != resourceapply.ActionNoop {
			t.Fatalf("replay changed resource: %#v", replayed)
		}
	}
	for _, desired := range upgraded.Profiles {
		profile, err := client.GetProfile(t.Context(), desired.Name)
		if err != nil {
			t.Fatal(err)
		}
		head := desired.Revisions[len(desired.Revisions)-1]
		actual, err := resourceapply.SpecDigest(profile.CurrentRevision.Spec)
		if err != nil || profile.CurrentRevision.Number != head.Number || actual != head.SpecDigest {
			t.Fatalf("Profile %s identity = revision %d digest %s error %v", desired.Name, profile.CurrentRevision.Number, actual, err)
		}
	}
}

func liveStandardDocument(t *testing.T) resourceapply.Document {
	t.Helper()
	runtimeDigest := "sha256:9279ca3f8bc3eac4adcd1953926a33fc42da99641d60af042eea12eb12ba0335"
	toolchainDigest := "sha256:cd859a7b0ef9849cc842c8b9c4d0b3b21340e50bed1ac712126585a9fa5553b4"
	agent, err := standardresources.ProfileLineage(standardresources.AgentCompartment, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	coding, err := standardresources.ProfileLineage(standardresources.DurableCoding, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	return resourceapply.Document{SchemaVersion: resourceapply.SchemaVersion, RunnerPools: []resourceapply.RunnerPool{{Name: standardresources.PoolAMD64, Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"}, CapacityPolicy: map[string]int64{"maxSandboxes": 20, "maxVcpuCount": 80, "maxMemoryBytes": 171798691840}, State: "ready", MutableFields: []string{"capacityPolicy", "state"}}}, Profiles: []resourceapply.Profile{agent, coding}}
}
