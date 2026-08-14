package resourceapply

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExperimentalMacOSMicrosandboxResourceFixture(t *testing.T) {
	path := filepath.Join(
		"..", "..", "runner", "deploy", "microsandbox-macos-arm64.resources.json",
	)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw Document
	if err := json.Unmarshal(content, &raw); err != nil {
		t.Fatal(err)
	}
	actualDigest, err := SpecDigest(raw.Profiles[0].Revisions[0].Spec)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Profiles[0].Revisions[0].SpecDigest != actualDigest {
		t.Fatalf("experimental macOS Profile spec digest = %s", actualDigest)
	}
	document, err := Decode(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.RunnerPools) != 1 || len(document.Profiles) != 1 {
		t.Fatalf("experimental macOS fixture shape = %#v", document)
	}
	pool := document.RunnerPools[0]
	revision := document.Profiles[0].Revisions[0]
	if pool.Name != "experimental-microsandbox-arm64" ||
		len(pool.Architectures) != 1 || pool.Architectures[0] != "arm64" ||
		revision.Spec.Pool != pool.Name || revision.Spec.Architecture != "arm64" ||
		revision.Spec.Startup.Mode != "cold_boot" {
		t.Fatalf("experimental macOS fixture identity = %#v %#v", pool, revision)
	}
}
