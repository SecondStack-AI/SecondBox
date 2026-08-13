package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

func TestInstallResumeOrchestratesEveryDurableStageWithoutPrintingSecrets(t *testing.T) {
	operation := filepath.Join(t.TempDir(), "operation")
	verified := fakeGuidedRelease()
	plan, err := install.ProposePlan(guidedFacts(), install.ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: time.Now(), DeploymentDirectory: operation, BinaryDirectory: filepath.Join(filepath.Dir(operation), "bin"), CLIConfigPath: filepath.Join(filepath.Dir(operation), "config", "secondbox", "config.json"), BackingAvailableBytes: 100 << 30, DeploymentAvailableBytes: 100 << 30, Release: releasePlan(verified, releasecontract.ArtifactManifestLocation("0.4.0")), StorageChoice: install.StorageBtrfsImage, StandardBundles: []string{"agent-compartment", "durable-coding", "agent-compartment-isolated"}, RetentionSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(operation, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt, err := install.NewReceipt(plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []install.Stage{install.StagePreflight, install.StagePlanAccepted, install.StageHostApply} {
		if err := receipt.CompleteStage(stage, time.Now(), map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := install.WriteAccepted(operation, plan, receipt); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	now := time.Now()
	dependencies := installResumeDependencies{
		OwnerUID: os.Getuid(), Now: func() time.Time { now = now.Add(time.Second); return now },
		HostApply:  func(context.Context, string, string) error { calls = append(calls, "host-apply"); return nil },
		Revalidate: func(install.InstallPlan) error { calls = append(calls, "revalidate"); return nil },
		VerifyRelease: func(context.Context, string) (releaseverify.VerifiedRelease, error) {
			calls = append(calls, "verify-release")
			return verified, nil
		},
		Postconditions: func(install.InstallPlan, install.InstallReceipt, releaseverify.VerifiedRelease) error {
			calls = append(calls, "postconditions")
			return nil
		},
		Materialize: func(_ context.Context, _ install.InstallPlan, current install.InstallReceipt, _ releaseverify.VerifiedRelease, persist func(install.InstallReceipt) error) (install.InstallReceipt, install.VerifiedArtifact, error) {
			calls = append(calls, "materialize")
			if err := current.CompleteStage(install.StageReleaseVerified, now, map[string]string{"verified": "true"}); err != nil {
				return current, install.VerifiedArtifact{}, err
			}
			if err := persist(current); err != nil {
				return current, install.VerifiedArtifact{}, err
			}
			if err := current.CompleteStage(install.StageAssetsMaterialized, now, map[string]string{"verified": "true"}); err != nil {
				return current, install.VerifiedArtifact{}, err
			}
			return current, install.VerifiedArtifact{ManifestDigest: "sha256:" + strings.Repeat("d", 64), SigningKeyID: strings.Repeat("a", 64)}, persist(current)
		},
		Initialize: func(plan install.InstallPlan, _ releaseverify.VerifiedRelease, _ install.VerifiedArtifact) (deployconfig.SingleHostInstallResult, error) {
			calls = append(calls, "initialize")
			skip := map[string]bool{"deployment": true, "runner-identity": true, "compose-environment": true, "compose-assets": true, "cli-config-root": true, "cli-config-directory": true, "cli-config": true, "binary-directory-root": true, "binary-directory": true, "secondbox-binary": true, "secondbox-deploy-binary": true, "artifacts": true, "workspace": true}
			for _, planned := range plan.Paths {
				if skip[planned.Name] || planned.RequiresSudo {
					continue
				}
				if planned.Kind == install.ResourceDirectory {
					if err := os.Mkdir(planned.Path, os.FileMode(planned.Mode)); err != nil {
						return deployconfig.SingleHostInstallResult{}, err
					}
				} else if err := os.WriteFile(planned.Path, []byte("fixture"), os.FileMode(planned.Mode)); err != nil {
					return deployconfig.SingleHostInstallResult{}, err
				}
			}
			manifest := installerPlannedPath(plan, "manifest")
			if err := os.WriteFile(manifest, []byte("explicit manifest"), 0o600); err != nil {
				return deployconfig.SingleHostInstallResult{}, err
			}
			return deployconfig.SingleHostInstallResult{ManifestPath: manifest, RunnerID: "runner-0123456789abcdef", RunnerIdentityDirectory: installerPlannedPath(plan, "runner-identity")}, nil
		},
		Enroll: func(deployconfig.SingleHostInstallResult) error { calls = append(calls, "enroll"); return nil },
		Compose: func(_ context.Context, _ string, action string) error {
			calls = append(calls, "compose-"+action)
			if action == "up" {
				if err := os.WriteFile(installerPlannedPath(plan, "compose-environment"), []byte("generated"), 0o600); err != nil {
					return err
				}
				if err := os.Mkdir(installerPlannedPath(plan, "compose-assets"), 0o700); err != nil {
					return err
				}
			}
			return nil
		},
		ComposeProject: func(string) (string, error) { return "secondbox-test", nil },
		Login: func(_ context.Context, plan install.InstallPlan) ([]install.CreatedResource, error) {
			calls = append(calls, "login")
			return []install.CreatedResource{{ID: "cli-config", Kind: install.ResourceFile, Path: plan.CLI.ConfigPath, Class: install.PathUserDeployment, Stage: install.StageCLILogin, Mode: 0o600, OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid()), Digest: "sha256:" + strings.Repeat("a", 64)}}, nil
		},
		Readiness: func(context.Context, install.InstallPlan) (map[string]string, error) {
			calls = append(calls, "readiness")
			return map[string]string{"runnerState": "ready"}, nil
		},
		Smoke: func(context.Context, install.InstallPlan) (map[string]string, error) {
			calls = append(calls, "smoke")
			return map[string]string{
				"qualification": "authenticated-runner-readiness",
				"runnerId":      "runner-0123456789abcdef",
				"runnerPool":    standardresources.PoolAMD64,
			}, nil
		},
	}
	var output, diagnostic bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: &diagnostic, Capabilities: cliui.ForWriter(&output, &diagnostic), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	if err := runInstallResumeWith(context.Background(), operation, renderer, dependencies); err != nil {
		t.Fatalf("resume: %v\n%s", err, diagnostic.String())
	}
	_, final, err := install.ReadOperation(operation, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != install.OperationSucceeded || len(final.CompletedStages) != len(install.StageSequence) {
		t.Fatalf("final receipt = status %s stages %#v", final.Status, final.CompletedStages)
	}
	smokeEvidence := final.CompletedStages[len(final.CompletedStages)-1].Evidence
	if smokeEvidence["qualification"] != "authenticated-runner-readiness" ||
		smokeEvidence["runnerId"] != "runner-0123456789abcdef" {
		t.Fatalf("installation qualification receipt evidence = %#v", smokeEvidence)
	}
	wantCalls := []string{"host-apply", "revalidate", "verify-release", "postconditions", "materialize", "initialize", "enroll", "compose-prepare", "compose-up", "login", "readiness", "smoke"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v", calls)
	}
	combined := output.String() + diagnostic.String()
	for _, secret := range []string{"platform-token", "Bearer ", `"token"`} {
		if strings.Contains(combined, secret) {
			t.Fatalf("installer presentation exposed %q: %s", secret, combined)
		}
	}
	calls = nil
	if err := runInstallResumeWith(context.Background(), operation, renderer, dependencies); err != nil {
		t.Fatalf("repeat resume: %v", err)
	}
	if want := "host-apply,revalidate,verify-release,postconditions,readiness"; strings.Join(calls, ",") != want {
		t.Fatalf("repeat resume calls = %#v, want %s", calls, want)
	}
}

func TestInstalledRunnerReadinessRequiresExactAuthenticatedColdBootCapacity(t *testing.T) {
	plan := install.InstallPlan{OperationID: "install_0123456789abcdef"}
	ready := contracts.Runner{ID: "runner-0123456789abcdef", PoolName: standardresources.PoolAMD64, State: "ready", CredentialState: "pre_shared", Architectures: []string{standardresources.ArchitectureAMD64}, Capabilities: []string{"compute", "network-policy", "storage", "cleanup", "local-workspace"}, Capacity: map[string]int64{"VCPUCount": install.DurableCodingVCPUCount, "MemoryBytes": install.DurableCodingMemoryBytes, "DiskBytes": install.MinimumWorkspaceBytes, "Instances": 1, "Operations": install.DurableCodingConcurrentOperations}}
	if evidence, ok := installedRunnerReadinessEvidence(plan, []contracts.Runner{{ID: "runner-unrelated", State: "ready"}, ready}); !ok || evidence["runnerId"] != ready.ID || evidence["runnerPool"] != standardresources.PoolAMD64 || evidence["coldBootCapacity"] != "advertised" || evidence["concurrentOperationCapacity"] != "16" {
		t.Fatalf("exact readiness evidence = %#v, %t", evidence, ok)
	}
	for _, mutate := range []func(*contracts.Runner){
		func(runner *contracts.Runner) { runner.ID = "runner-other" },
		func(runner *contracts.Runner) { runner.PoolName = "other-pool" },
		func(runner *contracts.Runner) { runner.CredentialState = "active" },
		func(runner *contracts.Runner) { runner.Capabilities = []string{"local-workspace"} },
		func(runner *contracts.Runner) { runner.Capacity["MemoryBytes"]-- },
		func(runner *contracts.Runner) { runner.Capacity["Operations"]-- },
	} {
		candidate := ready
		candidate.Architectures = slices.Clone(ready.Architectures)
		candidate.Capabilities = slices.Clone(ready.Capabilities)
		candidate.Capacity = maps.Clone(ready.Capacity)
		mutate(&candidate)
		if evidence, ok := installedRunnerReadinessEvidence(plan, []contracts.Runner{candidate}); ok {
			t.Fatalf("insufficient Runner accepted: %#v %#v", candidate, evidence)
		}
	}
}

func TestInstalledSmokeRequiresAuthenticatedRunnerPoolAndRunnerReadiness(t *testing.T) {
	plan := install.InstallPlan{OperationID: "install_0123456789abcdef"}
	pool := contracts.RunnerPool{Name: standardresources.PoolAMD64, State: contracts.RunnerPoolStateReady, ReadyRunnerCount: 1}
	runner := contracts.Runner{
		ID: "runner-0123456789abcdef", PoolName: standardresources.PoolAMD64, State: "ready", CredentialState: "pre_shared",
		Architectures: []string{standardresources.ArchitectureAMD64},
		Capabilities:  []string{"compute", "network-policy", "storage", "cleanup", "local-workspace"},
		Capacity: map[string]int64{
			"CPUMillis": install.DurableCodingCPUMillis, "MemoryBytes": install.DurableCodingMemoryBytes,
			"DiskBytes": install.MinimumWorkspaceBytes, "Instances": 1,
			"Operations": install.DurableCodingConcurrentOperations,
		},
	}
	evidence, err := installedSmokeEvidence(plan, pool, runner)
	if err != nil {
		t.Fatal(err)
	}
	if evidence["qualification"] != "authenticated-runner-readiness" {
		t.Fatalf("qualification evidence = %#v", evidence)
	}
	pool.ReadyRunnerCount = 0
	if _, err := installedSmokeEvidence(plan, pool, runner); err == nil {
		t.Fatal("unready RunnerPool was accepted")
	}
}

func TestInstalledSmokeUsesRealCLIInspectionGrammar(t *testing.T) {
	const platformToken = "installer-platform-token-at-least-24-bytes"
	plan := install.InstallPlan{
		OperationID: "install_0123456789abcdef",
		CLI:         install.CLIPlan{ConfigPath: filepath.Join(t.TempDir(), "config.json")},
		Paths: []install.PlannedPath{{
			Name: "secondbox-binary", Path: filepath.Join(t.TempDir(), "secondbox"),
		}},
	}
	pool := contracts.RunnerPool{
		Name: standardresources.PoolAMD64, State: contracts.RunnerPoolStateReady,
		ReadyRunnerCount: 1,
	}
	runner := contracts.Runner{
		ID: "runner-0123456789abcdef", PoolName: standardresources.PoolAMD64,
		State: "ready", CredentialState: "pre_shared",
		Architectures: []string{standardresources.ArchitectureAMD64},
		Capabilities:  []string{"compute", "network-policy", "storage", "cleanup", "local-workspace"},
		Capacity: map[string]int64{
			"CPUMillis": install.DurableCodingCPUMillis, "MemoryBytes": install.DurableCodingMemoryBytes,
			"DiskBytes": install.MinimumWorkspaceBytes, "Instances": 1,
			"Operations": install.DurableCodingConcurrentOperations,
		},
	}
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+platformToken {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		requests <- request.URL.Path
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/runner-pools/" + standardresources.PoolAMD64:
			_ = json.NewEncoder(writer).Encode(pool)
		case "/v1/runners/" + runner.ID:
			_ = json.NewEncoder(writer).Encode(runner)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	configuration, err := json.Marshal(map[string]string{
		"url": server.URL, "token": platformToken, "authorityKind": "platform",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.CLI.ConfigPath, configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", installerPlannedPath(plan, "secondbox-binary"), "./cmd/secondbox")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real SecondBox CLI: %v: %s", err, output)
	}

	evidence, err := runInstalledSmoke(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if evidence["qualification"] != "authenticated-runner-readiness" ||
		evidence["runnerId"] != runner.ID || evidence["runnerPool"] != pool.Name {
		t.Fatalf("real CLI qualification evidence = %#v", evidence)
	}
	updateEvidence, err := runInstalledUpdateSmoke(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if updateEvidence["qualification"] != "authenticated-runner-readiness" ||
		updateEvidence["postUpdateState"] != "runner-ready" {
		t.Fatalf("real CLI update qualification evidence = %#v", updateEvidence)
	}
	wantRequests := []string{
		"/v1/runner-pools/" + standardresources.PoolAMD64, "/v1/runners/" + runner.ID,
		"/v1/runner-pools/" + standardresources.PoolAMD64, "/v1/runners/" + runner.ID,
	}
	for index, want := range wantRequests {
		if got := <-requests; got != want {
			t.Fatalf("real CLI qualification request %d = %q, want %q", index, got, want)
		}
	}
}

func TestQualifiedGuestUsesRunnerReadinessAndExplicitWorkloadEvidence(t *testing.T) {
	path := filepath.Join("..", "..", "tests", "installer", "qualified-guest.sh")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`qualification == "authenticated-runner-readiness"`,
		`.evidence.runnerId`, `.evidence.runnerPool`, `.evidence.runnerState`,
		`.evidence.runnerCredentialState`, `.evidence.runnerPoolState`,
		`.evidence.runnerPoolReadyRunners`, `.evidence.coldBootCapacity`,
		`.evidence.concurrentOperationCapacity`,
		`create_qualification_workload`, `postUpdateState == "runner-ready"`,
	} {
		if !bytes.Contains(content, []byte(required)) {
			t.Errorf("qualified guest lacks %q", required)
		}
	}
	for _, retired := range []string{`.evidence.output`, `.evidence.exitStatus`, `.evidence.sandboxId`} {
		if bytes.Contains(content, []byte(retired)) {
			t.Errorf("qualified guest retains retired installer evidence %q", retired)
		}
	}
	check := exec.CommandContext(t.Context(), "bash", "-n", path)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("qualified guest syntax: %v: %s", err, output)
	}
}

func TestInstallerLifecycleCommandsAreFencedDuringActiveUpdate(t *testing.T) {
	operation, plan := activeUpdateOperation(t)
	renderer := cliui.Renderer{Output: io.Discard, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(io.Discard, io.Discard), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}

	if err := runInstallResumeWith(context.Background(), operation, renderer, systemInstallResumeDependencies(renderer)); err == nil || !strings.Contains(err.Error(), "update --resume") {
		t.Fatalf("install resume during update = %v", err)
	}
	uninstallCalled := false
	err := runInstallUninstallWith(context.Background(), operation, renderer, installUninstallDependencies{
		OwnerUID: os.Getuid(),
		Now:      time.Now,
		ValidateTeardown: func(install.InstallPlan, install.InstallReceipt) error {
			uninstallCalled = true
			return nil
		},
		ComposeDown: func(context.Context, string) error {
			uninstallCalled = true
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "update is incomplete") || uninstallCalled {
		t.Fatalf("uninstall during update = %v, side effect = %t", err, uninstallCalled)
	}
	recoveryCalled := false
	err = runInstallComposeRecoveryWith(context.Background(), operation, renderer, installComposeRecoveryDependencies{
		OwnerUID: os.Getuid(),
		Now:      time.Now,
		HostVerify: func(context.Context, string, string) error {
			recoveryCalled = true
			return nil
		},
		ValidateTeardown: func(install.InstallPlan, install.InstallReceipt) error {
			recoveryCalled = true
			return nil
		},
		ComposeDown: func(context.Context, string) error {
			recoveryCalled = true
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "update is incomplete") || recoveryCalled {
		t.Fatalf("Compose recovery during update = %v, side effect = %t", err, recoveryCalled)
	}

	readPlan, readReceipt, err := install.ReadOperation(operation, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if readPlan.OperationID != plan.OperationID || readReceipt.Status != install.OperationSucceeded {
		t.Fatalf("fenced commands changed operation = %#v %#v", readPlan, readReceipt)
	}
	if _, active := readReceipt.ActiveUpdate(); !active {
		t.Fatal("fenced commands removed active update")
	}
}

func activeUpdateOperation(t *testing.T) (string, install.InstallPlan) {
	t.Helper()
	operation := filepath.Join(t.TempDir(), "operation")
	verified := fakeGuidedRelease()
	plan, err := install.ProposePlan(guidedFacts(), install.ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: time.Now(), DeploymentDirectory: operation, BinaryDirectory: filepath.Join(filepath.Dir(operation), "bin"), CLIConfigPath: filepath.Join(filepath.Dir(operation), "config", "secondbox", "config.json"), BackingAvailableBytes: 100 << 30, DeploymentAvailableBytes: 100 << 30, Release: releasePlan(verified, releasecontract.ArtifactManifestLocation("0.4.0")), StorageChoice: install.StorageBtrfsImage, StandardBundles: []string{"agent-compartment", "durable-coding", "agent-compartment-isolated"}, RetentionSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(operation, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt, err := install.NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for index, stage := range install.StageSequence {
		if err := receipt.CompleteStage(stage, plan.CreatedAt.Add(time.Duration(index+1)*time.Second), map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	target := plan.Release
	target.Version = "0.5.0"
	target.ArtifactManifestURL = releasecontract.ArtifactManifestLocation(target.Version)
	target.ArtifactManifestDigest = "sha256:" + strings.Repeat("9", 64)
	if err := receipt.BeginUpdate("update_0123456789abcdef", plan.Release, target, plan.CreatedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := install.WriteAccepted(operation, plan, receipt); err != nil {
		t.Fatal(err)
	}
	return operation, plan
}

func TestOrdinaryUninstallStopsComposeAndPreservesDurableResources(t *testing.T) {
	operation := filepath.Join(t.TempDir(), "operation")
	verified := fakeGuidedRelease()
	plan, err := install.ProposePlan(guidedFacts(), install.ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: time.Now(), DeploymentDirectory: operation, BinaryDirectory: filepath.Join(filepath.Dir(operation), "bin"), CLIConfigPath: filepath.Join(filepath.Dir(operation), "config", "secondbox", "config.json"), BackingAvailableBytes: 100 << 30, DeploymentAvailableBytes: 100 << 30, Release: releasePlan(verified, releasecontract.ArtifactManifestLocation("0.4.0")), StorageChoice: install.StorageBtrfsImage, StandardBundles: []string{"agent-compartment", "durable-coding", "agent-compartment-isolated"}, RetentionSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(operation, 0o700); err != nil {
		t.Fatal(err)
	}
	preserved := filepath.Join(operation, "preserved-workspace-evidence")
	if err := os.WriteFile(preserved, []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := install.NewReceipt(plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range install.StageSequence {
		if err := receipt.CompleteStage(stage, time.Now(), map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := receipt.AppendResource(install.CreatedResource{ID: "compose-project", Kind: install.ResourceComposeProject, Stage: install.StageComposeStarted, Identity: "secondbox-test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := install.WriteAccepted(operation, plan, receipt); err != nil {
		t.Fatal(err)
	}
	composeCalls := 0
	var output, diagnostic bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: &diagnostic, Capabilities: cliui.ForWriter(&output, &diagnostic), OutputMode: cliui.OutputJSON, ColorMode: cliui.ColorNever}
	dependencies := installUninstallDependencies{OwnerUID: os.Getuid(), Now: time.Now, ValidateTeardown: func(install.InstallPlan, install.InstallReceipt) error { return nil }, ComposeDown: func(context.Context, string) error {
		composeCalls++
		if composeCalls == 1 {
			return errors.New("injected Compose shutdown interruption")
		}
		return nil
	}}
	if err := runInstallUninstallWith(context.Background(), operation, renderer, dependencies); err == nil || !strings.Contains(err.Error(), "injected Compose shutdown interruption") {
		t.Fatalf("first uninstall interruption = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("failed JSON uninstall emitted partial output: %q", output.String())
	}
	_, interrupted, err := install.ReadOperation(operation, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != install.OperationUninstalling {
		t.Fatalf("interrupted uninstall status = %s", interrupted.Status)
	}
	if err := runInstallUninstallWith(context.Background(), operation, renderer, dependencies); err != nil {
		t.Fatal(err)
	}
	if composeCalls != 2 {
		t.Fatalf("Compose down calls = %d", composeCalls)
	}
	var summary map[string]string
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil || summary["Workspace"] != plan.Storage.WorkspacePath {
		t.Fatalf("JSON uninstall summary = %q, %#v, %v", output.String(), summary, err)
	}
	if content, err := os.ReadFile(preserved); err != nil || string(content) != "durable" {
		t.Fatalf("ordinary uninstall changed durable evidence: %q, %v", content, err)
	}
	_, final, err := install.ReadOperation(operation, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != install.OperationUninstalled {
		t.Fatalf("uninstall receipt status = %s", final.Status)
	}
}

func TestFailedComposeNetworkRecoveryValidatesBeforeExactProjectTeardown(t *testing.T) {
	operation, plan := failedComposeRecoveryOperation(t)
	calls := []string{}
	var output, diagnostic bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: &diagnostic, Capabilities: cliui.ForWriter(&output, &diagnostic), OutputMode: cliui.OutputJSON, ColorMode: cliui.ColorNever}
	dependencies := installComposeRecoveryDependencies{
		OwnerUID: os.Getuid(),
		Now:      func() time.Time { return plan.CreatedAt.Add(time.Hour) },
		HostVerify: func(_ context.Context, directory, digest string) error {
			calls = append(calls, "host-verify")
			if directory != operation {
				t.Fatalf("host verification directory = %q", directory)
			}
			wantDigest, err := install.PlanDigest(plan)
			if err != nil || digest != wantDigest {
				t.Fatalf("host verification digest = %q, want %q, %v", digest, wantDigest, err)
			}
			return nil
		},
		ValidateTeardown: func(gotPlan install.InstallPlan, receipt install.InstallReceipt) error {
			calls = append(calls, "validate")
			if gotPlan.OperationID != plan.OperationID || receipt.Status != install.OperationFailed || receipt.FailureStage != install.StageComposeStarted {
				t.Fatalf("recovery authority = %#v %#v", gotPlan, receipt)
			}
			return nil
		},
		ComposeDown: func(_ context.Context, manifest string) error {
			calls = append(calls, "compose-down")
			if manifest != installerPlannedPath(plan, "manifest") {
				t.Fatalf("manifest = %q", manifest)
			}
			return nil
		},
	}
	if err := runInstallComposeRecoveryWith(context.Background(), operation, renderer, dependencies); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"host-verify", "validate", "compose-down"}) {
		t.Fatalf("recovery calls = %#v", calls)
	}
	var summary map[string]string
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil || summary["Project"] != "secondbox-0123456789abcdef" || summary["Operation"] != operation {
		t.Fatalf("recovery summary = %q, %#v, %v", output.String(), summary, err)
	}
	_, receipt, err := install.ReadOperation(operation, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != install.OperationFailed || receipt.FailureStage != install.StageComposeStarted || lastInstallStage(receipt) != install.StageRunnerEnrolled {
		t.Fatalf("recovery changed retryable receipt = %#v", receipt)
	}
}

func TestFailedComposeNetworkRecoveryFailsClosedBeforeTeardown(t *testing.T) {
	operation, _ := failedComposeRecoveryOperation(t)
	composeCalled := false
	renderer := cliui.Renderer{Output: io.Discard, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(io.Discard, io.Discard), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	want := errors.New("recorded manifest digest changed")
	err := runInstallComposeRecoveryWith(context.Background(), operation, renderer, installComposeRecoveryDependencies{
		OwnerUID:         os.Getuid(),
		Now:              time.Now,
		HostVerify:       func(context.Context, string, string) error { return nil },
		ValidateTeardown: func(install.InstallPlan, install.InstallReceipt) error { return want },
		ComposeDown:      func(context.Context, string) error { composeCalled = true; return nil },
	})
	if !errors.Is(err, want) || composeCalled {
		t.Fatalf("failed recovery = %v, Compose called = %t", err, composeCalled)
	}
}

func TestFailedComposeNetworkRecoveryRequiresPrivilegedHostVerification(t *testing.T) {
	operation, _ := failedComposeRecoveryOperation(t)
	validateCalled, composeCalled := false, false
	renderer := cliui.Renderer{Output: io.Discard, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(io.Discard, io.Discard), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	want := errors.New("accepted host resources changed")
	err := runInstallComposeRecoveryWith(context.Background(), operation, renderer, installComposeRecoveryDependencies{
		OwnerUID:   os.Getuid(),
		Now:        time.Now,
		HostVerify: func(context.Context, string, string) error { return want },
		ValidateTeardown: func(install.InstallPlan, install.InstallReceipt) error {
			validateCalled = true
			return nil
		},
		ComposeDown: func(context.Context, string) error { composeCalled = true; return nil },
	})
	if !errors.Is(err, want) || validateCalled || composeCalled {
		t.Fatalf("failed host verification = %v, validation called = %t, Compose called = %t", err, validateCalled, composeCalled)
	}
}

func TestFailedPostComposeRecoveryRewindsReceiptForFullRetry(t *testing.T) {
	operation, plan := failedComposeRecoveryOperationAt(t, install.StageCLILogin, install.StageReadiness)
	var output bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(&output, io.Discard), OutputMode: cliui.OutputJSON, ColorMode: cliui.ColorNever}
	calls := []string{}
	err := runInstallComposeRecoveryWith(context.Background(), operation, renderer, installComposeRecoveryDependencies{
		OwnerUID: os.Getuid(),
		Now:      func() time.Time { return plan.CreatedAt.Add(time.Hour) },
		HostVerify: func(context.Context, string, string) error {
			calls = append(calls, "host-verify")
			return nil
		},
		ValidateTeardown: func(_ install.InstallPlan, receipt install.InstallReceipt) error {
			calls = append(calls, "validate")
			if lastInstallStage(receipt) != install.StageCLILogin || receipt.FailureStage != install.StageReadiness {
				t.Fatalf("post-Compose recovery authority = %#v", receipt)
			}
			return nil
		},
		ComposeDown: func(context.Context, string) error {
			calls = append(calls, "compose-down")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"host-verify", "validate", "compose-down"}) {
		t.Fatalf("post-Compose recovery calls = %#v", calls)
	}
	_, receipt, err := install.ReadOperation(operation, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != install.OperationFailed || receipt.FailureStage != install.StageComposeStarted || receipt.FailureClass != install.FailureRetryable || lastInstallStage(receipt) != install.StageRunnerEnrolled {
		t.Fatalf("rewound post-Compose receipt = %#v", receipt)
	}
	if slices.ContainsFunc(receipt.CreatedResources, func(resource install.CreatedResource) bool { return resource.ID == "compose-project" }) {
		t.Fatal("rewound post-Compose receipt retained the Compose project")
	}
}

func failedComposeRecoveryOperation(t *testing.T) (string, install.InstallPlan) {
	return failedComposeRecoveryOperationAt(t, install.StageRunnerEnrolled, install.StageComposeStarted)
}

func failedComposeRecoveryOperationAt(t *testing.T, completed, failure install.Stage) (string, install.InstallPlan) {
	t.Helper()
	operation := filepath.Join(t.TempDir(), "operation")
	verified := fakeGuidedRelease()
	plan, err := install.ProposePlan(guidedFacts(), install.ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: time.Now(), DeploymentDirectory: operation, BinaryDirectory: filepath.Join(filepath.Dir(operation), "bin"), CLIConfigPath: filepath.Join(filepath.Dir(operation), "config", "secondbox", "config.json"), BackingAvailableBytes: 100 << 30, DeploymentAvailableBytes: 100 << 30, Release: releasePlan(verified, releasecontract.ArtifactManifestLocation("0.4.0")), StorageChoice: install.StorageBtrfsImage, StandardBundles: []string{"agent-compartment", "durable-coding", "agent-compartment-isolated"}, RetentionSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(operation, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt, err := install.NewReceipt(plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range install.StageSequence {
		if err := receipt.CompleteStage(stage, time.Now(), map[string]string{}); err != nil {
			t.Fatal(err)
		}
		if stage == completed {
			break
		}
	}
	if slices.Index(install.StageSequence, completed) >= slices.Index(install.StageSequence, install.StageComposeStarted) {
		if err := receipt.AppendResource(install.CreatedResource{ID: "compose-project", Kind: install.ResourceComposeProject, Stage: install.StageComposeStarted, Identity: "secondbox-0123456789abcdef"}); err != nil {
			t.Fatal(err)
		}
		for index := range receipt.CompletedStages {
			if receipt.CompletedStages[index].Stage == install.StageComposeStarted {
				receipt.CompletedStages[index].Evidence = map[string]string{"composeProject": "secondbox-0123456789abcdef"}
			}
		}
	}
	if err := receipt.Fail(failure, install.FailureRetryable, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := install.WriteAccepted(operation, plan, receipt); err != nil {
		t.Fatal(err)
	}
	return operation, plan
}

func TestBoundedCommandBufferCollectsSupportEvidence(t *testing.T) {
	buffer := boundedCommandBuffer{maximum: int(maximumInstallerEvidenceBytes())}
	if _, err := buffer.Write([]byte("systemd evidence")); err != nil {
		t.Fatal(err)
	}
	if buffer.String() != "systemd evidence" || buffer.tooLong {
		t.Fatalf("bounded support buffer = %q tooLong=%t", buffer.String(), buffer.tooLong)
	}
}

func TestInstallResumeFailureInjectionStopsAtEveryOrchestrationBoundary(t *testing.T) {
	verified := fakeGuidedRelease()
	tests := []struct {
		name         string
		initial      install.Stage
		failureStage install.Stage
		inject       func(*installResumeDependencies)
	}{
		{"release verification", install.StageHostApply, install.StageReleaseVerified, func(d *installResumeDependencies) {
			d.VerifyRelease = func(context.Context, string) (releaseverify.VerifiedRelease, error) {
				return releaseverify.VerifiedRelease{}, errors.New("injected release verification")
			}
		}},
		{"asset materialization", install.StageReleaseVerified, install.StageAssetsMaterialized, func(d *installResumeDependencies) {
			d.Materialize = func(context.Context, install.InstallPlan, install.InstallReceipt, releaseverify.VerifiedRelease, func(install.InstallReceipt) error) (install.InstallReceipt, install.VerifiedArtifact, error) {
				return install.InstallReceipt{}, install.VerifiedArtifact{}, errors.New("injected materialization")
			}
		}},
		{"deployment materialization", install.StageReleaseVerified, install.StageDeploymentMaterialized, func(d *installResumeDependencies) {
			d.Materialize = func(_ context.Context, _ install.InstallPlan, current install.InstallReceipt, _ releaseverify.VerifiedRelease, persist func(install.InstallReceipt) error) (install.InstallReceipt, install.VerifiedArtifact, error) {
				if err := current.CompleteStage(install.StageAssetsMaterialized, time.Now(), map[string]string{}); err != nil {
					return current, install.VerifiedArtifact{}, err
				}
				if err := persist(current); err != nil {
					return current, install.VerifiedArtifact{}, err
				}
				return current, install.VerifiedArtifact{ManifestDigest: "sha256:" + strings.Repeat("d", 64)}, nil
			}
			d.Initialize = func(install.InstallPlan, releaseverify.VerifiedRelease, install.VerifiedArtifact) (deployconfig.SingleHostInstallResult, error) {
				return deployconfig.SingleHostInstallResult{}, errors.New("injected initialization")
			}
		}},
		{"Runner enrollment", install.StageDeploymentMaterialized, install.StageRunnerEnrolled, func(d *installResumeDependencies) {
			d.Enroll = func(deployconfig.SingleHostInstallResult) error { return errors.New("injected enrollment") }
		}},
		{"Compose startup", install.StageRunnerEnrolled, install.StageComposeStarted, func(d *installResumeDependencies) {
			d.Compose = func(context.Context, string, string) error { return errors.New("injected Compose") }
		}},
		{"CLI login", install.StageComposeStarted, install.StageCLILogin, func(d *installResumeDependencies) {
			d.Login = func(context.Context, install.InstallPlan) ([]install.CreatedResource, error) {
				return nil, errors.New("injected login")
			}
		}},
		{"readiness", install.StageCLILogin, install.StageReadiness, func(d *installResumeDependencies) {
			d.Readiness = func(context.Context, install.InstallPlan) (map[string]string, error) {
				return nil, errors.New("injected readiness")
			}
		}},
		{"smoke", install.StageReadiness, install.StageSmokeExecution, func(d *installResumeDependencies) {
			d.Smoke = func(context.Context, install.InstallPlan) (map[string]string, error) {
				return nil, errors.New("injected smoke")
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := filepath.Join(t.TempDir(), "operation")
			plan, err := install.ProposePlan(guidedFacts(), install.ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: time.Now(), DeploymentDirectory: operation, BinaryDirectory: filepath.Join(filepath.Dir(operation), "bin"), CLIConfigPath: filepath.Join(filepath.Dir(operation), "config", "secondbox", "config.json"), BackingAvailableBytes: 100 << 30, DeploymentAvailableBytes: 100 << 30, Release: releasePlan(verified, releasecontract.ArtifactManifestLocation("0.4.0")), StorageChoice: install.StorageBtrfsImage, StandardBundles: []string{"agent-compartment", "durable-coding", "agent-compartment-isolated"}, RetentionSeconds: 86400})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(operation, 0o700); err != nil {
				t.Fatal(err)
			}
			receipt, err := install.NewReceipt(plan, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			for _, stage := range install.StageSequence {
				if err := receipt.CompleteStage(stage, time.Now(), map[string]string{}); err != nil {
					t.Fatal(err)
				}
				if stage == test.initial {
					break
				}
			}
			if _, _, err := install.WriteAccepted(operation, plan, receipt); err != nil {
				t.Fatal(err)
			}
			dependencies := installResumeDependencies{
				OwnerUID: os.Getuid(), Now: time.Now,
				HostApply: func(context.Context, string, string) error { return nil }, Revalidate: func(install.InstallPlan) error { return nil },
				VerifyRelease:  func(context.Context, string) (releaseverify.VerifiedRelease, error) { return verified, nil },
				Postconditions: func(install.InstallPlan, install.InstallReceipt, releaseverify.VerifiedRelease) error { return nil },
				Materialize: func(context.Context, install.InstallPlan, install.InstallReceipt, releaseverify.VerifiedRelease, func(install.InstallReceipt) error) (install.InstallReceipt, install.VerifiedArtifact, error) {
					return install.InstallReceipt{}, install.VerifiedArtifact{}, nil
				},
				Initialize: func(install.InstallPlan, releaseverify.VerifiedRelease, install.VerifiedArtifact) (deployconfig.SingleHostInstallResult, error) {
					return deployconfig.SingleHostInstallResult{}, nil
				},
				Enroll: func(deployconfig.SingleHostInstallResult) error { return nil }, Compose: func(context.Context, string, string) error { return nil }, ComposeProject: func(string) (string, error) { return "secondbox-test", nil },
				Login: func(context.Context, install.InstallPlan) ([]install.CreatedResource, error) { return nil, nil }, Readiness: func(context.Context, install.InstallPlan) (map[string]string, error) { return map[string]string{}, nil }, Smoke: func(context.Context, install.InstallPlan) (map[string]string, error) { return map[string]string{}, nil },
			}
			test.inject(&dependencies)
			renderer := cliui.Renderer{Output: &bytes.Buffer{}, Diagnostic: &bytes.Buffer{}, Capabilities: cliui.ForWriter(&bytes.Buffer{}, &bytes.Buffer{}), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
			if err := runInstallResumeWith(context.Background(), operation, renderer, dependencies); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("injected failure result = %v", err)
			}
			if test.failureStage == install.StageAssetsMaterialized {
				return // MaterializeRelease owns and tests its own durable failure record.
			}
			_, failed, err := install.ReadOperation(operation, os.Getuid())
			if err != nil {
				t.Fatal(err)
			}
			if failed.Status != install.OperationFailed || failed.FailureStage != test.failureStage {
				t.Fatalf("failure receipt = status %s stage %s", failed.Status, failed.FailureStage)
			}
		})
	}
}
