package install

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

type ReleaseMaterializeExecutor interface {
	PullImage(context.Context, string) error
	ExtractMicroVMImage(context.Context, string, string) error
	Fetch(context.Context, string) ([]byte, error)
}

type ReleaseMaterializeDependencies struct {
	Executor                  ReleaseMaterializeExecutor
	PersistReceipt            func(InstallReceipt) error
	Now                       func() time.Time
	ValidateBinaryReplacement func(PlannedPath, string) error
}

// MaterializeRelease installs only bytes named by an independently verified
// release. The artifact directory is published with one rename after all
// allowlist, hash, signature, component, platform, and rootfs checks succeed.
func MaterializeRelease(ctx context.Context, plan InstallPlan, receipt InstallReceipt, verified releaseverify.VerifiedRelease, dependencies ReleaseMaterializeDependencies) (InstallReceipt, VerifiedArtifact, error) {
	if dependencies.Executor == nil || dependencies.PersistReceipt == nil || dependencies.Now == nil {
		return receipt, VerifiedArtifact{}, installerError("release materialization dependencies are incomplete", nil)
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return receipt, VerifiedArtifact{}, err
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return receipt, VerifiedArtifact{}, err
	}
	if len(receipt.CompletedStages) == 0 {
		return receipt, VerifiedArtifact{}, installerError("release materialization requires completed host apply", nil)
	}
	lastStage := receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage
	if lastStage != StageHostApply && lastStage != StageReleaseVerified {
		return receipt, VerifiedArtifact{}, installerError("release materialization requires completed host apply or release verification", nil)
	}
	if err := validateVerifiedReleasePlan(plan, verified); err != nil {
		return failMaterialization(receipt, StageReleaseVerified, FailureBlocked, dependencies, err)
	}
	if err := validateReleaseBinaryTargets(plan, verified.Manifest, dependencies.ValidateBinaryReplacement); err != nil {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
	}
	if err := validateCLIConfigurationTarget(plan); err != nil {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
	}
	if lastStage == StageHostApply {
		if err := receipt.CompleteStage(StageReleaseVerified, dependencies.Now(), map[string]string{"artifactManifestDigest": plan.Release.ArtifactManifestDigest, "version": plan.Release.Version}); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
		if err := dependencies.PersistReceipt(receipt); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
	}
	for _, name := range []string{"control-plane", "runner", "installer-tools", "microvm-artifacts", "postgres", "object-store", "object-store-client"} {
		if err := dependencies.Executor.PullImage(ctx, plan.Release.Images[name]); err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureRetryable, dependencies, installerError("pull immutable "+name+" image", err))
		}
	}

	artifactTarget, found := plannedPathByName(plan.Paths, "artifacts")
	if !found || artifactTarget.Kind != ResourceDirectory {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, installerError("artifact target is absent from plan", nil))
	}
	artifactResource, artifactRecorded := receiptResource(receipt, artifactTarget.Name)
	var artifact VerifiedArtifact
	if artifactRecorded {
		if artifactResource.Path != artifactTarget.Path || artifactResource.Digest != verified.Manifest.MicroVM.SignedManifestDigest {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, installerError("recorded artifact resource differs from the accepted release", nil))
		}
		artifact, err = VerifyArtifactDirectory(artifactTarget.Path, verified.Manifest)
		if err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
		}
	} else if _, statErr := os.Lstat(artifactTarget.Path); statErr == nil {
		if !slices.Contains(receipt.PendingResourceIDs, artifactTarget.Name) {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, installerError("refusing to adopt pre-existing artifact directory "+artifactTarget.Path, nil))
		}
		if err := validateMaterializedPath(artifactTarget); err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
		}
		artifact, err = VerifyArtifactDirectory(artifactTarget.Path, verified.Manifest)
		if err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, installerError("unrecorded artifact publication differs from the verified release", err))
		}
		artifactResource := resourceFromPath(artifactTarget, StageAssetsMaterialized)
		artifactResource.Digest = artifact.ManifestDigest
		if err := receipt.CompleteResource(artifactResource, dependencies.Now()); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
		if err := dependencies.PersistReceipt(receipt); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
	} else if !os.IsNotExist(statErr) {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, installerError("inspect artifact target before atomic publication", statErr))
	} else {
		if err := receipt.BeginResource(artifactTarget.Name, dependencies.Now()); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
		if err := dependencies.PersistReceipt(receipt); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
		staging, err := os.MkdirTemp(filepath.Dir(artifactTarget.Path), ".secondbox-artifacts-")
		if err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, err)
		}
		if err := os.Chmod(staging, 0o700); err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, errors.Join(err, os.RemoveAll(staging)))
		}
		if err := dependencies.Executor.ExtractMicroVMImage(ctx, plan.Release.Images["microvm-artifacts"], staging); err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureRetryable, dependencies, errors.Join(installerError("extract immutable microVM artifact image", err), os.RemoveAll(staging)))
		}
		artifact, err = VerifyArtifactDirectory(staging, verified.Manifest)
		if err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureBlocked, dependencies, errors.Join(err, os.RemoveAll(staging)))
		}
		if err := os.Rename(staging, artifactTarget.Path); err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, errors.Join(installerError("atomically publish verified artifact directory", err), os.RemoveAll(staging)))
		}
		if err := syncInstallDirectory(filepath.Dir(artifactTarget.Path)); err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, err)
		}
		artifactResource := resourceFromPath(artifactTarget, StageAssetsMaterialized)
		artifactResource.Digest = artifact.ManifestDigest
		if err := receipt.CompleteResource(artifactResource, dependencies.Now()); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
		if err := dependencies.PersistReceipt(receipt); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
	}

	binaryDirectoryRoot, found := plannedPathByName(plan.Paths, "binary-directory-root")
	if !found {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, installerError("binary directory root is absent from plan", nil))
	}
	if created, err := ensureOwnedDirectory(binaryDirectoryRoot); err != nil {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
	} else if created {
		if err := appendAndPersistResource(&receipt, resourceFromPath(binaryDirectoryRoot, StageAssetsMaterialized), dependencies.PersistReceipt); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
	} else if err := validateOwnedDirectoryBoundary(binaryDirectoryRoot); err != nil {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
	}
	binaryDirectory, found := plannedPathByName(plan.Paths, "binary-directory")
	if !found {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, installerError("binary directory is absent from plan", nil))
	}
	if created, err := ensureOwnedDirectory(binaryDirectory); err != nil {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
	} else if created {
		if err := appendAndPersistResource(&receipt, resourceFromPath(binaryDirectory, StageAssetsMaterialized), dependencies.PersistReceipt); err != nil {
			return receipt, VerifiedArtifact{}, err
		}
	} else if err := validateOwnedDirectoryBoundary(binaryDirectory); err != nil {
		return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinaryForMaterialization(verified.Manifest, name)
		if !found {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, installerError("verified release omits linux/amd64 binary "+name, nil))
		}
		target, found := plannedPathByName(plan.Paths, name+"-binary")
		if !found {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureInternal, dependencies, installerError("binary target is absent from plan: "+name, nil))
		}
		content, err := dependencies.Executor.Fetch(ctx, binary.Location)
		if err != nil {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureRetryable, dependencies, err)
		}
		if Digest(content) != "sha256:"+binary.SHA256 {
			return failMaterialization(receipt, StageAssetsMaterialized, FailureBlocked, dependencies, installerError("downloaded binary digest mismatch for "+name, nil))
		}
		if resource, recorded := receiptResource(receipt, target.Name); recorded {
			actual, digestErr := fileSHA256(target.Path)
			validateErr := validateMaterializedPath(target)
			if digestErr != nil || validateErr != nil || actual != binary.SHA256 || resource.Digest != "sha256:"+binary.SHA256 {
				return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, installerError("recorded binary postcondition differs for "+name, errors.Join(digestErr, validateErr)))
			}
		} else {
			pending := slices.Contains(receipt.PendingResourceIDs, target.Name)
			if !pending {
				if err := receipt.BeginResource(target.Name, dependencies.Now()); err != nil {
					return receipt, VerifiedArtifact{}, err
				}
				if err := dependencies.PersistReceipt(receipt); err != nil {
					return receipt, VerifiedArtifact{}, err
				}
			}
			if err := publishReleaseBinary(target, name, binary.SHA256, content, dependencies.ValidateBinaryReplacement); err != nil {
				return failMaterialization(receipt, StageAssetsMaterialized, FailureNeedsAction, dependencies, err)
			}
			resource := resourceFromPath(target, StageAssetsMaterialized)
			resource.Digest = "sha256:" + binary.SHA256
			if err := receipt.CompleteResource(resource, dependencies.Now()); err != nil {
				return receipt, VerifiedArtifact{}, err
			}
			if err := dependencies.PersistReceipt(receipt); err != nil {
				return receipt, VerifiedArtifact{}, err
			}
		}
	}
	if err := receipt.CompleteStage(StageAssetsMaterialized, dependencies.Now(), map[string]string{"signedManifestDigest": artifact.ManifestDigest, "signingKeyId": artifact.SigningKeyID, "secondboxDigest": "sha256:" + plan.Release.BinaryDigests["secondbox"], "secondboxDeployDigest": "sha256:" + plan.Release.BinaryDigests["secondbox-deploy"]}); err != nil {
		return receipt, VerifiedArtifact{}, err
	}
	if err := dependencies.PersistReceipt(receipt); err != nil {
		return receipt, VerifiedArtifact{}, err
	}
	return receipt, artifact, nil
}

