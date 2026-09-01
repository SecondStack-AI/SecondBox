package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

type installResumeDependencies struct {
	OwnerUID               int
	Now                    func() time.Time
	HostApply              func(context.Context, string, string) error
	Revalidate             func(install.InstallPlan) error
	VerifyRelease          func(context.Context, string) (releaseverify.VerifiedRelease, error)
	Postconditions         func(install.InstallPlan, install.InstallReceipt, releaseverify.VerifiedRelease) error
	TeardownPostconditions func(install.InstallPlan, install.InstallReceipt) error
	Materialize            func(context.Context, install.InstallPlan, install.InstallReceipt, releaseverify.VerifiedRelease, func(install.InstallReceipt) error) (install.InstallReceipt, install.VerifiedArtifact, error)
	Initialize             func(install.InstallPlan, releaseverify.VerifiedRelease, install.VerifiedArtifact) (deployconfig.SingleHostInstallResult, error)
	Enroll                 func(deployconfig.SingleHostInstallResult) error
	Compose                func(context.Context, string, string) error
	ComposeProject         func(string) (string, error)
	Login                  func(context.Context, install.InstallPlan) ([]install.CreatedResource, error)
	Readiness              func(context.Context, install.InstallPlan) (map[string]string, error)
	Smoke                  func(context.Context, install.InstallPlan) (map[string]string, error)
}

func systemInstallResumeDependencies(renderer cliui.Renderer) installResumeDependencies {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return installResumeDependencies{
		OwnerUID: os.Getuid(), Now: time.Now,
		HostApply:  runPrivilegedHostApply,
		Revalidate: revalidateResumeHost,
		VerifyRelease: func(ctx context.Context, location string) (releaseverify.VerifiedRelease, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			return verifyReleaseLocationWithContext(ctx, location)
		},
		Postconditions:         validateInstallPostconditions,
		TeardownPostconditions: validateComposeTeardownAuthority,
		Materialize: func(ctx context.Context, plan install.InstallPlan, receipt install.InstallReceipt, verified releaseverify.VerifiedRelease, persist func(install.InstallReceipt) error) (install.InstallReceipt, install.VerifiedArtifact, error) {
			executor := install.SystemReleaseMaterializer{Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic, HTTPClient: httpClient}
			return install.MaterializeRelease(ctx, plan, receipt, verified, install.ReleaseMaterializeDependencies{Executor: executor, PersistReceipt: persist, Now: time.Now})
		},
		Initialize: func(plan install.InstallPlan, verified releaseverify.VerifiedRelease, artifact install.VerifiedArtifact) (deployconfig.SingleHostInstallResult, error) {
			return deployconfig.InitSingleHostFromReleaseOrValidate(plan, verified.Manifest, verified.ManifestBytes, artifact)
		},
		Enroll: func(result deployconfig.SingleHostInstallResult) error {
			return deployconfig.RunnerInitOrValidate(result.ManifestPath, result.RunnerID, result.RunnerIdentityDirectory)
		},
		Compose: func(ctx context.Context, manifestPath, action string) error {
			return deployconfig.RunComposeForAcceptedInstaller(ctx, manifestPath, action, deployconfig.SystemComposeExecutor{Input: os.Stdin, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic}, httpClient)
		},
		ComposeProject: func(manifestPath string) (string, error) {
			resolved, err := deployconfig.ResolveForAcceptedInstaller(manifestPath)
			if err != nil {
				return "", err
			}
			return resolved.ComposeProject(), nil
		},
		Login: func(ctx context.Context, plan install.InstallPlan) ([]install.CreatedResource, error) {
			return install.LoginCLI(ctx, plan, httpClient)
		},
		Readiness: func(ctx context.Context, plan install.InstallPlan) (map[string]string, error) {
			return waitForInstalledRunner(ctx, plan)
		},
		Smoke: runInstalledSmoke,
	}
}

func runPrivilegedHostApply(ctx context.Context, directory, digest string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "sudo", "--", executable, "_install-host-apply", directory, digest)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func runPrivilegedHostTeardownVerify(ctx context.Context, directory, digest string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "sudo", "--", executable, "_install-host-teardown-verify", directory, digest)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func runPrivilegedHostUpdateVerify(ctx context.Context, directory, digest string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "sudo", "--", executable, "_install-host-update-verify", directory, digest)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("SecondBox installer update privileged-resource verification: %w", err)
	}
	return nil
}

func runInstallResume(ctx context.Context, directory string, renderer cliui.Renderer) error {
	if renderer.OutputMode == cliui.OutputJSON {
		return &deployExitError{code: 3, err: errors.New("SecondBox installer: install --resume does not accept --output json")}
	}
	return runInstallResumeWith(ctx, directory, renderer, systemInstallResumeDependencies(renderer))
}

func runInstallCandidateResume(ctx context.Context, directory, candidateDirectory string, renderer cliui.Renderer) error {
	if renderer.OutputMode == cliui.OutputJSON {
		return &deployExitError{code: 3, err: errors.New("SecondBox installer: candidate resume does not accept --output json")}
	}
	return runInstallResumeWith(ctx, directory, renderer, candidateInstallResumeDependencies(renderer, candidateDirectory))
}

func candidateInstallResumeDependencies(renderer cliui.Renderer, directory string) installResumeDependencies {
	dependencies := systemInstallResumeDependencies(renderer)
	dependencies.VerifyRelease = func(ctx context.Context, _ string) (releaseverify.VerifiedRelease, error) {
		return verifyCandidateDirectory(ctx, directory)
	}
	dependencies.Materialize = func(ctx context.Context, plan install.InstallPlan, receipt install.InstallReceipt, verified releaseverify.VerifiedRelease, persist func(install.InstallReceipt) error) (install.InstallReceipt, install.VerifiedArtifact, error) {
		executor := install.CandidateReleaseMaterializer{Directory: directory, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic}
		return install.MaterializeRelease(ctx, plan, receipt, verified, install.ReleaseMaterializeDependencies{Executor: executor, PersistReceipt: persist, Now: time.Now})
	}
	return dependencies
}

