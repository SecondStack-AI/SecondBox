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
	// The source deployment is represented by the exact bytes a v0.6.0
	// release recorded (frozen in testdata, independent of current
	// generators); only the on-disk file placement is test-local.
	releaseBytes, err := os.ReadFile(filepath.Join("testdata", "v060-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var release releasecontract.ArtifactManifest
	if err := json.Unmarshal(releaseBytes, &release); err != nil {
		t.Fatal(err)
	}
	keyID := strings.ToLower(strings.TrimPrefix(release.MicroVM.SigningKeyFingerprint, "SHA256:"))
	artifact := install.VerifiedArtifact{SigningKeyID: keyID, ManifestDigest: release.MicroVM.SignedManifestDigest}
	catalog := struct {
		Assets []assetcatalog.Asset `json:"assets"`
	}{Assets: []assetcatalog.Asset{
		componentAsset(release.MicroVM.RuntimeBundle, release.GuestProtocol.Maximum),
		componentAsset(release.MicroVM.ToolchainBundle, release.GuestProtocol.Maximum),
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
	// A supported source release recorded the same assets with a per-asset
	// signatureKeyId; the exact bytes such a release wrote (frozen in
	// testdata, independent of current reconstruction helpers) must validate
	// as the same identity.
	signedCatalogBytes, err := os.ReadFile(filepath.Join("testdata", "v060-signed-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, signedCatalogBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSingleHostUpdateSource(plan, release, releaseBytes, artifact); err != nil {
		t.Fatalf("signed source catalog schema was rejected: %v", err)
	}
	if err := os.WriteFile(catalogPath, []byte("{\"assets\":[]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSingleHostUpdateSource(plan, release, releaseBytes, artifact); err == nil {
		t.Fatal("source catalog drift was accepted")
	}
	if err := os.WriteFile(catalogPath, append(catalogBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasePath, []byte("drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSingleHostUpdateSource(plan, release, releaseBytes, artifact); err == nil {
		t.Fatal("source release drift was accepted")
	}
}

func TestUpdateSourceValidationRefusesPreCleanInstallBoundary(t *testing.T) {
	err := ValidateSingleHostUpdateSource(
		install.InstallPlan{},
		releasecontract.ArtifactManifest{Identity: releasecontract.Identity{Version: "0.5.2"}},
		nil,
		install.VerifiedArtifact{},
	)
	if err == nil || !strings.Contains(err.Error(), "v0.6.0 clean-install boundary") ||
		!strings.Contains(err.Error(), "clean reinstall") ||
		!strings.Contains(err.Error(), "compatibility modes are not available") {
		t.Fatalf("pre-boundary update error = %v", err)
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
