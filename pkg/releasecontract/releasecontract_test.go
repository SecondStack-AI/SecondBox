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

func TestCompareVersionsUsesSemVerPrecedence(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"0.5.1", "0.5.0", 1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.2", "1.0.0-alpha.10", -1},
		{"1.0.0-beta", "1.0.0-alpha", 1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"2.0.0", "2.0.0", 0},
	}
	for _, test := range tests {
		got, err := CompareVersions(test.left, test.right)
		if err != nil {
			t.Fatalf("CompareVersions(%q, %q): %v", test.left, test.right, err)
		}
		if (got < 0 && test.want >= 0) || (got > 0 && test.want <= 0) || (got == 0 && test.want != 0) {
			t.Fatalf("CompareVersions(%q, %q) = %d, want sign %d", test.left, test.right, got, test.want)
		}
	}
	if _, err := CompareVersions("1.0", "1.0.0"); err == nil {
		t.Fatal("invalid version comparison succeeded")
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
		{name: "missing installer tools identity", mutate: func(value *ArtifactManifest) { value.InstallerTools.Identity = Identity{} }, want: "installer tools identity"},
		{name: "mutable installer tools", mutate: func(value *ArtifactManifest) { value.InstallerTools.Reference = InstallerToolsImage + ":latest" }, want: "immutable canonical"},
		{name: "malformed digest", mutate: func(value *ArtifactManifest) { value.OpenAPI.Digest = "sha256:no" }, want: "canonical sha256"},
		{name: "absent checksum", mutate: func(value *ArtifactManifest) { value.Binaries[0].SHA256 = "" }, want: "checksum"},
		{name: "unsupported platform", mutate: func(value *ArtifactManifest) { value.Platforms.Runner = append(value.Platforms.Runner, "linux/arm64") }, want: "lacks required qualification"},
		{name: "incompatible protocol window", mutate: func(value *ArtifactManifest) { value.RunnerProtocol = ProtocolWindow{Minimum: 3, Maximum: 2} }, want: "window is invalid"},
		{name: "malformed signing identity", mutate: func(value *ArtifactManifest) { value.MicroVM.SigningKeyFingerprint = "SHA256:no" }, want: "fingerprint"},
		{name: "missing qualification evidence", mutate: func(value *ArtifactManifest) { value.QualificationEvidence = Reference{} }, want: "qualification evidence"},
		{name: "noncanonical qualification evidence", mutate: func(value *ArtifactManifest) {
			value.QualificationEvidence.Location = "https://example.com/evidence.json"
		}, want: "not canonical"},
		{name: "missing runtime component", mutate: func(value *ArtifactManifest) { value.MicroVM.RuntimeBundle = SignedComponent{} }, want: "runtime bundle artifact ID"},
		{name: "aliased components", mutate: func(value *ArtifactManifest) {
			value.MicroVM.ToolchainBundle.ManifestDigest = value.MicroVM.RuntimeBundle.ManifestDigest
		}, want: "distinct identities"},
		{name: "missing isolated standard bundle", mutate: func(value *ArtifactManifest) {
			value.StandardBundles = value.StandardBundles[:2]
		}, want: "agent-compartment-isolated"},
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

func TestInstallerCandidateAndFinalReleaseShareQualificationSubject(t *testing.T) {
	final := validManifest()
	candidate := final
	candidate.Candidate = true
	candidate.InstallerQualificationEvidence = Reference{}
	if err := candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	finalSubject, err := final.InstallerQualificationSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	candidateSubject, err := candidate.InstallerQualificationSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	if candidateSubject != finalSubject {
		t.Fatalf("candidate subject %s != final subject %s", candidateSubject, finalSubject)
	}
	if candidate.InstallerQualificationEvidence != (Reference{}) || !candidate.Candidate {
		t.Fatal("candidate claimed final installer evidence")
	}
}

func TestQualificationEvidenceRequiresCompleteCleanReleaseRun(t *testing.T) {
	evidence := QualificationEvidence{
		SchemaVersion: QualificationEvidenceSchema, SourceCommit: testCommit,
		Suite: "test-scenario", PassCount: 16, WallClockSeconds: 600,
		Host: QualificationHostEvidence{
			Platform:            "linux-amd64",
			KVM:                 QualificationDeviceEvidence{Path: "/dev/kvm", Present: true, Readable: true, Writable: true},
			TUN:                 QualificationDeviceEvidence{Path: "/dev/net/tun", Present: true, Readable: true, Writable: true},
			WorkspaceFilesystem: QualificationFilesystemEvidence{Mount: "/srv/secondbox xfs", Type: "xfs"},
		},
		QualifiedAt: "2026-08-04T12:00:00Z",
	}
	decoded, err := DecodeQualificationEvidence(mustJSON(t, evidence))
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateForRelease(testCommit); err != nil {
		t.Fatal(err)
	}
	decoded.Host.Platform = ""
	if err := decoded.ValidateForRelease(testCommit); err == nil || !strings.Contains(err.Error(), "host platform") {
		t.Fatalf("missing host platform error = %v", err)
	}
	decoded.Host.Platform = "linux-amd64"
	decoded.SchemaVersion = LegacyQualificationEvidenceSchema
	decoded.Host.Platform = ""
	if err := decoded.ValidateForRelease(testCommit); err != nil {
		t.Fatalf("legacy v1 qualification evidence = %v", err)
	}
	decoded.SchemaVersion = QualificationEvidenceSchema
	decoded.Host.Platform = "linux-amd64"
	decoded.RepositoryDirty = true
	if err := decoded.ValidateForRelease(testCommit); err == nil || !strings.Contains(err.Error(), "dirty repository") {
		t.Fatalf("dirty qualification evidence error = %v", err)
	}
	decoded.RepositoryDirty = false
	if err := decoded.ValidateForRelease(strings.Repeat("f", 40)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("commit-mismatched qualification evidence error = %v", err)
	}
}

func TestInstallerQualificationSubjectExcludesOnlyItsOwnEvidenceReference(t *testing.T) {
	manifest := validManifest()
	one, err := manifest.InstallerQualificationSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.InstallerQualificationEvidence = Reference{Location: InstallerQualificationEvidenceLocation(manifest.Version), Digest: "sha256:" + strings.Repeat("e", 64)}
	two, err := manifest.InstallerQualificationSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("installer evidence reference changed qualification subject: %s != %s", one, two)
	}
	manifest.Runner.Reference = RunnerImage + "@sha256:" + strings.Repeat("e", 64)
	three, err := manifest.InstallerQualificationSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	if three == two {
		t.Fatal("Runner identity did not change installer qualification subject")
	}
}

