//go:build linux || darwin

package install

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
)

// ValidateRecordedResources treats the accepted plan and receipt as evidence,
// not deletion or replay authority. The privileged host-apply helper validates
// root-only resources before this unprivileged validation runs; every other
// still-present resource must retain its exact path, kind, owner, mode, and
// recorded content digest.
func ValidateRecordedResources(plan InstallPlan, receipt InstallReceipt) error {
	digest, err := PlanDigest(plan)
	if err != nil {
		return err
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return err
	}
	planned := make(map[string]PlannedPath, len(plan.Paths)+1)
	for _, path := range plan.Paths {
		planned[path.Name] = path
	}
	if deployment, found := planned["deployment"]; found {
		planned["operation-directory"] = deployment
	}
	for _, resource := range receipt.CreatedResources {
		if resource.ID == "compose-project" || slices.Contains(receipt.RemovedResourceIDs, resource.ID) {
			continue
		}
		expected, found := planned[resource.ID]
		if !found || resource.Path != expected.Path || resource.Kind != expected.Kind || resource.Class != expected.Class || resource.Mode != expected.Mode || resource.OwnerUID != expected.OwnerUID || resource.OwnerGID != expected.OwnerGID {
			return installerError("recorded resource differs from accepted plan: "+resource.ID, nil)
		}
		if expected.RequiresSudo {
			if resource.Stage != StageHostApply {
				return installerError("recorded privileged resource has an invalid stage: "+resource.ID, nil)
			}
			continue
		}
		info, err := os.Lstat(resource.Path)
		if err != nil {
			return installerError("recorded resource is missing: "+resource.ID, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !resourceKindMatches(resource.Kind, info.Mode()) || info.Mode().Perm() != os.FileMode(resource.Mode) {
			return installerError("recorded resource kind or mode changed: "+resource.ID, nil)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int64(stat.Uid) != resource.OwnerUID || int64(stat.Gid) != resource.OwnerGID {
			return installerError("recorded resource ownership changed: "+resource.ID, nil)
		}
		if resource.ID == "workspace" {
			hostApply, found := completedStage(receipt, StageHostApply)
			if !found || hostApply.Evidence["workspaceDeviceIdentity"] == "" || filesystemDeviceIdentity(stat) != hostApply.Evidence["workspaceDeviceIdentity"] {
				return installerError("recorded Workspace device identity changed", nil)
			}
		}
		if resource.Kind == ResourceFilesystemImage && info.Size() != plan.Storage.ImageSizeBytes {
			return installerError("recorded filesystem image size changed", nil)
		}
		if (resource.Kind == ResourceFile || resource.Kind == ResourceBinary || resource.Kind == ResourceMountUnit) && resource.Digest == "" {
			return installerError("recorded regular resource lacks content identity: "+resource.ID, nil)
		}
		if resource.Digest != "" && info.Mode().IsRegular() {
			actual, err := fileSHA256(resource.Path)
			if err != nil {
				return err
			}
			if "sha256:"+actual != resource.Digest {
				return installerError("recorded resource digest changed: "+resource.ID, nil)
			}
		}
	}
	if record, found := completedStage(receipt, StageDeploymentMaterialized); found {
		manifest, ok := planned["manifest"]
		if !ok || record.Evidence["manifestDigest"] == "" {
			return installerError("deployment manifest postcondition evidence is absent", nil)
		}
		content, err := os.ReadFile(manifest.Path)
		if err != nil {
			return err
		}
		if Digest(content) != record.Evidence["manifestDigest"] {
			return installerError("deployment manifest digest changed", nil)
		}
	}
	if record, found := completedStage(receipt, StageRunnerEnrolled); found {
		expectedID := "runner-" + strings.TrimPrefix(plan.OperationID, "install_")
		identity, ok := planned["runner-identity"]
		if !ok || record.Evidence["runnerId"] != expectedID || record.Evidence["identity"] != identity.Path {
			return installerError("Runner enrollment identity changed", nil)
		}
	}
	return nil
}

// ValidatePlannedPath proves one live filesystem object still has the exact
// kind, mode, and ownership accepted in the immutable install plan.
func ValidatePlannedPath(expected PlannedPath) error {
	info, err := os.Lstat(expected.Path)
	if err != nil {
		return installerError("planned resource is missing: "+expected.Name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !resourceKindMatches(expected.Kind, info.Mode()) || info.Mode().Perm() != os.FileMode(expected.Mode) {
		return installerError("planned resource kind or mode changed: "+expected.Name, nil)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != expected.OwnerUID || int64(stat.Gid) != expected.OwnerGID {
		return installerError("planned resource ownership changed: "+expected.Name, nil)
	}
	return nil
}

func resourceKindMatches(kind ResourceKind, mode os.FileMode) bool {
	switch kind {
	case ResourceDirectory:
		return mode.IsDir()
	case ResourceFile, ResourceBinary, ResourceFilesystemImage, ResourceMountUnit:
		return mode.IsRegular()
	default:
		return false
	}
}

func completedStage(receipt InstallReceipt, stage Stage) (StageRecord, bool) {
	for _, record := range receipt.CompletedStages {
		if record.Stage == stage {
			return record, true
		}
	}
	return StageRecord{}, false
}

func ValidateComposeProjectEvidence(receipt InstallReceipt, actual string) error {
	record, found := completedStage(receipt, StageComposeStarted)
	if !found {
		return nil
	}
	if expected := record.Evidence["composeProject"]; expected == "" || actual != expected {
		return fmt.Errorf("SecondBox installer: Compose project identity changed: recorded %q, actual %q", expected, actual)
	}
	return nil
}
