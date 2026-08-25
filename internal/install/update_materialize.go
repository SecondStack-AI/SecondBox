package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

type StagedUpdate struct {
	Root                    string
	Artifacts               string
	ArtifactPartial         string
	PreviousArtifacts       string
	SecondBoxBinary         string
	SecondBoxDeployBinary   string
	ReleaseArtifactManifest string
}

// ValidateUpdateStagingCapacity preserves the operational storage margin while
// a second verified execution bundle exists beside the active bundle. It runs
// before an update is journaled so lack of local capacity remains a read-only
// admission failure.
func ValidateUpdateStagingCapacity(plan InstallPlan, target ReleasePlan) error {
	if err := validateReleasePlan(target); err != nil {
		return err
	}
	artifacts, found := plannedPathByName(plan.Paths, "artifacts")
	if !found || artifacts.Kind != ResourceDirectory {
		return installerError("update staging capacity requires the active artifact path", nil)
	}
	if err := ValidatePlannedPath(artifacts); err != nil {
		return err
	}
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(artifacts.Path), &filesystem); err != nil {
		return installerError("inspect update artifact filesystem capacity", err)
	}
	available := int64(filesystem.Bavail) * int64(filesystem.Bsize)
	required := target.ExpectedDownloadBytes + RunnerStorageReserveBytes
	if required < target.ExpectedDownloadBytes || available < required {
		return installerError("update artifact filesystem capacity is insufficient for target staging and the Runner storage reserve", nil)
	}
	return nil
}

func UpdateStaging(plan InstallPlan, update UpdateRecord) (StagedUpdate, error) {
	if !updatePattern.MatchString(update.ID) {
		return StagedUpdate{}, installerError("update staging identity is invalid", nil)
	}
	deployment, found := plannedPathByName(plan.Paths, "deployment")
	if !found {
		return StagedUpdate{}, installerError("update staging requires deployment path", nil)
	}
	artifacts, found := plannedPathByName(plan.Paths, "artifacts")
	if !found {
		return StagedUpdate{}, installerError("update staging requires artifact path", nil)
	}
	root := filepath.Join(deployment.Path, ".secondbox-updates", update.ID)
	return StagedUpdate{
		Root:                    root,
		Artifacts:               filepath.Join(filepath.Dir(artifacts.Path), "artifacts-"+update.ID),
		ArtifactPartial:         filepath.Join(filepath.Dir(artifacts.Path), ".artifacts-"+update.ID+".partial"),
		PreviousArtifacts:       filepath.Join(filepath.Dir(artifacts.Path), "artifacts-before-"+update.ID),
		SecondBoxBinary:         filepath.Join(root, "secondbox"),
		SecondBoxDeployBinary:   filepath.Join(root, "secondbox-deploy"),
		ReleaseArtifactManifest: filepath.Join(root, "release-artifact-manifest.json"),
	}, nil
}

