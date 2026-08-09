package install

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

func TestSystemReleaseMaterializerExtractMicroVMImageOverridesMissingImageCommand(t *testing.T) {
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	const reference = "ghcr.io/secondstack-ai/secondbox/microvm-artifacts@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const script = `#!/bin/sh
case "$1" in
create)
	[ "$#" -eq 4 ] && [ "$2" = "--entrypoint" ] && [ "$3" = "/bin/true" ] && [ "$4" = "` + reference + `" ] || exit 91
	printf '%s\n' fake-container-id
	;;
cp)
	[ "$#" -eq 3 ] && [ "$2" = "fake-container-id:/secondbox-runner-microvm/." ] || exit 92
	;;
rm)
	[ "$#" -eq 3 ] && [ "$2" = "--force" ] && [ "$3" = "fake-container-id" ] || exit 93
	;;
*)
	exit 94
	;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	materializer := SystemReleaseMaterializer{Output: io.Discard, Diagnostic: io.Discard}
	if err := materializer.ExtractMicroVMImage(context.Background(), reference, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

type fakeReleaseMaterializer struct {
	source      string
	binaries    map[string][]byte
	pulls       []string
	extractions int
	failFetch   string
}

func (executor *fakeReleaseMaterializer) PullImage(_ context.Context, reference string) error {
	executor.pulls = append(executor.pulls, reference)
	return nil
}

func (executor *fakeReleaseMaterializer) ExtractMicroVMImage(_ context.Context, _ string, target string) error {
	executor.extractions++
	entries, err := os.ReadDir(executor.source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(executor.source, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(target, entry.Name()), content, 0o444); err != nil {
			return err
		}
	}
	return nil
}

func (executor *fakeReleaseMaterializer) Fetch(_ context.Context, location string) ([]byte, error) {
	if executor.failFetch == location {
		return nil, errors.New("injected download failure")
	}
	content, found := executor.binaries[location]
	if !found {
		return nil, errors.New("unexpected binary location")
	}
	return slices.Clone(content), nil
}

func TestMaterializeReleaseResumesWithoutReextractingVerifiedBundle(t *testing.T) {
	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)
	source := t.TempDir()
	release := writeSignedArtifactFixture(t, source)
	secondboxBytes := []byte("secondbox release binary")
	deployBytes := []byte("secondbox-deploy release binary")
	for index := range release.Binaries {
		if release.Binaries[index].Platform != "linux/amd64" {
			continue
		}
		switch release.Binaries[index].Name {
		case "secondbox":
			release.Binaries[index].SHA256 = fixtureChecksum(secondboxBytes)
		case "secondbox-deploy":
			release.Binaries[index].SHA256 = fixtureChecksum(deployBytes)
		}
	}
	if err := release.Validate(); err != nil {
		t.Fatal(err)
	}
	releaseBytes, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".local"), 0o700); err != nil {
		t.Fatal(err)
	}
	operation := filepath.Join(root, "operation")
	if err := os.Mkdir(operation, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := validPlan(t)
	plan.HostFacts.InvokingUID, plan.HostFacts.InvokingGID = int64(os.Getuid()), int64(os.Getgid())
	plan.HostFactsDigest, err = HostFactsDigest(plan.HostFacts)
	if err != nil {
		t.Fatal(err)
	}
	plan.Release = releasePlanForMaterializer(release, releaseBytes)
	runnerRoot := filepath.Join(root, "runner")
	runnerStorage := filepath.Join(runnerRoot, "storage")
	workspace := filepath.Join(runnerStorage, "workspaces")
	artifactParent := filepath.Join(runnerStorage, "release")
	if err := os.MkdirAll(artifactParent, 0o700); err != nil {
		t.Fatal(err)
	}
	plan.Storage = StoragePlan{Choice: StorageExistingMount, WorkspacePath: workspace, ExistingDeviceIdentity: "fixture-device"}
	plan.CLI = CLIPlan{ConfigPath: filepath.Join(root, "config", "secondbox", "config.json"), TenantRef: "tenant", SubjectRef: "subject"}
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	plan.Paths = []PlannedPath{
		plannedPath("deployment", operation, PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("runner-root", runnerRoot, PathExistingWorkspace, ResourceDirectory, 0o711, 0, 0, true, true),
		plannedPath("runner-storage", runnerStorage, PathExistingWorkspace, ResourceDirectory, 0o711, 0, 0, true, true),
		plannedPath("artifacts-parent", artifactParent, PathInstallerHost, ResourceDirectory, 0o700, uid, gid, true, true),
		plannedPath("artifacts", filepath.Join(artifactParent, "artifacts"), PathInstallerHost, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("state", filepath.Join(runnerStorage, "state"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("jail", filepath.Join(runnerStorage, "jail"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("run", filepath.Join(runnerStorage, "state", "run"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("binary-directory-root", filepath.Join(root, ".local"), PathUserDeployment, ResourceDirectory, 0o755, uid, gid, false, true),
		plannedPath("binary-directory", filepath.Join(root, ".local", "bin"), PathUserDeployment, ResourceDirectory, 0o755, uid, gid, false, true),
		plannedPath("secondbox-binary", filepath.Join(root, ".local", "bin", "secondbox"), PathUserDeployment, ResourceBinary, 0o755, uid, gid, false, true),
		plannedPath("secondbox-deploy-binary", filepath.Join(root, ".local", "bin", "secondbox-deploy"), PathUserDeployment, ResourceBinary, 0o755, uid, gid, false, true),
		plannedPath("cli-config", plan.CLI.ConfigPath, PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true),
		plannedPath("platform-token", filepath.Join(operation, "platform-token"), PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true),
		plannedPath("workspace", workspace, PathExistingWorkspace, ResourceDirectory, 0o750, runnerContainerUID, runnerContainerGID, true, true),
	}
	plan.SecretTargets = []SecretTarget{{Category: "platform-authority", Path: filepath.Join(operation, "platform-token")}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	receipt, err := NewReceipt(plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []Stage{StagePreflight, StagePlanAccepted, StageHostApply} {
		if err := receipt.CompleteStage(stage, time.Now(), map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	binaryData := map[string][]byte{}
	deployLocation := ""
	for _, binary := range release.Binaries {
		if binary.Platform != "linux/amd64" {
			continue
		}
		if binary.Name == "secondbox" {
			binaryData[binary.Location] = secondboxBytes
		} else {
			binaryData[binary.Location] = deployBytes
			deployLocation = binary.Location
		}
	}
	executor := &fakeReleaseMaterializer{source: source, binaries: binaryData, failFetch: deployLocation}
	verified := releaseverify.VerifiedRelease{Manifest: release, ManifestBytes: releaseBytes}
	persisted := receipt
	persist := func(value InstallReceipt) error { persisted = value; return nil }
	first, _, err := MaterializeRelease(context.Background(), plan, receipt, verified, ReleaseMaterializeDependencies{Executor: executor, PersistReceipt: persist, Now: time.Now})
	if err == nil || first.Status != OperationFailed || lastStageForTest(first) != StageReleaseVerified || executor.extractions != 1 {
		t.Fatalf("first materialization = status %s stage %s extractions %d error %v", first.Status, lastStageForTest(first), executor.extractions, err)
	}
	// Model a process exit after publication but before each corresponding
	// receipt write reached durable storage. Retry may adopt only outputs that
	// still match the accepted path metadata and verified release bytes.
	persisted.CreatedResources = slices.DeleteFunc(persisted.CreatedResources, func(resource CreatedResource) bool {
		return resource.ID == "artifacts" || resource.ID == "binary-directory" || resource.ID == "secondbox-binary"
	})
	persisted.PendingResourceIDs = append(persisted.PendingResourceIDs, "artifacts", "secondbox-binary")
	executor.failFetch = ""
	completed, artifact, err := MaterializeRelease(context.Background(), plan, persisted, verified, ReleaseMaterializeDependencies{Executor: executor, PersistReceipt: persist, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != OperationRunning || lastStageForTest(completed) != StageAssetsMaterialized || executor.extractions != 1 || artifact.ManifestDigest != release.MicroVM.SignedManifestDigest {
		t.Fatalf("resumed materialization = status %s stage %s extractions %d artifact %#v", completed.Status, lastStageForTest(completed), executor.extractions, artifact)
	}
	if len(executor.pulls) != 14 {
		t.Fatalf("immutable image pulls = %d, want seven per retry", len(executor.pulls))
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		info, err := os.Stat(filepath.Join(root, ".local", "bin", name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode under hardened umask = %o", name, info.Mode().Perm())
		}
	}
	localInfo, err := os.Stat(filepath.Join(root, ".local"))
	if err != nil {
		t.Fatal(err)
	}
	if localInfo.Mode().Perm() != 0o700 {
		t.Fatalf("pre-existing binary root mode = %o, want preserved 700", localInfo.Mode().Perm())
	}
	if _, recorded := receiptResource(completed, "binary-directory-root"); recorded {
		t.Fatal("pre-existing binary root was claimed as an installer-created resource")
	}
}

func releasePlanForMaterializer(release releasecontract.ArtifactManifest, releaseBytes []byte) ReleasePlan {
	binaries := map[string]string{}
	for _, binary := range release.Binaries {
		if binary.Platform == "linux/amd64" {
			binaries[binary.Name] = binary.SHA256
		}
	}
	return ReleasePlan{Version: release.Version, ArtifactManifestURL: releasecontract.ArtifactManifestLocation(release.Version), ArtifactManifestDigest: releasecontract.Digest(releaseBytes), SigningKeyFingerprint: release.MicroVM.SigningKeyFingerprint, Images: map[string]string{"control-plane": release.ControlPlane.Reference, "runner": release.Runner.Reference, "microvm-artifacts": release.MicroVM.ImageReference, "installer-tools": release.InstallerTools.Reference, "postgres": release.BundledServices.Postgres, "object-store": release.BundledServices.ObjectStore, "object-store-client": release.BundledServices.ObjectStoreClient}, BinaryDigests: binaries, ExpectedDownloadBytes: ExecutionBundleEstimateBytes}
}

func lastStageForTest(receipt InstallReceipt) Stage {
	if len(receipt.CompletedStages) == 0 {
		return ""
	}
	return receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage
}