func runInstallResumeWith(ctx context.Context, directory string, renderer cliui.Renderer, dependencies installResumeDependencies) (resultErr error) {
	if dependencies.Now == nil || dependencies.VerifyRelease == nil || dependencies.Postconditions == nil || dependencies.Materialize == nil || dependencies.Initialize == nil || dependencies.Enroll == nil || dependencies.Compose == nil || dependencies.ComposeProject == nil || dependencies.Login == nil || dependencies.Readiness == nil || dependencies.Smoke == nil || dependencies.HostApply == nil || dependencies.Revalidate == nil {
		return errors.New("SecondBox installer resume: dependencies are incomplete")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	plan, receipt, err := install.ReadOperationReadOnly(absolute, dependencies.OwnerUID)
	if err != nil {
		return err
	}
	if _, active := receipt.ActiveUpdate(); active {
		return errors.New("SecondBox installer resume: an update is incomplete; use secondbox-deploy update --resume " + absolute)
	}
	last := lastInstallStage(receipt)
	if slices.Index(install.StageSequence, last) >= slices.Index(install.StageSequence, install.StagePlanAccepted) &&
		receipt.Status != install.OperationPurging && receipt.Status != install.OperationPurged && receipt.Status != install.OperationUninstalling {
		digest, err := install.PlanDigest(plan)
		if err != nil {
			return err
		}
		name, detail := "Privileged host verification", "verify reviewed host resources"
		if last == install.StagePlanAccepted {
			name, detail = "Privileged host preparation", "apply reviewed host resources"
		}
		if err := runInstallPhase(ctx, renderer, name, detail, func() error { return dependencies.HostApply(ctx, absolute, digest) }); err != nil {
			return err
		}
	}
	lock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	// The pre-lock read is only a hint used to decide whether the separately
	// privileged host-apply helper is needed. All state used below must be
	// reloaded while this process owns the operation lock.
	plan, receipt, err = install.RecoverOperation(absolute, dependencies.OwnerUID, lock)
	if err != nil {
		return err
	}
	if _, active := receipt.ActiveUpdate(); active {
		return errors.New("SecondBox installer resume: an update is incomplete; use secondbox-deploy update --resume " + absolute)
	}
	last = lastInstallStage(receipt)
	if receipt.Status == install.OperationPurging || receipt.Status == install.OperationPurged {
		return errors.New("SecondBox installer resume: permanent purge is in progress or complete; use uninstall --purge to inspect the tombstone")
	}
	persist := func(value install.InstallReceipt) error {
		return install.SaveReceipt(absolute, plan, value, dependencies.OwnerUID)
	}
	if receipt.Status == install.OperationUninstalling {
		if dependencies.TeardownPostconditions == nil {
			return errors.New("SecondBox installer resume: teardown postcondition dependency is absent")
		}
		if err := dependencies.TeardownPostconditions(plan, receipt); err != nil {
			return err
		}
		if err := runInstallPhase(ctx, renderer, "Compose shutdown recovery", "complete journaled ordinary uninstall", func() error {
			return dependencies.Compose(ctx, installerPlannedPath(plan, "manifest"), "down")
		}); err != nil {
			return err
		}
		if err := finishJournaledUninstall(&receipt, dependencies.Now()); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
		return writeUninstallSummary(renderer, plan)
	}
	if err := dependencies.Revalidate(plan); err != nil {
		return markResumeFailure(&receipt, install.StageHostApply, install.FailureNeedsAction, dependencies.Now(), persist, err)
	}
	var verified releaseverify.VerifiedRelease
	if err := runInstallPhase(ctx, renderer, "Release verification", plan.Release.Version, func() error {
		var verifyErr error
		verified, verifyErr = dependencies.VerifyRelease(ctx, plan.Release.ArtifactManifestURL)
		return verifyErr
	}); err != nil {
		return markResumeFailure(&receipt, install.StageReleaseVerified, install.FailureRetryable, dependencies.Now(), persist, err)
	}
	if err := dependencies.Postconditions(plan, receipt, verified); err != nil {
		return markResumeFailure(&receipt, last, install.FailureNeedsAction, dependencies.Now(), persist, err)
	}
	if receipt.Status == install.OperationUninstalled && last == install.StageSmokeExecution {
		manifestPath := installerPlannedPath(plan, "manifest")
		if err := runInstallPhase(ctx, renderer, "Compose restart", "restore preserved deployment", func() error {
			if err := dependencies.Compose(ctx, manifestPath, "prepare"); err != nil {
				return err
			}
			return dependencies.Compose(ctx, manifestPath, "up")
		}); err != nil {
			return err
		}
		if _, err := dependencies.Readiness(ctx, plan); err != nil {
			return err
		}
		if err := receipt.RestoreSucceeded(dependencies.Now()); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
		return writeInstallerSuccess(renderer, plan, receipt)
	}
	if (receipt.Status == install.OperationSucceeded || receipt.Status == install.OperationFailed) && last == install.StageSmokeExecution {
		if _, err := dependencies.Readiness(ctx, plan); err != nil {
			return markResumeFailure(&receipt, install.StageReadiness, install.FailureRetryable, dependencies.Now(), persist, err)
		}
		if receipt.Status == install.OperationFailed {
			if err := receipt.RecoverSucceeded(dependencies.Now()); err != nil {
				return err
			}
			if err := persist(receipt); err != nil {
				return err
			}
		}
		return writeInstallerSuccess(renderer, plan, receipt)
	}
	last = lastInstallStage(receipt)
	var artifact install.VerifiedArtifact
	if last == install.StageHostApply || last == install.StageReleaseVerified {
		if err := runInstallPhase(ctx, renderer, "Release assets", "pull, verify, and atomically publish", func() error {
			var materializeErr error
			receipt, artifact, materializeErr = dependencies.Materialize(ctx, plan, receipt, verified, persist)
			return materializeErr
		}); err != nil {
			return err
		}
		last = lastInstallStage(receipt)
	}
	if last == install.StageAssetsMaterialized {
		if artifact.ManifestDigest == "" {
			artifact, err = install.VerifyArtifactDirectory(installerPlannedPath(plan, "artifacts"), verified.Manifest)
			if err != nil {
				return markResumeFailure(&receipt, install.StageDeploymentMaterialized, install.FailureBlocked, dependencies.Now(), persist, err)
			}
		}
		var initialized deployconfig.SingleHostInstallResult
		if err := runInstallPhase(ctx, renderer, "Deployment materialization", "generate explicit authority and manifest", func() error {
			var initializeErr error
			initialized, initializeErr = dependencies.Initialize(plan, verified, artifact)
			return initializeErr
		}); err != nil {
			return markResumeFailure(&receipt, install.StageDeploymentMaterialized, install.FailureInternal, dependencies.Now(), persist, err)
		}
		if err := appendExistingPlannedResources(&receipt, plan, install.StageDeploymentMaterialized, persist, map[string]bool{"deployment": true, "runner-identity": true, "compose-environment": true, "compose-assets": true, "cli-config-root": true, "cli-config-directory": true, "cli-config": true, "binary-directory-root": true, "binary-directory": true, "secondbox-binary": true, "secondbox-deploy-binary": true, "artifacts": true, "workspace": true}); err != nil {
			return err
		}
		manifestBytes, err := os.ReadFile(initialized.ManifestPath)
		if err != nil {
			return err
		}
		if err := receipt.CompleteStage(install.StageDeploymentMaterialized, dependencies.Now(), map[string]string{"manifestDigest": install.Digest(manifestBytes), "runnerId": initialized.RunnerID}); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
		last = install.StageDeploymentMaterialized
	}
	manifestPath := installerPlannedPath(plan, "manifest")
	runnerResult := deployconfig.SingleHostInstallResult{ManifestPath: manifestPath, RunnerID: "runner-" + strings.TrimPrefix(plan.OperationID, "install_"), RunnerIdentityDirectory: installerPlannedPath(plan, "runner-identity"), PlatformTokenPath: installerSecretPath(plan, "platform-authority")}
	if last == install.StageDeploymentMaterialized {
		if err := runInstallPhase(ctx, renderer, "Runner enrollment", runnerResult.RunnerID, func() error { return dependencies.Enroll(runnerResult) }); err != nil {
			return markResumeFailure(&receipt, install.StageRunnerEnrolled, install.FailureInternal, dependencies.Now(), persist, err)
		}
		identity, err := installerPlannedResource(plan, "runner-identity", install.StageRunnerEnrolled)
		if err != nil {
			return err
		}
		if err := receipt.AppendResource(identity); err != nil {
			return err
		}
		if err := receipt.CompleteStage(install.StageRunnerEnrolled, dependencies.Now(), map[string]string{"runnerId": runnerResult.RunnerID, "identity": runnerResult.RunnerIdentityDirectory}); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
		last = install.StageRunnerEnrolled
	}
	if last == install.StageRunnerEnrolled {
		if err := runInstallPhase(ctx, renderer, "Compose startup", "database, control plane, and Runner", func() error {
			if err := dependencies.Compose(ctx, manifestPath, "prepare"); err != nil {
				return err
			}
			return dependencies.Compose(ctx, manifestPath, "up")
		}); err != nil {
			return markResumeFailure(&receipt, install.StageComposeStarted, install.FailureRetryable, dependencies.Now(), persist, err)
		}
		composeProject, err := dependencies.ComposeProject(manifestPath)
		if err != nil {
			return err
		}
		if err := receipt.AppendResource(install.CreatedResource{ID: "compose-project", Kind: install.ResourceComposeProject, Stage: install.StageComposeStarted, Identity: composeProject}); err != nil {
			return err
		}
		for _, name := range []string{"compose-environment", "compose-assets"} {
			path := installerPlannedPath(plan, name)
			if _, statErr := os.Lstat(path); statErr != nil {
				return fmt.Errorf("SecondBox installer Compose output %s: %w", name, statErr)
			}
			resource, err := installerPlannedResource(plan, name, install.StageComposeStarted)
			if err != nil {
				return err
			}
			if err := receipt.AppendResource(resource); err != nil {
				return err
			}
		}
		if err := receipt.CompleteStage(install.StageComposeStarted, dependencies.Now(), map[string]string{"composeProject": composeProject}); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
		last = install.StageComposeStarted
	}
	if last == install.StageComposeStarted {
		var resources []install.CreatedResource
		if err := runInstallPhase(ctx, renderer, "CLI login", plan.CLI.ConfigPath, func() error {
			var loginErr error
			resources, loginErr = dependencies.Login(ctx, plan)
			return loginErr
		}); err != nil {
			return markResumeFailure(&receipt, install.StageCLILogin, install.FailureNeedsAction, dependencies.Now(), persist, err)
		}
		for _, resource := range resources {
			if err := receipt.AppendResource(resource); err != nil {
				return err
			}
		}
		if err := receipt.CompleteStage(install.StageCLILogin, dependencies.Now(), map[string]string{"configPath": plan.CLI.ConfigPath, "authorityVerified": "true"}); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
		last = install.StageCLILogin
	}
	if last == install.StageCLILogin {
		var evidence map[string]string
		if err := runInstallPhase(ctx, renderer, "Service readiness", "authenticated Runner and cold-boot capacity", func() error {
			var readinessErr error
			evidence, readinessErr = dependencies.Readiness(ctx, plan)
			return readinessErr
		}); err != nil {
			return markResumeFailure(&receipt, install.StageReadiness, install.FailureRetryable, dependencies.Now(), persist, err)
		}
		if err := receipt.CompleteStage(install.StageReadiness, dependencies.Now(), evidence); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
		last = install.StageReadiness
	}
	if last == install.StageReadiness {
		var evidence map[string]string
		if err := runInstallPhase(ctx, renderer, "Installation qualification", "authenticated RunnerPool and Runner readiness", func() error {
			var smokeErr error
			evidence, smokeErr = dependencies.Smoke(ctx, plan)
			return smokeErr
		}); err != nil {
			return markResumeFailure(&receipt, install.StageSmokeExecution, install.FailureRetryable, dependencies.Now(), persist, err)
		}
		if err := receipt.CompleteStage(install.StageSmokeExecution, dependencies.Now(), evidence); err != nil {
			return err
		}
		if err := persist(receipt); err != nil {
			return err
		}
	}
	return writeInstallerSuccess(renderer, plan, receipt)
}

func validateInstallPostconditions(plan install.InstallPlan, receipt install.InstallReceipt, verified releaseverify.VerifiedRelease) error {
	if err := install.ValidateRecordedResources(plan, receipt); err != nil {
		return err
	}
	last := lastInstallStage(receipt)
	if slices.Index(install.StageSequence, last) >= slices.Index(install.StageSequence, install.StageAssetsMaterialized) {
		if _, err := install.VerifyArtifactDirectory(installerPlannedPath(plan, "artifacts"), verified.Manifest); err != nil {
			return err
		}
	}
	if slices.Index(install.StageSequence, last) >= slices.Index(install.StageSequence, install.StageComposeStarted) {
		resolved, err := deployconfig.ResolveForAcceptedInstaller(installerPlannedPath(plan, "manifest"))
		if err != nil {
			return err
		}
		if err := install.ValidateComposeProjectEvidence(receipt, resolved.ComposeProject()); err != nil {
			return err
		}
	}
	if slices.Index(install.StageSequence, last) >= slices.Index(install.StageSequence, install.StageCLILogin) {
		return install.ValidateCLIConfig(plan)
	}
	return nil
}

func validateComposeTeardownAuthority(plan install.InstallPlan, receipt install.InstallReceipt) error {
	if err := install.ValidateRecordedResources(plan, receipt); err != nil {
		return err
	}
	resolved, err := deployconfig.ResolveForAcceptedInstaller(installerPlannedPath(plan, "manifest"))
	if err != nil {
		return err
	}
	return install.ValidateComposeProjectEvidence(receipt, resolved.ComposeProject())
}

func runInstallPhase(ctx context.Context, renderer cliui.Renderer, name, detail string, action func() error) error {
	activity, err := renderer.StartActivity(ctx, name)
	if err != nil {
		return err
	}
	actionErr := action()
	status := cliui.StatusComplete
	message := detail
	if actionErr != nil {
		status, message = cliui.StatusFailed, "failed"
	}
	return errors.Join(actionErr, activity.Complete(status, message))
}

func lastInstallStage(receipt install.InstallReceipt) install.Stage {
	if len(receipt.CompletedStages) == 0 {
		return ""
	}
	return receipt.CompletedStages[len(receipt.CompletedStages)-1].Stage
}

func markResumeFailure(receipt *install.InstallReceipt, stage install.Stage, class install.FailureClass, now time.Time, persist func(install.InstallReceipt) error, problem error) error {
	return errors.Join(problem, receipt.Fail(stage, class, now), persist(*receipt))
}

func installerPlannedPath(plan install.InstallPlan, name string) string {
	for _, planned := range plan.Paths {
		if planned.Name == name {
			return planned.Path
		}
	}
	return ""
}

func installerPlannedResource(plan install.InstallPlan, name string, stage install.Stage) (install.CreatedResource, error) {
	for _, planned := range plan.Paths {
		if planned.Name == name {
			resource := install.CreatedResource{ID: planned.Name, Kind: planned.Kind, Path: planned.Path, Class: planned.Class, Stage: stage, Mode: planned.Mode, OwnerUID: planned.OwnerUID, OwnerGID: planned.OwnerGID}
			if planned.Kind == install.ResourceFile || planned.Kind == install.ResourceBinary || planned.Kind == install.ResourceMountUnit {
				content, err := os.ReadFile(planned.Path)
				if err != nil {
					return install.CreatedResource{}, fmt.Errorf("SecondBox installer digest planned resource %s: %w", name, err)
				}
				resource.Digest = install.Digest(content)
			}
			return resource, nil
		}
	}
	return install.CreatedResource{}, fmt.Errorf("SecondBox installer planned resource is absent: %s", name)
}

func installerSecretPath(plan install.InstallPlan, category string) string {
	for _, target := range plan.SecretTargets {
		if target.Category == category {
			return target.Path
		}
	}
	return ""
}

func appendExistingPlannedResources(receipt *install.InstallReceipt, plan install.InstallPlan, stage install.Stage, persist func(install.InstallReceipt) error, skip map[string]bool) error {
	existing := map[string]bool{}
	for _, resource := range receipt.CreatedResources {
		existing[resource.ID] = true
	}
	for _, planned := range plan.Paths {
		if existing[planned.Name] || skip[planned.Name] || planned.RequiresSudo {
			continue
		}
		if err := install.ValidatePlannedPath(planned); err != nil {
			return fmt.Errorf("SecondBox installer deployment resource %s: %w", planned.Name, err)
		}
		resource, err := installerPlannedResource(plan, planned.Name, stage)
		if err != nil {
			return err
		}
		if err := receipt.AppendResource(resource); err != nil {
			return err
		}
		if err := persist(*receipt); err != nil {
			return err
		}
	}
	return nil
}

func revalidateResumeHost(plan install.InstallPlan) error {
	machineID, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return err
	}
	if "machine-id:"+strings.TrimSpace(string(machineID)) != plan.HostFacts.HostIdentity {
		return errors.New("SecondBox installer resume: host identity differs from the accepted plan")
	}
	for _, path := range []string{"/dev/kvm", "/dev/net/tun", "/sys/fs/cgroup/cgroup.controllers"} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("SecondBox installer resume prerequisite %s: %w", path, err)
		}
	}
	return nil
}

