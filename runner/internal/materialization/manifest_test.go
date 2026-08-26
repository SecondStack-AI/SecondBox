package materialization

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestValidatesBackendSpecificIdentity(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := Manifest{
		SchemaVersion:           SchemaVersion,
		Key:                     Key{BackendKind: BackendMicrosandbox, GuestArchitecture: "amd64", RuntimeManifestDigest: digest, ToolchainManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		SourceOCIManifestDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		FlatRootDigest:          "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		LaunchArtifacts:         []LaunchArtifact{{ID: "flat-root", SHA256: digest}},
		AgentProtocolGeneration: 1,
		AgentFeatures:           []string{"exec", "files"},
		BackendBuildID:          "microsandbox-v0.6.8+local.1",
		HelperBuildID:           "secondbox-microsandbox-helper-v1",
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.SourceOCIManifestDigest = "latest"
	if err := manifest.Validate(); err == nil {
		t.Fatal("mutable Microsandbox source identity was accepted")
	}
}

func TestLoadRejectsWrongPinnedMaterializationDigest(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := Manifest{
		SchemaVersion:           SchemaVersion,
		Key:                     Key{BackendKind: BackendMicrosandbox, GuestArchitecture: "amd64", RuntimeManifestDigest: digest, ToolchainManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		SourceOCIManifestDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		FlatRootDigest:          "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		LaunchArtifacts:         []LaunchArtifact{{ID: "helper", SHA256: digest}},
		AgentProtocolGeneration: 1,
		AgentFeatures:           []string{"exec"},
		BackendBuildID:          "microsandbox-v0.6.8+local.1",
		HelperBuildID:           "secondbox-microsandbox-helper-v1",
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "materialization.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path, "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if err == nil || !strings.Contains(err.Error(), "digest differs from pinned identity") {
		t.Fatalf("wrong pinned materialization digest error = %v", err)
	}
}

func TestManifestValidatesGVisorIdentity(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := Manifest{
		SchemaVersion:           SchemaVersion,
		Key:                     Key{BackendKind: BackendGVisor, GuestArchitecture: "amd64", RuntimeManifestDigest: digest, ToolchainManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		SourceOCIManifestDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		FlatRootDigest:          "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		LaunchArtifacts:         []LaunchArtifact{{ID: "runsc", SHA256: digest}},
		AgentProtocolGeneration: 1,
		AgentFeatures:           []string{"exec", "files"},
		BackendBuildID:          "gvisor-runner-v1",
		HelperBuildID:           "runsc-release-20260817.0",
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	withoutFlatRoot := manifest
	withoutFlatRoot.FlatRootDigest = ""
	if err := withoutFlatRoot.Validate(); err == nil {
		t.Fatal("gVisor materialization without a flat root digest was accepted")
	}
	withoutHelper := manifest
	withoutHelper.HelperBuildID = ""
	if err := withoutHelper.Validate(); err == nil {
		t.Fatal("gVisor materialization without a runsc identity was accepted")
	}
}

func TestManifestRejectsFirecrackerWithOCIBackendIdentity(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := Manifest{
		SchemaVersion:           SchemaVersion,
		Key:                     Key{BackendKind: BackendFirecracker, GuestArchitecture: "amd64", RuntimeManifestDigest: digest, ToolchainManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		FlatRootDigest:          "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		LaunchArtifacts:         []LaunchArtifact{{ID: "kernel", SHA256: digest}},
		AgentProtocolGeneration: 1,
		AgentFeatures:           []string{"exec"},
		BackendBuildID:          "firecracker-v1",
	}
	if err := manifest.Validate(); err == nil {
		t.Fatal("Firecracker materialization carrying OCI-backend identity was accepted")
	}
}

func TestExperimentalGVisorMaterializationFixture(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "gvisor-linux-amd64.materialization.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture Manifest
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	digest, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Key.BackendKind != BackendGVisor ||
		loaded.Key.GuestArchitecture != "amd64" ||
		loaded.HelperBuildID != "runsc-release-20260817.0" {
		t.Fatalf("experimental gVisor materialization identity = %#v", loaded)
	}
}
