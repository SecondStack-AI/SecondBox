package install

import (
	"fmt"
	"slices"
	"time"
)

func (receipt *InstallReceipt) BeginUpdate(id string, source, target ReleasePlan, now time.Time) error {
	if receipt.Status != OperationSucceeded {
		return installerError("update requires a successful complete installation", nil)
	}
	if !updatePattern.MatchString(id) {
		return installerError("update identity is invalid", nil)
	}
	if err := validateReleasePlan(target); err != nil {
		return err
	}
	if err := validateReleasePlan(source); err != nil {
		return err
	}
	if source.ArtifactManifestDigest == target.ArtifactManifestDigest {
		return installerError("update target is already active", nil)
	}
	if len(receipt.Updates) > 0 && receipt.Updates[len(receipt.Updates)-1].Status != UpdateSucceeded {
		return installerError("another update is incomplete", nil)
	}
	if slices.ContainsFunc(receipt.Updates, func(update UpdateRecord) bool { return update.ID == id }) {
		return installerError("update identity is duplicated", nil)
	}
	started := now.UTC()
	receipt.Updates = append(receipt.Updates, UpdateRecord{ID: id, SourceRelease: source, TargetRelease: target, Status: UpdateRunning, CompletedStages: []UpdateStageRecord{}, StartedAt: started, UpdatedAt: started})
	receipt.UpdatedAt = started
	return nil
}

func (receipt *InstallReceipt) CompleteUpdateStage(stage UpdateStage, now time.Time, evidence map[string]string) error {
	update, err := receipt.activeUpdate()
	if err != nil {
		return err
	}
	index := slices.Index(UpdateStageSequence, stage)
	next := len(update.CompletedStages)
	if index < 0 || index != next {
		return installerError(fmt.Sprintf("update stage %s cannot follow completed update stage sequence", stage), nil)
	}
	if evidence == nil {
		evidence = map[string]string{}
	}
	completed := now.UTC()
	update.CompletedStages = append(update.CompletedStages, UpdateStageRecord{Stage: stage, CompletedAt: completed, Evidence: evidence})
	update.Status = UpdateRunning
	update.FailureClass, update.FailureStage = "", ""
	update.UpdatedAt = completed
	receipt.UpdatedAt = completed
	return nil
}

func (receipt *InstallReceipt) FailUpdate(stage UpdateStage, class FailureClass, now time.Time) error {
	update, err := receipt.activeUpdate()
	if err != nil {
		return err
	}
	if slices.Index(UpdateStageSequence, stage) < 0 {
		return installerError("update failure stage is invalid", nil)
	}
	valid := map[FailureClass]bool{FailureBlocked: true, FailureNeedsAction: true, FailureRetryable: true, FailureInternal: true}
	if !valid[class] {
		return installerError("update failure class is invalid", nil)
	}
	failed := now.UTC()
	update.Status = UpdateFailed
	update.FailureClass = class
	update.FailureStage = stage
	update.UpdatedAt = failed
	receipt.UpdatedAt = failed
	return nil
}

func (receipt *InstallReceipt) ActivateUpdate(plan *InstallPlan, now time.Time) error {
	update, err := receipt.activeUpdate()
	if err != nil {
		return err
	}
	if len(update.CompletedStages) != len(UpdateStageSequence) || update.CompletedStages[len(update.CompletedStages)-1].Stage != UpdateStageSmokeExecution {
		return installerError("update activation requires every update stage", nil)
	}
	activated := now.UTC()
	if !activated.After(plan.ReleaseHistory[len(plan.ReleaseHistory)-1].ActivatedAt) {
		return installerError("update activation time must follow the active release", nil)
	}
	plan.Release = update.TargetRelease
	plan.ReleaseHistory = append(plan.ReleaseHistory, ReleaseActivation{Release: update.TargetRelease, ActivatedAt: activated, UpdateID: update.ID})
	update.Status = UpdateSucceeded
	update.FailureClass, update.FailureStage = "", ""
	update.UpdatedAt = activated
	receipt.UpdatedAt = activated
	digest, err := PlanDigest(*plan)
	if err != nil {
		return err
	}
	receipt.PlanDigest = digest
	return nil
}

func (receipt *InstallReceipt) ActiveUpdate() (UpdateRecord, bool) {
	if len(receipt.Updates) == 0 || receipt.Updates[len(receipt.Updates)-1].Status == UpdateSucceeded {
		return UpdateRecord{}, false
	}
	return receipt.Updates[len(receipt.Updates)-1], true
}

func (receipt *InstallReceipt) RefreshUpdatedResource(id, digest string) error {
	if !digestPattern.MatchString(digest) {
		return installerError("updated resource digest is invalid", nil)
	}
	for index := range receipt.CreatedResources {
		if receipt.CreatedResources[index].ID == id {
			if receipt.CreatedResources[index].Kind != ResourceFile && receipt.CreatedResources[index].Kind != ResourceBinary && id != "artifacts" {
				return installerError("updated resource does not carry content identity: "+id, nil)
			}
			receipt.CreatedResources[index].Digest = digest
			return nil
		}
	}
	return installerError("updated resource is absent from installation ledger: "+id, nil)
}

func FileDigest(path string) (string, error) {
	digest, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	return "sha256:" + digest, nil
}

func (receipt *InstallReceipt) activeUpdate() (*UpdateRecord, error) {
	if len(receipt.Updates) == 0 {
		return nil, installerError("active update is absent", nil)
	}
	update := &receipt.Updates[len(receipt.Updates)-1]
	if update.Status != UpdateRunning && update.Status != UpdateFailed {
		return nil, installerError("active update is already complete", nil)
	}
	return update, nil
}
