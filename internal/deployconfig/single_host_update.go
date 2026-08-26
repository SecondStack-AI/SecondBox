package deployconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/assetcatalog"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

type StagedSingleHostUpdate struct {
	ManifestPath            string
	SignedAssetCatalog      string
	ReleaseArtifactManifest string
}

func ValidateSingleHostUpdateSource(plan install.InstallPlan, release releasecontract.ArtifactManifest, releaseBytes []byte, artifact install.VerifiedArtifact) error {
	comparison, err := releasecontract.CompareVersions(release.Version, cleanInstallBoundaryVersion)
	if err != nil {
		return err
	}
	if release.Version != "0.0.0-development" && comparison < 0 {
		return cleanInstallBoundaryError("installed deployment predates v" + cleanInstallBoundaryVersion)
	}
	if err := release.Validate(); err != nil {
		return err
	}
	if releasecontract.Digest(releaseBytes) != plan.Release.ArtifactManifestDigest || release.Version != plan.Release.Version ||
		release.ControlPlane.Reference != plan.Release.Images["control-plane"] || release.Runner.Reference != plan.Release.Images["runner"] ||
		release.MicroVM.ImageReference != plan.Release.Images["microvm-artifacts"] || release.InstallerTools.Reference != plan.Release.Images["installer-tools"] ||
		release.BundledServices.Postgres != plan.Release.Images["postgres"] ||
		release.MicroVM.SigningKeyFingerprint != plan.Release.SigningKeyFingerprint || artifact.ManifestDigest != release.MicroVM.SignedManifestDigest ||
		artifact.SigningKeyID != strings.ToLower(strings.TrimPrefix(release.MicroVM.SigningKeyFingerprint, "SHA256:")) {
		return manifestError("existing single-host release identity differs from the accepted install plan", nil)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinary(release, name)
		if !found || binary.SHA256 != plan.Release.BinaryDigests[name] {
			return manifestError("existing single-host binary identity differs from the accepted install plan: "+name, nil)
		}
	}
	actualReleaseBytes, err := readSingleHostPlannedFile(plan, "release-artifact-manifest")
	if err != nil || !bytes.Equal(actualReleaseBytes, releaseBytes) {
		return manifestError("existing single-host release manifest differs from the verified release", err)
	}
	catalog := struct {
		Assets []assetcatalog.Asset `json:"assets"`
	}{Assets: []assetcatalog.Asset{
		componentAsset(release.MicroVM.RuntimeBundle, release.GuestProtocol.Maximum),
		componentAsset(release.MicroVM.ToolchainBundle, release.GuestProtocol.Maximum),
	}}
	expectedCatalog, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	// Supported source releases from the clean-install boundary until the
	// provider-neutral catalog recorded a signatureKeyId on every asset, so
	// the source deployment is reconstructed in its own recorded schema too
	// and either exact form proves the same release identity.
	signedCatalog := struct {
		Assets []signedSourceAsset `json:"assets"`
	}{Assets: []signedSourceAsset{
		signedComponentAsset(release.MicroVM.RuntimeBundle, artifact.SigningKeyID, release.GuestProtocol.Maximum),
		signedComponentAsset(release.MicroVM.ToolchainBundle, artifact.SigningKeyID, release.GuestProtocol.Maximum),
	}}
	expectedSignedCatalog, err := json.Marshal(signedCatalog)
	if err != nil {
		return err
	}
	actualCatalog, err := readSingleHostPlannedFile(plan, "signed-asset-catalog")
	if err != nil || (!bytes.Equal(actualCatalog, append(expectedCatalog, '\n')) &&
		!bytes.Equal(actualCatalog, append(expectedSignedCatalog, '\n'))) {
		return manifestError("existing single-host signed-asset catalog differs from the verified release", err)
	}
	// The accepted plan/receipt ledger validates the active manifest's exact
	// recorded bytes. Do not regenerate a source-era manifest with target-era
	// code-owned policy, tuning, Compose assets, or Profile lineage here.
	return nil
}

