package deployconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/assetcatalog"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func TestUpdateSourceValidationUsesRecordedSourceFiles(t *testing.T) {
	release, err := developmentReleaseManifest()
	if err != nil {
		t.Fatal(err)
	}
	releaseBytes, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	keyID := strings.ToLower(strings.TrimPrefix(release.MicroVM.SigningKeyFingerprint, "SHA256:"))
	artifact := install.VerifiedArtifact{SigningKeyID: keyID, ManifestDigest: release.MicroVM.SignedManifestDigest}
	catalog := struct {
		Assets []assetcatalog.SignedAsset `json:"assets"`
	}{Assets: []assetcatalog.SignedAsset{
		componentAsset(release.MicroVM.RuntimeBundle, keyID, release.GuestProtocol.Maximum),
		componentAsset(release.MicroVM.ToolchainBundle, keyID, release.GuestProtocol.Maximum),
	}}
	catalogBytes, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	releasePath := filepath.Join(directory, "release.json")
	catalogPath := filepath.Join(directory, "catalog.json")
	if err := os.WriteFile(releasePath, releaseBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, append(catalogBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	images := map[string]string{
		"control-plane": release.ControlPlane.Reference, "runner": release.Runner.Reference,
		"microvm-artifacts": release.MicroVM.ImageReference, "installer-tools": release.InstallerTools.Reference,
		"postgres": release.BundledServices.Postgres,
	}
	binaries := map[string]string{}
	for _, binary := range release.Binaries {
		if binary.Platform == "linux/amd64" && (binary.Name == "secondbox" || binary.Name == "secondbox-deploy") {
			binaries[binary.Name] = binary.SHA256
		}
	}
	plan := install.InstallPlan{
		Release: install.ReleasePlan{Version: release.Version, ArtifactManifestURL: releasecontract.ArtifactManifestLocation(release.Version), ArtifactManifestDigest: releasecontract.Digest(releaseBytes), SigningKeyFingerprint: release.MicroVM.SigningKeyFingerprint, Images: images, BinaryDigests: binaries},
		Paths: []install.PlannedPath{
			{Name: "release-artifact-manifest", Path: releasePath, Kind: install.ResourceFile, Mode: 0o600, OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid())},
			{Name: "signed-asset-catalog", Path: catalogPath, Kind: install.ResourceFile, Mode: 0o600, OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid())},
		},
	}
	if err := ValidateSingleHostUpdateSource(plan, release, releaseBytes, artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasePath, []byte("drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSingleHostUpdateSource(plan, release, releaseBytes, artifact); err == nil {
		t.Fatal("source release drift was accepted")
	}
}

func TestPublishedUpdateFileMustMatchVerifiedTargetBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secondbox.toml")
	expected := []byte("verified target\n")
	if err := os.WriteFile(path, expected, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := install.InstallPlan{Paths: []install.PlannedPath{{
		Name: "manifest", Path: path, Class: install.PathUserDeployment,
		Kind: install.ResourceFile, Mode: 0o600,
		OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid()),
	}}}
	if err := validatePublishedUpdateFile(plan, "manifest", expected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unverified drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedUpdateFile(plan, "manifest", expected); err == nil || !strings.Contains(err.Error(), "verified target") {
		t.Fatalf("published drift was adopted: %v", err)
	}
}