func waitForInstalledRunner(ctx context.Context, plan install.InstallPlan) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		command, stdout, stderr := installedCLICommand(ctx, plan, "--output", "json", "runners", "list")
		err := command.Run()
		if err == nil {
			var response contracts.RunnerPage
			if json.Unmarshal(stdout.Bytes(), &response) == nil {
				if evidence, ready := installedRunnerReadinessEvidence(plan, response.Items); ready {
					return evidence, nil
				}
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("SecondBox installer readiness deadline: %w; last diagnostic: %s", ctx.Err(), cliui.Sanitize(stderr.String()))
		case <-timer.C:
		}
	}
}

func installedRunnerReadinessEvidence(plan install.InstallPlan, runners []contracts.Runner) (map[string]string, bool) {
	expectedID := "runner-" + strings.TrimPrefix(plan.OperationID, "install_")
	for _, runner := range runners {
		if runner.ID != expectedID || runner.PoolName != standardresources.PoolAMD64 || runner.State != "ready" || runner.CredentialState != "pre_shared" || !slices.Contains(runner.Architectures, standardresources.ArchitectureAMD64) {
			continue
		}
		if slices.ContainsFunc([]string{"compute", "network-policy", "storage", "cleanup", "local-workspace"}, func(capability string) bool { return !slices.Contains(runner.Capabilities, capability) }) {
			continue
		}
		if runner.Capacity["VCPUCount"] < install.DurableCodingVCPUCount || runner.Capacity["MemoryBytes"] < install.DurableCodingMemoryBytes || runner.Capacity["DiskBytes"] < install.MinimumWorkspaceBytes || runner.Capacity["Instances"] < 1 || runner.Capacity["Operations"] < install.DurableCodingConcurrentOperations {
			continue
		}
		expectedContext := expectedInstallerComposeProject(plan)
		if !slices.Contains(runner.SupportedEgressContexts, expectedContext) {
			continue
		}
		return map[string]string{"runnerId": runner.ID, "runnerPool": runner.PoolName, "runnerState": runner.State, "runnerCredentialState": runner.CredentialState, "egressContext": expectedContext, "coldBootCapacity": "advertised", "concurrentOperationCapacity": strconv.FormatInt(runner.Capacity["Operations"], 10)}, true
	}
	return nil, false
}

