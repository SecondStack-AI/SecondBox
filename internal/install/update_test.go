package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

func testUpdateStageEvidence(stage UpdateStage) map[string]string {
	evidence := map[string]string{"verified": "true"}
	if stage == UpdateStageActivationStarted {
		evidence["sourceComposeSubject"] = "sha256:" + strings.Repeat("e", 64)
	}
	return evidence
}

func successfulReceipt(t *testing.T, plan InstallPlan) InstallReceipt {
	t.Helper()
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for index, stage := range StageSequence {
		if err := receipt.CompleteStage(stage, plan.CreatedAt.Add(time.Duration(index+1)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}
	return receipt
}

func targetRelease(plan InstallPlan, version string) ReleasePlan {
	target := plan.Release
	target.Version = version
	target.ArtifactManifestURL = "https://github.com/SecondStack-AI/SecondBox/releases/download/v" + version + "/secondbox-" + version + "-artifact-manifest.json"
	target.ArtifactManifestDigest = "sha256:" + strings.Repeat("9", 64)
	target.BinaryDigests = map[string]string{"secondbox": strings.Repeat("8", 64), "secondbox-deploy": strings.Repeat("7", 64)}
	target.Images = map[string]string{}
	for name := range plan.Release.Images {
		target.Images[name] = "ghcr.io/secondstack-ai/secondbox/" + name + "@sha256:" + strings.Repeat("6", 64)
	}
	return target
}

func TestUpdateStagingCapacityIsCheckedBeforeJournaling(t *testing.T) {
	plan := validPlan(t)
	artifactParent := t.TempDir()
	artifacts := filepath.Join(artifactParent, "artifacts")
	if err := os.Mkdir(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range plan.Paths {
		if plan.Paths[index].Name == "artifacts" {
			plan.Paths[index] = plannedPath("artifacts", artifacts, PathUserDeployment, ResourceDirectory, 0o700, int64(os.Getuid()), int64(os.Getgid()), false, true)
		}
	}
	target := targetRelease(plan, "0.5.0")
	target.ExpectedDownloadBytes = 1
	if err := ValidateUpdateStagingCapacity(plan, target); err != nil {
		t.Fatalf("available staging capacity = %v", err)
	}
	target.ExpectedDownloadBytes = int64(^uint64(0) >> 1)
	if err := ValidateUpdateStagingCapacity(plan, target); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("impossible staging capacity = %v", err)
	}
}

func TestDecodeMigratesV1PlanAndReceiptWithoutLosingIdentity(t *testing.T) {
	plan := validPlan(t)
	legacyPlan := plan
	legacyPlan.SchemaVersion = PlanSchemaV1
	legacyPlan.ReleaseHistory = nil
	legacyPlanBytes, err := Canonical(legacyPlan)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest, err := PlanDigest(legacyPlan)
	if err != nil {
		t.Fatal(err)
	}
	receipt := successfulReceipt(t, plan)
	receipt.SchemaVersion = ReceiptSchemaV1
	receipt.PlanDigest = legacyDigest
	receipt.Updates = nil
	legacyReceiptBytes, err := Canonical(receipt)
	if err != nil {
		t.Fatal(err)
	}

	migratedPlan, err := DecodePlan(legacyPlanBytes)
	if err != nil {
		t.Fatal(err)
	}
	migratedReceipt, err := DecodeReceipt(legacyReceiptBytes, migratedPlan)
	if err != nil {
		t.Fatal(err)
	}
	if migratedPlan.SchemaVersion != PlanSchema || len(migratedPlan.ReleaseHistory) != 1 || migratedReceipt.SchemaVersion != ReceiptSchema || len(migratedReceipt.Updates) != 0 {
		t.Fatalf("migration = plan %#v receipt %#v", migratedPlan.ReleaseHistory, migratedReceipt.Updates)
	}
	wantDigest, err := PlanDigest(migratedPlan)
	if err != nil {
		t.Fatal(err)
	}
	if migratedReceipt.PlanDigest != wantDigest {
		t.Fatalf("migrated receipt digest = %s, want %s", migratedReceipt.PlanDigest, wantDigest)
	}
}

func TestUpdateLedgerAdvancesReleaseOnlyAfterSmoke(t *testing.T) {
	plan := validPlan(t)
	receipt := successfulReceipt(t, plan)
	target := targetRelease(plan, "0.5.0")
	started := plan.CreatedAt.Add(time.Hour)
	if err := receipt.BeginUpdate("update_0123456789abcdef", plan.Release, target, started); err != nil {
		t.Fatal(err)
	}
	for index, stage := range UpdateStageSequence {
		if err := receipt.CompleteUpdateStage(stage, started.Add(time.Duration(index+1)*time.Second), testUpdateStageEvidence(stage)); err != nil {
			t.Fatal(err)
		}
		if plan.Release.Version != "0.4.0" {
			t.Fatalf("release advanced before activation at %s", stage)
		}
	}
	activated := started.Add(time.Duration(len(UpdateStageSequence)+1) * time.Second)
	if err := receipt.ActivateUpdate(&plan, activated); err != nil {
		t.Fatal(err)
	}
	if plan.Release.Version != "0.5.0" || len(plan.ReleaseHistory) != 2 || receipt.Updates[0].Status != UpdateSucceeded {
		t.Fatalf("activation = plan %#v update %#v", plan.ReleaseHistory, receipt.Updates[0])
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		t.Fatal(err)
	}
	encoded, err := Canonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReceipt(encoded, plan); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptRejectsActivationBoundaryWithoutSourceComposeIdentity(t *testing.T) {
	plan := validPlan(t)
	receipt := successfulReceipt(t, plan)
	started := plan.CreatedAt.Add(time.Hour)
	if err := receipt.BeginUpdate("update_0123456789abcdef", plan.Release, targetRelease(plan, "0.5.0"), started); err != nil {
		t.Fatal(err)
	}
	for index, stage := range UpdateStageSequence[:4] {
		if err := receipt.CompleteUpdateStage(stage, started.Add(time.Duration(index+1)*time.Second), map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err == nil || !strings.Contains(err.Error(), "source Compose identity") {
		t.Fatalf("activation without source Compose identity validation = %v", err)
	}
}

func TestFailedUpdateResumesAtFirstIncompleteStage(t *testing.T) {
	plan := validPlan(t)
	receipt := successfulReceipt(t, plan)
	started := plan.CreatedAt.Add(time.Hour)
	if err := receipt.BeginUpdate("update_0123456789abcdef", plan.Release, targetRelease(plan, "0.5.0"), started); err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteUpdateStage(UpdateStagePreflight, started.Add(time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if err := receipt.FailUpdate(UpdateStageReleaseVerified, FailureRetryable, started.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteUpdateStage(UpdateStageReleaseVerified, started.Add(3*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	update, ok := receipt.ActiveUpdate()
	if !ok || update.Status != UpdateRunning || len(update.CompletedStages) != 2 || update.FailureStage != "" {
		t.Fatalf("resumed update = %#v, active %v", update, ok)
	}
}

func TestUpdateRequiresAssetsCompatibleWithPinnedProfileRevisions(t *testing.T) {
	runtimeDigest := "sha256:" + strings.Repeat("a", 64)
	toolchainDigest := "sha256:" + strings.Repeat("b", 64)
	source := releasecontract.ArtifactManifest{MicroVM: releasecontract.MicroVMArtifact{
		RuntimeBundle:   releasecontract.SignedComponent{ManifestDigest: runtimeDigest},
		ToolchainBundle: releasecontract.SignedComponent{ManifestDigest: toolchainDigest},
	}}
	target := source
	if err := ValidateUpdateAssetCompatibility(source, target); err != nil {
		t.Fatalf("unchanged execution assets = %v", err)
	}
	target.MicroVM.RuntimeBundle.ManifestDigest = "sha256:" + strings.Repeat("c", 64)
	if err := ValidateUpdateAssetCompatibility(source, target); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("changed runtime asset = %v", err)
	}
	target = source
	target.MicroVM.ToolchainBundle.ManifestDigest = "sha256:" + strings.Repeat("d", 64)
	if err := ValidateUpdateAssetCompatibility(source, target); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("changed toolchain asset = %v", err)
	}
}

func TestReceiptRejectsLifecycleStateDuringActiveUpdate(t *testing.T) {
	plan := validPlan(t)
	receipt := successfulReceipt(t, plan)
	if err := receipt.BeginUpdate("update_0123456789abcdef", plan.Release, targetRelease(plan, "0.5.0"), plan.CreatedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	receipt.Status = OperationUninstalled
	encoded, err := Canonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReceipt(encoded, plan); err == nil || !strings.Contains(err.Error(), "active update requires") {
		t.Fatalf("conflicting lifecycle state = %v", err)
	}
}

func TestStageUpdateReleaseIsResumableAndDoesNotReplaceActivePaths(t *testing.T) {
	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)
	source := t.TempDir()
	release := writeSignedArtifactFixture(t, source)
	secondboxBytes, deployBytes := []byte("target secondbox"), []byte("target secondbox-deploy")
	for index := range release.Binaries {
		if release.Binaries[index].Platform != "linux/amd64" {
			continue
		}
		if release.Binaries[index].Name == "secondbox" {
			release.Binaries[index].SHA256 = fixtureChecksum(secondboxBytes)
		} else if release.Binaries[index].Name == "secondbox-deploy" {
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
	plan := validPlan(t)
	deployment := t.TempDir()
	runner := t.TempDir()
	artifactParent := filepath.Join(runner, "release")
	if err := os.Mkdir(artifactParent, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range plan.Paths {
		switch plan.Paths[index].Name {
		case "deployment":
			plan.Paths[index].Path = deployment
		case "artifacts-parent":
			plan.Paths[index].Path = artifactParent
		case "artifacts":
			plan.Paths[index].Path = filepath.Join(artifactParent, "artifacts")
		case "secondbox-binary":
			plan.Paths[index].Path = filepath.Join(deployment, "secondbox")
		case "secondbox-deploy-binary":
			plan.Paths[index].Path = filepath.Join(deployment, "secondbox-deploy")
		}
	}
	plan.Paths = append(plan.Paths,
		plannedPath("secondbox-binary", filepath.Join(deployment, "secondbox"), PathUserDeployment, ResourceBinary, 0o755, int64(os.Getuid()), int64(os.Getgid()), false, true),
		plannedPath("secondbox-deploy-binary", filepath.Join(deployment, "secondbox-deploy"), PathUserDeployment, ResourceBinary, 0o755, int64(os.Getuid()), int64(os.Getgid()), false, true),
	)
	target := releasePlanForMaterializer(release, releaseBytes)
	update := UpdateRecord{ID: "update_0123456789abcdef", SourceRelease: plan.Release, TargetRelease: target, Status: UpdateRunning, StartedAt: time.Now(), UpdatedAt: time.Now()}
	binaries := map[string][]byte{}
	for _, binary := range release.Binaries {
		if binary.Name == "secondbox" && binary.Platform == "linux/amd64" {
			binaries[binary.Location] = secondboxBytes
		}
		if binary.Name == "secondbox-deploy" && binary.Platform == "linux/amd64" {
			binaries[binary.Location] = deployBytes
		}
	}
	executor := &fakeReleaseMaterializer{source: source, binaries: binaries}
	verified := releaseverify.VerifiedRelease{Manifest: release, ManifestBytes: releaseBytes}
	interrupted, err := UpdateStaging(plan, update)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(interrupted.ArtifactPartial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interrupted.ArtifactPartial, "incomplete"), []byte("crash residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, artifact, err := StageUpdateRelease(context.Background(), plan, update, verified, executor)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ManifestDigest != release.MicroVM.SignedManifestDigest || executor.extractions != 1 || len(executor.pulls) != 5 {
		t.Fatalf("first stage = artifact %#v extractions %d pulls %d", artifact, executor.extractions, len(executor.pulls))
	}
	if _, err := os.Lstat(filepath.Join(artifactParent, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("active artifact path changed before activation: %v", err)
	}
	if _, _, err := StageUpdateRelease(context.Background(), plan, update, verified, executor); err != nil {
		t.Fatal(err)
	}
	if executor.extractions != 1 || len(executor.pulls) != 10 {
		t.Fatalf("resumed stage = extractions %d pulls %d", executor.extractions, len(executor.pulls))
	}
	for _, path := range []string{staged.SecondBoxBinary, staged.SecondBoxDeployBinary, staged.ReleaseArtifactManifest, staged.Artifacts} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("staged path %s: %v", path, err)
		}
	}
	active := filepath.Join(artifactParent, "artifacts")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := executor.ExtractMicroVMImage(context.Background(), release.MicroVM.ImageReference, active); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateUpdateArtifactsAndBinaries(plan, update, release, verified); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateActivatedUpdateArtifactsAndBinaries(plan, update, verified); err != nil {
		t.Fatalf("activated target validation = %v", err)
	}
	deployPath, _ := plannedPathByName(plan.Paths, "secondbox-deploy-binary")
	if err := os.WriteFile(deployPath.Path, []byte("unverified replacement"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateActivatedUpdateArtifactsAndBinaries(plan, update, verified); err == nil || !strings.Contains(err.Error(), "verified target") {
		t.Fatalf("changed active update binary was adopted: %v", err)
	}
	if err := os.WriteFile(deployPath.Path, deployBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateActivatedUpdateArtifactsAndBinaries(plan, update, verified); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("injected nested mount")
	if err := cleanupUpdateStaging(plan, update, release, release, func(path string) error {
		if path == staged.PreviousArtifacts {
			return refused
		}
		return nil
	}); !errors.Is(err, refused) {
		t.Fatalf("cleanup nested-mount refusal = %v", err)
	}
	if _, err := os.Lstat(staged.PreviousArtifacts); err != nil {
		t.Fatalf("refused cleanup changed previous artifacts: %v", err)
	}
	if err := CleanupUpdateStaging(plan, update, release, release); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateActivatedUpdateArtifactsAndBinaries(plan, update, verified); err != nil {
		t.Fatalf("post-cleanup target validation = %v", err)
	}
	if _, err := VerifyArtifactDirectory(active, release); err != nil {
		t.Fatalf("active target changed during cleanup: %v", err)
	}
	for _, path := range []string{staged.PreviousArtifacts, staged.Root, staged.Artifacts, staged.ArtifactPartial} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("update-owned cleanup residue %s: %v", path, err)
		}
	}
}

func TestInterruptedUpdateArtifactCleanupRejectsUnsafePath(t *testing.T) {
	outside := t.TempDir()
	partial := filepath.Join(t.TempDir(), ".artifacts-update_0123456789abcdef.partial")
	if err := os.Symlink(outside, partial); err != nil {
		t.Fatal(err)
	}
	if err := removeInterruptedUpdateArtifacts(partial); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe interrupted staging cleanup = %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory changed: %v", err)
	}
}
