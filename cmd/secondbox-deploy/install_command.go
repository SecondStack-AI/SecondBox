package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/buildinfo"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

type guidedInstallDependencies struct {
	Input          io.Reader
	Now            func() time.Time
	HomeDirectory  func() (string, error)
	AvailableBytes func(string) (int64, error)
	VerifyRelease  func(context.Context, string) (releaseverify.VerifiedRelease, error)
	OperationID    func() (string, error)
	MakeDirectory  func(string) error
	WriteAccepted  func(string, install.InstallPlan, install.InstallReceipt) (string, string, error)
	RunForm        func(context.Context, cliui.Form, cliui.FormHandles) error
	HostApply      func(context.Context, string, string) error
	Continue       func(context.Context, string) error
}

func systemGuidedInstallDependencies() guidedInstallDependencies {
	return guidedInstallDependencies{
		Input:         os.Stdin,
		Now:           time.Now,
		HomeDirectory: os.UserHomeDir,
		AvailableBytes: func(path string) (int64, error) {
			for {
				if _, err := os.Lstat(path); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					return 0, err
				}
				next := filepath.Dir(path)
				if next == path {
					return 0, errors.New("SecondBox installer capacity path has no existing ancestor")
				}
				path = next
			}
			var value syscall.Statfs_t
			if err := syscall.Statfs(path, &value); err != nil {
				return 0, err
			}
			return int64(value.Bavail) * int64(value.Bsize), nil
		},
		VerifyRelease: func(ctx context.Context, location string) (releaseverify.VerifiedRelease, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			return verifyReleaseLocationWithContext(ctx, location)
		},
		OperationID:   install.NewOperationID,
		MakeDirectory: func(path string) error { return os.Mkdir(path, 0o700) },
		WriteAccepted: install.WriteAccepted,
		RunForm: func(ctx context.Context, form cliui.Form, handles cliui.FormHandles) error {
			return form.Run(ctx, handles)
		},
		HostApply: func(ctx context.Context, directory, digest string) error {
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("SecondBox installer resolve deployment binary: %w", err)
			}
			command := exec.CommandContext(ctx, "sudo", "--", executable, "_install-host-apply", directory, digest)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := command.Run(); err != nil {
				return fmt.Errorf("SecondBox installer privileged host apply: %w", err)
			}
			return nil
		},
	}
}

func runGuidedInstall(ctx context.Context, renderer cliui.Renderer, facts install.HostFacts, advanced bool) error {
	dependencies := systemGuidedInstallDependencies()
	dependencies.Continue = func(ctx context.Context, directory string) error {
		return runInstallResumeWith(ctx, directory, renderer, systemInstallResumeDependencies(renderer))
	}
	return runGuidedInstallWith(ctx, renderer, facts, advanced, dependencies)
}

func runInstallCandidate(ctx context.Context, directory string, renderer cliui.Renderer) error {
	guide := func(ctx context.Context, renderer cliui.Renderer, facts install.HostFacts, advanced bool) error {
		dependencies := systemGuidedInstallDependencies()
		dependencies.VerifyRelease = func(ctx context.Context, _ string) (releaseverify.VerifiedRelease, error) {
			return verifyCandidateDirectory(ctx, directory)
		}
		dependencies.Continue = func(ctx context.Context, operation string) error {
			return runInstallResumeWith(ctx, operation, renderer, candidateInstallResumeDependencies(renderer, directory))
		}
		return runGuidedInstallWith(ctx, renderer, facts, advanced, dependencies)
	}
	return runInstallPreflightWithGuide(ctx, nil, renderer, func(ctx context.Context) (install.HostFacts, error) {
		return install.Preflight(ctx, install.SystemPreflightProbes())
	}, guide)
}

func verifyCandidateDirectory(ctx context.Context, directory string) (releaseverify.VerifiedRelease, error) {
	verified, err := releaseverify.CandidateDirectory(ctx, directory)
	if err != nil {
		return releaseverify.VerifiedRelease{}, err
	}
	if verified.Manifest.Version != buildinfo.Version || verified.Manifest.SourceCommit != buildinfo.SourceCommit {
		return releaseverify.VerifiedRelease{}, fmt.Errorf("SecondBox installer candidate identity %s at %s differs from running binary %s at %s", verified.Manifest.Version, verified.Manifest.SourceCommit, buildinfo.Version, buildinfo.SourceCommit)
	}
	return verified, nil
}

