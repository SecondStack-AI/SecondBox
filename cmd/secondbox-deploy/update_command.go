package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

// Guided updates begin at the customer-shared-tenancy clean-install boundary.
const minimumGuidedUpdateSourceVersion = "0.6.0"

type updateDependencies struct {
	OwnerUID      int
	Now           func() time.Time
	HostVerify    func(context.Context, string, string) error
	VerifyRelease func(context.Context, string) (releaseverify.VerifiedRelease, error)
	VerifySource  func(context.Context, string) (releaseverify.VerifiedRelease, error)
	CheckCapacity func(install.InstallPlan, install.ReleasePlan) error
	Materializer  install.ReleaseMaterializeExecutor
	Compose       func(context.Context, string, string) error
	SourceCompose func(context.Context, string, string, string) error
	SourceSubject func(context.Context, install.InstallPlan) (string, error)
	Readiness     func(context.Context, install.InstallPlan) (map[string]string, error)
	Smoke         func(context.Context, install.InstallPlan) (map[string]string, error)
	Quiescent     func(context.Context, install.InstallPlan, string) ([]string, error)
	Confirm       func(context.Context, string) error
	NewUpdateID   func() (string, error)
	SaveReceipt   func(string, install.InstallPlan, install.InstallReceipt, int) error
	SaveOperation func(string, install.InstallPlan, install.InstallReceipt, int) error
	TargetVersion string
}

func systemUpdateDependencies(renderer cliui.Renderer) updateDependencies {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return updateDependencies{
		OwnerUID: os.Getuid(), Now: time.Now, TargetVersion: buildinfo.Version,
		HostVerify: runPrivilegedHostUpdateVerify,
		VerifyRelease: func(ctx context.Context, location string) (releaseverify.VerifiedRelease, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			return verifyReleaseLocationWithContext(ctx, location)
		},
		VerifySource: func(ctx context.Context, location string) (releaseverify.VerifiedRelease, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			return releaseverify.RecordedArtifactManifest(ctx, location, releaseverify.HTTPFetcher(httpClient))
		},
		CheckCapacity: install.ValidateUpdateStagingCapacity,
		Materializer:  install.SystemReleaseMaterializer{Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic, HTTPClient: httpClient},
		Compose: func(ctx context.Context, manifestPath, action string) error {
			return deployconfig.RunComposeForAcceptedInstaller(ctx, manifestPath, action, deployconfig.SystemComposeExecutor{Input: os.Stdin, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic}, httpClient)
		},
		SourceCompose: func(ctx context.Context, manifestPath, action, subject string) error {
			return deployconfig.RunExistingComposeForAcceptedInstaller(ctx, manifestPath, action, subject, deployconfig.SystemComposeExecutor{Input: os.Stdin, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic})
		},
		SourceSubject: authenticatedSourceComposeSubject,
		Readiness:     waitForInstalledRunner,
		Smoke:         runInstalledUpdateSmoke,
		Quiescent:     installedDeploymentSandboxesRequiringStop,
		Confirm: func(ctx context.Context, review string) error {
			accepted := false
			handles := cliui.FormHandles{Input: os.Stdin, Output: renderer.Diagnostic, Width: renderer.Capabilities.Diagnostic.Width, Accessible: renderer.Capabilities.Accessible, Dark: renderer.Capabilities.Diagnostic.Background != cliui.BackgroundLight}
			if err := cliui.FinalUpdateConfirmationForm(review, &accepted).Run(ctx, handles); err != nil {
				return installerFormError(err)
			}
			if !accepted {
				return errors.New("SecondBox installer update: release activation was not accepted")
			}
			return nil
		},
		NewUpdateID: install.NewUpdateID, SaveReceipt: install.SaveReceipt, SaveOperation: install.SaveOperation,
	}
}

