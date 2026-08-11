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
	_, err := validateExistingSingleHostInstall(plan, release, releaseBytes, artifact)
	return err
}

func StageSingleHostUpdate(plan install.InstallPlan, update install.UpdateRecord, verified releaseverify.VerifiedRelease, artifact install.VerifiedArtifact) (StagedSingleHostUpdate, error) {
	staging, err := install.UpdateStaging(plan, update)
	if err != nil {
		return StagedSingleHostUpdate{}, err
	}
	if verified.Manifest.Version != update.TargetRelease.Version || artifact.ManifestDigest != verified.Manifest.MicroVM.SignedManifestDigest || artifact.SigningKeyID != strings.ToLower(strings.TrimPrefix(verified.Manifest.MicroVM.SigningKeyFingerprint, "SHA256:")) {
		return StagedSingleHostUpdate{}, manifestError("staged single-host update release identity differs", nil)
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
	manifest, err := singleHostManifest(plan, verified.Manifest, artifact.SigningKeyID, runnerID, installPath(plan, "runner-identity"), relativeTarget("database-password"), relativeTarget("platform-authority"), relativeTarget("runner-enrollment"), relativeTarget("application-authority"), relativeTo(deployment, catalogPath), relativeTo(deployment, releasePath), relativeTo(deployment, pkiPath))
	if err != nil {
		return StagedSingleHostUpdate{}, err
	}
	manifestBytes, err := encodeManifest(manifest)
	if err != nil {
		return StagedSingleHostUpdate{}, err
	}
	catalog := struct {
		Assets []assetcatalog.SignedAsset `json:"assets"`
	}{Assets: []assetcatalog.SignedAsset{
		componentAsset(verified.Manifest.MicroVM.RuntimeBundle, artifact.SigningKeyID, verified.Manifest.GuestProtocol.Maximum),
		componentAsset(verified.Manifest.MicroVM.ToolchainBundle, artifact.SigningKeyID, verified.Manifest.GuestProtocol.Maximum),
	}}
	catalogBytes, err := json.Marshal(catalog)
	if err != nil {
		return StagedSingleHostUpdate{}, err
	}
	result := StagedSingleHostUpdate{ManifestPath: filepath.Join(staging.Root, "secondbox.toml"), SignedAssetCatalog: filepath.Join(staging.Root, "signed-assets.json"), ReleaseArtifactManifest: staging.ReleaseArtifactManifest}
	if err := writeOrValidateStagedUpdate(result.ManifestPath, manifestBytes, 0o600); err != nil {
		return StagedSingleHostUpdate{}, err
	}
	if err := writeOrValidateStagedUpdate(result.SignedAssetCatalog, append(catalogBytes, '\n'), 0o600); err != nil {
		return StagedSingleHostUpdate{}, err
	}
	return result, nil
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