func runGuidedInstallWith(ctx context.Context, renderer cliui.Renderer, facts install.HostFacts, advanced bool, dependencies guidedInstallDependencies) error {
	if renderer.OutputMode == cliui.OutputJSON {
		return &deployExitError{code: 3, err: errors.New("SecondBox installer: guided installation does not accept --output json; use install --check for JSON host facts")}
	}
	if !renderer.Capabilities.Input.TTY && !renderer.Capabilities.Accessible {
		return &deployExitError{code: 3, err: errors.New("SecondBox installer: guided installation requires a terminal or --accessible; use install --check for unattended preflight")}
	}
	if buildinfo.Version == "0.0.0-development" || buildinfo.SourceCommit == "development" {
		return &deployExitError{code: 3, err: fmt.Errorf("SecondBox installer: guided installation requires a published qualified release binary, got version %q and source commit %q; local development binaries cannot select release assets, so publish and download a release containing this installer first", buildinfo.Version, buildinfo.SourceCommit)}
	}
	if _, err := releasecontract.ParseTag("v" + buildinfo.Version); err != nil {
		return &deployExitError{code: 3, err: fmt.Errorf("SecondBox installer: guided installation requires a versioned release binary, got %q", buildinfo.Version)}
	}
	location := releasecontract.ArtifactManifestLocation(buildinfo.Version)
	activity, err := renderer.StartActivity(ctx, "Verify release "+buildinfo.Version)
	if err != nil {
		return err
	}
	verified, verifyErr := dependencies.VerifyRelease(ctx, location)
	if verifyErr != nil {
		return errors.Join(verifyErr, activity.Complete(cliui.StatusFailed, "release verification failed"))
	}
	if err := activity.Complete(cliui.StatusComplete, "manifest and referenced release objects verified"); err != nil {
		return err
	}
	home, err := dependencies.HomeDirectory()
	if err != nil {
		return fmt.Errorf("SecondBox installer home directory: %w", err)
	}
	backingAvailable, err := dependencies.AvailableBytes("/var/lib")
	if err != nil {
		return fmt.Errorf("SecondBox installer backing capacity: %w", err)
	}
	operationID, err := dependencies.OperationID()
	if err != nil {
		return err
	}
	deploymentDirectory := filepath.Join(home, "secondbox-"+operationID)
	deploymentAvailable, err := dependencies.AvailableBytes(deploymentDirectory)
	if err != nil {
		return fmt.Errorf("SecondBox installer deployment capacity: %w", err)
	}
	release := releasePlan(verified, location)
	options := install.StorageOptions(facts, backingAvailable, release.ExpectedDownloadBytes)
	if len(options) == 0 {
		return &deployExitError{code: 2, err: errors.New("SecondBox installer: no safe workspace storage option has sufficient capacity")}
	}
	selected := "0"
	formOptions := make([]cliui.Option, 0, len(options))
	for index, option := range options {
		formOptions = append(formOptions, cliui.Option{Label: option.Label, Value: strconv.Itoa(index)})
	}
	handles := cliui.FormHandles{Input: dependencies.Input, Output: renderer.Diagnostic, Width: renderer.Capabilities.Diagnostic.Width, Accessible: renderer.Capabilities.Accessible, Dark: renderer.Capabilities.Diagnostic.Background != cliui.BackgroundLight}
	if err := dependencies.RunForm(ctx, cliui.WorkspaceChoiceForm(formOptions, &selected), handles); err != nil {
		return installerFormError(err)
	}
	selectedIndex, err := strconv.Atoi(selected)
	if err != nil || selectedIndex < 0 || selectedIndex >= len(options) {
		return errors.New("SecondBox installer: workspace form returned an invalid selection")
	}
	standardBundlesAccepted := false
	if err := dependencies.RunForm(ctx, cliui.StandardBundleSelectionForm(&standardBundlesAccepted), handles); err != nil {
		return installerFormError(err)
	}
	if !standardBundlesAccepted {
		return errors.New("SecondBox installer: standard Profile bundles were not explicitly selected")
	}
	retention := "86400"
	if err := dependencies.RunForm(ctx, cliui.RetentionChoiceForm(&retention), handles); err != nil {
		return installerFormError(err)
	}
	retentionSeconds, err := strconv.ParseInt(retention, 10, 64)
	if err != nil || retentionSeconds <= 0 {
		return errors.New("SecondBox installer: retention form returned an invalid selection")
	}
	input := install.ProposalInput{OperationID: operationID, CreatedAt: dependencies.Now().UTC(), DeploymentDirectory: deploymentDirectory, BinaryDirectory: filepath.Join(home, ".local", "bin"), CLIConfigPath: filepath.Join(home, ".config", "secondbox", "config.json"), CLITenantRef: "local-tenant", CLISubjectRef: "local-operator", BackingAvailableBytes: backingAvailable, DeploymentAvailableBytes: deploymentAvailable, Release: release, StorageChoice: options[selectedIndex].Choice, ExistingMountpoint: options[selectedIndex].Mountpoint, StandardBundles: []string{"agent-compartment", "durable-coding"}, RetentionSeconds: retentionSeconds}
	plan, err := install.ProposePlan(facts, input)
	if err != nil {
		return err
	}
	if advanced {
		if err := reviewAdvancedInstallSettings(ctx, dependencies, handles, facts, &input, &plan); err != nil {
			return installerFormError(err)
		}
	}
	capacityAccepted := false
	capacitySummary := fmt.Sprintf("%d Sandboxes; %d concurrent starts; %s workspace; %s Runner memory", plan.Capacity.MaxSandboxes, plan.Capacity.ConcurrentStarts, humanBytes(plan.Capacity.MaxWorkspaceBytes), humanBytes(plan.Capacity.MaxMemoryBytes))
	if err := dependencies.RunForm(ctx, cliui.CapacityReviewForm(capacitySummary, &capacityAccepted), handles); err != nil {
		return installerFormError(err)
	}
	finalAccepted := false
	review := install.RenderPlanReview(plan)
	if err := dependencies.RunForm(ctx, cliui.FinalInstallConfirmationForm(review, &finalAccepted), handles); err != nil {
		return installerFormError(err)
	}
	if err := dependencies.MakeDirectory(input.DeploymentDirectory); err != nil {
		return fmt.Errorf("SecondBox installer create operation directory: %w", err)
	}
	receipt, err := install.NewReceipt(plan, input.CreatedAt)
	if err != nil {
		return err
	}
	if err := receipt.CompleteStage(install.StagePreflight, dependencies.Now().UTC(), map[string]string{"hostFactsDigest": plan.HostFactsDigest}); err != nil {
		return err
	}
	if err := receipt.CompleteStage(install.StagePlanAccepted, dependencies.Now().UTC(), map[string]string{"reviewed": "true"}); err != nil {
		return err
	}
	if err := receipt.AppendResource(install.CreatedResource{ID: "operation-directory", Kind: install.ResourceDirectory, Path: input.DeploymentDirectory, Class: install.PathUserDeployment, Stage: install.StagePlanAccepted, Mode: 0o700, OwnerUID: facts.InvokingUID, OwnerGID: facts.InvokingGID}); err != nil {
		return err
	}
	planPath, receiptPath, err := dependencies.WriteAccepted(input.DeploymentDirectory, plan, receipt)
	if err != nil {
		return err
	}
	planDigest, err := install.PlanDigest(plan)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(renderer.Diagnostic, "Privileged host actions accepted for sudo:"); err != nil {
		return err
	}
	for _, action := range plan.PrivilegedActions {
		if _, err := fmt.Fprintln(renderer.Diagnostic, "  - "+cliui.Sanitize(action)); err != nil {
			return err
		}
	}
	if dependencies.HostApply == nil {
		return errors.New("SecondBox installer: privileged host-apply dependency is absent")
	}
	if err := dependencies.HostApply(ctx, input.DeploymentDirectory, planDigest); err != nil {
		return err
	}
	if dependencies.Continue != nil {
		return dependencies.Continue(ctx, input.DeploymentDirectory)
	}
	return renderer.WriteSummary(cliui.Summary{Title: "Host preparation complete", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Operation", Value: operationID}, {Key: "Plan", Value: planPath}, {Key: "Receipt", Value: receiptPath}, {Key: "Workspace", Value: plan.Storage.WorkspacePath}}, Next: "Resume this operation to verify and materialize the release deployment."})
}

func runPrivateHostApply(ctx context.Context, arguments []string) error {
	if len(arguments) != 2 {
		return errors.New("SecondBox installer private host apply: expected OPERATION_DIRECTORY PLAN_DIGEST")
	}
	uidText, present := os.LookupEnv("SUDO_UID")
	uid, err := strconv.Atoi(uidText)
	if !present || err != nil || uid < 0 {
		return errors.New("SecondBox installer private host apply: SUDO_UID is required and must be a non-negative integer")
	}
	_, err = install.ApplyAcceptedHost(ctx, arguments[0], arguments[1], uid, install.SystemHostApplyExecutor{CallerUID: uid}, time.Now)
	if err != nil {
		return fmt.Errorf("SecondBox installer private host apply: %w", err)
	}
	return nil
}

func reviewAdvancedInstallSettings(ctx context.Context, dependencies guidedInstallDependencies, handles cliui.FormHandles, facts install.HostFacts, input *install.ProposalInput, plan *install.InstallPlan) error {
	api := portFromAddress(plan.Network.APIAddress)
	runner := portFromAddress(plan.Network.RunnerAddress)
	data := portFromAddress(plan.Network.DataPlaneAddress)
	database := portFromAddress(plan.Network.DatabaseAddress)
	objectStore := portFromAddress(plan.Network.ObjectStoreAddress)
	objectStoreConsole := portFromAddress(plan.Network.ObjectStoreConsoleAddress)
	uidStart := strconv.FormatInt(plan.Network.JailerUIDRange.Start, 10)
	retention := strconv.FormatInt(plan.RetentionSeconds, 10)
	imageGiB := strconv.FormatInt(plan.Storage.ImageSizeBytes>>30, 10)
	bindings := []cliui.TextBinding{
		{Title: "Deployment directory", Value: &input.DeploymentDirectory, Validate: validateInstallerPath},
		{Title: "Binary directory", Value: &input.BinaryDirectory, Validate: validateInstallerPath},
		{Title: "API loopback port", Value: &api, Validate: validateInstallerPort},
		{Title: "Runner control loopback port", Value: &runner, Validate: validateInstallerPort},
		{Title: "Data-plane loopback port", Value: &data, Validate: validateInstallerPort},
		{Title: "Database loopback port", Value: &database, Validate: validateInstallerPort},
		{Title: "Object-store loopback port", Value: &objectStore, Validate: validateInstallerPort},
		{Title: "Object-store console loopback port", Value: &objectStoreConsole, Validate: validateInstallerPort},
		{Title: "Guest bridge CIDR", Value: &plan.Network.GuestBridgeCIDR, Validate: validateInstallerCIDR},
		{Title: "Compose backend CIDR", Value: &plan.Network.ComposeBackendCIDR, Validate: validateInstallerComposeCIDR},
		{Title: "TAP prefix", Value: &plan.Network.TAPPrefix, Validate: validateInstallerName},
		{Title: "Cgroup parent", Value: &plan.Network.CgroupParent, Validate: validateInstallerName},
		{Title: "Non-loopback DNS upstream", Value: &plan.Network.DNSUpstream, Validate: validateInstallerIP},
		{Title: "Jailer UID range start", Value: &uidStart, Validate: validateInstallerUIDRange(plan.Network.JailerUIDRange.Count)},
		{Title: "Retention seconds", Value: &retention, Validate: validateInstallerPositive},
	}
	if input.StorageChoice == install.StorageBtrfsImage {
		bindings = append(bindings, cliui.TextBinding{Title: "Fully allocated filesystem image GiB", Value: &imageGiB, Validate: validateInstallerPositive})
	}
	if err := dependencies.RunForm(ctx, cliui.AdvancedSettingsForm(bindings), handles); err != nil {
		return err
	}
	apiPort, _ := strconv.Atoi(api)
	runnerPort, _ := strconv.Atoi(runner)
	dataPort, _ := strconv.Atoi(data)
	databasePort, _ := strconv.Atoi(database)
	objectStorePort, _ := strconv.Atoi(objectStore)
	objectStoreConsolePort, _ := strconv.Atoi(objectStoreConsole)
	uid, _ := strconv.ParseInt(uidStart, 10, 64)
	retentionSeconds, _ := strconv.ParseInt(retention, 10, 64)
	input.RetentionSeconds = retentionSeconds
	input.NetworkOverrides = install.NetworkOverrides{APIPort: apiPort, RunnerPort: runnerPort, DataPlanePort: dataPort, DatabasePort: databasePort, ObjectStorePort: objectStorePort, ObjectStoreConsolePort: objectStoreConsolePort, GuestCIDR: plan.Network.GuestBridgeCIDR, ComposeCIDR: plan.Network.ComposeBackendCIDR, TAPPrefix: plan.Network.TAPPrefix, CgroupParent: plan.Network.CgroupParent, DNSUpstream: plan.Network.DNSUpstream, JailerUID: install.UIDRange{Start: uid, Count: plan.Network.JailerUIDRange.Count}}
	if input.StorageChoice == install.StorageBtrfsImage {
		gib, _ := strconv.ParseInt(imageGiB, 10, 64)
		input.FilesystemImageBytes = gib << 30
	}
	deploymentAvailable, err := dependencies.AvailableBytes(input.DeploymentDirectory)
	if err != nil {
		return fmt.Errorf("SecondBox installer deployment capacity: %w", err)
	}
	input.DeploymentAvailableBytes = deploymentAvailable
	updated, err := install.ProposePlan(facts, *input)
	if err != nil {
		return err
	}
	*plan = updated
	return nil
}

func installerFormError(err error) error {
	if errors.Is(err, context.Canceled) {
		return &deployExitError{code: 130, err: errors.New("SecondBox installer: installation cancelled")}
	}
	return err
}

func releasePlan(verified releaseverify.VerifiedRelease, location string) install.ReleasePlan {
	manifest := verified.Manifest
	images := map[string]string{"control-plane": manifest.ControlPlane.Reference, "runner": manifest.Runner.Reference, "microvm-artifacts": manifest.MicroVM.ImageReference, "installer-tools": manifest.InstallerTools.Reference, "postgres": manifest.BundledServices.Postgres, "object-store": manifest.BundledServices.ObjectStore, "object-store-client": manifest.BundledServices.ObjectStoreClient}
	binaries := map[string]string{}
	for _, binary := range manifest.Binaries {
		if binary.Platform == "linux/amd64" && (binary.Name == "secondbox" || binary.Name == "secondbox-deploy") {
			binaries[binary.Name] = binary.SHA256
		}
	}
	return install.ReleasePlan{Version: manifest.Version, ArtifactManifestURL: location, ArtifactManifestDigest: releasecontract.Digest(verified.ManifestBytes), SigningKeyFingerprint: manifest.MicroVM.SigningKeyFingerprint, Images: images, BinaryDigests: binaries, ExpectedDownloadBytes: install.ExecutionBundleEstimateBytes}
}

func humanBytes(value int64) string {
	return fmt.Sprintf("%.1f GiB", float64(value)/float64(1<<30))
}

func validateInstallerPort(value string) error {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1024 || port > 65535 {
		return errors.New("enter an unprivileged TCP port between 1024 and 65535")
	}
	return nil
}

func validateInstallerPath(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" || strings.ContainsAny(value, "*$?[]{}") {
		return errors.New("enter an absolute normalized non-root path without variables or globs")
	}
	return nil
}

func validateInstallerCIDR(value string) error {
	ip, network, err := net.ParseCIDR(strings.TrimSpace(value))
	ones, bits := 0, 0
	if err == nil {
		ones, bits = network.Mask.Size()
	}
	if err != nil || ip.To4() == nil || bits != 32 || ones > 30 || !network.IP.Equal(ip) {
		return errors.New("enter a canonical IPv4 CIDR with at least two usable host addresses")
	}
	return nil
}

func validateInstallerComposeCIDR(value string) error {
	if err := validateInstallerCIDR(value); err != nil {
		return err
	}
	ip, network, _ := net.ParseCIDR(strings.TrimSpace(value))
	ones, _ := network.Mask.Size()
	if ones != 24 || !ip.IsPrivate() {
		return errors.New("enter an RFC1918 IPv4 /24 for the Compose backend")
	}
	return nil
}

func validateInstallerIP(value string) error {
	address := net.ParseIP(strings.TrimSpace(value))
	if address == nil || address.IsLoopback() || address.IsUnspecified() {
		return errors.New("enter a non-loopback IP address")
	}
	return nil
}

func validateInstallerName(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " /\\*$?[]{}") {
		return errors.New("enter a non-empty name without whitespace, paths, variables, or globs")
	}
	return nil
}

func validateInstallerUIDRange(count int64) func(string) error {
	return func(value string) error {
		uid, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		maximumUID := int64(^uint32(0))
		if err != nil || uid < 10000 || count < 1 || uid > maximumUID || count > maximumUID-uid+1 {
			return errors.New("enter an unprivileged UID range that fits Linux 32-bit user IDs")
		}
		return nil
	}
}

func validateInstallerPositive(value string) error {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return errors.New("enter a positive integer")
	}
	return nil
}

func portFromAddress(address string) string {
	_, port, found := strings.Cut(address, ":")
	if !found {
		return ""
	}
	return port
}