func runInstalledSmoke(ctx context.Context, plan install.InstallPlan) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	command, stdout, stderr := installedCLICommand(
		ctx, plan, "--output", "json", "runner-pools", "get",
		"--path", "runnerPoolName="+standardresources.PoolAMD64,
	)
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("SecondBox installer qualification RunnerPool: %w: %s", err, cliui.Sanitize(stderr.String()))
	}
	var pool contracts.RunnerPool
	if err := json.Unmarshal(stdout.Bytes(), &pool); err != nil {
		return nil, fmt.Errorf("SecondBox installer qualification RunnerPool decode: %w", err)
	}
	runnerID := "runner-" + strings.TrimPrefix(plan.OperationID, "install_")
	command, stdout, stderr = installedCLICommand(
		ctx, plan, "--output", "json", "runners", "get",
		"--path", "runnerId="+runnerID,
	)
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("SecondBox installer qualification Runner: %w: %s", err, cliui.Sanitize(stderr.String()))
	}
	var runner contracts.Runner
	if err := json.Unmarshal(stdout.Bytes(), &runner); err != nil {
		return nil, fmt.Errorf("SecondBox installer qualification Runner decode: %w", err)
	}
	evidence, err := installedSmokeEvidence(plan, pool, runner)
	if err != nil {
		return nil, err
	}
	command, stdout, stderr = installedCLICommand(
		ctx, plan, "--output", "json", "diagnostics", "egress-contexts",
	)
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("SecondBox installer qualification egress-context preflight: %w: %s", err, cliui.Sanitize(stderr.String()))
	}
	var preflight contracts.EgressContextPreflight
	if err := json.Unmarshal(stdout.Bytes(), &preflight); err != nil {
		return nil, fmt.Errorf("SecondBox installer qualification egress-context preflight decode: %w", err)
	}
	if !preflight.Ready || preflight.Truncated {
		return nil, errors.New("SecondBox installer qualification egress-context preflight is not ready and complete")
	}
	evidence["egressContextPreflight"] = "ready"
	return evidence, nil
}