func validateMaterializedPath(target PlannedPath) error {
	return ValidatePlannedPath(target)
}

func receiptResource(receipt InstallReceipt, id string) (CreatedResource, bool) {
	for _, resource := range receipt.CreatedResources {
		if resource.ID == id {
			return resource, true
		}
	}
	return CreatedResource{}, false
}

func validateVerifiedReleasePlan(plan InstallPlan, verified releaseverify.VerifiedRelease) error {
	manifest := verified.Manifest
	if releasecontract.Digest(verified.ManifestBytes) != plan.Release.ArtifactManifestDigest || manifest.Version != plan.Release.Version || manifest.ControlPlane.Reference != plan.Release.Images["control-plane"] || manifest.Runner.Reference != plan.Release.Images["runner"] || manifest.InstallerTools.Reference != plan.Release.Images["installer-tools"] || manifest.MicroVM.ImageReference != plan.Release.Images["microvm-artifacts"] || manifest.BundledServices.Postgres != plan.Release.Images["postgres"] || manifest.BundledServices.ObjectStore != plan.Release.Images["object-store"] || manifest.BundledServices.ObjectStoreClient != plan.Release.Images["object-store-client"] || manifest.MicroVM.SigningKeyFingerprint != plan.Release.SigningKeyFingerprint {
		return installerError("verified release differs from accepted plan identity", nil)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinaryForMaterialization(manifest, name)
		if !found || binary.SHA256 != plan.Release.BinaryDigests[name] {
			return installerError("verified binary differs from accepted plan: "+name, nil)
		}
	}
	return nil
}

