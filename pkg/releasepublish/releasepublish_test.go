package releasepublish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func TestValidateEvidence(t *testing.T) {
	identity := releasecontract.Identity{Version: "1.2.3", Tag: "v1.2.3", SourceCommit: strings.Repeat("a", 40)}
	manifest := releasecontract.ArtifactManifest{Identity: identity, MicroVM: releasecontract.MicroVMArtifact{SignedManifestDigest: "sha256:" + strings.Repeat("6", 64)}}
	digest := "sha256:" + strings.Repeat("9", 64)
	evidence := CandidateEvidence{SchemaVersion: 1, Identity: manifest.Identity, ArtifactManifestDigest: digest, SignedGuestManifestDigest: manifest.MicroVM.SignedManifestDigest, Architecture: "linux/amd64", RunnerEnvironment: "sha256:" + strings.Repeat("4", 64), Result: "passed"}
	if err := ValidateEvidence(evidence, manifest, digest); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*CandidateEvidence)
	}{
		{"absent qualification", func(value *CandidateEvidence) { value.Result = "" }},
		{"tag mismatch", func(value *CandidateEvidence) { value.Tag = "v9.9.9" }},
		{"manifest mismatch", func(value *CandidateEvidence) { value.ArtifactManifestDigest = "sha256:" + strings.Repeat("8", 64) }},
		{"guest mismatch", func(value *CandidateEvidence) { value.SignedGuestManifestDigest = "sha256:" + strings.Repeat("7", 64) }},
		{"architecture mismatch", func(value *CandidateEvidence) { value.Architecture = "linux/arm64" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := evidence
			test.mutate(&changed)
			if ValidateEvidence(changed, manifest, digest) == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestPublicationInputBindsCandidateAndEvidence(t *testing.T) {
	directory := t.TempDir()
	manifest := publicationTestManifest()
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestName := "secondbox-1.2.3-artifact-manifest.json"
	if err := os.WriteFile(filepath.Join(directory, manifestName), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "control-plane.oci.tar"), []byte("oci"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := CandidateEvidenceFor(manifest, manifestData, "sha256:"+strings.Repeat("9", 64))
	if err != nil {
		t.Fatal(err)
	}
	evidenceData, _ := json.Marshal(evidence)
	evidencePath := filepath.Join(t.TempDir(), "secondbox-1.2.3-candidate-kvm-evidence.json")
	if err := os.WriteFile(evidencePath, evidenceData, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := BuildPublicationInput(directory, evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublicationSources(directory, evidencePath, input); err != nil {
		t.Fatal(err)
	}
	inputName := "secondbox-1.2.3-publication-input.json"
	inputData, _ := json.Marshal(input)
	if err := os.WriteFile(filepath.Join(directory, inputName), inputData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, filepath.Base(evidencePath)), evidenceData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublicationDirectory(directory, input, inputName); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "control-plane.oci.tar"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublicationDirectory(directory, input, inputName); err == nil {
		t.Fatal("transport checksum drift was accepted")
	}
}

func publicationTestManifest() releasecontract.ArtifactManifest {
	identity := releasecontract.Identity{Version: "1.2.3", Tag: "v1.2.3", SourceCommit: strings.Repeat("a", 40)}
	digest := "sha256:" + strings.Repeat("1", 64)
	ref := func(name string) releasecontract.Reference {
		return releasecontract.Reference{Location: "https://github.com/SecondStack-AI/SecondBox/releases/download/v1.2.3/" + name, Digest: digest}
	}
	binaries := make([]releasecontract.BinaryArtifact, 0, 8)
	for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		for _, name := range []string{"secondbox", "secondbox-deploy"} {
			binaries = append(binaries, releasecontract.BinaryArtifact{Identity: identity, Name: name, Platform: platform, Location: releasecontract.BinaryLocation(identity.Version, name, platform), SHA256: strings.TrimPrefix(digest, "sha256:")})
		}
	}
	return releasecontract.ArtifactManifest{
		SchemaVersion: releasecontract.ArtifactManifestSchema, Identity: identity,
		OpenAPI:        releasecontract.OpenAPIArtifact{Identity: identity, Reference: ref("secondbox-1.2.3-openapi.json")},
		RunnerProtocol: releasecontract.ProtocolWindow{Minimum: 1, Maximum: 1}, GuestProtocol: releasecontract.ProtocolWindow{Minimum: 1, Maximum: 1},
		Platforms:     releasecontract.PlatformMatrix{HostBinaries: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}, ControlPlane: []string{"linux/amd64", "linux/arm64"}, Runner: []string{"linux/amd64"}, Guest: []string{"linux/amd64"}, QualifiedRunnerGuest: []string{"linux/amd64"}},
		GoSDK:         releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.GoModule + "@v1.2.3", Package: ref("secondbox-1.2.3-go-module.tar.gz")},
		TypeScriptSDK: releasecontract.SDKArtifact{Identity: identity, Coordinate: releasecontract.TypeScriptPackage + "@1.2.3", Package: ref("secondstack-ai-secondbox-1.2.3.tgz")},
		ControlPlane:  releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.ControlPlaneImage + "@" + digest},
		Runner:        releasecontract.OCIArtifact{Identity: identity, Reference: releasecontract.RunnerImage + "@" + digest},
		MicroVM:       releasecontract.MicroVMArtifact{Identity: identity, ImageReference: releasecontract.MicroVMImage + "@" + digest, SignedManifestDigest: digest, SigningKeyFingerprint: "SHA256:" + strings.Repeat("A", 64)},
		Binaries:      binaries, SBOMs: []releasecontract.Reference{ref("secondbox-1.2.3.spdx.json")}, ArtifactAttestations: []releasecontract.Reference{ref("secondbox-1.2.3-provenance.json")}, SourceFreeSuite: ref("secondbox-1.2.3-source-free-qualify"),
		StandardBundles: []releasecontract.StandardBundleArtifact{{Identity: identity, Name: "agent-compartment", Document: ref("agent-compartment.standard-bundle.json"), Profiles: []releasecontract.StandardProfileIdentity{{Name: "agent-compartment", Revision: 1, SpecDigest: digest}}}, {Identity: identity, Name: "durable-coding", Document: ref("durable-coding.standard-bundle.json"), Profiles: []releasecontract.StandardProfileIdentity{{Name: "durable-coding", Revision: 1, SpecDigest: digest}}}},
	}
}

func TestPlanIsRetrySafe(t *testing.T) {
	desired := []Object{{Coordinate: "npm:@secondstack-ai/secondbox@1.2.3", Digest: "sha512:one"}, {Coordinate: "ghcr:runner:v1.2.3", Digest: "sha256:two"}}
	missing, err := Plan(map[string]string{desired[0].Coordinate: desired[0].Digest}, desired)
	if err != nil || len(missing) != 1 || missing[0] != desired[1] {
		t.Fatalf("unexpected partial publication plan: %#v, %v", missing, err)
	}
	if replay, err := Plan(map[string]string{desired[0].Coordinate: desired[0].Digest, desired[1].Coordinate: desired[1].Digest}, desired); err != nil || len(replay) != 0 {
		t.Fatalf("exact replay failed: %#v, %v", replay, err)
	}
	if _, err := Plan(map[string]string{desired[0].Coordinate: "sha512:mutated"}, desired); err == nil {
		t.Fatal("mutation was accepted")
	}
}