// signedSourceAsset reproduces the exact catalog schema written by source
// releases that still recorded per-asset signing-key identity.
type signedSourceAsset struct {
	ArtifactID              string   `json:"artifactId"`
	ManifestDigest          string   `json:"manifestDigest"`
	SignatureKeyID          string   `json:"signatureKeyId"`
	Architecture            string   `json:"architecture"`
	GuestProtocolGeneration uint32   `json:"guestProtocolGeneration"`
	MandatoryGuestFeatures  []string `json:"mandatoryGuestFeatures"`
}

func signedComponentAsset(component releasecontract.SignedComponent, keyID string, protocol uint32) signedSourceAsset {
	asset := componentAsset(component, protocol)
	return signedSourceAsset{
		ArtifactID: asset.ArtifactID, ManifestDigest: asset.ManifestDigest,
		SignatureKeyID: keyID, Architecture: asset.Architecture,
		GuestProtocolGeneration: asset.GuestProtocolGeneration,
		MandatoryGuestFeatures:  asset.MandatoryGuestFeatures,
	}
}

func StageSingleHostUpdate(plan install.InstallPlan, update install.UpdateRecord, verified releaseverify.VerifiedRelease, artifact install.VerifiedArtifact) (StagedSingleHostUpdate, error) {
	staging, err := install.UpdateStaging(plan, update)
	if err != nil {
		return StagedSingleHostUpdate{}, err
	}
	manifestBytes, catalogBytes, err := singleHostUpdateContents(plan, update, verified, artifact)
	if err != nil {
		return StagedSingleHostUpdate{}, err
	}
	result := StagedSingleHostUpdate{ManifestPath: filepath.Join(staging.Root, "secondbox.toml"), SignedAssetCatalog: filepath.Join(staging.Root, "signed-assets.json"), ReleaseArtifactManifest: staging.ReleaseArtifactManifest}
	if err := writeOrValidateStagedUpdate(result.ManifestPath, manifestBytes, 0o600); err != nil {
		return StagedSingleHostUpdate{}, err
	}
	if err := writeOrValidateStagedUpdate(result.SignedAssetCatalog, catalogBytes, 0o600); err != nil {
		return StagedSingleHostUpdate{}, err
	}
	return result, nil
}

func singleHostUpdateContents(plan install.InstallPlan, update install.UpdateRecord, verified releaseverify.VerifiedRelease, artifact install.VerifiedArtifact) ([]byte, []byte, error) {
	if verified.Manifest.Version != update.TargetRelease.Version || artifact.ManifestDigest != verified.Manifest.MicroVM.SignedManifestDigest || artifact.SigningKeyID != strings.ToLower(strings.TrimPrefix(verified.Manifest.MicroVM.SigningKeyFingerprint, "SHA256:")) {
		return nil, nil, manifestError("staged single-host update release identity differs", nil)
	}
	deployment := installPath(plan, "deployment")
	runnerID := "runner-" + strings.TrimPrefix(plan.OperationID, "install_")
	targets := make(map[string]string, len(plan.SecretTargets))
	for _, target := range plan.SecretTargets {
		targets[target.Category] = target.Path
	}
	relativeTarget := func(category string) string { return relativeTo(deployment, targets[category]) }
	catalogPath := installPath(plan, "signed-asset-catalog")
	releasePath := installPath(plan, "release-artifact-manifest")
	pkiPath := installPath(plan, "runner-pki")
	manifest, err := singleHostManifest(plan, verified.Manifest, artifact.SigningKeyID, runnerID, installPath(plan, "runner-identity"), relativeTarget("database-password"), relativeTarget("platform-authority"), relativeTarget("runner-enrollment"), relativeTo(deployment, catalogPath), relativeTo(deployment, releasePath), relativeTo(deployment, pkiPath))
	if err != nil {
		return nil, nil, err
	}
	manifestBytes, err := encodeManifest(manifest)
	if err != nil {
		return nil, nil, err
	}
	catalog := struct {
		Assets []assetcatalog.Asset `json:"assets"`
	}{Assets: []assetcatalog.Asset{
		componentAsset(verified.Manifest.MicroVM.RuntimeBundle, verified.Manifest.GuestProtocol.Maximum),
		componentAsset(verified.Manifest.MicroVM.ToolchainBundle, verified.Manifest.GuestProtocol.Maximum),
	}}
	catalogBytes, err := json.Marshal(catalog)
	if err != nil {
		return nil, nil, err
	}
	return manifestBytes, append(catalogBytes, '\n'), nil
}

