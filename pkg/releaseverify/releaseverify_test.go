package releaseverify

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

func TestHTTPFetcherRejectsNonPublicLocation(t *testing.T) {
	_, err := HTTPFetcher(http.DefaultClient)(context.Background(), "http://example.com/artifact-manifest.json")
	if err == nil || !strings.Contains(err.Error(), "public HTTPS") {
		t.Fatalf("error = %v", err)
	}
}

func TestManifestObjectsBindStandardProfilesToSignedComponents(t *testing.T) {
	signed := "sha256:" + strings.Repeat("a", 64)
	runtimeDigest := "sha256:" + strings.Repeat("b", 64)
	toolchainDigest := "sha256:" + strings.Repeat("c", 64)
	documents, err := standardresources.Documents(signed, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	objects := make(map[string][]byte, len(documents))
	baseLocation := "https://example.com/base"
	baseData := []byte("base")
	objects[baseLocation] = baseData
	baseReference := releasecontract.Reference{Location: baseLocation, Digest: releasecontract.Digest(baseData)}
	sourceCommit := strings.Repeat("a", 40)
	evidenceData, err := json.Marshal(releasecontract.QualificationEvidence{
		SchemaVersion: releasecontract.QualificationEvidenceSchema, SourceCommit: sourceCommit,
		Suite: "test-scenario", PassCount: 16, WallClockSeconds: 600,
		Host: releasecontract.QualificationHostEvidence{
			KVM:                 releasecontract.QualificationDeviceEvidence{Path: "/dev/kvm", Present: true, Readable: true, Writable: true},
			TUN:                 releasecontract.QualificationDeviceEvidence{Path: "/dev/net/tun", Present: true, Readable: true, Writable: true},
			WorkspaceFilesystem: releasecontract.QualificationFilesystemEvidence{Mount: "/srv xfs", Type: "xfs"},
		},
		QualifiedAt: "2026-08-04T12:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceLocation := "https://example.com/qualification-evidence.json"
	objects[evidenceLocation] = evidenceData
	evidenceReference := releasecontract.Reference{Location: evidenceLocation, Digest: releasecontract.Digest(evidenceData)}
	bundles := make([]releasecontract.StandardBundleArtifact, 0, len(documents))
	for _, document := range documents {
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		location := "https://example.com/" + document.Name + ".json"
		objects[location] = data
		profiles := make([]releasecontract.StandardProfileIdentity, 0, len(document.Profile.Revisions))
		for _, revision := range document.Profile.Revisions {
			profiles = append(profiles, releasecontract.StandardProfileIdentity{Name: document.Name, Revision: revision.Number, SpecDigest: revision.SpecDigest})
		}
		bundles = append(bundles, releasecontract.StandardBundleArtifact{Name: document.Name, Document: releasecontract.Reference{Location: location, Digest: releasecontract.Digest(data)}, Profiles: profiles})
	}
	manifest := releasecontract.ArtifactManifest{Identity: releasecontract.Identity{SourceCommit: sourceCommit}, OpenAPI: releasecontract.OpenAPIArtifact{Reference: baseReference}, GoSDK: releasecontract.SDKArtifact{Package: baseReference}, TypeScriptSDK: releasecontract.SDKArtifact{Package: baseReference}, SourceFreeSuite: baseReference, QualificationEvidence: evidenceReference, MicroVM: releasecontract.MicroVMArtifact{SignedManifestDigest: signed, RuntimeBundle: releasecontract.SignedComponent{ManifestDigest: runtimeDigest}, ToolchainBundle: releasecontract.SignedComponent{ManifestDigest: toolchainDigest}}, StandardBundles: bundles}
	fetch := func(_ context.Context, location string) ([]byte, error) { return objects[location], nil }
	if err := verifyManifestObjects(t.Context(), manifest, fetch); err != nil {
		t.Fatal(err)
	}
	manifest.MicroVM.RuntimeBundle.ManifestDigest = "sha256:" + strings.Repeat("d", 64)
	if err := verifyManifestObjects(t.Context(), manifest, fetch); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("component substitution error = %v", err)
	}
}

func TestStrictTopLevelDocuments(t *testing.T) {
	fetch := func(context.Context, string) ([]byte, error) { return []byte(`{"schemaVersion":"wrong"}`), nil }
	if _, err := ArtifactManifest(context.Background(), "https://example.com/manifest", fetch); err == nil {
		t.Fatal("malformed artifact manifest was accepted")
	}
}