func installedSmokeEvidence(
	plan install.InstallPlan,
	pool contracts.RunnerPool,
	runner contracts.Runner,
) (map[string]string, error) {
	if pool.Name != standardresources.PoolAMD64 || pool.State != contracts.RunnerPoolStateReady ||
		pool.ReadyRunnerCount < 1 {
		return nil, errors.New("SecondBox installer qualification RunnerPool is not ready")
	}
	evidence, ready := installedRunnerReadinessEvidence(plan, []contracts.Runner{runner})
	if !ready {
		return nil, errors.New("SecondBox installer qualification Runner is not ready")
	}
	evidence["qualification"] = "authenticated-runner-readiness"
	evidence["runnerPoolState"] = pool.State
	evidence["runnerPoolReadyRunners"] = strconv.FormatInt(pool.ReadyRunnerCount, 10)
	return evidence, nil
}

func installedCLICommand(ctx context.Context, plan install.InstallPlan, arguments ...string) (*exec.Cmd, *boundedCommandBuffer, *boundedCommandBuffer) {
	command := exec.CommandContext(ctx, installerPlannedPath(plan, "secondbox-binary"), arguments...)
	stdout, stderr := &boundedCommandBuffer{maximum: 4 << 20}, &boundedCommandBuffer{maximum: 4 << 20}
	command.Stdout, command.Stderr = stdout, stderr
	command.Stdin = bytes.NewReader(nil)
	command.Env = installerCommandEnvironment(plan.CLI.ConfigPath)
	return command, stdout, stderr
}

type boundedCommandBuffer struct {
	buffer  bytes.Buffer
	maximum int
	tooLong bool
}

func (buffer *boundedCommandBuffer) Write(content []byte) (int, error) {
	original := len(content)
	remaining := buffer.maximum - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.tooLong = true
		return original, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		buffer.tooLong = true
	}
	_, _ = buffer.buffer.Write(content)
	return original, nil
}

func (buffer *boundedCommandBuffer) Bytes() []byte  { return buffer.buffer.Bytes() }
func (buffer *boundedCommandBuffer) String() string { return buffer.buffer.String() }

func installerCommandEnvironment(configPath string) []string {
	allow := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	result := []string{"SECONDBOX_CONFIG=" + configPath}
	for _, entry := range os.Environ() {
		if allow[strings.SplitN(entry, "=", 2)[0]] {
			result = append(result, entry)
		}
	}
	slices.Sort(result)
	return result
}

func writeInstallerSuccess(renderer cliui.Renderer, plan install.InstallPlan, receipt install.InstallReceipt) error {
	pairs := []cliui.Pair{{Key: "Release", Value: plan.Release.Version}, {Key: "Manifest", Value: installerPlannedPath(plan, "manifest")}, {Key: "Workspace", Value: plan.Storage.WorkspacePath}, {Key: "Generated authority", Value: installerPlannedPath(plan, "secrets")}, {Key: "Runner logs", Value: installerPlannedPath(plan, "logs")}, {Key: "Installed binaries", Value: installerPlannedPath(plan, "binary-directory")}, {Key: "CLI configuration", Value: plan.CLI.ConfigPath}, {Key: "Receipt", Value: filepath.Join(installerPlannedPath(plan, "deployment"), "install-receipt.json")}}
	for _, record := range receipt.CompletedStages {
		if record.Stage == install.StageReadiness {
			if value := record.Evidence["runnerId"]; value != "" {
				pairs = append(pairs, cliui.Pair{Key: "Runner", Value: value})
			}
		}
	}
	return renderer.WriteSummary(cliui.Summary{Title: "SecondBox single-host installation complete", Status: cliui.StatusComplete, Pairs: pairs, Next: "Health: secondbox whoami && secondbox runners list\nBootstrap an explicit Tenant and Subject before creating a Sandbox.\nSupport: secondbox-deploy install --support " + installerPlannedPath(plan, "deployment") + " --output /secure/path/secondbox-installer-support.tar.gz\nUninstall: secondbox-deploy uninstall " + installerPlannedPath(plan, "deployment") + "\nResume: secondbox-deploy install --resume " + installerPlannedPath(plan, "deployment")})
}