func PublishSingleHostUpdate(plan install.InstallPlan, staged StagedSingleHostUpdate) error {
	targets := []struct {
		staged string
		active string
		mode   os.FileMode
	}{
		{staged.SignedAssetCatalog, installPath(plan, "signed-asset-catalog"), 0o600},
		{staged.ReleaseArtifactManifest, installPath(plan, "release-artifact-manifest"), 0o644},
		{staged.ManifestPath, installPath(plan, "manifest"), 0o600},
	}
	for _, target := range targets {
		content, err := os.ReadFile(target.staged)
		if err != nil {
			return manifestError("read staged single-host update", err)
		}
		if err := writeAtomic(target.active, content, target.mode, true); err != nil {
			return manifestError("publish single-host update", err)
		}
	}
	_, err := ResolveForAcceptedInstaller(installPath(plan, "manifest"))
	return err
}

// ValidatePublishedSingleHostUpdate proves that every release-derived active
// file still matches the verified target before the installer journals its
// final content identities. It returns those expected identities without
// deriving trust from the active files themselves.
func ValidatePublishedSingleHostUpdate(plan install.InstallPlan, update install.UpdateRecord, verified releaseverify.VerifiedRelease, artifact install.VerifiedArtifact) (map[string]string, error) {
	manifestBytes, catalogBytes, err := singleHostUpdateContents(plan, update, verified, artifact)
	if err != nil {
		return nil, err
	}
	expected := map[string][]byte{
		"manifest":                  manifestBytes,
		"signed-asset-catalog":      catalogBytes,
		"release-artifact-manifest": verified.ManifestBytes,
	}
	for name, content := range expected {
		if err := validatePublishedUpdateFile(plan, name, content); err != nil {
			return nil, err
		}
	}
	resolved, err := ResolveForAcceptedInstaller(installPath(plan, "manifest"))
	if err != nil {
		return nil, err
	}
	environmentBytes, err := EncodeComposeEnvironment(resolved.Environment)
	if err != nil {
		return nil, err
	}
	if err := validatePublishedUpdateFile(plan, "compose-environment", environmentBytes); err != nil {
		return nil, err
	}
	expected["compose-environment"] = environmentBytes
	digests := make(map[string]string, len(expected))
	for name, content := range expected {
		digests[name] = install.Digest(content)
	}
	return digests, nil
}

func validatePublishedUpdateFile(plan install.InstallPlan, name string, expected []byte) error {
	var target *install.PlannedPath
	for index := range plan.Paths {
		if plan.Paths[index].Name == name {
			target = &plan.Paths[index]
			break
		}
	}
	if target == nil || (target.Kind != install.ResourceFile && target.Kind != install.ResourceBinary) {
		return manifestError("published update file is absent from the accepted plan: "+name, nil)
	}
	if err := install.ValidatePlannedPath(*target); err != nil {
		return manifestError("published update file metadata differs: "+name, err)
	}
	actual, err := os.ReadFile(target.Path)
	if err != nil || !bytes.Equal(actual, expected) {
		return manifestError("published update file differs from the verified target: "+name, err)
	}
	return nil
}

func writeOrValidateStagedUpdate(path string, content []byte, mode os.FileMode) error {
	existing, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || !bytes.Equal(existing, content) {
			return manifestError("staged update file differs: "+path, statErr)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return manifestError("inspect staged update file: "+path, err)
	}
	if err := writeAtomic(path, slices.Clone(content), mode, false); err != nil {
		return err
	}
	return nil
}
