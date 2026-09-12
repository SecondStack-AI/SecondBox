package standardresources

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestDurableCodingRegistriesOperatorDocument(t *testing.T) {
	content, err := os.ReadFile("../../examples/resources/durable-coding-registries.json")
	if err != nil {
		t.Fatal(err)
	}
	// Decode is the strict resource engine path used by resources check/apply;
	// it verifies lineage continuity, spec digests, and declared pool references.
	document, err := resourceapply.Decode(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Profiles) != 1 || len(document.RunnerPools) != 1 {
		t.Fatalf("document = %+v", document)
	}
	profile := document.Profiles[0]
	if profile.Name != "durable-coding-registries" || slices.Contains(BundleNames(), profile.Name) || len(profile.Revisions) != 1 {
		t.Fatalf("operator Profile = %+v", profile)
	}
	revision := profile.Revisions[0]
	base, err := ProfileLineage(DurableCoding, revision.Spec.RuntimeBundleDigest, revision.Spec.ToolchainBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	want := base.Revisions[len(base.Revisions)-1].Spec
	for _, domain := range []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "deb.debian.org", "proxy.golang.org"} {
		want.Network.Destinations = append(want.Network.Destinations, sb.NetworkDestination{Protocol: "https", Domain: domain, Port: 443})
	}
	if !reflect.DeepEqual(revision.Spec, want) {
		t.Fatalf("example must differ from durable-coding only in registry HTTPS destinations:\ngot %+v\nwant %+v", revision.Spec, want)
	}
	if len(document.RunnerPools[0].MutableFields) != 0 {
		t.Fatal("example must not silently modify existing pool inventory")
	}
}
