package deployconfig

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWorkspaceArchiveManifestSelectsReadOnlyConfiguration(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(manifestPath)
	archivePath := filepath.Join(base, "workspace-archives.json")
	if err := os.WriteFile(archivePath, []byte(`{"bucket":"test","region":"us-east-1","transferLifetimeSeconds":600}`), 0600); err != nil {
		t.Fatal(err)
	}
	manifest.Deployment.WorkspaceArchiveConfigFile = "workspace-archives.json"
	resolved, err := resolveManifestWithOptions(manifest, base, true)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Environment["SECONDBOX_WORKSPACE_ARCHIVE_CONFIG_HOST_PATH"] != archivePath || resolved.Environment["SECONDBOX_WORKSPACE_ARCHIVE_CONFIG_FILE"] != "/run/secondbox-archive/config.json" || !slices.Contains(resolved.ComposeFiles, "deploy/compose.workspace-archives.yml") {
		t.Fatalf("archive deployment env=%#v compose=%v", resolved.Environment, resolved.ComposeFiles)
	}
	manifest.Deployment.WorkspaceArchiveConfigFile = "missing-archives.json"
	if _, err := resolveManifestWithOptions(manifest, base, true); err == nil {
		t.Fatal("missing archive configuration accepted")
	}
}