func StageUpdateRelease(ctx context.Context, plan InstallPlan, update UpdateRecord, verified releaseverify.VerifiedRelease, executor ReleaseMaterializeExecutor) (StagedUpdate, VerifiedArtifact, error) {
	if executor == nil {
		return StagedUpdate{}, VerifiedArtifact{}, installerError("update release materializer is absent", nil)
	}
	if err := validateVerifiedRelease(update.TargetRelease, verified); err != nil {
		return StagedUpdate{}, VerifiedArtifact{}, err
	}
	staged, err := UpdateStaging(plan, update)
	if err != nil {
		return StagedUpdate{}, VerifiedArtifact{}, err
	}
	updatesRoot := filepath.Dir(staged.Root)
	if err := ensureUpdateDirectory(updatesRoot); err != nil {
		return StagedUpdate{}, VerifiedArtifact{}, err
	}
	if err := ensureUpdateDirectory(staged.Root); err != nil {
		return StagedUpdate{}, VerifiedArtifact{}, err
	}
	for _, name := range []string{"control-plane", "runner", "installer-tools", "microvm-artifacts", "postgres"} {
		if err := executor.PullImage(ctx, update.TargetRelease.Images[name]); err != nil {
			return StagedUpdate{}, VerifiedArtifact{}, installerError("pull immutable update "+name+" image", err)
		}
	}
	artifact, err := stageUpdateArtifacts(ctx, staged.Artifacts, staged.ArtifactPartial, verified, executor)
	if err != nil {
		return StagedUpdate{}, VerifiedArtifact{}, err
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinaryForMaterialization(verified.Manifest, name)
		if !found {
			return StagedUpdate{}, VerifiedArtifact{}, installerError("verified update release omits linux/amd64 binary "+name, nil)
		}
		content, err := executor.Fetch(ctx, binary.Location)
		if err != nil {
			return StagedUpdate{}, VerifiedArtifact{}, err
		}
		if Digest(content) != "sha256:"+binary.SHA256 {
			return StagedUpdate{}, VerifiedArtifact{}, installerError("downloaded update binary digest mismatch for "+name, nil)
		}
		path := staged.SecondBoxBinary
		if name == "secondbox-deploy" {
			path = staged.SecondBoxDeployBinary
		}
		if err := writeOrValidateUpdateFile(path, content, 0o755, "sha256:"+binary.SHA256); err != nil {
			return StagedUpdate{}, VerifiedArtifact{}, err
		}
	}
	if err := writeOrValidateUpdateFile(staged.ReleaseArtifactManifest, verified.ManifestBytes, 0o644, releasecontract.Digest(verified.ManifestBytes)); err != nil {
		return StagedUpdate{}, VerifiedArtifact{}, err
	}
	return staged, artifact, nil
}

func validateVerifiedRelease(target ReleasePlan, verified releaseverify.VerifiedRelease) error {
	manifest := verified.Manifest
	if releasecontract.Digest(verified.ManifestBytes) != target.ArtifactManifestDigest || manifest.Version != target.Version || manifest.ControlPlane.Reference != target.Images["control-plane"] || manifest.Runner.Reference != target.Images["runner"] || manifest.InstallerTools.Reference != target.Images["installer-tools"] || manifest.MicroVM.ImageReference != target.Images["microvm-artifacts"] || manifest.BundledServices.Postgres != target.Images["postgres"] || manifest.MicroVM.SigningKeyFingerprint != target.SigningKeyFingerprint {
		return installerError("verified update release differs from journaled target identity", nil)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinaryForMaterialization(manifest, name)
		if !found || binary.SHA256 != target.BinaryDigests[name] {
			return installerError("verified update binary differs from journaled target: "+name, nil)
		}
	}
	return nil
}

func stageUpdateArtifacts(ctx context.Context, target, partial string, verified releaseverify.VerifiedRelease, executor ReleaseMaterializeExecutor) (VerifiedArtifact, error) {
	if err := removeInterruptedUpdateArtifacts(partial); err != nil {
		return VerifiedArtifact{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		return VerifyArtifactDirectory(target, verified.Manifest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return VerifiedArtifact{}, installerError("inspect staged update artifacts", err)
	}
	if err := os.Mkdir(partial, 0o700); err != nil {
		return VerifiedArtifact{}, installerError("create update artifact staging directory", err)
	}
	if err := os.Chmod(partial, 0o700); err != nil {
		return VerifiedArtifact{}, errors.Join(installerError("protect update artifact staging directory", err), os.RemoveAll(partial))
	}
	if err := executor.ExtractMicroVMImage(ctx, verified.Manifest.MicroVM.ImageReference, partial); err != nil {
		return VerifiedArtifact{}, errors.Join(installerError("extract immutable update microVM artifact image", err), os.RemoveAll(partial))
	}
	artifact, err := VerifyArtifactDirectory(partial, verified.Manifest)
	if err != nil {
		return VerifiedArtifact{}, errors.Join(err, os.RemoveAll(partial))
	}
	if err := os.Rename(partial, target); err != nil {
		return VerifiedArtifact{}, errors.Join(installerError("publish staged update artifact directory", err), os.RemoveAll(partial))
	}
	if err := syncInstallDirectory(filepath.Dir(target)); err != nil {
		return VerifiedArtifact{}, err
	}
	return artifact, nil
}

func removeInterruptedUpdateArtifacts(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return installerError("inspect interrupted update artifact staging directory", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return installerError("interrupted update artifact staging directory is unsafe", nil)
	}
	if err := validateUpdatePartialRemoval(path); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return installerError("remove interrupted update artifact staging directory", err)
	}
	return syncInstallDirectory(filepath.Dir(path))
}

func ensureUpdateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return installerError("create update staging directory "+path, err)
		}
		return syncInstallDirectory(filepath.Dir(path))
	}
	if err != nil {
		return installerError("inspect update staging directory "+path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return installerError("update staging directory is unsafe: "+path, err)
	}
	return nil
}