func failMaterialization(receipt InstallReceipt, stage Stage, class FailureClass, dependencies ReleaseMaterializeDependencies, problem error) (InstallReceipt, VerifiedArtifact, error) {
	if err := receipt.Fail(stage, class, dependencies.Now()); err != nil {
		return receipt, VerifiedArtifact{}, errors.Join(problem, err)
	}
	return receipt, VerifiedArtifact{}, errors.Join(problem, dependencies.PersistReceipt(receipt))
}

func appendAndPersistResource(receipt *InstallReceipt, resource CreatedResource, persist func(InstallReceipt) error) error {
	if err := receipt.AppendResource(resource); err != nil {
		return err
	}
	if err := persist(*receipt); err != nil {
		return err
	}
	return nil
}

func releaseBinaryForMaterialization(manifest releasecontract.ArtifactManifest, name string) (releasecontract.BinaryArtifact, bool) {
	for _, binary := range manifest.Binaries {
		if binary.Name == name && binary.Platform == "linux/amd64" {
			return binary, true
		}
	}
	return releasecontract.BinaryArtifact{}, false
}

type releaseBinaryTargetState uint8

const (
	releaseBinaryTargetMissing releaseBinaryTargetState = iota
	releaseBinaryTargetVerified
	releaseBinaryTargetReplaceable
)

