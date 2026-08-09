//go:build linux

package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func TestRemoveTreeConfinesDeletionToOpenedExactTarget(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "owned-target")
	neighbor := filepath.Join(parent, "unrelated-neighbor")
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "nested", "data"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(neighbor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neighbor, "keep"), []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := removeTreeIfExists(target)
	if err != nil || !removed {
		t.Fatalf("remove exact tree = %t, %v", removed, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target remains: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(neighbor, "keep")); err != nil || string(content) != "unrelated" {
		t.Fatalf("neighbor changed: %q, %v", content, err)
	}
}

func TestUserPurgeResumesAfterVerifiedArtifactsWereRemoved(t *testing.T) {
	root := t.TempDir()
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	plan := validPlan(t)
	artifactIndex := slices.IndexFunc(plan.Paths, func(path PlannedPath) bool { return path.Name == "artifacts" })
	plan.Paths[artifactIndex] = plannedPath("artifacts", filepath.Join(root, "artifacts"), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true)
	plan.Paths = append(plan.Paths, plannedPath("release-artifact-manifest", filepath.Join(root, "release-artifact-manifest.json"), PathUserDeployment, ResourceFile, 0o644, uid, gid, false, true))
	receipt := InstallReceipt{Status: OperationPurging, CreatedResources: []CreatedResource{
		resourceFromPath(plan.Paths[artifactIndex], StageAssetsMaterialized),
		resourceFromPath(plan.Paths[len(plan.Paths)-1], StageAssetsMaterialized),
	}}
	receipt.CreatedResources[1].Digest = "sha256:" + strings.Repeat("a", 64)
	receipt.RemovedResourceIDs = []string{"artifacts"}
	persisted := 0
	updated, err := PurgeUserResources(plan, receipt, func() time.Time { return plan.CreatedAt.Add(time.Minute) }, func(InstallReceipt) error {
		persisted++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 1 || !slices.Contains(updated.RemovedResourceIDs, "artifacts") || !slices.Contains(updated.RemovedResourceIDs, "release-artifact-manifest") {
		t.Fatalf("recovered removal ledger = %#v after %d persists", updated.RemovedResourceIDs, persisted)
	}
}

func TestVerifiedArtifactPurgePrecedesPrivilegedRunnerStorageDeletion(t *testing.T) {
	root := t.TempDir()
	artifacts := filepath.Join(root, "runner", "storage", "release", "artifacts")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	release := writeSignedArtifactFixture(t, artifacts)
	releaseBytes, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "deployment", "release-artifact-manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, releaseBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	plan := validPlan(t)
	plan.Release.ArtifactManifestDigest = releasecontract.Digest(releaseBytes)
	for index := range plan.Paths {
		switch plan.Paths[index].Name {
		case "artifacts":
			plan.Paths[index] = plannedPath("artifacts", artifacts, PathInstallerHost, ResourceDirectory, 0o700, uid, gid, false, true)
		case "release-artifact-manifest":
			plan.Paths[index].Path = manifest
		}
	}
	if _, found := plannedPathByName(plan.Paths, "release-artifact-manifest"); !found {
		plan.Paths = append(plan.Paths, plannedPath("release-artifact-manifest", manifest, PathUserDeployment, ResourceFile, 0o644, uid, gid, false, true))
	}
	artifactPath, _ := plannedPathByName(plan.Paths, "artifacts")
	releasePath, _ := plannedPathByName(plan.Paths, "release-artifact-manifest")
	receipt := InstallReceipt{Status: OperationPurging, CreatedResources: []CreatedResource{resourceFromPath(artifactPath, StageAssetsMaterialized), resourceFromPath(releasePath, StageAssetsMaterialized)}}
	receipt.CreatedResources[0].Digest = release.MicroVM.SignedManifestDigest
	receipt.CreatedResources[1].Digest = Digest(releaseBytes)
	persisted := 0
	updated, err := PurgeVerifiedArtifacts(plan, receipt, time.Now, func(InstallReceipt) error { persisted++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 1 || !slices.Contains(updated.RemovedResourceIDs, "artifacts") {
		t.Fatalf("artifact removal was not journaled exactly once: %#v after %d persists", updated.RemovedResourceIDs, persisted)
	}
	if _, err := os.Lstat(artifacts); !os.IsNotExist(err) {
		t.Fatalf("verified artifacts remain: %v", err)
	}
	if content, err := os.ReadFile(manifest); err != nil || string(content) != string(releaseBytes) {
		t.Fatalf("release verification authority changed before sudo purge: %v", err)
	}
}

func TestRemoveTreeRefusesSymlinkTargetWithoutTouchingReferent(t *testing.T) {
	parent := t.TempDir()
	referent := filepath.Join(parent, "referent")
	if err := os.Mkdir(referent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(referent, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "owned-target")
	if err := os.Symlink(referent, link); err != nil {
		t.Fatal(err)
	}
	if _, err := removeTreeIfExists(link); err == nil {
		t.Fatal("symlink purge target succeeded")
	}
	if content, err := os.ReadFile(filepath.Join(referent, "keep")); err != nil || string(content) != "keep" {
		t.Fatalf("symlink referent changed: %q, %v", content, err)
	}
}

func TestPrivilegedPurgeRequiresExactPlanAndReceiptAuthority(t *testing.T) {
	plan := validPlan(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requirePrivilegedPurgeResource(plan, receipt, "runner-root"); err == nil {
		t.Fatal("plan-only deletion authority was accepted")
	}
	runnerRoot, _ := plannedPathByName(plan.Paths, "runner-root")
	resource := resourceFromPath(runnerRoot, StageHostApply)
	if err := receipt.AppendResource(resource); err != nil {
		t.Fatal(err)
	}
	if _, err := requirePrivilegedPurgeResource(plan, receipt, "runner-root"); err != nil {
		t.Fatal(err)
	}
	receipt.CreatedResources[len(receipt.CreatedResources)-1].Mode = 0o755
	if _, err := requirePrivilegedPurgeResource(plan, receipt, "runner-root"); err == nil {
		t.Fatal("changed receipt ownership evidence was accepted")
	}
}

func TestPurgeRefusesChangedRegularFileDigest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "generated")
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := removeRegularPathIfExists(path, Digest([]byte("accepted"))); err == nil {
		t.Fatal("changed generated file was purged")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "changed" {
		t.Fatalf("changed file was touched: %q, %v", content, err)
	}
}

func TestPurgeWorkspaceRequiresHostApplyDeviceIdentity(t *testing.T) {
	workspace := t.TempDir()
	identity := "btrfs-uuid:01234567-89ab-cdef-0123-456789abcdef"
	plan := validPlan(t)
	plan.Storage.Choice = StorageBtrfsImage
	resource := CreatedResource{ID: "workspace", Path: workspace}
	receipt := InstallReceipt{CompletedStages: []StageRecord{{Stage: StageHostApply, CompletedAt: time.Now(), Evidence: map[string]string{"workspaceDeviceIdentity": identity}}}}
	identify := func(string) (string, error) { return identity, nil }
	if err := validatePurgeWorkspaceIdentityWith(plan, receipt, resource, identify); err != nil {
		t.Fatal(err)
	}
	receipt.CompletedStages[0].Evidence["workspaceDeviceIdentity"] = "changed-device"
	if err := validatePurgeWorkspaceIdentityWith(plan, receipt, resource, identify); err == nil {
		t.Fatal("workspace on a different device was accepted for recursive purge")
	}
}