func TestInstallerQualificationEvidenceRequiresRebootAndPinnedRelease(t *testing.T) {
	evidence := InstallerQualificationEvidence{
		SchemaVersion: InstallerQualificationEvidenceSchema, SourceCommit: testCommit,
		Suite: "test-installer-qualified", PassCount: 19, WallClockSeconds: 1200,
		Host: QualificationHostEvidence{
			Platform:            "linux-amd64",
			KVM:                 QualificationDeviceEvidence{Path: "/dev/kvm", Present: true, Readable: true, Writable: true},
			TUN:                 QualificationDeviceEvidence{Path: "/dev/net/tun", Present: true, Readable: true, Writable: true},
			WorkspaceFilesystem: QualificationFilesystemEvidence{Mount: "/srv/secondbox xfs", Type: "xfs"},
		},
		ReleaseManifestDigest: testDigest, FilesystemIdentity: "8:16", RebootPassed: true,
		QualifiedAt: "2026-08-08T12:00:00Z",
	}
	decoded, err := DecodeInstallerQualificationEvidence(mustJSON(t, evidence))
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateForRelease(testCommit, testDigest); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateForRelease(testCommit, "sha256:"+strings.Repeat("f", 64)); err == nil || !strings.Contains(err.Error(), "release identity") {
		t.Fatalf("mismatched installer release identity error = %v", err)
	}
	decoded.RebootPassed = false
	if err := decoded.Validate(); err == nil {
		t.Fatal("installer evidence without reboot recovery was accepted")
	}
	decoded.RebootPassed = true
	decoded.SchemaVersion = LegacyInstallerQualificationEvidenceSchema
	decoded.Host.Platform = ""
	if err := decoded.ValidateForRelease(testCommit, testDigest); err != nil {
		t.Fatalf("legacy v1 installer evidence = %v", err)
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
			InstallerTools: []string{"linux/amd64"}, Guest: []string{"linux/amd64"}, QualifiedRunnerGuest: []string{"linux/amd64"},
		},
		GoSDK:                          SDKArtifact{Identity: identity, Coordinate: GoModule + "@" + identity.Tag, Package: ref("go-sdk.zip")},
		TypeScriptSDK:                  SDKArtifact{Identity: identity, Coordinate: TypeScriptPackage + "@" + identity.Version, Package: ref("secondbox.tgz")},
		ControlPlane:                   OCIArtifact{Identity: identity, Reference: ControlPlaneImage + "@" + testDigest},
		Runner:                         OCIArtifact{Identity: identity, Reference: RunnerImage + "@" + testDigest},
		InstallerTools:                 OCIArtifact{Identity: identity, Reference: InstallerToolsImage + "@" + testDigest},
		BundledServices:                BundledServiceImages{Postgres: "docker.io/library/postgres@" + testDigest},
		InstallBootstrap:               Reference{Location: InstallBootstrapLocation(identity.Version), Digest: testDigest},
		MicroVM:                        MicroVMArtifact{Identity: identity, ImageReference: MicroVMImage + "@" + testDigest, SignedManifestDigest: testDigest, SigningKeyFingerprint: testKey, RuntimeBundle: SignedComponent{ArtifactID: "test-runtime", ManifestDigest: testRuntimeDigest, MandatoryGuestFeatures: []string{}}, ToolchainBundle: SignedComponent{ArtifactID: "test-toolchain", ManifestDigest: testToolchainDigest, MandatoryGuestFeatures: []string{}}},
		Binaries:                       binaries,
		SBOMs:                          []Reference{ref("sbom.spdx.json")},
		ArtifactAttestations:           []Reference{ref("provenance.intoto.jsonl")},
		SourceFreeSuite:                Reference{Location: SourceFreeSuiteLocation(identity.Version), Digest: testDigest},
		QualificationEvidence:          Reference{Location: QualificationEvidenceLocation(identity.Version), Digest: testDigest},
		InstallerQualificationEvidence: Reference{Location: InstallerQualificationEvidenceLocation(identity.Version), Digest: testDigest},
		StandardBundles: []StandardBundleArtifact{
			{Identity: identity, Name: "agent-compartment", Document: ref("agent-compartment.json"), Profiles: []StandardProfileIdentity{{Name: "agent-compartment", Revision: 1, SpecDigest: testDigest}}},
			{Identity: identity, Name: "durable-coding", Document: ref("durable-coding.json"), Profiles: []StandardProfileIdentity{{Name: "durable-coding", Revision: 1, SpecDigest: testDigest}}},
			{Identity: identity, Name: "agent-compartment-isolated", Document: ref("agent-compartment-isolated.json"), Profiles: []StandardProfileIdentity{{Name: "agent-compartment-isolated", Revision: 1, SpecDigest: testDigest}}},
		},
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
