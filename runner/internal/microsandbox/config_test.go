package microsandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/materialization"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func TestValidateConfigRejectsFlatRootThatDiffersFromMaterialization(t *testing.T) {
	temporary := t.TempDir()
	root := filepath.Join(temporary, "rootfs")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(root, "init")
	if err := os.WriteFile(rootFile, []byte("immutable"), 0o755); err != nil {
		t.Fatal(err)
	}
	asset := func(name string) string {
		t.Helper()
		path := filepath.Join(temporary, name)
		if err := os.WriteFile(path, []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	helper := asset("helper")
	agentd := asset("agentd")
	firmware := asset("libkrunfw")
	flatRootDigest, err := materialization.DigestFlatRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := materialization.Manifest{
		SchemaVersion: materialization.SchemaVersion,
		Key: materialization.Key{
			BackendKind:             materialization.BackendMicrosandbox,
			GuestArchitecture:       runtime.GOARCH,
			RuntimeManifestDigest:   configTestDigest("runtime"),
			ToolchainManifestDigest: configTestDigest("toolchain"),
		},
		SourceOCIManifestDigest: configTestDigest("source"),
		FlatRootDigest:          flatRootDigest,
		LaunchArtifacts: []materialization.LaunchArtifact{
			{ID: "agentd", SHA256: configTestFileDigest(t, agentd)},
			{ID: "helper", SHA256: configTestFileDigest(t, helper)},
			{ID: "libkrunfw", SHA256: configTestFileDigest(t, firmware)},
		},
		AgentProtocolGeneration: 1,
		AgentFeatures:           []string{"exec"},
		BackendBuildID:          "test-backend",
		HelperBuildID:           "test-helper",
	}
	manifestDigest, err := manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(temporary, "materialization.json")
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{
		HelperExecutable: helper, LibkrunfwPath: firmware, AgentdPath: agentd,
		FlatRootPath: root, MaterializationPath: manifestPath, MaterializationDigest: manifestDigest,
		MaximumVCPUs: 1, MaximumMemoryBytes: 1, MaximumDiskBytes: 1,
		MaximumInstances: 1, MaximumOperations: 1, WorkspaceStore: &workspacestore.Store{},
	}
	if _, err := validateConfig(config); err != nil {
		t.Fatalf("valid materialization: %v", err)
	}
	if err := os.WriteFile(rootFile, []byte("mutated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "flat root differs from materialization") {
		t.Fatalf("mutated flat root error = %v", err)
	}
}

func configTestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func configTestFileDigest(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
