package deployconfig

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

func setBuildIdentity(t *testing.T, version, sourceCommit string) {
	t.Helper()
	originalVersion, originalCommit := buildinfo.Version, buildinfo.SourceCommit
	buildinfo.Version, buildinfo.SourceCommit = version, sourceCommit
	t.Cleanup(func() { buildinfo.Version, buildinfo.SourceCommit = originalVersion, originalCommit })
}

// sourceBuildManifest converts the reviewed development topology to a source
// build: no artifact manifest, an explicit guest architecture and its pool.
func sourceBuildManifest(t *testing.T, architecture string) (ManifestV1, string) {
	t.Helper()
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := standardresources.StandardPool(architecture)
	if err != nil {
		t.Fatal(err)
	}
	manifest.StandardResources.ArtifactManifest = ""
	manifest.StandardResources.GuestArchitecture = architecture
	manifest.StandardResources.RunnerPools[0].Name = pool
	manifest.StandardResources.RunnerPools[0].Architectures = []string{architecture}
	return manifest, manifestPath
}

func writeTestManifest(t *testing.T, manifestPath string, manifest ManifestV1) {
	t.Helper()
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "artifact_manifest") != (manifest.StandardResources.ArtifactManifest != "") {
		t.Fatalf("encoded manifest artifact_manifest presence differs from the typed manifest:\n%s", encoded)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
}

func TestSourceBuildStandardResourcesFollowTheDeployBuildIdentity(t *testing.T) {
	for _, architecture := range []string{standardresources.ArchitectureAMD64, standardresources.ArchitectureARM64} {
		for _, test := range []struct {
			name, version, sourceCommit string
			lineage                     func(string, string) (resourceapply.Profile, error)
		}{
			{"development", buildinfo.DevelopmentVersion, buildinfo.DevelopmentSourceCommit, standardresources.DevelopmentProfileLineage},
			{"stamped", "0.23.0", strings.Repeat("a", 40), standardresources.ProfileLineage},
		} {
			t.Run(architecture+"/"+test.name, func(t *testing.T) {
				setBuildIdentity(t, test.version, test.sourceCommit)
				manifest, manifestPath := sourceBuildManifest(t, architecture)
				writeTestManifest(t, manifestPath, manifest)
				resolved, err := Render(manifestPath, filepath.Join(filepath.Dir(manifestPath), "generated.env"))
				if err != nil {
					t.Fatal(err)
				}
				document := resolved.ResourceDocument
				pool, _ := standardresources.StandardPool(architecture)
				if len(document.RunnerPools) != 1 || document.RunnerPools[0].Name != pool || len(document.Profiles) != len(manifest.StandardResources.Bundles) {
					t.Fatalf("source-build standard resources = %#v", document)
				}
				for _, profile := range document.Profiles {
					want, err := test.lineage(profile.Name, architecture)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(profile, want) {
						t.Fatalf("Profile %s does not carry the %s lineage", profile.Name, test.name)
					}
				}
				if _, err := Inspect(manifestPath); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSourceBuildProductionSkipsReleaseSigningIdentity(t *testing.T) {
	setBuildIdentity(t, "0.23.0", strings.Repeat("a", 40))
	manifest, manifestPath := sourceBuildManifest(t, standardresources.ArchitectureAMD64)
	digestRef := "registry.example/secondbox@sha256:" + strings.Repeat("a", 64)
	manifest.Deployment.Mode = "production"
	manifest.Deployment.PublicBaseURL = "https://secondbox.example.com"
	manifest.Deployment.TLSTermination = "external"
	manifest.Deployment.ControlPlaneImage = digestRef
	manifest.Deployment.RunnerImage = digestRef
	manifest.Deployment.PostgresImage = digestRef
	runner := validTestRunner("runner-source", "remote")
	runner.PoolID = standardresources.PoolAMD64
	runner.ArtifactPublicKeySHA256 = strings.Repeat("c", 64)
	manifest.Runners = []Runner{runner}
	writeTestManifest(t, manifestPath, manifest)
	if _, err := Resolve(manifestPath); err != nil {
		t.Fatal(err)
	}
	automated, err := InitProductionFromManifest(manifestPath, filepath.Join(t.TempDir(), "production"))
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := ReadManifest(automated)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.StandardResources.ArtifactManifest != "" || materialized.StandardResources.GuestArchitecture != standardresources.ArchitectureAMD64 {
		t.Fatalf("materialized standard resources = %#v", materialized.StandardResources)
	}
}

func TestStandardResourcesRequireExactlyOneArchitectureSource(t *testing.T) {
	for _, test := range []struct {
		name, want string
		mutate     func(*ManifestV1)
	}{
		{"both", "exactly one of artifact_manifest", func(manifest *ManifestV1) {
			manifest.StandardResources.ArtifactManifest = "development-artifact-manifest.json"
		}},
		{"neither", "exactly one of artifact_manifest", func(manifest *ManifestV1) { manifest.StandardResources.GuestArchitecture = "" }},
		{"unsupported architecture", "standard_resources.guest_architecture must be amd64 or arm64", func(manifest *ManifestV1) {
			manifest.StandardResources.GuestArchitecture = "riscv64"
		}},
		{"pool of the other architecture", "must declare pool standard-amd64 for the amd64 release", func(manifest *ManifestV1) {
			manifest.StandardResources.RunnerPools[0].Name = standardresources.PoolARM64
			manifest.StandardResources.RunnerPools[0].Architectures = []string{standardresources.ArchitectureARM64}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest, manifestPath := sourceBuildManifest(t, standardresources.ArchitectureAMD64)
			test.mutate(&manifest)
			writeTestManifest(t, manifestPath, manifest)
			if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSourceBuildRejectsPartiallyStampedDeployIdentity(t *testing.T) {
	setBuildIdentity(t, "0.23.0", buildinfo.DevelopmentSourceCommit)
	manifest, manifestPath := sourceBuildManifest(t, standardresources.ArchitectureAMD64)
	writeTestManifest(t, manifestPath, manifest)
	if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "partially stamped") {
		t.Fatalf("partial identity error = %v", err)
	}
}