func authenticatedSourceComposeSubject(ctx context.Context, plan install.InstallPlan) (subject string, resultErr error) {
	temporary, err := os.MkdirTemp("", "secondbox-source-compose-")
	if err != nil {
		return "", fmt.Errorf("SecondBox installer update create source Compose authentication directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temporary)) }()
	environmentPath := filepath.Join(temporary, ".secondbox.generated.env")
	manifestPath := installerPlannedPath(plan, "manifest")
	binaryPath := installerPlannedPath(plan, "secondbox-deploy-binary")
	stdout, stderr := &boundedCommandBuffer{maximum: 1 << 20}, &boundedCommandBuffer{maximum: 1 << 20}
	command := exec.CommandContext(ctx, binaryPath, "--output", "plain", "render", "--output", environmentPath, manifestPath)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("SecondBox installer update render verified source Compose assets: %w: %s", err, cliui.Sanitize(stderr.String()))
	}
	if stdout.tooLong || stderr.tooLong {
		return "", errors.New("SecondBox installer update source Compose render exceeded the bounded output limit")
	}
	expected, err := deployconfig.RecordedInstallerComposeSubject(manifestPath, environmentPath)
	if err != nil {
		return "", err
	}
	actual, err := deployconfig.RecordedInstallerComposeSubject(manifestPath, "")
	if err != nil {
		return "", err
	}
	if actual != expected {
		return "", errors.New("SecondBox installer update: recorded Compose assets differ from the verified source release")
	}
	return expected, nil
}

func runUpdateCommand(ctx context.Context, arguments []string, renderer cliui.Renderer) error {
	dependencies := systemUpdateDependencies(renderer)
	if len(arguments) >= 3 && arguments[len(arguments)-2] == "--candidate-directory" {
		candidateDirectory := arguments[len(arguments)-1]
		arguments = arguments[:len(arguments)-2]
		publicVerify := dependencies.VerifyRelease
		dependencies.VerifyRelease = func(ctx context.Context, location string) (releaseverify.VerifiedRelease, error) {
			if location == releasecontract.ArtifactManifestLocation(dependencies.TargetVersion) {
				return verifyCandidateDirectory(ctx, candidateDirectory)
			}
			return publicVerify(ctx, location)
		}
		dependencies.Materializer = install.CandidateReleaseMaterializer{Directory: candidateDirectory, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic}
	}
	check, resume, directory, err := parseUpdateArguments(arguments)
	if err != nil {
		return usage(renderer)
	}
	if !check && renderer.OutputMode == cliui.OutputJSON {
		return &deployExitError{code: 3, err: errors.New("SecondBox installer update: activation does not accept --output json; use update --check")}
	}
	if !check && !resume && !renderer.Capabilities.Input.TTY && !renderer.Capabilities.Accessible {
		return &deployExitError{code: 3, err: errors.New("SecondBox installer update: activation requires a terminal or --accessible")}
	}
	return runUpdateWith(ctx, directory, check, resume, renderer, dependencies)
}

func parseUpdateArguments(arguments []string) (check, resume bool, directory string, err error) {
	switch {
	case len(arguments) == 1 && !strings.HasPrefix(arguments[0], "-"):
		return false, false, arguments[0], nil
	case len(arguments) == 2 && arguments[0] == "--check":
		return true, false, arguments[1], nil
	case len(arguments) == 2 && arguments[0] == "--resume":
		return false, true, arguments[1], nil
	default:
		return false, false, "", errors.New("invalid update arguments")
	}
}

func runUpdateWith(ctx context.Context, directory string, check, resume bool, renderer cliui.Renderer, dependencies updateDependencies) (resultErr error) {
	if dependencies.Now == nil || dependencies.HostVerify == nil || dependencies.VerifyRelease == nil || dependencies.VerifySource == nil || dependencies.CheckCapacity == nil || dependencies.Materializer == nil || dependencies.Compose == nil || dependencies.Readiness == nil || dependencies.Smoke == nil || dependencies.Quiescent == nil || dependencies.Confirm == nil || dependencies.NewUpdateID == nil || dependencies.SaveReceipt == nil || dependencies.SaveOperation == nil {
		return errors.New("SecondBox installer update: dependencies are incomplete")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if dependencies.TargetVersion == "0.0.0-development" || dependencies.TargetVersion == "development" {
		return errors.New("SecondBox installer update: a published qualified target-release binary is required")
	}
	sourceVersion, err := install.InspectOperationReleaseVersionReadOnly(absolute, dependencies.OwnerUID)
	if err != nil {
		return err
	}
	if err := validateGuidedUpdateSourceVersion(sourceVersion); err != nil {
		return err
	}
	var targetVerified releaseverify.VerifiedRelease
	var targetPlan install.ReleasePlan
	if !resume {
		targetLocation := releasecontract.ArtifactManifestLocation(dependencies.TargetVersion)
		targetVerified, err = dependencies.VerifyRelease(ctx, targetLocation)
		if err != nil {
			return err
		}
		targetPlan = releasePlan(targetVerified, targetLocation)
	}
	if check {
		verifiedPlan, _, err := install.ReadOperationReadOnly(absolute, dependencies.OwnerUID)
		if err != nil {
			return err
		}
		verifiedPlanDigest, err := install.PlanDigest(verifiedPlan)
		if err != nil {
			return err
		}
		if err := dependencies.HostVerify(ctx, absolute, verifiedPlanDigest); err != nil {
			return err
		}
		return runUpdateCheck(ctx, absolute, verifiedPlanDigest, targetPlan, targetVerified, renderer, dependencies)
	}
	verificationLock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	verifiedPlan, _, err := install.RecoverOperation(absolute, dependencies.OwnerUID, verificationLock)
	if closeErr := verificationLock.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return err
	}
	verifiedPlanDigest, err := install.PlanDigest(verifiedPlan)
	if err != nil {
		return err
	}
	if err := dependencies.HostVerify(ctx, absolute, verifiedPlanDigest); err != nil {
		return err
	}
	lock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	plan, receipt, err := install.RecoverOperation(absolute, dependencies.OwnerUID, lock)
	if err != nil {
		return err
	}
	lockedPlanDigest, err := install.PlanDigest(plan)
	if err != nil {
		return err
	}
	if lockedPlanDigest != verifiedPlanDigest {
		return errors.New("SecondBox installer update: accepted operation changed after privileged-resource verification")
	}
	active, hasActive := receipt.ActiveUpdate()
	if resume {
		targetVerified, targetPlan, err = verifyRecordedResumeTarget(ctx, receipt, dependencies)
		if err != nil {
			return err
		}
	}
	if hasActive {
		if receipt.Status != install.OperationSucceeded {
			return errors.New("SecondBox installer update: incomplete update requires a successful installation receipt")
		}
		if !resume {
			return errors.New("SecondBox installer update: an update is incomplete; resume it with update --resume " + absolute)
		}
		if !sameUpdateTarget(active.TargetRelease, targetPlan) {
			return fmt.Errorf("SecondBox installer update: running target %s differs from bootstrap target %s", active.TargetRelease.Version, targetPlan.Version)
		}
	} else {
		if resume {
			if completedUpdateMatchesTarget(receipt, targetPlan) && sameUpdateTarget(plan.Release, targetPlan) {
				return writeUpdateSuccess(renderer, absolute, plan)
			}
			return errors.New("SecondBox installer update: no incomplete update is available to resume")
		}
		if err := validateNewUpdate(ctx, plan, receipt, targetPlan, targetVerified, dependencies); err != nil {
			return err
		}
		review := fmt.Sprintf("Source release: %s\nTarget release: %s\nDeployment: %s\nCompose project and durable volumes are preserved. Every Sandbox must be stopped. Activation is forward-only after Compose stops.", plan.Release.Version, targetPlan.Version, absolute)
		if err := dependencies.Confirm(ctx, review); err != nil {
			return err
		}
		id, err := dependencies.NewUpdateID()
		if err != nil {
			return err
		}
		if err := receipt.BeginUpdate(id, plan.Release, targetPlan, dependencies.Now()); err != nil {
			return err
		}
		if err := receipt.CompleteUpdateStage(install.UpdateStagePreflight, dependencies.Now(), map[string]string{"sourceVersion": plan.Release.Version, "targetVersion": targetPlan.Version, "stoppedSandboxes": "verified"}); err != nil {
			return err
		}
		if err := dependencies.SaveReceipt(absolute, plan, receipt, dependencies.OwnerUID); err != nil {
			return err
		}
		active, _ = receipt.ActiveUpdate()
	}
	return continueUpdate(ctx, absolute, plan, receipt, active, targetVerified, renderer, dependencies)
}

func runUpdateCheck(ctx context.Context, directory, verifiedPlanDigest string, target install.ReleasePlan, verified releaseverify.VerifiedRelease, renderer cliui.Renderer, dependencies updateDependencies) error {
	plan, receipt, err := install.ReadOperationReadOnly(directory, dependencies.OwnerUID)
	if err != nil {
		return err
	}
	if _, active := receipt.ActiveUpdate(); active {
		return errors.New("SecondBox installer update check: an update is already incomplete; use update --resume")
	}
	currentPlanDigest, err := install.PlanDigest(plan)
	if err != nil {
		return err
	}
	if currentPlanDigest != verifiedPlanDigest {
		return errors.New("SecondBox installer update check: accepted operation changed after privileged-resource verification")
	}
	if err := validateNewUpdate(ctx, plan, receipt, target, verified, dependencies); err != nil {
		return err
	}
	values := map[string]string{"sourceVersion": plan.Release.Version, "targetVersion": target.Version, "deployment": directory, "status": "ready"}
	if renderer.OutputMode == cliui.OutputJSON {
		return json.NewEncoder(renderer.Output).Encode(values)
	}
	return renderer.WriteSummary(cliui.Summary{Title: "SecondBox update check", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Source release", Value: plan.Release.Version}, {Key: "Target release", Value: target.Version}, {Key: "Deployment", Value: directory}, {Key: "Sandboxes", Value: "all stopped"}}})
}

func verifyRecordedResumeTarget(ctx context.Context, receipt install.InstallReceipt, dependencies updateDependencies) (releaseverify.VerifiedRelease, install.ReleasePlan, error) {
	recorded, found := receipt.ActiveUpdate()
	if !found && len(receipt.Updates) != 0 && receipt.Updates[len(receipt.Updates)-1].Status == install.UpdateSucceeded {
		recorded, found = receipt.Updates[len(receipt.Updates)-1], true
	}
	if !found {
		return releaseverify.VerifiedRelease{}, install.ReleasePlan{}, errors.New("SecondBox installer update: no incomplete update is available to resume")
	}
	verified, err := dependencies.VerifyRelease(ctx, recorded.TargetRelease.ArtifactManifestURL)
	if err != nil {
		return releaseverify.VerifiedRelease{}, install.ReleasePlan{}, err
	}
	resolved := releasePlan(verified, recorded.TargetRelease.ArtifactManifestURL)
	if !sameUpdateTarget(recorded.TargetRelease, resolved) {
		return releaseverify.VerifiedRelease{}, install.ReleasePlan{}, errors.New("SecondBox installer update: journaled target differs from its verified public identity")
	}
	return verified, resolved, nil
}

func validateNewUpdate(ctx context.Context, plan install.InstallPlan, receipt install.InstallReceipt, target install.ReleasePlan, targetVerified releaseverify.VerifiedRelease, dependencies updateDependencies) error {
	if receipt.Status != install.OperationSucceeded || len(receipt.CompletedStages) != len(install.StageSequence) {
		return errors.New("SecondBox installer update: only a successful complete guided installation can be updated")
	}
	if err := validateGuidedUpdateSourceVersion(plan.Release.Version); err != nil {
		return err
	}
	comparison, err := releasecontract.CompareVersions(target.Version, plan.Release.Version)
	if err != nil {
		return err
	}
	if comparison <= 0 {
		return fmt.Errorf("SecondBox installer update: target release %s must be newer than active release %s", target.Version, plan.Release.Version)
	}
	if !sameUpdateTarget(target, releasePlan(targetVerified, target.ArtifactManifestURL)) {
		return errors.New("SecondBox installer update: target release verification changed")
	}
	sourceVerified, err := validateUpdateSource(ctx, plan, receipt, dependencies)
	if err != nil {
		return err
	}
	if err := install.ValidateUpdateAssetCompatibility(sourceVerified.Manifest, targetVerified.Manifest); err != nil {
		return fmt.Errorf("SecondBox installer update: %w", err)
	}
	if err := dependencies.CheckCapacity(plan, target); err != nil {
		return err
	}
	sourceComposeSubject, err := dependencies.SourceSubject(ctx, plan)
	if err != nil {
		return err
	}
	active, err := dependencies.Quiescent(ctx, plan, sourceComposeSubject)
	if err != nil {
		return err
	}
	if len(active) != 0 {
		return fmt.Errorf("SecondBox installer update: stop every Sandbox before updating; still active: %s", strings.Join(active, ", "))
	}
	return nil
}

func validateGuidedUpdateSourceVersion(version string) error {
	comparison, err := releasecontract.CompareVersions(version, minimumGuidedUpdateSourceVersion)
	if err != nil {
		return err
	}
	if comparison < 0 {
		return fmt.Errorf("SecondBox installer update: v0.6.0 clean-install boundary: active release %s predates v%s; perform a clean reinstall; import and compatibility modes are not available", version, minimumGuidedUpdateSourceVersion)
	}
	return nil
}

func validateUpdateSource(ctx context.Context, plan install.InstallPlan, receipt install.InstallReceipt, dependencies updateDependencies) (releaseverify.VerifiedRelease, error) {
	if err := install.ValidateRecordedResources(plan, receipt); err != nil {
		return releaseverify.VerifiedRelease{}, err
	}
	sourceVerified, err := dependencies.VerifySource(ctx, plan.Release.ArtifactManifestURL)
	if err != nil {
		return releaseverify.VerifiedRelease{}, err
	}
	if !sameUpdateTarget(plan.Release, releasePlan(sourceVerified, plan.Release.ArtifactManifestURL)) {
		return releaseverify.VerifiedRelease{}, errors.New("SecondBox installer update: active release differs from its verified public identity")
	}
	artifact, err := install.VerifyArtifactDirectory(installerPlannedPath(plan, "artifacts"), sourceVerified.Manifest)
	if err != nil {
		return releaseverify.VerifiedRelease{}, err
	}
	if err := deployconfig.ValidateSingleHostUpdateSource(plan, sourceVerified.Manifest, sourceVerified.ManifestBytes, artifact); err != nil {
		return releaseverify.VerifiedRelease{}, err
	}
	return sourceVerified, nil
}

func continueUpdate(ctx context.Context, directory string, plan install.InstallPlan, receipt install.InstallReceipt, update install.UpdateRecord, target releaseverify.VerifiedRelease, renderer cliui.Renderer, dependencies updateDependencies) error {
	source, err := dependencies.VerifySource(ctx, update.SourceRelease.ArtifactManifestURL)
	if err != nil {
		return err
	}
	if !sameUpdateTarget(update.SourceRelease, releasePlan(source, update.SourceRelease.ArtifactManifestURL)) {
		return errors.New("SecondBox installer update: resumed source release differs from its journaled public identity")
	}
	if err := install.ValidateUpdateAssetCompatibility(source.Manifest, target.Manifest); err != nil {
		return fmt.Errorf("SecondBox installer update: %w", err)
	}
	persist := func() error { return dependencies.SaveReceipt(directory, plan, receipt, dependencies.OwnerUID) }
	fail := func(stage install.UpdateStage, class install.FailureClass, problem error) error {
		if err := receipt.FailUpdate(stage, class, dependencies.Now()); err != nil {
			return errors.Join(problem, err)
		}
		return errors.Join(problem, persist())
	}
	complete := func(stage install.UpdateStage, evidence map[string]string) error {
		if err := receipt.CompleteUpdateStage(stage, dependencies.Now(), evidence); err != nil {
			return err
		}
		return persist()
	}
	current, _ := receipt.ActiveUpdate()
	if len(current.CompletedStages) == 1 {
		if err := complete(install.UpdateStageReleaseVerified, map[string]string{"artifactManifestDigest": current.TargetRelease.ArtifactManifestDigest}); err != nil {
			return err
		}
		current, _ = receipt.ActiveUpdate()
	}
	var artifact install.VerifiedArtifact
	var staged install.StagedUpdate
	if len(current.CompletedStages) == 2 {
		staged, artifact, err = install.StageUpdateRelease(ctx, plan, current, target, dependencies.Materializer)
		if err != nil {
			return fail(install.UpdateStageAssetsStaged, install.FailureRetryable, err)
		}
		if _, err := deployconfig.StageSingleHostUpdate(plan, current, target, artifact); err != nil {
			return fail(install.UpdateStageAssetsStaged, install.FailureInternal, err)
		}
		if err := complete(install.UpdateStageAssetsStaged, map[string]string{"artifactManifestDigest": artifact.ManifestDigest, "stagingRoot": staged.Root}); err != nil {
			return err
		}
		current, _ = receipt.ActiveUpdate()
	}
	if len(current.CompletedStages) == 3 {
		validatedSource, err := validateUpdateSource(ctx, plan, receipt, dependencies)
		if err != nil {
			return fail(install.UpdateStageActivationStarted, install.FailureNeedsAction, err)
		}
		if !sameUpdateTarget(current.SourceRelease, releasePlan(validatedSource, current.SourceRelease.ArtifactManifestURL)) {
			return fail(install.UpdateStageActivationStarted, install.FailureNeedsAction, errors.New("SecondBox installer update: source release changed before activation"))
		}
		source = validatedSource
		sourceComposeSubject, err := dependencies.SourceSubject(ctx, plan)
		if err != nil {
			return fail(install.UpdateStageActivationStarted, install.FailureNeedsAction, err)
		}
		active, err := dependencies.Quiescent(ctx, plan, sourceComposeSubject)
		if err != nil {
			return fail(install.UpdateStageActivationStarted, install.FailureRetryable, err)
		}
		if len(active) != 0 {
			return fail(install.UpdateStageActivationStarted, install.FailureNeedsAction, fmt.Errorf("SecondBox installer update: Sandboxes became active before activation: %s", strings.Join(active, ", ")))
		}
		manifestPath := installerPlannedPath(plan, "manifest")
		if err := dependencies.SourceCompose(ctx, manifestPath, "stop-control-plane", sourceComposeSubject); err != nil {
			return fail(install.UpdateStageActivationStarted, install.FailureRetryable, fmt.Errorf("SecondBox installer update fence control-plane admission: %w", err))
		}
		restoreSource := func(problem error) error {
			return errors.Join(problem, dependencies.SourceCompose(ctx, manifestPath, "start-control-plane", sourceComposeSubject))
		}
		active, err = dependencies.Quiescent(ctx, plan, sourceComposeSubject)
		if err != nil {
			return fail(install.UpdateStageActivationStarted, install.FailureRetryable, restoreSource(err))
		}
		if len(active) != 0 {
			problem := fmt.Errorf("SecondBox installer update: Sandboxes changed while control-plane admission was being fenced: %s", strings.Join(active, ", "))
			return fail(install.UpdateStageActivationStarted, install.FailureNeedsAction, restoreSource(problem))
		}
		activationCommitted := func() (bool, error) {
			_, durableReceipt, err := install.ReadOperationReadOnly(directory, dependencies.OwnerUID)
			if err != nil {
				return false, err
			}
			durableUpdate, ok := durableReceipt.ActiveUpdate()
			if !ok || durableUpdate.ID != current.ID {
				return false, nil
			}
			return len(durableUpdate.CompletedStages) > 3 && durableUpdate.CompletedStages[3].Stage == install.UpdateStageActivationStarted, nil
		}
		if err := journalUpdateActivationBoundary(complete, sourceComposeSubject, activationCommitted, restoreSource); err != nil {
			return err
		}
		current, _ = receipt.ActiveUpdate()
	}
	if len(current.CompletedStages) == 4 {
		manifestPath := installerPlannedPath(plan, "manifest")
		sourceComposeSubject := current.CompletedStages[3].Evidence["sourceComposeSubject"]
		if sourceComposeSubject == "" {
			return fail(install.UpdateStageDeploymentPublished, install.FailureNeedsAction, errors.New("SecondBox installer update: authenticated source Compose identity is absent from the activation boundary"))
		}
		if err := dependencies.SourceCompose(ctx, manifestPath, "down", sourceComposeSubject); err != nil {
			return fail(install.UpdateStageDeploymentPublished, install.FailureRetryable, err)
		}
		artifact, err = install.ActivateUpdateArtifactsAndBinaries(plan, current, source.Manifest, target)
		if err != nil {
			return fail(install.UpdateStageDeploymentPublished, install.FailureNeedsAction, err)
		}
		stagedFiles, err := deployconfig.StageSingleHostUpdate(plan, current, target, artifact)
		if err != nil {
			return fail(install.UpdateStageDeploymentPublished, install.FailureInternal, err)
		}
		if err := deployconfig.PublishSingleHostUpdate(plan, stagedFiles); err != nil {
			return fail(install.UpdateStageDeploymentPublished, install.FailureNeedsAction, err)
		}
		if err := complete(install.UpdateStageDeploymentPublished, map[string]string{"manifestDigest": current.TargetRelease.ArtifactManifestDigest}); err != nil {
			return err
		}
		current, _ = receipt.ActiveUpdate()
	}
	if len(current.CompletedStages) == 5 {
		manifestPath := installerPlannedPath(plan, "manifest")
		if err := dependencies.Compose(ctx, manifestPath, "prepare"); err != nil {
			return fail(install.UpdateStageComposeStarted, install.FailureRetryable, err)
		}
		if err := dependencies.Compose(ctx, manifestPath, "up"); err != nil {
			return fail(install.UpdateStageComposeStarted, install.FailureRetryable, err)
		}
		if err := complete(install.UpdateStageComposeStarted, map[string]string{"composeProject": expectedInstallerComposeProject(plan)}); err != nil {
			return err
		}
		current, _ = receipt.ActiveUpdate()
	}
	if len(current.CompletedStages) == 6 {
		if err := complete(install.UpdateStageResourcesApplied, map[string]string{"standardBundles": strings.Join(plan.StandardBundles, ",")}); err != nil {
			return err
		}
		current, _ = receipt.ActiveUpdate()
	}
	if len(current.CompletedStages) == 7 {
		evidence, err := dependencies.Readiness(ctx, plan)
		if err != nil {
			return fail(install.UpdateStageReadiness, install.FailureRetryable, err)
		}
		if err := complete(install.UpdateStageReadiness, evidence); err != nil {
			return err
		}
		current, _ = receipt.ActiveUpdate()
	}
	if len(current.CompletedStages) == 8 {
		evidence, err := dependencies.Smoke(ctx, plan)
		if err != nil {
			return fail(install.UpdateStageSmokeExecution, install.FailureRetryable, err)
		}
		artifact, err = install.ValidateActivatedUpdateArtifactsAndBinaries(plan, current, target)
		if err != nil {
			return fail(install.UpdateStageSmokeExecution, install.FailureNeedsAction, err)
		}
		expectedDigests, err := deployconfig.ValidatePublishedSingleHostUpdate(plan, current, target, artifact)
		if err != nil {
			return fail(install.UpdateStageSmokeExecution, install.FailureNeedsAction, err)
		}
		if err := install.CleanupUpdateStaging(plan, current, source.Manifest, target.Manifest); err != nil {
			return fail(install.UpdateStageSmokeExecution, install.FailureRetryable, err)
		}
		if err := receipt.CompleteUpdateStage(install.UpdateStageSmokeExecution, dependencies.Now(), evidence); err != nil {
			return err
		}
		if err := refreshUpdateResourceLedger(&receipt, current, target, expectedDigests); err != nil {
			return err
		}
		if err := receipt.ActivateUpdate(&plan, dependencies.Now()); err != nil {
			return err
		}
		if err := dependencies.SaveOperation(directory, plan, receipt, dependencies.OwnerUID); err != nil {
			return err
		}
	}
	return writeUpdateSuccess(renderer, directory, plan)
}

func journalUpdateActivationBoundary(complete func(install.UpdateStage, map[string]string) error, sourceComposeSubject string, committed func() (bool, error), restoreSource func(error) error) error {
	if sourceComposeSubject == "" {
		return restoreSource(errors.New("SecondBox installer update activation boundary lacks the authenticated source Compose identity"))
	}
	if err := complete(install.UpdateStageActivationStarted, map[string]string{"forwardOnly": "true", "controlPlaneAdmission": "stopped", "deploymentQuiescence": "verified", "sourceComposeSubject": sourceComposeSubject}); err != nil {
		visible, inspectErr := committed()
		if inspectErr != nil {
			return errors.Join(err, fmt.Errorf("SecondBox installer update cannot determine whether the forward-only activation boundary committed; control-plane admission remains stopped: %w", inspectErr))
		}
		if visible {
			return err
		}
		return restoreSource(err)
	}
	return nil
}

func completedUpdateMatchesTarget(receipt install.InstallReceipt, target install.ReleasePlan) bool {
	if len(receipt.Updates) == 0 {
		return false
	}
	latest := receipt.Updates[len(receipt.Updates)-1]
	return latest.Status == install.UpdateSucceeded && sameUpdateTarget(latest.TargetRelease, target)
}

func writeUpdateSuccess(renderer cliui.Renderer, directory string, plan install.InstallPlan) error {
	return renderer.WriteSummary(cliui.Summary{Title: "SecondBox single-host update complete", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Release", Value: plan.Release.Version}, {Key: "Deployment", Value: directory}, {Key: "Manifest", Value: installerPlannedPath(plan, "manifest")}}, Next: "Health: secondbox whoami && secondbox runners list"})
}

func refreshUpdateResourceLedger(receipt *install.InstallReceipt, update install.UpdateRecord, target releaseverify.VerifiedRelease, expected map[string]string) error {
	if err := receipt.RefreshUpdatedResource("artifacts", target.Manifest.MicroVM.SignedManifestDigest); err != nil {
		return err
	}
	expected["secondbox-binary"] = "sha256:" + update.TargetRelease.BinaryDigests["secondbox"]
	expected["secondbox-deploy-binary"] = "sha256:" + update.TargetRelease.BinaryDigests["secondbox-deploy"]
	for _, id := range []string{"secondbox-binary", "secondbox-deploy-binary", "signed-asset-catalog", "release-artifact-manifest", "manifest", "compose-environment"} {
		digest := expected[id]
		if digest == "" {
			return fmt.Errorf("SecondBox installer update: verified target digest is absent for %s", id)
		}
		if err := receipt.RefreshUpdatedResource(id, digest); err != nil {
			return err
		}
	}
	return nil
}

type blockingUpdateSandbox struct {
	ID                string `json:"id"`
	TenantRef         string `json:"tenantRef"`
	SubjectRef        string `json:"subjectRef"`
	State             string `json:"state"`
	DesiredState      string `json:"desiredState"`
	WorkspaceMutation string `json:"workspaceMutation"`
}

const installedDeploymentQuiescenceQuery = `
WITH blockers AS (
SELECT
  0 AS sort_group,
  sandbox.id,
  sandbox.tenant_ref,
  sandbox.subject_ref,
  sandbox.state,
  sandbox.desired_state,
  COALESCE(workspace.mutation_state, 'missing') AS workspace_mutation
FROM secondbox.sandboxes AS sandbox
LEFT JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
WHERE sandbox.deleted_at IS NULL AND sandbox.state<>'deleted'
AND NOT (
  workspace.id IS NOT NULL
  AND sandbox.state='stopped' AND sandbox.desired_state='stopped'
  AND sandbox.current_instance_id=''
  AND workspace.mutation_state=''
  AND sandbox.next_reconcile_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM secondbox.instances AS instance
    WHERE instance.sandbox_id=sandbox.id
      AND instance.state NOT IN ('stopped','lost','failed')
  )
)
UNION ALL
SELECT
  1 AS sort_group,
  instance.id,
  'orphan-instance' AS tenant_ref,
  instance.sandbox_id AS subject_ref,
  'instance ' || instance.state AS state,
  'missing sandbox' AS desired_state,
  '' AS workspace_mutation
FROM secondbox.instances AS instance
LEFT JOIN secondbox.sandboxes AS sandbox
  ON sandbox.id=instance.sandbox_id
  AND sandbox.deleted_at IS NULL
  AND sandbox.state<>'deleted'
WHERE instance.state NOT IN ('stopped','lost','failed')
AND sandbox.id IS NULL
)
SELECT COALESCE(json_agg(json_build_object(
  'id', blocker.id,
  'tenantRef', blocker.tenant_ref,
  'subjectRef', blocker.subject_ref,
  'state', blocker.state,
  'desiredState', blocker.desired_state,
  'workspaceMutation', blocker.workspace_mutation
) ORDER BY blocker.sort_group, blocker.tenant_ref, blocker.subject_ref, blocker.id), '[]'::json)
FROM blockers AS blocker`

func installedDeploymentSandboxesRequiringStop(ctx context.Context, plan install.InstallPlan, sourceComposeSubject string) ([]string, error) {
	manifestPath := installerPlannedPath(plan, "manifest")
	query := installedDeploymentQuiescenceQuery
	command := `exec psql --no-psqlrc --set=ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --tuples-only --no-align --command "$1"`
	arguments, err := deployconfig.ComposeDiagnosticArgumentsForRecordedInstaller(
		manifestPath, sourceComposeSubject, "exec", "--no-TTY", "postgres", "sh", "-eu", "-c", command, "secondbox-update-quiescence", query,
	)
	if err != nil {
		return nil, err
	}
	stdout, stderr := &boundedCommandBuffer{maximum: 4 << 20}, &boundedCommandBuffer{maximum: 4 << 20}
	executor := deployconfig.SystemComposeExecutor{Output: stdout, Diagnostic: stderr}
	if err := executor.Run(ctx, arguments); err != nil {
		return nil, fmt.Errorf("SecondBox installer update inspect deployment-wide Sandbox state: %w: %s", err, cliui.Sanitize(stderr.String()))
	}
	if stdout.tooLong || stderr.tooLong {
		return nil, errors.New("SecondBox installer update deployment-wide Sandbox response exceeded the bounded output limit")
	}
	return decodeBlockingUpdateSandboxes(stdout.Bytes())
}

func decodeBlockingUpdateSandboxes(content []byte) ([]string, error) {
	var blocking []blockingUpdateSandbox
	if err := json.Unmarshal(content, &blocking); err != nil {
		return nil, fmt.Errorf("SecondBox installer update decode deployment-wide Sandbox state: %w", err)
	}
	result := make([]string, 0, len(blocking))
	for _, sandbox := range blocking {
		state := sandbox.State + " -> " + sandbox.DesiredState
		if sandbox.WorkspaceMutation != "" {
			state += "; workspace " + sandbox.WorkspaceMutation
		}
		result = append(result, sandbox.TenantRef+"/"+sandbox.SubjectRef+"/"+sandbox.ID+" ("+state+")")
	}
	return result, nil
}

func runInstalledUpdateSmoke(ctx context.Context, plan install.InstallPlan) (map[string]string, error) {
	evidence, err := runInstalledSmoke(ctx, plan)
	if err != nil {
		return nil, err
	}
	evidence["postUpdateState"] = "runner-ready"
	return evidence, nil
}

func sameUpdateTarget(left, right install.ReleasePlan) bool {
	leftBytes, leftErr := install.Canonical(left)
	rightBytes, rightErr := install.Canonical(right)
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}
