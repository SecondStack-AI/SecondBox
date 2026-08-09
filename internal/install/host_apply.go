package install

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

type HostApplyExecutor interface {
	EffectiveUID() int
	Revalidate(context.Context, InstallPlan, InstallReceipt) error
	CreateDirectory(PlannedPath) error
	AllocateFilesystemImage(PlannedPath, int64) error
	FormatBtrfs(context.Context, string, string) error
	WriteMountUnit(PlannedPath, string) error
	EnableMountUnit(context.Context, string) error
	SecureDirectory(PlannedPath) error
	ProveReflinkTopology(string, string, string) (string, error)
	RemoveEmpty(CreatedResource) (bool, error)
}

type HostApplyDependencies struct {
	Executor       HostApplyExecutor
	PersistReceipt func(InstallReceipt) error
	Now            func() time.Time
}

func ApplyAcceptedHost(ctx context.Context, directory, expectedDigest string, callerUID int, executor HostApplyExecutor, now func() time.Time) (result InstallReceipt, resultErr error) {
	lock, err := AcquireLock(directory)
	if err != nil {
		return InstallReceipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	plan, receipt, err := ReadHostApply(directory, expectedDigest, callerUID)
	if err != nil {
		return InstallReceipt{}, err
	}
	return ApplyHost(ctx, plan, receipt, HostApplyDependencies{Executor: executor, Now: now, PersistReceipt: func(updated InstallReceipt) error {
		digest, err := PlanDigest(plan)
		if err != nil {
			return err
		}
		if err := updated.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
			return err
		}
		return writeReceiptAtomic(directory, updated, callerUID)
	}})
}