func validateReleaseBinaryTargets(plan InstallPlan, manifest releasecontract.ArtifactManifest, validateReplacement func(PlannedPath, string) error) error {
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinaryForMaterialization(manifest, name)
		if !found {
			return installerError("verified release omits linux/amd64 binary "+name, nil)
		}
		target, found := plannedPathByName(plan.Paths, name+"-binary")
		if !found {
			return installerError("binary target is absent from plan: "+name, nil)
		}
		if _, err := inspectReleaseBinaryTarget(target, name, binary.SHA256, validateReplacement); err != nil {
			return err
		}
	}
	return nil
}

func inspectReleaseBinaryTarget(target PlannedPath, name, expectedSHA256 string, validateReplacement func(PlannedPath, string) error) (releaseBinaryTargetState, error) {
	if _, err := os.Lstat(target.Path); errors.Is(err, os.ErrNotExist) {
		return releaseBinaryTargetMissing, nil
	} else if err != nil {
		return 0, installerError("inspect verified binary "+target.Path, err)
	}
	if err := validateMaterializedPath(target); err != nil {
		return 0, err
	}
	actual, err := fileSHA256(target.Path)
	if err != nil {
		return 0, installerError("hash pre-existing binary "+target.Path, err)
	}
	if actual == expectedSHA256 {
		return releaseBinaryTargetVerified, nil
	}
	if validateReplacement == nil {
		validateReplacement = validateReplaceableSecondBoxBinary
	}
	if err := validateReplacement(target, name); err != nil {
		return 0, installerError("refusing to replace pre-existing non-SecondBox binary "+target.Path, err)
	}
	return releaseBinaryTargetReplaceable, nil
}

func validateReplaceableSecondBoxBinary(target PlannedPath, name string) error {
	info, err := buildinfo.ReadFile(target.Path)
	if err != nil {
		return installerError("read embedded Go build identity", err)
	}
	const module = "github.com/SecondStack-AI/SecondBox"
	if info.Path != module+"/cmd/"+name || info.Main.Path != module {
		return installerError("embedded Go build identity is not SecondBox "+name, nil)
	}
	return nil
}

func publishReleaseBinary(target PlannedPath, name, expectedSHA256 string, content []byte, validateReplacement func(PlannedPath, string) error) error {
	state, err := inspectReleaseBinaryTarget(target, name, expectedSHA256, validateReplacement)
	if err != nil {
		return err
	}
	switch state {
	case releaseBinaryTargetVerified:
		return nil
	case releaseBinaryTargetMissing:
		return writeExecutableCreateOnly(target.Path, content, os.FileMode(target.Mode))
	case releaseBinaryTargetReplaceable:
		return writeExecutableAtomicReplace(target, name, expectedSHA256, content, validateReplacement)
	default:
		return installerError("binary target state is invalid: "+name, nil)
	}
}

func writeExecutableAtomicReplace(target PlannedPath, name, expectedSHA256 string, content []byte, validateReplacement func(PlannedPath, string) error) error {
	file, err := os.CreateTemp(filepath.Dir(target.Path), ".secondbox-binary-")
	if err != nil {
		return installerError("stage verified binary "+target.Path, err)
	}
	staging := file.Name()
	cleanup := func() { _ = os.Remove(staging) }
	defer cleanup()
	modeErr := file.Chmod(os.FileMode(target.Mode))
	_, writeErr := file.Write(content)
	closeErr := errors.Join(file.Sync(), file.Close())
	if err := errors.Join(modeErr, writeErr, closeErr); err != nil {
		return installerError("stage verified binary "+target.Path, err)
	}
	state, err := inspectReleaseBinaryTarget(target, name, expectedSHA256, validateReplacement)
	if err != nil {
		return err
	}
	if state == releaseBinaryTargetVerified {
		return nil
	}
	if state == releaseBinaryTargetMissing {
		return writeExecutableCreateOnly(target.Path, content, os.FileMode(target.Mode))
	}
	if state != releaseBinaryTargetReplaceable {
		return installerError("binary target changed before atomic replacement: "+target.Path, nil)
	}
	if err := os.Rename(staging, target.Path); err != nil {
		return installerError("atomically replace verified SecondBox binary "+target.Path, err)
	}
	if err := syncInstallDirectory(filepath.Dir(target.Path)); err != nil {
		return installerError("sync replaced verified SecondBox binary "+target.Path, err)
	}
	return nil
}

