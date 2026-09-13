package standardresources

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestDurableCodingFlexibleOperatorDocument(t *testing.T) {
	content, err := os.ReadFile("../../examples/resources/durable-coding-flexible.json")
	if err != nil {
		t.Fatal(err)
	}
	document, err := resourceapply.Decode(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Profiles) != 1 || len(document.RunnerPools) != 1 {
		t.Fatalf("document=%+v", document)
	}
	profile := document.Profiles[0]
	if profile.Name != "durable-coding-flexible" || slices.Contains(BundleNames(), profile.Name) || len(profile.Revisions) != 1 {
		t.Fatalf("operator Profile=%+v", profile)
	}
	revision := profile.Revisions[0]
	base, err := ProfileLineage(DurableCoding, revision.Spec.RuntimeBundleDigest, revision.Spec.ToolchainBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	want := base.Revisions[len(base.Revisions)-1].Spec
	if want.ResourceCeiling != nil {
		t.Fatal("standard bundle ceiling changed")
	}
	disk := int64(256 << 30)
	want.ResourceCeiling = sb.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": &disk}
	if !reflect.DeepEqual(revision.Spec, want) {
		t.Fatalf("example must differ from durable-coding only in resourceCeiling:\ngot %+v\nwant %+v", revision.Spec, want)
	}
	if len(document.RunnerPools[0].MutableFields) != 0 {
		t.Fatal("example must not silently modify existing pool inventory")
	}
}
