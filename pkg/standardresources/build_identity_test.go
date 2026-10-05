package standardresources

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
)

func TestBuildForBuildIdentitySelectsLineageFromIdentity(t *testing.T) {
	development := buildinfo.Identity{Version: buildinfo.DevelopmentVersion, SourceCommit: buildinfo.DevelopmentSourceCommit}
	stamped := buildinfo.Identity{Version: "0.23.0", SourceCommit: strings.Repeat("a", 40)}
	for _, architecture := range []string{ArchitectureAMD64, ArchitectureARM64} {
		pool, err := StandardPool(architecture)
		if err != nil {
			t.Fatal(err)
		}
		binding := PoolBinding{Name: pool, Architectures: []string{architecture}, Capabilities: []string{"compute"}, State: "ready"}
		selection := Selection{Bundles: BundleNames(), Pools: map[string]PoolBinding{}}
		for _, name := range BundleNames() {
			selection.Pools[name] = binding
		}
		for _, test := range []struct {
			name     string
			identity buildinfo.Identity
			lineage  func(string, string) (resourceapply.Profile, error)
		}{
			{"development", development, DevelopmentProfileLineage},
			{"stamped", stamped, ProfileLineage},
		} {
			t.Run(architecture+"/"+test.name, func(t *testing.T) {
				document, err := BuildForBuildIdentity(architecture, test.identity, selection)
				if err != nil {
					t.Fatal(err)
				}
				if len(document.RunnerPools) != 1 || document.RunnerPools[0].Name != pool {
					t.Fatalf("runner pools = %#v", document.RunnerPools)
				}
				if len(document.Profiles) != len(BundleNames()) {
					t.Fatalf("profiles = %d", len(document.Profiles))
				}
				for _, profile := range document.Profiles {
					want, err := test.lineage(profile.Name, architecture)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(profile, want) {
						t.Fatalf("Profile %s lineage differs from the %s lineage", profile.Name, test.name)
					}
				}
			})
		}
	}
}

func TestBuildForBuildIdentityRejectsPartialIdentityAndForeignPool(t *testing.T) {
	binding := PoolBinding{Name: PoolAMD64, Architectures: []string{ArchitectureAMD64}, Capabilities: []string{"compute"}, State: "ready"}
	selection := Selection{Bundles: []string{AgentCompartmentIsolated}, Pools: map[string]PoolBinding{AgentCompartmentIsolated: binding}}
	if _, err := BuildForBuildIdentity(ArchitectureAMD64, buildinfo.Identity{Version: "0.23.0", SourceCommit: buildinfo.DevelopmentSourceCommit}, selection); err == nil || !strings.Contains(err.Error(), "partially stamped") {
		t.Fatalf("partial identity error = %v", err)
	}
	stamped := buildinfo.Identity{Version: "0.23.0", SourceCommit: strings.Repeat("a", 40)}
	if _, err := BuildForBuildIdentity(ArchitectureARM64, stamped, selection); err == nil || !strings.Contains(err.Error(), "requires the arm64 RunnerPool standard-arm64") {
		t.Fatalf("foreign pool error = %v", err)
	}
	if _, err := BuildForBuildIdentity("riscv64", stamped, selection); err == nil || !strings.Contains(err.Error(), "do not support guest architecture") {
		t.Fatalf("unsupported architecture error = %v", err)
	}
}
