//go:build linux || darwin

package install

import (
	"os"
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
	if err := ValidateRecordedResources(plan, receipt); err != nil {
		t.Fatal(err)
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
