package releasepublish

import (
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func TestValidateEvidence(t *testing.T) {
	identity := releasecontract.Identity{Version: "1.2.3", Tag: "v1.2.3", SourceCommit: strings.Repeat("a", 40)}
	manifest := releasecontract.ArtifactManifest{Identity: identity, MicroVM: releasecontract.MicroVMArtifact{SignedManifestDigest: "sha256:" + strings.Repeat("6", 64)}}
	digest := "sha256:" + strings.Repeat("9", 64)
	evidence := CandidateEvidence{SchemaVersion: 1, Identity: manifest.Identity, ArtifactManifestDigest: digest, SignedGuestManifestDigest: manifest.MicroVM.SignedManifestDigest, Architecture: "linux/amd64", RunnerEnvironment: "runner-environment-digest", Result: "passed"}
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
