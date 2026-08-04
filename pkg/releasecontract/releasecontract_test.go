package releasecontract

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testCommit          = "0123456789abcdef0123456789abcdef01234567"
	testDigest          = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testRuntimeDigest   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testToolchainDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testKey             = "SHA256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"
)

func TestParseTag(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "v1.2.3-rc.1"} {
		if _, err := ParseTag(tag); err != nil {
			t.Fatalf("ParseTag(%q): %v", tag, err)
		}
	}
	for _, tag := range []string{"1.2.3", "v01.2.3", "v1.2", "v1.2.3+local", "latest"} {
		if _, err := ParseTag(tag); err == nil {
			t.Fatalf("ParseTag(%q) unexpectedly succeeded", tag)
		}
	}
}

func TestArtifactManifestValidationRejectsInvalidReleaseContent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ArtifactManifest)
		want   string
	}{
		{name: "missing artifacts", mutate: func(value *ArtifactManifest) { value.Binaries = nil }, want: "requires binaries"},
		{name: "inconsistent version", mutate: func(value *ArtifactManifest) { value.GoSDK.Identity.Version = "9.9.9" }, want: "identity does not match"},
		{name: "inconsistent commit", mutate: func(value *ArtifactManifest) { value.Runner.Identity.SourceCommit = strings.Repeat("f", 40) }, want: "identity does not match"},
		{name: "mutable OCI reference", mutate: func(value *ArtifactManifest) { value.Runner.Reference = RunnerImage + ":latest" }, want: "immutable canonical"},
		{name: "malformed digest", mutate: func(value *ArtifactManifest) { value.OpenAPI.Digest = "sha256:no" }, want: "canonical sha256"},
		{name: "absent checksum", mutate: func(value *ArtifactManifest) { value.Binaries[0].SHA256 = "" }, want: "checksum"},
		{name: "unsupported platform", mutate: func(value *ArtifactManifest) { value.Platforms.Runner = append(value.Platforms.Runner, "linux/arm64") }, want: "lacks required qualification"},
		{name: "incompatible protocol window", mutate: func(value *ArtifactManifest) { value.RunnerProtocol = ProtocolWindow{Minimum: 3, Maximum: 2} }, want: "window is invalid"},
		{name: "malformed signing identity", mutate: func(value *ArtifactManifest) { value.MicroVM.SigningKeyFingerprint = "SHA256:no" }, want: "fingerprint"},
		{name: "missing runtime component", mutate: func(value *ArtifactManifest) { value.MicroVM.RuntimeBundle = SignedComponent{} }, want: "runtime bundle artifact ID"},
		{name: "aliased components", mutate: func(value *ArtifactManifest) {
			value.MicroVM.ToolchainBundle.ManifestDigest = value.MicroVM.RuntimeBundle.ManifestDigest
		}, want: "distinct identities"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifest()
			test.mutate(&manifest)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestVerifyFinalRelease(t *testing.T) {
	manifest := validManifest()
	manifestBytes := mustJSON(t, manifest)
	manifestRef := Reference{Location: ArtifactManifestLocation(manifest.Version), Digest: Digest(manifestBytes)}
	qualification := validQualification(manifest, manifestRef)
	qualificationBytes := mustJSON(t, qualification)
	index := ReleaseIndex{
		SchemaVersion:    ReleaseIndexSchema,
		Identity:         manifest.Identity,
		ArtifactManifest: manifestRef,
		Qualification: Reference{
			Location: QualificationAttestationLocation(manifest.Version),
			Digest:   Digest(qualificationBytes),
		},
	}
	indexBytes := mustJSON(t, index)
	if _, _, _, err := VerifyFinalRelease(indexBytes, manifestBytes, qualificationBytes); err != nil {
		t.Fatalf("VerifyFinalRelease(): %v", err)
	}

	t.Run("qualification bound to another manifest", func(t *testing.T) {
		changed := qualification
		changed.ArtifactManifest.Digest = testDigest
		changedBytes := mustJSON(t, changed)
		changedIndex := index
		changedIndex.Qualification.Digest = Digest(changedBytes)
		_, _, _, err := VerifyFinalRelease(mustJSON(t, changedIndex), manifestBytes, changedBytes)
		if err == nil || !strings.Contains(err.Error(), "different artifact manifest") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("signing identity mismatch", func(t *testing.T) {
		changed := qualification
		changed.Guest.SigningKeyFingerprint = "SHA256:FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"
		changedBytes := mustJSON(t, changed)
		changedIndex := index
		changedIndex.Qualification.Digest = Digest(changedBytes)
		_, _, _, err := VerifyFinalRelease(mustJSON(t, changedIndex), manifestBytes, changedBytes)
		if err == nil || !strings.Contains(err.Error(), "guest signing identity") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("protocol outside compatibility window", func(t *testing.T) {
		changed := qualification
		changed.RunnerProtocolVersion = 3
		changedBytes := mustJSON(t, changed)
		changedIndex := index
		changedIndex.Qualification.Digest = Digest(changedBytes)
		_, _, _, err := VerifyFinalRelease(mustJSON(t, changedIndex), manifestBytes, changedBytes)
		if err == nil || !strings.Contains(err.Error(), "compatibility windows") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("incomplete final index", func(t *testing.T) {
		changed := index
		changed.Qualification = Reference{}
		if _, err := DecodeReleaseIndex(mustJSON(t, changed)); err == nil {
			t.Fatal("DecodeReleaseIndex() unexpectedly succeeded")
		}
	})

	t.Run("self referential evidence", func(t *testing.T) {
		var raw map[string]any
		if err := json.Unmarshal(indexBytes, &raw); err != nil {
			t.Fatal(err)
		}
		raw["evidence"] = map[string]any{"digest": testDigest}
		if _, err := DecodeReleaseIndex(mustJSON(t, raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestImmutableRetryAndMixedVersions(t *testing.T) {
	if err := AcceptImmutableRetry([]byte("same"), []byte("same")); err != nil {
		t.Fatal(err)
	}
	if err := AcceptImmutableRetry([]byte("old"), []byte("new")); err == nil {
		t.Fatal("different immutable publication bytes unexpectedly accepted")
	}
	manifest := validManifest()
	if err := ValidateRuntimeCombination(manifest, manifest.Identity, manifest.Identity, manifest.Identity); err != nil {
		t.Fatal(err)
	}
	mixed := manifest.Identity
	mixed.Version = "1.2.2"
	mixed.Tag = "v1.2.2"
	if err := ValidateRuntimeCombination(manifest, manifest.Identity, mixed, manifest.Identity); err == nil {
		t.Fatal("mixed runtime identities unexpectedly accepted")
	}
}

func validManifest() ArtifactManifest {
	identity := Identity{Version: "1.2.3", Tag: "v1.2.3", SourceCommit: testCommit}
	ref := func(name string) Reference {
		return Reference{Location: "https://github.com/SecondStack-AI/SecondBox/releases/download/v1.2.3/" + name, Digest: testDigest}
	}
	binaries := make([]BinaryArtifact, 0, 8)
	for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		for _, name := range []string{"secondbox", "secondbox-deploy"} {
			binaries = append(binaries, BinaryArtifact{
				Identity: identity, Name: name, Platform: platform,
				Location: BinaryLocation(identity.Version, name, platform), SHA256: strings.TrimPrefix(testDigest, "sha256:"),
			})
		}
	}
	return ArtifactManifest{
		SchemaVersion:  ArtifactManifestSchema,
		Identity:       identity,
		OpenAPI:        OpenAPIArtifact{Identity: identity, Reference: ref("secondbox.openapi.json")},
		RunnerProtocol: ProtocolWindow{Minimum: 1, Maximum: 2},
		GuestProtocol:  ProtocolWindow{Minimum: 1, Maximum: 1},
		Platforms: PlatformMatrix{
			HostBinaries: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"},
			ControlPlane: []string{"linux/amd64", "linux/arm64"}, Runner: []string{"linux/amd64"},
			Guest: []string{"linux/amd64"}, QualifiedRunnerGuest: []string{"linux/amd64"},
		},
		GoSDK:                SDKArtifact{Identity: identity, Coordinate: GoModule + "@" + identity.Tag, Package: ref("go-sdk.zip")},
		TypeScriptSDK:        SDKArtifact{Identity: identity, Coordinate: TypeScriptPackage + "@" + identity.Version, Package: ref("secondbox.tgz")},
		ControlPlane:         OCIArtifact{Identity: identity, Reference: ControlPlaneImage + "@" + testDigest},
		Runner:               OCIArtifact{Identity: identity, Reference: RunnerImage + "@" + testDigest},
		MicroVM:              MicroVMArtifact{Identity: identity, ImageReference: MicroVMImage + "@" + testDigest, SignedManifestDigest: testDigest, SigningKeyFingerprint: testKey, RuntimeBundle: SignedComponent{ArtifactID: "test-runtime", ManifestDigest: testRuntimeDigest, MandatoryGuestFeatures: []string{}}, ToolchainBundle: SignedComponent{ArtifactID: "test-toolchain", ManifestDigest: testToolchainDigest, MandatoryGuestFeatures: []string{}}},
		Binaries:             binaries,
		SBOMs:                []Reference{ref("sbom.spdx.json")},
		ArtifactAttestations: []Reference{ref("provenance.intoto.jsonl")},
		SourceFreeSuite:      Reference{Location: SourceFreeSuiteLocation(identity.Version), Digest: testDigest},
		StandardBundles: []StandardBundleArtifact{
			{Identity: identity, Name: "agent-compartment", Document: ref("agent-compartment.json"), Profiles: []StandardProfileIdentity{{Name: "agent-compartment", Revision: 1, SpecDigest: testDigest}}},
			{Identity: identity, Name: "durable-coding", Document: ref("durable-coding.json"), Profiles: []StandardProfileIdentity{{Name: "durable-coding", Revision: 1, SpecDigest: testDigest}}},
		},
	}
}

func validQualification(manifest ArtifactManifest, manifestRef Reference) QualificationAttestation {
	return QualificationAttestation{
		SchemaVersion:    QualificationAttestationSchema,
		Identity:         manifest.Identity,
		ArtifactManifest: manifestRef,
		Suite:            "secondbox-source-free-v1", SuiteDigest: testDigest, Architecture: "linux/amd64",
		RunnerProtocolVersion: 2, GuestProtocolGeneration: 1,
		Guest: QualifiedGuest{ManifestDigest: manifest.MicroVM.SignedManifestDigest, SigningKeyFingerprint: manifest.MicroVM.SigningKeyFingerprint},
		RunnerEnvironment: RunnerEnvironment{
			RunnerImageReference: manifest.Runner.Reference, OperatingSystem: "linux", Kernel: "6.12.0",
			FirecrackerVersion: "1.16.1", CPUModel: "test-cpu",
		},
		Result: "passed", CompletedAt: "2026-08-03T12:00:00Z",
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