func runInstallUninstall(ctx context.Context, arguments []string, renderer cliui.Renderer) error {
	purge := len(arguments) == 2 && arguments[0] == "--purge"
	if len(arguments) != 1 && !purge {
		return usage(renderer)
	}
	directory := arguments[len(arguments)-1]
	if purge {
		return runInstallPurge(ctx, directory, renderer)
	}
	return runInstallUninstallWith(ctx, directory, renderer, installUninstallDependencies{OwnerUID: os.Getuid(), Now: time.Now, ValidateTeardown: validateComposeTeardownAuthority, ComposeDown: func(ctx context.Context, manifest string) error {
		return deployconfig.RunComposeForAcceptedInstaller(ctx, manifest, "down", deployconfig.SystemComposeExecutor{Input: os.Stdin, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic}, http.DefaultClient)
	}})
}

func runInstallComposeRecovery(ctx context.Context, directory string, renderer cliui.Renderer) error {
	return runInstallComposeRecoveryWith(ctx, directory, renderer, installComposeRecoveryDependencies{
		OwnerUID:         os.Getuid(),
		Now:              time.Now,
		HostVerify:       runPrivilegedHostTeardownVerify,
		ValidateTeardown: validatePartialComposeTeardownAuthority,
		ComposeDown: func(ctx context.Context, manifest string) error {
			return deployconfig.RunComposeForAcceptedInstaller(ctx, manifest, "down", deployconfig.SystemComposeExecutor{Input: os.Stdin, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic}, http.DefaultClient)
		},
	})
}

type installComposeRecoveryDependencies struct {
	OwnerUID         int
	Now              func() time.Time
	HostVerify       func(context.Context, string, string) error
	ValidateTeardown func(install.InstallPlan, install.InstallReceipt) error
	ComposeDown      func(context.Context, string) error
}

