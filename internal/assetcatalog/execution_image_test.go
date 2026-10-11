package assetcatalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImportExecutionImageManifest(t *testing.T) {
	manifestWith := func(t *testing.T, architecture string, runtimeDigest string) []byte {
		t.Helper()
		manifest, err := json.Marshal(map[string]any{
			"architecture": architecture, "guestProtocol": map[string]int{"minimum": 1, "maximum": 1},
			"runtimeBundle":   imageComponent{ArtifactID: "runtime", ManifestDigest: runtimeDigest},
			"toolchainBundle": imageComponent{ArtifactID: "toolchain", ManifestDigest: "sha256:" + strings.Repeat("b", 64)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	assets, err := ImportExecutionImageManifest(manifestWith(t, "amd64", "sha256:"+strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 || assets[0].ArtifactId != "runtime" || assets[1].ArtifactId != "toolchain" || assets[0].Architecture != "amd64" {
		t.Fatalf("unexpected imported assets: %v", assets)
	}
	for name, manifest := range map[string][]byte{
		"empty manifest":         nil,
		"malformed manifest":     []byte("{"),
		"unknown architecture":   manifestWith(t, "riscv64", "sha256:"+strings.Repeat("a", 64)),
		"noncanonical component": manifestWith(t, "amd64", "sha256:no"),
	} {
		if _, err := ImportExecutionImageManifest(manifest); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