func ApplyHost(ctx context.Context, plan InstallPlan, receipt InstallReceipt, dependencies HostApplyDependencies) (InstallReceipt, error) {
	if dependencies.Executor == nil || dependencies.PersistReceipt == nil || dependencies.Now == nil {
		return receipt, installerError("host apply dependencies are incomplete", nil)
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return receipt, err
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return receipt, err
	}
	if dependencies.Executor.EffectiveUID() != 0 {
		return receipt, installerError("private host apply must run as root through sudo", nil)
	}
	_, hostApplyComplete := completedStage(receipt, StageHostApply)
	if !hostApplyComplete && (len(receipt.CompletedStages) != 2 || receipt.CompletedStages[1].Stage != StagePlanAccepted) {
		return receipt, installerError("host apply requires an accepted, not-yet-applied plan", nil)
	}
	if err := dependencies.Executor.Revalidate(ctx, plan, receipt); err != nil {
		return receipt, installerError("root prerequisite revalidation", err)
	}
	if hostApplyComplete {
		// A completed host-apply replay is a privileged postcondition check,
		// not permission to mutate the accepted host paths again.
		return receipt, nil
	}
	createdThisAttempt := []CreatedResource{}
	fail := func(problem error) (InstallReceipt, error) {
		removed, rollbackErr := rollbackEmpty(dependencies.Executor, createdThisAttempt)
		if len(removed) > 0 {
			receipt.CreatedResources = slices.DeleteFunc(receipt.CreatedResources, func(resource CreatedResource) bool { return removed[resource.ID] })
		}
		if len(createdThisAttempt) > 0 {
			// Resources that became non-empty cannot be rolled back safely. Keep
			// them in the durable ledger so a retry can validate and adopt them.
			rollbackErr = errors.Join(rollbackErr, dependencies.PersistReceipt(receipt))
		}
		return receipt, errors.Join(problem, rollbackErr)
	}
	materialize := func(resource CreatedResource, action func() error) error {
		if !slices.Contains(receipt.PendingResourceIDs, resource.ID) {
			if err := receipt.BeginResource(resource.ID, dependencies.Now().UTC()); err != nil {
				return err
			}
			if err := dependencies.PersistReceipt(receipt); err != nil {
				return err
			}
		}
		if err := action(); err != nil {
			return err
		}
		createdThisAttempt = append(createdThisAttempt, resource)
		if err := receipt.CompleteResource(resource, dependencies.Now().UTC()); err != nil {
			return err
		}
		return dependencies.PersistReceipt(receipt)
	}
	recorded := func(path PlannedPath) (bool, error) {
		resource, found := receiptResource(receipt, path.Name)
		if !found {
			return false, nil
		}
		if resource.Path != path.Path || resource.Kind != path.Kind || resource.Class != path.Class || resource.Mode != path.Mode || resource.OwnerUID != path.OwnerUID || resource.OwnerGID != path.OwnerGID || resource.Stage != StageHostApply {
			return false, installerError("recorded host resource differs from accepted plan: "+path.Name, nil)
		}
		return true, nil
	}
	createDirectory := func(path PlannedPath) error {
		already, err := recorded(path)
		if err != nil {
			return err
		}
		if already {
			return nil
		}
		resource := resourceFromPath(path, StageHostApply)
		if err := materialize(resource, func() error { return dependencies.Executor.CreateDirectory(path) }); err != nil {
			return installerError("create privileged directory "+path.Name, err)
		}
		return nil
	}
	for _, path := range plan.Paths {
		if !path.RequiresSudo || !path.Create || path.Kind != ResourceDirectory || (path.Name != "runner-root" && path.Name != "runner-storage") {
			continue
		}
		if err := createDirectory(path); err != nil {
			return fail(err)
		}
	}
	workspace, found := plannedPathByName(plan.Paths, "workspace")
	if !found {
		return fail(installerError("workspace resource is absent from plan", nil))
	}
	if plan.Storage.Choice == StorageBtrfsImage {
		image, found := plannedPathByName(plan.Paths, "filesystem-image")
		if !found {
			return fail(installerError("filesystem image resource is absent from plan", nil))
		}
		imageRecorded, err := recorded(image)
		if err != nil {
			return fail(err)
		}
		if !imageRecorded {
			if err := materialize(resourceFromPath(image, StageHostApply), func() error { return dependencies.Executor.AllocateFilesystemImage(image, plan.Storage.ImageSizeBytes) }); err != nil {
				return fail(installerError("fully allocate filesystem image", err))
			}
		}
		unit, found := plannedPathByName(plan.Paths, "workspace-mount-unit")
		if !found {
			return fail(installerError("workspace mount unit resource is absent from plan", nil))
		}
		unitRecorded, err := recorded(unit)
		if err != nil {
			return fail(err)
		}
		if !unitRecorded {
			if err := dependencies.Executor.FormatBtrfs(ctx, image.Path, plan.Release.Images["installer-tools"]); err != nil {
				return fail(installerError("format filesystem image with release-pinned installer tools", err))
			}
		}
	}
	runnerStorage, found := plannedPathByName(plan.Paths, "runner-storage")
	if !found {
		return fail(installerError("runner storage resource is absent from plan", nil))
	}
	if plan.Storage.Choice == StorageBtrfsImage {
		unit, found := plannedPathByName(plan.Paths, "workspace-mount-unit")
		if !found {
			return fail(installerError("workspace mount unit resource is absent from plan", nil))
		}
		content := MountUnit(plan.Storage.FilesystemImagePath, runnerStorage.Path)
		unitRecorded, err := recorded(unit)
		if err != nil {
			return fail(err)
		}
		if !unitRecorded {
			resource := resourceFromPath(unit, StageHostApply)
			resource.Digest = Digest([]byte(content))
			if err := materialize(resource, func() error { return dependencies.Executor.WriteMountUnit(unit, content) }); err != nil {
				return fail(installerError("validate and install workspace mount unit", err))
			}
		}
		if err := dependencies.Executor.EnableMountUnit(ctx, unit.Path); err != nil {
			return fail(installerError("enable and start workspace mount unit", err))
		}
	}
	if err := dependencies.Executor.SecureDirectory(runnerStorage); err != nil {
		return fail(installerError("secure runner storage root", err))
	}
	for _, path := range plan.Paths {
		if !path.RequiresSudo || !path.Create || path.Kind != ResourceDirectory || path.Name == "runner-root" || path.Name == "runner-storage" {
			continue
		}
		if err := createDirectory(path); err != nil {
			return fail(err)
		}
	}
	artifactParent, artifactParentFound := plannedPathByName(plan.Paths, "artifacts-parent")
	run, runFound := plannedPathByName(plan.Paths, "run")
	if !artifactParentFound || !runFound {
		return fail(installerError("runner reflink topology resources are absent from plan", nil))
	}
	identity, err := dependencies.Executor.ProveReflinkTopology(artifactParent.Path, run.Path, workspace.Path)
	if err != nil {
		return fail(installerError("runner asset-to-run FICLONE and Workspace identity proof", err))
	}
	beforeStageCompletion := receipt
	beforeStageCompletion.CompletedStages = slices.Clone(receipt.CompletedStages)
	beforeStageCompletion.CreatedResources = slices.Clone(receipt.CreatedResources)
	beforeStageCompletion.PendingResourceIDs = slices.Clone(receipt.PendingResourceIDs)
	beforeStageCompletion.RemovedResourceIDs = slices.Clone(receipt.RemovedResourceIDs)
	beforeStageCompletion.CompletedPurgeSteps = slices.Clone(receipt.CompletedPurgeSteps)
	if err := receipt.CompleteStage(StageHostApply, dependencies.Now().UTC(), map[string]string{"workspaceDeviceIdentity": identity, "runnerStorageDeviceIdentity": identity, "reflinkMutationIsolation": "passed", "reflinkTopology": "artifacts-to-run-and-workspace"}); err != nil {
		return fail(err)
	}
	if err := dependencies.PersistReceipt(receipt); err != nil {
		// CompleteStage mutates the receipt before persistence. Restore the
		// pre-stage snapshot so failure handling cannot accidentally persist a
		// completed host-apply stage that never reached durable storage.
		receipt = beforeStageCompletion
		return fail(err)
	}
	return receipt, nil
}

func plannedPathByName(paths []PlannedPath, name string) (PlannedPath, bool) {
	index := slices.IndexFunc(paths, func(path PlannedPath) bool { return path.Name == name })
	if index < 0 {
		return PlannedPath{}, false
	}
	return paths[index], true
}

func resourceFromPath(path PlannedPath, stage Stage) CreatedResource {
	return CreatedResource{ID: path.Name, Kind: path.Kind, Path: path.Path, Class: path.Class, Stage: stage, Mode: path.Mode, OwnerUID: path.OwnerUID, OwnerGID: path.OwnerGID}
}

func rollbackEmpty(executor HostApplyExecutor, resources []CreatedResource) (map[string]bool, error) {
	removedResources := map[string]bool{}
	var result error
	for index := len(resources) - 1; index >= 0; index-- {
		removed, err := executor.RemoveEmpty(resources[index])
		if err != nil {
			result = errors.Join(result, fmt.Errorf("SecondBox installer rollback empty %s: %w", resources[index].ID, err))
			continue
		}
		if removed {
			removedResources[resources[index].ID] = true
		}
	}
	return removedResources, result
}

func MountUnit(imagePath, storagePath string) string {
	return "[Unit]\nDescription=SecondBox reflink-capable Runner storage\nAfter=local-fs-pre.target\nBefore=docker.service\n\n[Mount]\nWhat=" + imagePath + "\nWhere=" + storagePath + "\nType=btrfs\nOptions=loop,nodev,nosuid,noexec\nTimeoutSec=120\n\n[Install]\nWantedBy=local-fs.target\n"
}
