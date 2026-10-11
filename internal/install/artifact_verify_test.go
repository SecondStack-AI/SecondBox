package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

func TestVerifyArtifactDirectoryAuthenticatesCompleteReleaseIdentity(t *testing.T) {
	directory := t.TempDir()
	release := writeBundleArtifactFixture(t, directory)
	verified, err := VerifyArtifactDirectory(directory, release)
	if err != nil {
		t.Fatal(err)
	}
	if verified.ManifestDigest != release.MicroVM.ManifestDigest() {
		t.Fatalf("verified identity = %#v", verified)
	}
}

// An update or rollback re-verifies a directory materialized from a v6 release.
func TestVerifyArtifactDirectoryToleratesRetiredSigningFilesOnlyForRecordedReleases(t *testing.T) {
	directory := t.TempDir()
	current := writeBundleArtifactFixture(t, directory)
	writeFixtureFile(t, directory, "manifest.sig", []byte("signature"))
	writeFixtureFile(t, directory, "signing.pub", []byte("key"))
	recorded := current
	recorded.SchemaVersion = releasecontract.SignedArtifactManifestSchema
	recorded.MicroVM.LegacySignedManifestDigest = current.MicroVM.BundleManifestDigest
	recorded.MicroVM.LegacySigningKeyFingerprint = "SHA256:" + strings.Repeat("A", 64)
	recorded.MicroVM.BundleManifestDigest = ""
	if _, err := VerifyArtifactDirectory(directory, recorded); err != nil {
		t.Fatalf("recorded v6 artifact directory rejected: %v", err)
	}
	if _, err := VerifyArtifactDirectory(directory, current); err == nil {
		t.Fatal("current artifact directory with retired signing files accepted")
	}
}

// Bundles built before the rootfs secret scan was removed still carry secretScanPolicySha256.
func TestVerifyArtifactDirectoryAcceptsRootfsContractWithOrWithoutTheSecretScanPolicyDigest(t *testing.T) {
	tests := map[string][]map[string]string{
		"contract built before the secret scan was removed": {{"secretScanPolicySha256": strings.Repeat("2", 64)}},
		"contract built after the secret scan was removed":  nil,
	}
	for name, extraContractFields := range tests {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			release := writeBundleArtifactFixture(t, directory, extraContractFields...)
			if _, err := VerifyArtifactDirectory(directory, release); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifyArtifactDirectoryRejectsExtraSubstitutedAndSymlinkedEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"extra file", func(t *testing.T, directory string) { writeFixtureFile(t, directory, "unexpected", []byte("no")) }},
		{"retired signature file", func(t *testing.T, directory string) {
			writeFixtureFile(t, directory, "manifest.sig", []byte("signature"))
		}},
		{"substituted manifest", func(t *testing.T, directory string) {
			if err := os.Chmod(filepath.Join(directory, "manifest.json"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "manifest.json"), []byte("{}"), 0o444); err != nil {
				t.Fatal(err)
			}
		}},
		{"substituted rootfs", func(t *testing.T, directory string) {
			if err := os.Chmod(filepath.Join(directory, "rootfs.ext4"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "rootfs.ext4"), []byte("substitution"), 0o444); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, "kernel")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("rootfs.ext4", filepath.Join(directory, "kernel")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			release := writeBundleArtifactFixture(t, directory)
			test.mutate(t, directory)
			if _, err := VerifyArtifactDirectory(directory, release); err == nil {
				t.Fatal("tampered artifact directory verified")
			}
		})
	}
}

