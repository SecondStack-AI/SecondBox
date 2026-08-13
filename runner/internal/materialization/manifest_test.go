package materialization

import "testing"

func TestManifestValidatesBackendSpecificIdentity(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Key: Key{BackendKind: BackendMicrosandbox, GuestArchitecture: "amd64", RuntimeManifestDigest: digest, ToolchainManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		SourceOCIManifestDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		FlatRootDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		LaunchArtifacts: []LaunchArtifact{{ID: "flat-root", SHA256: digest}},
		AgentProtocolGeneration: 1,
		AgentFeatures: []string{"exec", "files"},
		BackendBuildID: "microsandbox-v0.6.8+local.1",
		HelperBuildID: "secondbox-microsandbox-helper-v1",
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.SourceOCIManifestDigest = "latest"
	if err := manifest.Validate(); err == nil {
		t.Fatal("mutable Microsandbox source identity was accepted")
	}
}
