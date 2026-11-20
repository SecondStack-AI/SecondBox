package deployconfig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestWorkspaceArchiveAWSConfigDirectory(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(manifestPath)
	if err := os.WriteFile(filepath.Join(base, "workspace-archives.json"), []byte(`{"bucket":"test","region":"us-east-1","transferLifetimeSeconds":600}`), 0o600); err != nil {
		t.Fatal(err)
	}
	awsDirectory := filepath.Join(base, "archive-aws")
	if err := os.Mkdir(awsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest.Deployment.WorkspaceArchiveAWSConfigDirectory = "archive-aws"
	if _, err := resolveManifestWithOptions(manifest, base, true); err == nil || !strings.Contains(err.Error(), "requires workspace_archive_config_file") {
		t.Fatalf("AWS credentials without archive tier: %v", err)
	}
	manifest.Deployment.WorkspaceArchiveConfigFile = "workspace-archives.json"
	resolved, err := resolveManifestWithOptions(manifest, base, true)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Environment["SECONDBOX_WORKSPACE_ARCHIVE_AWS_CONFIG_HOST_PATH"] != awsDirectory || !slices.Contains(resolved.ComposeFiles, "deploy/compose.workspace-archives.aws-config.yml") {
		t.Fatalf("archive AWS configuration missing: env=%#v compose=%v", resolved.Environment, resolved.ComposeFiles)
	}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, recordedFiles, err := recordedInstallerComposeIdentity(manifestPath)
	if err != nil || !slices.Contains(recordedFiles, "deploy/compose.workspace-archives.aws-config.yml") {
		t.Fatalf("recorded installer lost archive AWS configuration: files=%v err=%v", recordedFiles, err)
	}
	inspection, err := Inspect(manifestPath)
	if err != nil || strings.Contains(string(inspection), awsDirectory) {
		t.Fatalf("inspection exposed archive AWS credential path: err=%v", err)
	}
	if err := os.Chmod(awsDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveManifestWithOptions(manifest, base, true); err == nil {
		t.Fatal("group-readable AWS credential directory accepted")
	}
	if err := os.Chmod(awsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(awsDirectory, filepath.Join(base, "archive-aws-link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"archive-aws-link", filepath.Join(base, "archive-aws-link") + "/", filepath.Join(base, "archive-aws-link") + "/."} {
		manifest.Deployment.WorkspaceArchiveAWSConfigDirectory = path
		if _, err := resolveManifestWithOptions(manifest, base, true); err == nil {
			t.Fatalf("symlinked AWS credential directory accepted: %s", path)
		}
	}
}