func writeBundleArtifactFixture(t *testing.T, directory string, extraContractFields ...map[string]string) releasecontract.ArtifactManifest {
	t.Helper()
	files := map[string][]byte{
		"kernel":                               []byte("kernel"),
		"kernel-provenance.json":               []byte(`{"source":"fixture"}`),
		"rootfs-source-manifest.json":          []byte(`{"source":"fixture"}`),
		"rootfs-debian-packages.lock":          []byte("package=1\n"),
		"rootfs-python.freeze":                 []byte("package==1\n"),
		"rootfs-debian-license-inventory.json": []byte(`{"licenses":[]}`),
		"rootfs-python-license-inventory.json": []byte(`{"licenses":[]}`),
		"runtime-manifest.json":                []byte(`{"component":"runtime"}`),
		"toolchain-manifest.json":              []byte(`{"component":"toolchain"}`),
		"rootfs.ext4":                          []byte("rootfs"),
		"shared.img":                           []byte("shared"),
	}
	rootfsDigest := fixtureChecksum(files["rootfs.ext4"])
	contractFields := map[string]any{"schemaVersion": 1, "contract": "secondbox-guest-rootfs", "state": "verified", "surfaceContract": "qualified", "browserPolicy": "forbid", "rootfsSha256": rootfsDigest, "policySha256": strings.Repeat("1", 64), "browserSurfacePolicySha256": strings.Repeat("3", 64)}
	for _, extra := range extraContractFields {
		for field, value := range extra {
			contractFields[field] = value
		}
	}
	contract, err := json.Marshal(contractFields)
	if err != nil {
		t.Fatal(err)
	}
	files["secondbox-rootfs-contract.json"] = contract
	for name, content := range files {
		writeFixtureFile(t, directory, name, content)
	}
	runtimeDigest := "sha256:" + fixtureChecksum(files["runtime-manifest.json"])
	toolchainDigest := "sha256:" + fixtureChecksum(files["toolchain-manifest.json"])
	entry := func(path string) map[string]any {
		return map[string]any{"path": path, "sha256": fixtureChecksum(files[path])}
	}
	manifest := map[string]any{
		"artifactVersion": "1.0.0", "architecture": "amd64", "guestProtocol": map[string]any{"minimum": 1, "maximum": 1},
		"runtimeBundle":   map[string]any{"artifactId": "fixture-runtime", "path": "runtime-manifest.json", "manifestDigest": runtimeDigest, "mandatoryGuestFeatures": []string{}},
		"toolchainBundle": map[string]any{"artifactId": "fixture-toolchain", "path": "toolchain-manifest.json", "manifestDigest": toolchainDigest, "mandatoryGuestFeatures": []string{}},
		"createdAt":       "2026-08-08T00:00:00Z", "kernel": entry("kernel"), "kernelProvenance": entry("kernel-provenance.json"), "rootfsSource": entry("rootfs-source-manifest.json"),
		"rootfsContract":   map[string]any{"path": "secondbox-rootfs-contract.json", "sha256": fixtureChecksum(files["secondbox-rootfs-contract.json"]), "state": "verified"},
		"rootfsProvenance": map[string]any{"debianPackages": entry("rootfs-debian-packages.lock"), "pythonFreeze": entry("rootfs-python.freeze"), "debianLicenses": entry("rootfs-debian-license-inventory.json"), "pythonLicenses": entry("rootfs-python-license-inventory.json")},
		"rootfs":           map[string]any{"path": "rootfs.ext4", "sha256": rootfsDigest, "format": "ext4", "sizeMiB": 1}, "shared": map[string]any{"path": "shared.img", "sha256": fixtureChecksum(files["shared.img"]), "format": "ext4"},
		"entrypoint": "/init", "runtimeEntrypoint": "/usr/local/bin/secondbox-runner-guest-entrypoint",
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, directory, "manifest.json", manifestBytes)
	var sums strings.Builder
	for _, name := range checksummedArtifactFiles {
		sums.WriteString(fixtureChecksum(files[name]) + "  " + name + "\n")
	}
	writeFixtureFile(t, directory, "SHA256SUMS", []byte(sums.String()))
	return artifactFixtureRelease(t, "sha256:"+fixtureChecksum(manifestBytes), runtimeDigest, toolchainDigest)
}