func runInstallComposeRecoveryWith(ctx context.Context, directory string, renderer cliui.Renderer, dependencies installComposeRecoveryDependencies) (resultErr error) {
	if dependencies.Now == nil || dependencies.HostVerify == nil || dependencies.ValidateTeardown == nil || dependencies.ComposeDown == nil {
		return errors.New("SecondBox installer Compose recovery: dependencies are incomplete")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	plan, receipt, err := install.ReadOperationReadOnly(absolute, dependencies.OwnerUID)
	if err != nil {
		return err
	}
	if _, active := receipt.ActiveUpdate(); active {
		return errors.New("SecondBox installer Compose recovery: an update is incomplete; resume it before recovery")
	}
	if err := validatePartialComposeRecoveryState(receipt); err != nil {
		return err
	}
	digest, err := install.PlanDigest(plan)
	if err != nil {
		return err
	}
	if err := runInstallPhase(ctx, renderer, "Privileged host verification", "verify reviewed host resources", func() error {
		return dependencies.HostVerify(ctx, absolute, digest)
	}); err != nil {
		return err
	}
	lock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	plan, receipt, err = install.RecoverOperation(absolute, dependencies.OwnerUID, lock)
	if err != nil {
		return err
	}
	if _, active := receipt.ActiveUpdate(); active {
		return errors.New("SecondBox installer Compose recovery: an update is incomplete; resume it before recovery")
	}
	if err := validatePartialComposeRecoveryState(receipt); err != nil {
		return err
	}
	lockedDigest, err := install.PlanDigest(plan)
	if err != nil {
		return err
	}
	if lockedDigest != digest {
		return errors.New("SecondBox installer Compose recovery: accepted plan changed after privileged host verification")
	}
	if err := dependencies.ValidateTeardown(plan, receipt); err != nil {
		return err
	}
	if err := receipt.PrepareComposeRetry(dependencies.Now()); err != nil {
		return err
	}
	if err := install.SaveReceipt(absolute, plan, receipt, dependencies.OwnerUID); err != nil {
		return err
	}
	if err := runInstallPhase(ctx, renderer, "Partial Compose shutdown", "preserve the accepted operation and durable resources", func() error {
		return dependencies.ComposeDown(ctx, installerPlannedPath(plan, "manifest"))
	}); err != nil {
		return err
	}
	return writeDeployReceipt(renderer, "Partial Compose project stopped", []cliui.Pair{{Key: "Project", Value: expectedInstallerComposeProject(plan)}, {Key: "Operation", Value: absolute}, {Key: "Manifest", Value: installerPlannedPath(plan, "manifest")}}, "")
}

func validatePartialComposeRecoveryState(receipt install.InstallReceipt) error {
	composeIndex := slices.Index(install.StageSequence, install.StageComposeStarted)
	runnerIndex := slices.Index(install.StageSequence, install.StageRunnerEnrolled)
	if receipt.Status != install.OperationFailed || slices.Index(install.StageSequence, receipt.FailureStage) < composeIndex || slices.Index(install.StageSequence, lastInstallStage(receipt)) < runnerIndex {
		return errors.New("SecondBox installer Compose recovery: operation must have failed at or after Compose startup")
	}
	return nil
}

func validatePartialComposeTeardownAuthority(plan install.InstallPlan, receipt install.InstallReceipt) error {
	if err := install.ValidateRecordedResources(plan, receipt); err != nil {
		return err
	}
	manifestIndex := slices.IndexFunc(receipt.CreatedResources, func(resource install.CreatedResource) bool {
		return resource.ID == "manifest" && resource.Kind == install.ResourceFile && resource.Stage == install.StageDeploymentMaterialized && resource.Digest != ""
	})
	composeResourceIndex := slices.IndexFunc(receipt.CreatedResources, func(resource install.CreatedResource) bool { return resource.ID == "compose-project" })
	if manifestIndex < 0 {
		return errors.New("SecondBox installer Compose recovery: receipt lacks the materialized deployment manifest")
	}
	resolved, err := deployconfig.ResolveForAcceptedInstaller(installerPlannedPath(plan, "manifest"))
	if err != nil {
		return err
	}
	if expected := expectedInstallerComposeProject(plan); resolved.ComposeProject() != expected {
		return fmt.Errorf("SecondBox installer Compose recovery: manifest project %q differs from accepted project %q", resolved.ComposeProject(), expected)
	}
	composeCompleted := slices.Index(install.StageSequence, lastInstallStage(receipt)) >= slices.Index(install.StageSequence, install.StageComposeStarted)
	if !composeCompleted {
		if composeResourceIndex >= 0 {
			return errors.New("SecondBox installer Compose recovery: incomplete Compose startup unexpectedly recorded a Compose project")
		}
		return nil
	}
	if composeResourceIndex < 0 {
		return errors.New("SecondBox installer Compose recovery: completed Compose startup lacks its project resource")
	}
	resource := receipt.CreatedResources[composeResourceIndex]
	if resource.Kind != install.ResourceComposeProject || resource.Stage != install.StageComposeStarted || resource.Identity != resolved.ComposeProject() {
		return errors.New("SecondBox installer Compose recovery: recorded Compose project differs from the accepted manifest")
	}
	if err := install.ValidateComposeProjectEvidence(receipt, resolved.ComposeProject()); err != nil {
		return err
	}
	return nil
}

func expectedInstallerComposeProject(plan install.InstallPlan) string {
	return "secondbox-" + strings.TrimPrefix(plan.OperationID, "install_")
}

type installUninstallDependencies struct {
	OwnerUID         int
	Now              func() time.Time
	ValidateTeardown func(install.InstallPlan, install.InstallReceipt) error
	ComposeDown      func(context.Context, string) error
}

func runInstallUninstallWith(ctx context.Context, directory string, renderer cliui.Renderer, dependencies installUninstallDependencies) (resultErr error) {
	if dependencies.Now == nil || dependencies.ValidateTeardown == nil || dependencies.ComposeDown == nil {
		return errors.New("SecondBox installer uninstall: dependencies are incomplete")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
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
	if _, active := receipt.ActiveUpdate(); active {
		return errors.New("SecondBox installer uninstall: an update is incomplete; resume it before uninstalling")
	}
	if receipt.Status == install.OperationUninstalled {
		return writeUninstallSummary(renderer, plan)
	}
	if receipt.Status != install.OperationSucceeded && receipt.Status != install.OperationUninstalling {
		return errors.New("SecondBox installer uninstall: only a completed installation can be uninstalled; resume or diagnose this operation first")
	}
	if err := dependencies.ValidateTeardown(plan, receipt); err != nil {
		return err
	}
	if receipt.Status == install.OperationSucceeded {
		if err := receipt.MarkUninstalling(dependencies.Now()); err != nil {
			return err
		}
		if err := install.SaveReceipt(absolute, plan, receipt, dependencies.OwnerUID); err != nil {
			return err
		}
	}
	if err := runInstallPhase(ctx, renderer, "Compose shutdown", "preserve volumes, workspace, authority, artifacts, and manifest", func() error {
		return dependencies.ComposeDown(ctx, installerPlannedPath(plan, "manifest"))
	}); err != nil {
		return err
	}
	if err := finishJournaledUninstall(&receipt, dependencies.Now()); err != nil {
		return err
	}
	if err := install.SaveReceipt(absolute, plan, receipt, dependencies.OwnerUID); err != nil {
		return err
	}
	return writeUninstallSummary(renderer, plan)
}

func finishJournaledUninstall(receipt *install.InstallReceipt, now time.Time) error {
	if err := receipt.MarkUninstalled(now); err != nil {
		return err
	}
	return receipt.MarkResourceRemoved("compose-project", now)
}

func writeUninstallSummary(renderer cliui.Renderer, plan install.InstallPlan) error {
	pairs := []cliui.Pair{{Key: "Manifest", Value: installerPlannedPath(plan, "manifest")}, {Key: "Authority", Value: installerPlannedPath(plan, "secrets")}, {Key: "Artifacts", Value: installerPlannedPath(plan, "artifacts")}, {Key: "Runner identity", Value: installerPlannedPath(plan, "runner-identity")}, {Key: "Workspace", Value: plan.Storage.WorkspacePath}, {Key: "Receipt", Value: filepath.Join(installerPlannedPath(plan, "deployment"), "install-receipt.json")}}
	if renderer.OutputMode == cliui.OutputJSON {
		return writeDeployReceipt(renderer, "SecondBox deployment stopped; durable data preserved", pairs, "")
	}
	return renderer.WriteSummary(cliui.Summary{Title: "SecondBox deployment stopped; durable data preserved", Status: cliui.StatusComplete, Pairs: pairs, Next: "Resume: secondbox-deploy install --resume " + installerPlannedPath(plan, "deployment") + "\nPurge: secondbox-deploy uninstall --purge " + installerPlannedPath(plan, "deployment")})
}

func runInstallPurge(ctx context.Context, directory string, renderer cliui.Renderer) (resultErr error) {
	if renderer.OutputMode == cliui.OutputJSON || (!renderer.Capabilities.Input.TTY && !renderer.Capabilities.Accessible) {
		return &deployExitError{code: 3, err: errors.New("SecondBox installer purge: an interactive terminal or --accessible is required")}
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	plan, receipt, err := install.ReadOperationReadOnly(absolute, os.Getuid())
	if err != nil {
		return err
	}
	if receipt.Status == install.OperationPurged {
		return renderer.WriteSummary(cliui.Summary{Title: "SecondBox installation already purged", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Plan", Value: filepath.Join(absolute, "install-plan.json")}, {Key: "Receipt", Value: filepath.Join(absolute, "install-receipt.json")}}})
	}
	if receipt.Status != install.OperationUninstalled && receipt.Status != install.OperationPurging {
		return errors.New("SecondBox installer purge: run ordinary uninstall and inspect the preserved deployment before purge")
	}
	inspection := "healthy inspection completed"
	if _, inspectErr := deployconfig.Inspect(installerPlannedPath(plan, "manifest")); inspectErr != nil {
		inspection = "deployment inspection failed; typed confirmation explicitly acknowledges unavailable state inspection"
	}
	if _, err := fmt.Fprintln(renderer.Diagnostic, "Permanent purge targets (plan-and-receipt matched):"); err != nil {
		return err
	}
	for _, resource := range receipt.CreatedResources {
		if resource.ID == "operation-directory" || slices.Contains(receipt.RemovedResourceIDs, resource.ID) {
			continue
		}
		target := resource.Path
		if target == "" {
			target = resource.Identity
		}
		if _, err := fmt.Fprintf(renderer.Diagnostic, "  - %s: %s\n", cliui.Sanitize(resource.ID), cliui.Sanitize(target)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(renderer.Diagnostic, "Inspection: "+inspection); err != nil {
		return err
	}
	expected := "PURGE " + plan.OperationID
	answer := ""
	handles := cliui.FormHandles{Input: os.Stdin, Output: renderer.Diagnostic, Width: renderer.Capabilities.Diagnostic.Width, Accessible: renderer.Capabilities.Accessible, Dark: renderer.Capabilities.Diagnostic.Background != cliui.BackgroundLight}
	if err := cliui.PurgeConfirmationForm(expected, &answer).Run(ctx, handles); err != nil {
		return installerFormError(err)
	}
	// Confirmation is deliberately outside the lock, so acquire ownership and
	// re-read the operation before the first destructive action. Marking the
	// durable purge intent prevents a concurrent resume from restoring volumes
	// while the privilege-separated cleanup proceeds.
	purgeLock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	plan, receipt, err = install.RecoverOperation(absolute, os.Getuid(), purgeLock)
	if err == nil && receipt.Status != install.OperationUninstalled && receipt.Status != install.OperationPurging {
		err = errors.New("SecondBox installer purge: operation changed after confirmation; no resources were removed")
	}
	if err == nil && plan.OperationID != strings.TrimPrefix(expected, "PURGE ") {
		err = errors.New("SecondBox installer purge: operation identity changed after confirmation")
	}
	if err == nil && !slices.Contains(receipt.CompletedPurgeSteps, "compose-volumes") {
		err = validateComposeTeardownAuthority(plan, receipt)
	}
	if err == nil && receipt.Status == install.OperationUninstalled {
		err = receipt.MarkPurging(time.Now())
		if err == nil {
			err = install.SaveReceipt(absolute, plan, receipt, os.Getuid())
		}
	}
	closeErr := purgeLock.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	validationLock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	plan, receipt, err = install.RecoverOperation(absolute, os.Getuid(), validationLock)
	if err == nil {
		err = install.ValidatePurgeVerifiedArtifacts(plan, receipt)
	}
	if err == nil {
		err = install.ValidatePurgeUserResources(plan, receipt)
	}
	if closeErr := validationLock.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return err
	}
	digest, err := install.PlanDigest(plan)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	validationCommand := exec.CommandContext(ctx, "sudo", "--", executable, "_install-host-purge-validate", absolute, digest)
	validationCommand.Stdin, validationCommand.Stdout, validationCommand.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := validationCommand.Run(); err != nil {
		return fmt.Errorf("SecondBox installer purge validate privileged resources: %w", err)
	}
	manifestPath := installerPlannedPath(plan, "manifest")
	if !slices.Contains(receipt.CompletedPurgeSteps, "compose-volumes") {
		if _, statErr := os.Lstat(manifestPath); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return errors.New("SecondBox installer purge: manifest is missing before Compose durable-volume purge was journaled")
			}
			return statErr
		}
		if err := runInstallPhase(ctx, renderer, "Compose durable-data purge", "remove the exact bundled database volume", func() error {
			return deployconfig.PurgeComposeVolumesForAcceptedInstaller(ctx, manifestPath, deployconfig.SystemComposeExecutor{Input: os.Stdin, Output: renderer.Diagnostic, Diagnostic: renderer.Diagnostic})
		}); err != nil {
			return err
		}
		stepLock, err := install.AcquireLock(absolute)
		if err != nil {
			return err
		}
		plan, receipt, err = install.RecoverOperation(absolute, os.Getuid(), stepLock)
		if err == nil {
			err = receipt.CompletePurgeStep("compose-volumes", time.Now())
		}
		if err == nil {
			err = install.SaveReceipt(absolute, plan, receipt, os.Getuid())
		}
		if closeErr := stepLock.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		if err != nil {
			return err
		}
	}
	artifactLock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	plan, receipt, err = install.RecoverOperation(absolute, os.Getuid(), artifactLock)
	if err == nil {
		persistArtifacts := func(value install.InstallReceipt) error {
			return install.SaveReceipt(absolute, plan, value, os.Getuid())
		}
		receipt, err = install.PurgeVerifiedArtifacts(plan, receipt, time.Now, persistArtifacts)
	}
	if closeErr := artifactLock.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "sudo", "--", executable, "_install-host-purge", absolute, digest)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("SecondBox installer purge privileged resources: %w", err)
	}
	lock, err := install.AcquireLock(absolute)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	plan, receipt, err = install.RecoverOperation(absolute, os.Getuid(), lock)
	if err != nil {
		return err
	}
	persist := func(value install.InstallReceipt) error {
		return install.SaveReceipt(absolute, plan, value, os.Getuid())
	}
	receipt, err = install.PurgeUserResources(plan, receipt, time.Now, persist)
	if err != nil {
		return err
	}
	if err := receipt.MarkPurged(time.Now()); err != nil {
		return err
	}
	if err := persist(receipt); err != nil {
		return err
	}
	return renderer.WriteSummary(cliui.Summary{Title: "SecondBox installation-owned resources purged", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Operation", Value: plan.OperationID}, {Key: "Plan tombstone", Value: filepath.Join(absolute, "install-plan.json")}, {Key: "Receipt tombstone", Value: filepath.Join(absolute, "install-receipt.json")}}, Next: "The plan and receipt remain as the bounded audit tombstone; all listed installation-owned runtime and durable resources were removed."})
}

func runPrivateHostPurge(ctx context.Context, arguments []string) error {
	if len(arguments) != 2 {
		return errors.New("SecondBox installer private host purge: expected OPERATION_DIRECTORY PLAN_DIGEST")
	}
	uidText, present := os.LookupEnv("SUDO_UID")
	uid, err := strconv.Atoi(uidText)
	if !present || err != nil || uid < 0 {
		return errors.New("SecondBox installer private host purge: SUDO_UID is required and must be a non-negative integer")
	}
	_, err = install.PurgeAcceptedHost(ctx, arguments[0], arguments[1], uid, time.Now)
	if err != nil {
		return fmt.Errorf("SecondBox installer private host purge: %w", err)
	}
	return nil
}

func runPrivateHostPurgeValidate(arguments []string) error {
	if len(arguments) != 2 {
		return errors.New("SecondBox installer private host purge validation: expected OPERATION_DIRECTORY PLAN_DIGEST")
	}
	uidText, present := os.LookupEnv("SUDO_UID")
	uid, err := strconv.Atoi(uidText)
	if !present || err != nil || uid < 0 {
		return errors.New("SecondBox installer private host purge validation: SUDO_UID is required and must be a non-negative integer")
	}
	if err := install.ValidateAcceptedHostPurge(arguments[0], arguments[1], uid); err != nil {
		return fmt.Errorf("SecondBox installer private host purge validation: %w", err)
	}
	return nil
}

var _ io.Writer = (*boundedCommandBuffer)(nil)
