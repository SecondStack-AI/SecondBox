package materialization

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExperimentalMacOSMicrosandboxMaterializationFixture(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "microsandbox-macos-arm64.materialization.json")
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
	if loaded.Key.BackendKind != BackendMicrosandbox ||
		loaded.Key.GuestArchitecture != "arm64" ||
		loaded.Key.RuntimeManifestDigest != "sha256:1111111111111111111111111111111111111111111111111111111111111111" ||
		loaded.Key.ToolchainManifestDigest != "sha256:2222222222222222222222222222222222222222222222222222222222222222" {
		t.Fatalf("experimental macOS materialization identity = %#v", loaded.Key)
	}
}