func artifactFixtureRelease(t *testing.T, manifestDigest, runtimeDigest, toolchainDigest string) releasecontract.ArtifactManifest {
	t.Helper()
	identity := releasecontract.Identity{Version: "1.2.3", Tag: "v1.2.3", SourceCommit: strings.Repeat("a", 40)}
	digest := "sha256:" + strings.Repeat("b", 64)
	ref := func(name string) releasecontract.Reference {
		return releasecontract.Reference{Location: "https://github.com/SecondStack-AI/SecondBox/releases/download/v1.2.3/" + name, Digest: digest}
	}
	binaries := []releasecontract.BinaryArtifact{}
	for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		for _, name := range []string{"secondbox", "secondbox-deploy"} {
			binaries = append(binaries, releasecontract.BinaryArtifact{Identity: identity, Name: name, Platform: platform, Location: releasecontract.BinaryLocation(identity.Version, name, platform), SHA256: strings.Repeat("b", 64)})
		}
	}
	release := releasecontract.ArtifactManifest{SchemaVersion: releasecontract.ArtifactManifestSchema, Identity: identity, OpenAPI: releasecontract.OpenAPIArtifact{Identity: identity, Reference: ref("secondbox.openapi.json")}, RunnerProtocol: releasecontract.ProtocolWindow{Minimum: 1, Maximum: 2}, GuestProtocol: releasecontract.ProtocolWindow{Minimum: 1, Maximum: 1}, Platforms: releasecontract.PlatformMatrix{HostBinaries: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}, ControlPlane: []string{"linux/amd64", "linux/arm64"}, Runner: []string{"linux/amd64"}, InstallerTools: []string{"linux/amd64"}, Guest: []string{"linux/amd64"}, QualifiedRunnerGuest: []string{"linux/amd64"}}, GoSDK: releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.GoModule + "@" + identity.Tag, Package: ref("go-sdk.zip")}, TypeScriptSDK: releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.TypeScriptPackage + "@" + identity.Version, Package: ref("secondbox.tgz")}, ControlPlane: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.ControlPlaneImage + "@" + digest}, Runner: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.RunnerImage + "@" + digest}, InstallerTools: releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.InstallerToolsImage + "@" + digest}, InstallBootstrap: releasecontract.Reference{Location: releasecontract.InstallBootstrapLocation(identity.Version), Digest: digest}, MicroVM: releasecontract.MicroVMArtifact{Identity: identity, ImageReference: releasecontract.MicroVMImage + "@" + digest, BundleManifestDigest: manifestDigest, RuntimeBundle: releasecontract.BundleComponent{ArtifactID: "fixture-runtime", ManifestDigest: runtimeDigest, MandatoryGuestFeatures: []string{}}, ToolchainBundle: releasecontract.BundleComponent{ArtifactID: "fixture-toolchain", ManifestDigest: toolchainDigest, MandatoryGuestFeatures: []string{}}}, Binaries: binaries, SBOMs: []releasecontract.Reference{ref("sbom.spdx.json")}, ArtifactAttestations: []releasecontract.Reference{ref("provenance.intoto.jsonl")}, SourceFreeSuite: releasecontract.Reference{Location: releasecontract.SourceFreeSuiteLocation(identity.Version), Digest: digest}, QualificationEvidence: releasecontract.Reference{Location: releasecontract.QualificationEvidenceLocation(identity.Version, standardresources.ArchitectureAMD64), Digest: digest}, InstallerQualificationEvidence: releasecontract.Reference{Location: releasecontract.InstallerQualificationEvidenceLocation(identity.Version), Digest: digest}, StandardBundles: []releasecontract.StandardBundleArtifact{{Identity: identity, Name: "agent-compartment", Document: ref("agent-compartment.json"), Profiles: []releasecontract.StandardProfileIdentity{{Name: "agent-compartment", Revision: 1, SpecDigest: digest}}}, {Identity: identity, Name: "durable-coding", Document: ref("durable-coding.json"), Profiles: []releasecontract.StandardProfileIdentity{{Name: "durable-coding", Revision: 1, SpecDigest: digest}}}, {Identity: identity, Name: "agent-compartment-isolated", Document: ref("agent-compartment-isolated.json"), Profiles: []releasecontract.StandardProfileIdentity{{Name: "agent-compartment-isolated", Revision: 1, SpecDigest: digest}}}}}
	release.BundledServices = releasecontract.BundledServiceImages{Postgres: "docker.io/library/postgres@" + digest}
	release.GVisor = &releasecontract.GVisorArtifact{Identity: identity, RunnerReference: releasecontract.GVisorRunnerImage + "@" + digest, ImageReference: releasecontract.GVisorImage + "@" + digest, Materialization: ref("secondbox-1.2.3-gvisor-materialization.json"), MaterializationDigest: digest, FlatRootDigest: digest, RunscRelease: "20260817.0", QualificationEvidence: ref("secondbox-1.2.3-gvisor-qualification-evidence.json"), PodQualificationEvidence: ref("secondbox-1.2.3-gvisor-pod-qualification-evidence.json")}
	if err := release.Validate(); err != nil {
		t.Fatal(err)
	}
	return release
}

func writeFixtureFile(t *testing.T, directory, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), content, 0o444); err != nil {
		t.Fatal(err)
	}
}

func fixtureChecksum(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
