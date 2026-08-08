package install

import (
	"fmt"
	"slices"
	"time"
)

func NewReceipt(plan InstallPlan, now time.Time) (InstallReceipt, error) {
	digest, err := PlanDigest(plan)
	if err != nil {
		return InstallReceipt{}, err
	}
	receipt := InstallReceipt{SchemaVersion: ReceiptSchema, OperationID: plan.OperationID, PlanDigest: digest, HostIdentity: plan.HostFacts.HostIdentity, Status: OperationPlanned, CompletedStages: []StageRecord{}, CreatedResources: []CreatedResource{}, PendingResourceIDs: []string{}, RemovedResourceIDs: []string{}, CompletedPurgeSteps: []string{}, UpdatedAt: now.UTC()}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return InstallReceipt{}, err
	}
	return receipt, nil
}

func (receipt *InstallReceipt) CompleteStage(stage Stage, now time.Time, evidence map[string]string) error {
	index := slices.Index(StageSequence, stage)
	if index < 0 {
		return installerError("stage is invalid", nil)
	}
	next := 0
	if len(receipt.CompletedStages) > 0 {
		next = slices.Index(StageSequence, receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage) + 1
	}
	if index != next {
		return installerError(fmt.Sprintf("stage %s cannot follow completed stage sequence", stage), nil)
	}
	if evidence == nil {
		evidence = map[string]string{}
	}
	receipt.CompletedStages = append(receipt.CompletedStages, StageRecord{Stage: stage, CompletedAt: now.UTC(), Evidence: evidence})
	receipt.Status = OperationRunning
	receipt.FailureClass = ""
	receipt.FailureStage = ""
	receipt.UpdatedAt = now.UTC()
	if stage == StageSmokeExecution {
		receipt.Status = OperationSucceeded
	}
	return nil
}

// Fail records a resumable stage failure without inventing a completed stage.
func (receipt *InstallReceipt) Fail(stage Stage, class FailureClass, now time.Time) error {
	if slices.Index(StageSequence, stage) < 0 {
		return installerError("failure stage is invalid", nil)
	}
	valid := map[FailureClass]bool{FailureBlocked: true, FailureNeedsAction: true, FailureRetryable: true, FailureInternal: true}
	if !valid[class] {
		return installerError("failure class is invalid", nil)
	}
	receipt.Status = OperationFailed
	receipt.FailureClass = class
	receipt.FailureStage = stage
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) MarkUninstalling(now time.Time) error {
	if receipt.Status != OperationSucceeded || len(receipt.CompletedStages) == 0 || receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage != StageSmokeExecution {
		return installerError("only a successful complete installation can begin uninstall", nil)
	}
	receipt.Status = OperationUninstalling
	receipt.FailureClass, receipt.FailureStage = "", ""
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) MarkUninstalled(now time.Time) error {
	if receipt.Status != OperationUninstalling || len(receipt.CompletedStages) == 0 || receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage != StageSmokeExecution {
		return installerError("only a journaled complete installation can finish uninstall", nil)
	}
	receipt.Status = OperationUninstalled
	receipt.FailureClass, receipt.FailureStage = "", ""
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) RestoreSucceeded(now time.Time) error {
	if receipt.Status != OperationUninstalled || len(receipt.CompletedStages) == 0 || receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage != StageSmokeExecution {
		return installerError("only an uninstalled complete operation can be restored", nil)
	}
	receipt.Status = OperationSucceeded
	receipt.RemovedResourceIDs = slices.DeleteFunc(receipt.RemovedResourceIDs, func(id string) bool { return id == "compose-project" })
	receipt.UpdatedAt = now.UTC()
	return nil
}

// RecoverSucceeded clears a transient post-install health failure after the
// complete installation has passed readiness again. It does not invent or
// repeat any completed stage.
func (receipt *InstallReceipt) RecoverSucceeded(now time.Time) error {
	if receipt.Status != OperationFailed || len(receipt.CompletedStages) == 0 || receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage != StageSmokeExecution {
		return installerError("only a failed complete operation can recover to succeeded", nil)
	}
	receipt.Status = OperationSucceeded
	receipt.FailureClass, receipt.FailureStage = "", ""
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) AppendResource(resource CreatedResource) error {
	for _, existing := range receipt.CreatedResources {
		if existing.ID == resource.ID {
			return installerError("created resource ID is duplicated", nil)
		}
	}
	if slices.Contains(receipt.RemovedResourceIDs, resource.ID) {
		return installerError("created resource ID was already removed", nil)
	}
	if slices.Index(StageSequence, resource.Stage) < 0 {
		return installerError("created resource stage is invalid", nil)
	}
	receipt.CreatedResources = append(receipt.CreatedResources, resource)
	return nil
}

func (receipt *InstallReceipt) BeginResource(id string, now time.Time) error {
	if id == "" || slices.Contains(receipt.RemovedResourceIDs, id) || slices.ContainsFunc(receipt.CreatedResources, func(resource CreatedResource) bool { return resource.ID == id }) {
		return installerError("resource mutation intent is invalid", nil)
	}
	if !slices.Contains(receipt.PendingResourceIDs, id) {
		receipt.PendingResourceIDs = append(receipt.PendingResourceIDs, id)
		slices.Sort(receipt.PendingResourceIDs)
	}
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) CompleteResource(resource CreatedResource, now time.Time) error {
	if !slices.Contains(receipt.PendingResourceIDs, resource.ID) {
		return installerError("resource mutation completion lacks durable intent", nil)
	}
	if err := receipt.AppendResource(resource); err != nil {
		return err
	}
	receipt.PendingResourceIDs = slices.DeleteFunc(receipt.PendingResourceIDs, func(id string) bool { return id == resource.ID })
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) MarkResourceRemoved(id string, now time.Time) error {
	if slices.Contains(receipt.RemovedResourceIDs, id) {
		return nil
	}
	if !slices.ContainsFunc(receipt.CreatedResources, func(resource CreatedResource) bool { return resource.ID == id }) {
		return installerError("removed resource is absent from the created-resource ledger", nil)
	}
	receipt.RemovedResourceIDs = append(receipt.RemovedResourceIDs, id)
	slices.Sort(receipt.RemovedResourceIDs)
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) MarkPurged(now time.Time) error {
	if receipt.Status != OperationPurging {
		return installerError("purge completion requires a purge-in-progress receipt", nil)
	}
	for _, resource := range receipt.CreatedResources {
		if resource.ID != "operation-directory" && !slices.Contains(receipt.RemovedResourceIDs, resource.ID) {
			return installerError("purge is incomplete for resource "+resource.ID, nil)
		}
	}
	if !slices.Contains(receipt.CompletedPurgeSteps, "compose-volumes") {
		return installerError("purge is incomplete for Compose durable volumes", nil)
	}
	receipt.Status = OperationPurged
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) CompletePurgeStep(step string, now time.Time) error {
	if receipt.Status != OperationPurging || step != "compose-volumes" {
		return installerError("purge step is invalid for the current operation", nil)
	}
	if !slices.Contains(receipt.CompletedPurgeSteps, step) {
		receipt.CompletedPurgeSteps = append(receipt.CompletedPurgeSteps, step)
		slices.Sort(receipt.CompletedPurgeSteps)
	}
	receipt.UpdatedAt = now.UTC()
	return nil
}

func (receipt *InstallReceipt) MarkPurging(now time.Time) error {
	if receipt.Status != OperationUninstalled {
		return installerError("purge requires an ordinarily uninstalled deployment", nil)
	}
	receipt.Status = OperationPurging
	receipt.UpdatedAt = now.UTC()
	return nil
}
