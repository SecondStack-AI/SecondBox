package releaseverify

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

func TestDirectoryFetcherAcceptsOnlyExactCandidateObjects(t *testing.T) {
	directory := t.TempDir()
	object := filepath.Join(directory, "secondbox-object.json")
	if err := os.WriteFile(object, []byte("candidate bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetch := DirectoryFetcher(directory)
	location := "https://github.com/SecondStack-AI/SecondBox/releases/download/v0.4.0/secondbox-object.json"
	data, err := fetch(t.Context(), location)
	if err != nil || string(data) != "candidate bytes" {
		t.Fatalf("candidate fetch = %q, %v", data, err)
	}
	for _, rejected := range []string{
		"http://github.com/SecondStack-AI/SecondBox/releases/download/v0.4.0/secondbox-object.json",
		"https://example.com/SecondStack-AI/SecondBox/releases/download/v0.4.0/secondbox-object.json",
		location + "?replacement=true",
	} {
		if _, err := fetch(t.Context(), rejected); err == nil {
			t.Fatalf("candidate fetch accepted %q", rejected)
		}
	}
	if err := os.Symlink(object, filepath.Join(directory, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(t.Context(), "https://github.com/SecondStack-AI/SecondBox/releases/download/v0.4.0/linked.json"); err == nil || !strings.Contains(err.Error(), "non-symbolic-link regular file") {
		t.Fatalf("candidate symlink error = %v", err)
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
	installerEvidence := releasecontract.InstallerQualificationEvidence{
		SchemaVersion: releasecontract.InstallerQualificationEvidenceSchema, SourceCommit: sourceCommit,
		Suite: "test-installer-qualified", PassCount: 19, WallClockSeconds: 1200,
		Host: releasecontract.QualificationHostEvidence{
			KVM:                 releasecontract.QualificationDeviceEvidence{Path: "/dev/kvm", Present: true, Readable: true, Writable: true},
			TUN:                 releasecontract.QualificationDeviceEvidence{Path: "/dev/net/tun", Present: true, Readable: true, Writable: true},
			WorkspaceFilesystem: releasecontract.QualificationFilesystemEvidence{Mount: "/srv xfs", Type: "xfs"},
		},
		ReleaseManifestDigest: "sha256:" + strings.Repeat("d", 64), FilesystemIdentity: "8:16", RebootPassed: true,
		QualifiedAt: "2026-08-04T12:00:00Z",
	}
	installerEvidenceData, err := json.Marshal(installerEvidence)
	if err != nil {
		t.Fatal(err)
	}
	installerEvidenceLocation := "https://example.com/installer-qualification-evidence.json"
	objects[installerEvidenceLocation] = installerEvidenceData
	installerEvidenceReference := releasecontract.Reference{Location: installerEvidenceLocation, Digest: releasecontract.Digest(installerEvidenceData)}
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
	manifest := releasecontract.ArtifactManifest{Identity: releasecontract.Identity{SourceCommit: sourceCommit}, OpenAPI: releasecontract.OpenAPIArtifact{Reference: baseReference}, GoSDK: releasecontract.SDKArtifact{Package: baseReference}, TypeScriptSDK: releasecontract.SDKArtifact{Package: baseReference}, InstallBootstrap: baseReference, SourceFreeSuite: baseReference, QualificationEvidence: evidenceReference, InstallerQualificationEvidence: installerEvidenceReference, MicroVM: releasecontract.MicroVMArtifact{SignedManifestDigest: signed, RuntimeBundle: releasecontract.SignedComponent{ManifestDigest: runtimeDigest}, ToolchainBundle: releasecontract.SignedComponent{ManifestDigest: toolchainDigest}}, StandardBundles: bundles}
	qualificationSubject, err := manifest.InstallerQualificationSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	installerEvidence.ReleaseManifestDigest = qualificationSubject
	installerEvidenceData, err = json.Marshal(installerEvidence)
	if err != nil {
		t.Fatal(err)
	}
	objects[installerEvidenceLocation] = installerEvidenceData
	manifest.InstallerQualificationEvidence.Digest = releasecontract.Digest(installerEvidenceData)
	fetchCalls := map[string]int{}
	fetch := func(_ context.Context, location string) ([]byte, error) {
		fetchCalls[location]++
		return objects[location], nil
	}
	if err := verifyManifestObjects(t.Context(), manifest, fetch); err != nil {
		t.Fatal(err)
	}
	if fetchCalls[evidenceLocation] != 1 || fetchCalls[installerEvidenceLocation] != 1 {
		t.Fatalf("qualification evidence fetches = scenario %d installer %d", fetchCalls[evidenceLocation], fetchCalls[installerEvidenceLocation])
	}
	manifest.MicroVM.RuntimeBundle.ManifestDigest = "sha256:" + strings.Repeat("d", 64)
	qualificationSubject, err = manifest.InstallerQualificationSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	installerEvidence.ReleaseManifestDigest = qualificationSubject
	installerEvidenceData, err = json.Marshal(installerEvidence)
	if err != nil {
		t.Fatal(err)
	}
	objects[installerEvidenceLocation] = installerEvidenceData
	manifest.InstallerQualificationEvidence.Digest = releasecontract.Digest(installerEvidenceData)
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