func writeExecutableCreateOnly(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return installerError("create verified binary "+path, err)
	}
	modeErr := file.Chmod(mode)
	_, writeErr := file.Write(content)
	closeErr := errors.Join(file.Sync(), file.Close())
	if err := errors.Join(modeErr, writeErr, closeErr); err != nil {
		return errors.Join(installerError("write verified binary "+path, err), os.Remove(path))
	}
	return syncInstallDirectory(filepath.Dir(path))
}

func syncInstallDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// SystemReleaseMaterializer executes the narrow Docker and HTTPS operations
// required by MaterializeRelease while forwarding bounded command output.
type SystemReleaseMaterializer struct {
	Output     io.Writer
	Diagnostic io.Writer
	HTTPClient *http.Client
}

// CandidateReleaseMaterializer uses only exact images already loaded by the
// qualification controller and exact staged release objects. It never contacts
// a registry or release server before publication.
type CandidateReleaseMaterializer struct {
	Directory  string
	Output     io.Writer
	Diagnostic io.Writer
}

func (executor CandidateReleaseMaterializer) PullImage(ctx context.Context, reference string) error {
	command := exec.CommandContext(ctx, "docker", "image", "inspect", reference)
	command.Env = materializerEnvironment()
	command.Stdout, command.Stderr = executor.Output, executor.Diagnostic
	if err := command.Run(); err != nil {
		return fmt.Errorf("inspect preloaded candidate image %s: %w", reference, err)
	}
	return nil
}

func (executor CandidateReleaseMaterializer) ExtractMicroVMImage(ctx context.Context, reference, target string) error {
	return (SystemReleaseMaterializer{Output: executor.Output, Diagnostic: executor.Diagnostic}).ExtractMicroVMImage(ctx, reference, target)
}

func (executor CandidateReleaseMaterializer) Fetch(ctx context.Context, location string) ([]byte, error) {
	return releaseverify.DirectoryFetcher(executor.Directory)(ctx, location)
}

func (executor SystemReleaseMaterializer) PullImage(ctx context.Context, reference string) error {
	return executor.runDocker(ctx, "pull", reference)
}

func (executor SystemReleaseMaterializer) ExtractMicroVMImage(ctx context.Context, reference, target string) (resultErr error) {
	create := exec.CommandContext(ctx, "docker", "create", "--entrypoint", "/bin/true", reference)
	create.Env = materializerEnvironment()
	create.Stderr = executor.Diagnostic
	created, err := create.Output()
	if err != nil {
		return fmt.Errorf("docker create immutable artifact image: %w", err)
	}
	containerID := strings.TrimSpace(string(created))
	if containerID == "" || strings.ContainsAny(containerID, " \t\r\n/") {
		return installerError("docker create returned an invalid container identity", nil)
	}
	defer func() {
		remove := exec.Command("docker", "rm", "--force", containerID)
		remove.Env = materializerEnvironment()
		remove.Stdout, remove.Stderr = executor.Output, executor.Diagnostic
		resultErr = errors.Join(resultErr, remove.Run())
	}()
	return executor.runDocker(ctx, "cp", containerID+":/secondbox-runner-microvm/.", target)
}

func (executor SystemReleaseMaterializer) Fetch(ctx context.Context, location string) ([]byte, error) {
	client := executor.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	return releaseverify.HTTPFetcher(client)(ctx, location)
}

func (executor SystemReleaseMaterializer) runDocker(ctx context.Context, arguments ...string) error {
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Env = materializerEnvironment()
	command.Stdout, command.Stderr = executor.Output, executor.Diagnostic
	return command.Run()
}

func materializerEnvironment() []string {
	allow := map[string]bool{"PATH": true, "HOME": true, "DOCKER_CONFIG": true, "DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true, "DOCKER_API_VERSION": true, "SSH_AUTH_SOCK": true, "XDG_RUNTIME_DIR": true}
	result := []string{}
	for _, entry := range os.Environ() {
		name := strings.SplitN(entry, "=", 2)[0]
		if allow[name] {
			result = append(result, entry)
		}
	}
	slices.Sort(result)
	return result
}