func writeOrValidateUpdateFile(path string, content []byte, mode os.FileMode, expectedDigest string) error {
	if existing, err := os.ReadFile(path); err == nil {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || !slices.Equal(existing, content) || Digest(existing) != expectedDigest {
			return installerError("staged update file differs: "+path, statErr)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return installerError("inspect staged update file "+path, err)
	}
	if err := writeExecutableCreateOnly(path, content, mode); err != nil {
		return err
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, content) || Digest(actual) != expectedDigest {
		return installerError("verify staged update file "+path, err)
	}
	return nil
}

func ActivateUpdateArtifactsAndBinaries(plan InstallPlan, update UpdateRecord, source releasecontract.ArtifactManifest, target releaseverify.VerifiedRelease) (VerifiedArtifact, error) {
	if err := validateVerifiedRelease(update.TargetRelease, target); err != nil {
		return VerifiedArtifact{}, err
	}
	staged, err := UpdateStaging(plan, update)
	if err != nil {
		return VerifiedArtifact{}, err
	}
	activePath, found := plannedPathByName(plan.Paths, "artifacts")
	if !found {
		return VerifiedArtifact{}, installerError("active artifact path is absent", nil)
	}
	artifact, err := activateUpdateArtifacts(activePath.Path, staged, source, target.Manifest)
	if err != nil {
		return VerifiedArtifact{}, err
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		path := staged.SecondBoxBinary
		if name == "secondbox-deploy" {
			path = staged.SecondBoxDeployBinary
		}
		content, err := os.ReadFile(path)
		if err != nil || Digest(content) != "sha256:"+update.TargetRelease.BinaryDigests[name] {
			return VerifiedArtifact{}, installerError("staged update binary differs before activation: "+name, err)
		}
		targetPath, found := plannedPathByName(plan.Paths, name+"-binary")
		if !found {
			return VerifiedArtifact{}, installerError("active binary path is absent: "+name, nil)
		}
		if err := publishReleaseBinary(targetPath, name, update.TargetRelease.BinaryDigests[name], content, nil); err != nil {
			return VerifiedArtifact{}, err
		}
	}
	return artifact, nil
}

// ValidateActivatedUpdateArtifactsAndBinaries verifies active target bytes
// without relying on staging paths that cleanup may already have removed.
func ValidateActivatedUpdateArtifactsAndBinaries(plan InstallPlan, update UpdateRecord, target releaseverify.VerifiedRelease) (VerifiedArtifact, error) {
	if err := validateVerifiedRelease(update.TargetRelease, target); err != nil {
		return VerifiedArtifact{}, err
	}
	active, found := plannedPathByName(plan.Paths, "artifacts")
	if !found {
		return VerifiedArtifact{}, installerError("active artifact path is absent", nil)
	}
	artifact, err := VerifyArtifactDirectory(active.Path, target.Manifest)
	if err != nil {
		return VerifiedArtifact{}, installerError("active update artifacts differ from the verified target", err)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		path, found := plannedPathByName(plan.Paths, name+"-binary")
		if !found || path.Kind != ResourceBinary {
			return VerifiedArtifact{}, installerError("active update binary path is absent: "+name, nil)
		}
		if err := ValidatePlannedPath(path); err != nil {
			return VerifiedArtifact{}, err
		}
		digest, err := fileSHA256(path.Path)
		if err != nil || digest != update.TargetRelease.BinaryDigests[name] {
			return VerifiedArtifact{}, installerError("active update binary differs from the verified target: "+name, err)
		}
	}
	return artifact, nil
}

func activateUpdateArtifacts(active string, staged StagedUpdate, source, target releasecontract.ArtifactManifest) (VerifiedArtifact, error) {
	if _, err := os.Lstat(staged.PreviousArtifacts); err == nil {
		if _, err := VerifyArtifactDirectory(staged.PreviousArtifacts, source); err != nil {
			return VerifiedArtifact{}, installerError("previous update artifacts differ from source release", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return VerifiedArtifact{}, installerError("inspect previous update artifacts", err)
	} else {
		if _, err := VerifyArtifactDirectory(active, source); err != nil {
			if targetArtifact, targetErr := VerifyArtifactDirectory(active, target); targetErr == nil {
				return targetArtifact, nil
			}
			return VerifiedArtifact{}, installerError("active artifacts differ from source release", err)
		}
		if err := os.Rename(active, staged.PreviousArtifacts); err != nil {
			return VerifiedArtifact{}, installerError("journal previous update artifacts", err)
		}
		if err := syncInstallDirectory(filepath.Dir(active)); err != nil {
			return VerifiedArtifact{}, err
		}
	}
	if targetArtifact, err := VerifyArtifactDirectory(active, target); err == nil {
		return targetArtifact, nil
	} else if _, statErr := os.Lstat(active); !errors.Is(statErr, os.ErrNotExist) {
		return VerifiedArtifact{}, installerError("active update artifact target is occupied", err)
	}
	if _, err := VerifyArtifactDirectory(staged.Artifacts, target); err != nil {
		return VerifiedArtifact{}, installerError("staged update artifacts differ from target release", err)
	}
	if err := os.Rename(staged.Artifacts, active); err != nil {
		return VerifiedArtifact{}, installerError("activate verified update artifacts", err)
	}
	if err := syncInstallDirectory(filepath.Dir(active)); err != nil {
		return VerifiedArtifact{}, err
	}
	return VerifyArtifactDirectory(active, target)
}

// CleanupUpdateStaging removes only the verified, update-owned source backup
// and staging paths after the target deployment has passed its smoke test.
func CleanupUpdateStaging(plan InstallPlan, update UpdateRecord, source, target releasecontract.ArtifactManifest) error {
	return cleanupUpdateStaging(plan, update, source, target, validateUpdatePartialRemoval)
}

func cleanupUpdateStaging(plan InstallPlan, update UpdateRecord, source, target releasecontract.ArtifactManifest, validateRemoval func(string) error) error {
	staged, err := UpdateStaging(plan, update)
	if err != nil {
		return err
	}
	active, found := plannedPathByName(plan.Paths, "artifacts")
	if !found {
		return installerError("active artifact path is absent", nil)
	}
	if _, err := VerifyArtifactDirectory(active.Path, target); err != nil {
		return installerError("refuse update cleanup before target artifact activation", err)
	}
	for _, candidate := range []struct {
		path     string
		manifest releasecontract.ArtifactManifest
		label    string
	}{{staged.PreviousArtifacts, source, "previous"}, {staged.Artifacts, target, "staged"}} {
		if _, err := os.Lstat(candidate.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return installerError("inspect "+candidate.label+" update artifacts before cleanup", err)
		}
		if _, err := VerifyArtifactDirectory(candidate.path, candidate.manifest); err != nil {
			return installerError("refuse cleanup of unverified "+candidate.label+" update artifacts", err)
		}
		if err := validateRemoval(candidate.path); err != nil {
			return installerError("refuse cleanup of "+candidate.label+" update artifacts with nested mounts", err)
		}
		if err := os.RemoveAll(candidate.path); err != nil {
			return installerError("remove "+candidate.label+" update artifacts", err)
		}
		if err := syncInstallDirectory(filepath.Dir(candidate.path)); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(staged.Root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return installerError("inspect update staging root before cleanup", err)
	} else if stat, ok := info.Sys().(*syscall.Stat_t); !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return installerError("refuse cleanup of unsafe update staging root", nil)
	}
	if err := validateRemoval(staged.Root); err != nil {
		return installerError("refuse cleanup of update staging root with nested mounts", err)
	}
	if err := os.RemoveAll(staged.Root); err != nil {
		return installerError("remove update staging root", err)
	}
	updatesRoot := filepath.Dir(staged.Root)
	if err := syncInstallDirectory(updatesRoot); err != nil {
		return err
	}
	if err := os.Remove(updatesRoot); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return installerError("remove empty update staging parent", err)
	}
	return syncInstallDirectory(filepath.Dir(updatesRoot))
}
