//go:build linux || darwin

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordedResourcePostconditionsRejectChangedModeAndMissingTarget(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := validPlan(t)
	plan.HostFacts.InvokingUID = int64(os.Getuid())
	plan.HostFacts.InvokingGID = int64(os.Getgid())
	var err error
	plan.HostFactsDigest, err = HostFactsDigest(plan.HostFacts)
	if err != nil {
		t.Fatal(err)
	}
	for index := range plan.Paths {
		if plan.Paths[index].Name == "deployment" {
			plan.Paths[index].Path = path
			plan.Paths[index].OwnerUID = int64(os.Getuid())
			plan.Paths[index].OwnerGID = int64(os.Getgid())
		}
	}
	receipt, err := NewReceipt(plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.AppendResource(CreatedResource{ID: "operation-directory", Kind: ResourceDirectory, Path: path, Class: PathUserDeployment, Stage: StagePlanAccepted, Mode: 0o700, OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid())}); err != nil {
		t.Fatal(err)
	}
	workspace, found := plannedPathByName(plan.Paths, "workspace")
	if !found {
		t.Fatal("workspace path is absent")
	}
	if err := receipt.AppendResource(resourceFromPath(workspace, StageHostApply)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedResources(plan, receipt); err != nil {
		t.Fatal(err)
	}
	changedStage := receipt
	changedStage.CreatedResources = append([]CreatedResource(nil), receipt.CreatedResources...)
	changedStage.CreatedResources[len(changedStage.CreatedResources)-1].Stage = StageAssetsMaterialized
	if err := ValidateRecordedResources(plan, changedStage); err == nil {
		t.Fatal("privileged resource with an invalid stage was accepted")
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedResources(plan, receipt); err == nil {
		t.Fatal("changed resource mode was accepted")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedResources(plan, receipt); err == nil {
		t.Fatal("missing recorded resource was accepted")
	}
}

func TestRecordedResourcePostconditionsUseActiveManifestAfterSuccessfulUpdate(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "secondbox.yaml")
	original := []byte("release: 0.4.0\n")
	active := []byte("release: 0.5.0\n")
	if err := os.WriteFile(manifestPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	plan := validPlan(t)
	plan.HostFacts.InvokingUID = int64(os.Getuid())
	plan.HostFacts.InvokingGID = int64(os.Getgid())
	var err error
	plan.HostFactsDigest, err = HostFactsDigest(plan.HostFacts)
	if err != nil {
		t.Fatal(err)
	}
	plan.Paths = append(plan.Paths, PlannedPath{Name: "manifest", Path: manifestPath, Class: PathUserDeployment, Kind: ResourceFile, Mode: 0o600, OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid()), Create: true})
	plan.Paths = append(plan.Paths, PlannedPath{Name: "runner-identity", Path: filepath.Join(directory, "runner-identity"), Class: PathUserDeployment, Kind: ResourceDirectory, Mode: 0o700, OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid()), Create: true})
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for index, stage := range StageSequence {
		evidence := map[string]string{}
		if stage == StageDeploymentMaterialized {
			evidence["manifestDigest"] = Digest(original)
		}
		if stage == StageRunnerEnrolled {
			evidence["runnerId"] = "runner-0123456789abcdef"
			evidence["identity"] = filepath.Join(directory, "runner-identity")
		}
		if err := receipt.CompleteStage(stage, plan.CreatedAt.Add(time.Duration(index+1)*time.Second), evidence); err != nil {
			t.Fatal(err)
		}
	}
	if err := receipt.AppendResource(CreatedResource{ID: "manifest", Kind: ResourceFile, Path: manifestPath, Class: PathUserDeployment, Stage: StageDeploymentMaterialized, Mode: 0o600, OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid()), Digest: Digest(original)}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedResources(plan, receipt); err != nil {
		t.Fatal(err)
	}

	target := plan.Release
	target.Version = "0.5.0"
	target.ArtifactManifestURL = "https://github.com/SecondStack-AI/SecondBox/releases/download/v0.5.0/secondbox-0.5.0-artifact-manifest.json"
	target.ArtifactManifestDigest = "sha256:" + strings.Repeat("9", 64)
	started := plan.CreatedAt.Add(time.Hour)
	if err := receipt.BeginUpdate("update_0123456789abcdef", plan.Release, target, started); err != nil {
		t.Fatal(err)
	}
	for index, stage := range UpdateStageSequence {
		if err := receipt.CompleteUpdateStage(stage, started.Add(time.Duration(index+1)*time.Second), map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(manifestPath, active, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := receipt.RefreshUpdatedResource("manifest", Digest(active)); err != nil {
		t.Fatal(err)
	}
	if err := receipt.ActivateUpdate(&plan, started.Add(time.Duration(len(UpdateStageSequence)+1)*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedResources(plan, receipt); err != nil {
		t.Fatalf("active updated manifest was rejected: %v", err)
	}

	if err := os.WriteFile(manifestPath, []byte("release: tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecordedResources(plan, receipt); err == nil {
		t.Fatal("tampered updated manifest was accepted")
	}
}
