package release_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedCandidateAndQualificationWorkflowsAreCanonical(t *testing.T) {
	candidate := readReleaseRepositoryFile(t, ".github/workflows/release-candidate.yml")
	for _, required := range []string{
		"environment: release-candidate",
		"group: secondbox-release",
		"secondbox-release-builder",
		"DOCKER_CONFIG: ${{ github.workspace }}/.tmp/release-docker-config",
		"packages: write",
		"persist-credentials: false",
		"scripts/package-release-guest-assets.sh",
		"runner/deploy/microvm-artifact-transport.Dockerfile",
		"scripts/generate-release-candidate-provenance.mjs",
		"scripts/write-protected-release-workflow-identity.mjs",
		"scripts/verify-protected-release-environment.mjs",
		"name: release-candidate-${{ github.sha }}",
	} {
		if !strings.Contains(candidate, required) {
			t.Errorf("protected candidate workflow must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"contents: write",
		"id-token: write",
		"SECONDBOX_RELEASE_SIGNING_PRIVATE_KEY",
		":latest",
		"--tag v",
	} {
		if strings.Contains(candidate, forbidden) {
			t.Errorf("protected candidate workflow must not contain %q", forbidden)
		}
	}

	qualification := readReleaseRepositoryFile(t, ".github/workflows/release-qualification.yml")
	for _, required := range []string{
		"environment: release-qualification",
		"group: secondbox-release",
		"secondbox-release-qualification",
		"DOCKER_CONFIG: ${{ github.workspace }}/.tmp/release-docker-config",
		"candidate-run-id:",
		"scripts/verify-protected-release-workflow-run.mjs",
		"scripts/verify-protected-release-environment.mjs",
		"scripts/import-release-qualification-evidence.mjs",
		`any(.hosts[]; .id == $runner_name)`,
		"name: release-qualification-${{ github.sha }}",
	} {
		if !strings.Contains(qualification, required) {
			t.Errorf("protected qualification workflow must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"contents: write",
		"packages: write",
		"id-token: write",
	} {
		if strings.Contains(qualification, forbidden) {
			t.Errorf("protected qualification workflow must not contain %q", forbidden)
		}
	}

	evidence := readReleaseRepositoryFile(t, ".github/workflows/release-evidence.yml")
	for _, required := range []string{
		"candidate-run-id:",
		"environment: release-evidence",
		"scripts/verify-protected-release-workflow-run.mjs",
		"scripts/verify-protected-release-environment.mjs",
		"release-candidate-${{ github.sha }}",
		"release-qualification-${{ github.sha }}",
		"SECONDBOX_RELEASE_SIGNING_PRIVATE_KEY_PEM: ${{ secrets.SECONDBOX_RELEASE_SIGNING_PRIVATE_KEY_PEM }}",
		"SECONDBOX_RELEASE_EXPECTED_GUEST_BUNDLE_SHA256",
		"SECONDBOX_RELEASE_EXPECTED_GUEST_BUNDLE_SIZE_BYTES",
	} {
		if !strings.Contains(evidence, required) {
			t.Errorf("release evidence workflow must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"contents: write",
		"packages: write",
		"id-token: write",
		"scripts/package-release-artifacts.sh",
		"scripts/package-release-sdk-artifacts.sh",
	} {
		if strings.Contains(evidence, forbidden) {
			t.Errorf("release evidence workflow must not contain %q", forbidden)
		}
	}
}

func TestProtectedReleaseWorkflowRunVerifierRejectsArbitraryRunIdentity(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	verifier := filepath.Join(
		repositoryRoot,
		"scripts",
		"verify-protected-release-workflow-run.mjs",
	)
	const sourceCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	for _, kind := range []string{"candidate", "qualification"} {
		t.Run(kind, func(t *testing.T) {
			workflowPath := ".github/workflows/release-candidate.yml"
			environment := "release-candidate"
			jobName := "assemble-release-candidate"
			runnerLabel := "secondbox-release-builder"
			if kind == "qualification" {
				workflowPath = ".github/workflows/release-qualification.yml"
				environment = "release-qualification"
				jobName = "qualify-packaged-release"
				runnerLabel = "secondbox-release-qualification"
			}
			run := map[string]any{
				"id":          101,
				"run_attempt": 2,
				"path":        workflowPath + "@main",
				"event":       "workflow_dispatch",
				"head_branch": "main",
				"head_sha":    sourceCommit,
				"status":      "completed",
				"conclusion":  "success",
				"repository": map[string]any{
					"full_name": "SecondStack-AI/SecondBox",
				},
				"head_repository": map[string]any{
					"full_name": "SecondStack-AI/SecondBox",
				},
			}
			jobs := map[string]any{
				"total_count": 1,
				"jobs": []map[string]any{{
					"name":              jobName,
					"status":            "completed",
					"conclusion":        "success",
					"runner_group_name": "secondbox-release",
					"runner_name":       "qualified-host-01",
					"labels": []string{
						"self-hosted",
						"linux",
						"x64",
						runnerLabel,
					},
				}},
			}
			identity := map[string]any{
				"schemaVersion":        1,
				"kind":                 kind,
				"repository":           "SecondStack-AI/SecondBox",
				"workflowPath":         workflowPath,
				"workflowRef":          "SecondStack-AI/SecondBox/" + workflowPath + "@refs/heads/main",
				"protectedEnvironment": environment,
				"sourceCommit":         sourceCommit,
				"ref":                  "refs/heads/main",
				"eventName":            "workflow_dispatch",
				"runID":                101,
				"runAttempt":           2,
				"jobName":              jobName,
				"runnerName":           "qualified-host-01",
				"runnerEnvironment":    "self-hosted",
				"runnerOS":             "Linux",
				"runnerArch":           "X64",
			}

			runProtectedWorkflowVerifier(
				t,
				verifier,
				kind,
				sourceCommit,
				run,
				jobs,
				identity,
				true,
				"",
			)

			mutations := []struct {
				name        string
				mutate      func(map[string]any, map[string]any, map[string]any)
				wantFailure string
			}{
				{
					name: "arbitrary workflow",
					mutate: func(run, _, _ map[string]any) {
						run["path"] = ".github/workflows/ci.yml"
					},
					wantFailure: "successful " + workflowPath,
				},
				{
					name: "non-main branch",
					mutate: func(run, _, _ map[string]any) {
						run["head_branch"] = "feature"
					},
					wantFailure: "protected main",
				},
				{
					name: "wrong protected environment",
					mutate: func(_, _, identity map[string]any) {
						identity["protectedEnvironment"] = "unprotected"
					},
					wantFailure: environment,
				},
				{
					name: "wrong runner group",
					mutate: func(_, jobs, _ map[string]any) {
						jobs["jobs"].([]any)[0].(map[string]any)["runner_group_name"] = "default"
					},
					wantFailure: "runner group secondbox-release",
				},
				{
					name: "run id self-attestation",
					mutate: func(_, _, identity map[string]any) {
						identity["runID"] = 999
					},
					wantFailure: "canonical run",
				},
			}
			for _, mutation := range mutations {
				t.Run(mutation.name, func(t *testing.T) {
					runCopy := cloneJSONMap(t, run)
					jobsCopy := cloneJSONMap(t, jobs)
					identityCopy := cloneJSONMap(t, identity)
					mutation.mutate(runCopy, jobsCopy, identityCopy)
					runProtectedWorkflowVerifier(
						t,
						verifier,
						kind,
						sourceCommit,
						runCopy,
						jobsCopy,
						identityCopy,
						false,
						mutation.wantFailure,
					)
				})
			}
		})
	}
}

func TestProtectedReleaseEnvironmentVerifierRequiresReviewAndProtectedMain(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	verifier := filepath.Join(
		repositoryRoot,
		"scripts",
		"verify-protected-release-environment.mjs",
	)
	validEnvironment := map[string]any{
		"name": "release-qualification",
		"protection_rules": []map[string]any{
			{
				"id":                  1,
				"type":                "required_reviewers",
				"prevent_self_review": true,
				"reviewers": []map[string]any{{
					"type": "Team",
					"reviewer": map[string]any{
						"id":   10,
						"name": "release-approvers",
					},
				}},
			},
			{
				"id":   2,
				"type": "branch_policy",
			},
		},
		"deployment_branch_policy": map[string]any{
			"protected_branches":     true,
			"custom_branch_policies": false,
		},
	}
	runProtectedEnvironmentVerifier(
		t,
		verifier,
		validEnvironment,
		true,
		"",
	)

	tests := []struct {
		name        string
		mutate      func(map[string]any)
		wantFailure string
	}{
		{
			name: "self review allowed",
			mutate: func(environment map[string]any) {
				environment["protection_rules"].([]any)[0].(map[string]any)["prevent_self_review"] = false
			},
			wantFailure: "prevent self-review",
		},
		{
			name: "reviewers absent",
			mutate: func(environment map[string]any) {
				environment["protection_rules"].([]any)[0].(map[string]any)["reviewers"] = []any{}
			},
			wantFailure: "require a reviewer",
		},
		{
			name: "unprotected branches",
			mutate: func(environment map[string]any) {
				environment["deployment_branch_policy"].(map[string]any)["protected_branches"] = false
				environment["deployment_branch_policy"].(map[string]any)["custom_branch_policies"] = true
			},
			wantFailure: "protected branches",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := cloneJSONMap(t, validEnvironment)
			test.mutate(environment)
			runProtectedEnvironmentVerifier(
				t,
				verifier,
				environment,
				false,
				test.wantFailure,
			)
		})
	}
}

func TestGitHubReleaseStateVerifierRejectsUnexpectedAndDuplicateAssets(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	verifier := filepath.Join(
		repositoryRoot,
		"scripts",
		"verify-github-release-state.mjs",
	)
	configuration := map[string]any{"enabled": true, "enforced_by_owner": true}
	releases := []map[string]any{{
		"id":         17,
		"tag_name":   "v1.0.0",
		"draft":      true,
		"prerelease": false,
		"immutable":  false,
	}}
	assets := []map[string]any{{
		"id":    101,
		"name":  "secondbox-1.0.0.tar.gz",
		"state": "uploaded",
	}}
	expected := []string{
		"release-evidence.json",
		"secondbox-1.0.0.tar.gz",
	}

	runGitHubReleaseStateVerifier(
		t,
		verifier,
		configuration,
		releases,
		assets,
		expected,
		"before-upload",
		true,
		"",
	)
	duplicateReleases := append(
		cloneJSONSlice(t, releases),
		cloneJSONMap(t, releases[0]),
	)
	duplicateReleases[1]["id"] = 18
	runGitHubReleaseStateVerifier(
		t,
		verifier,
		configuration,
		duplicateReleases,
		assets,
		expected,
		"before-upload",
		false,
		"multiple draft or public releases",
	)

	tests := []struct {
		name        string
		mutate      func(map[string]any, []map[string]any, []map[string]any)
		wantFailure string
	}{
		{
			name: "immutable releases disabled",
			mutate: func(configuration map[string]any, _, _ []map[string]any) {
				configuration["enabled"] = false
			},
			wantFailure: "immutable releases are not enabled",
		},
		{
			name: "unexpected draft asset",
			mutate: func(_ map[string]any, _, assets []map[string]any) {
				assets[0]["name"] = "replacement.tar.gz"
			},
			wantFailure: "unexpected asset",
		},
		{
			name: "duplicate draft asset",
			mutate: func(_ map[string]any, _, assets []map[string]any) {
				assets[1]["name"] = assets[0]["name"]
			},
			wantFailure: "duplicate asset names",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configurationCopy := cloneJSONMap(t, configuration)
			releasesCopy := cloneJSONSlice(t, releases)
			assetsCopy := cloneJSONSlice(t, append(assets, map[string]any{
				"id":    102,
				"name":  "release-evidence.json",
				"state": "uploaded",
			}))
			test.mutate(configurationCopy, releasesCopy, assetsCopy)
			runGitHubReleaseStateVerifier(
				t,
				verifier,
				configurationCopy,
				releasesCopy,
				assetsCopy,
				expected,
				"before-upload",
				false,
				test.wantFailure,
			)
		})
	}

	publicRelease := cloneJSONSlice(t, releases)
	publicRelease[0]["draft"] = false
	publicRelease[0]["immutable"] = true
	completeAssets := append(cloneJSONSlice(t, assets), map[string]any{
		"id":    102,
		"name":  "release-evidence.json",
		"state": "uploaded",
	})
	runGitHubReleaseStateVerifier(
		t,
		verifier,
		nil,
		publicRelease,
		completeAssets,
		expected,
		"public",
		true,
		"",
	)
}

func TestGitHubPublicationPreflightsImmutableConfigurationAndWholeAssetSet(t *testing.T) {
	helper := readReleaseRepositoryFile(t, "scripts/publish-qualified-release.sh")
	for _, required := range []string{
		"verify_github_immutable_release_configuration",
		"SECONDBOX_RELEASE_CONFIGURATION_TOKEN",
		"repos/$expected_repository/immutable-releases",
		"X-GitHub-Api-Version: 2026-03-10",
		"fetch_github_release_inventory",
		"fetch_github_release_assets",
		"verify-github-release-state.mjs",
		"before-upload",
		"after-upload",
	} {
		if !strings.Contains(helper, required) {
			t.Errorf("GitHub publication preflight must contain %q", required)
		}
	}
	preflightIndex := strings.Index(
		helper,
		"verify_github_immutable_release_configuration \"$immutable_configuration\"",
	)
	tagIndex := strings.LastIndex(helper, "ensure_exact_github_tag")
	if preflightIndex < 0 || tagIndex < 0 || preflightIndex > tagIndex {
		t.Error("immutable-release configuration must be proven before tag creation")
	}

	workflow := readReleaseRepositoryFile(t, ".github/workflows/publish.yml")
	if !strings.Contains(
		workflow,
		"SECONDBOX_RELEASE_CONFIGURATION_TOKEN: ${{ secrets.SECONDBOX_RELEASE_CONFIGURATION_TOKEN }}",
	) {
		t.Error("publication workflow must receive the read-only configuration token only from the protected release environment")
	}
}

func TestReleaseCandidateImporterRequiresExactProtectedInputSet(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	generator := filepath.Join(
		repositoryRoot,
		"scripts",
		"generate-release-candidate-inputs.mjs",
	)
	importer := filepath.Join(
		repositoryRoot,
		"scripts",
		"import-release-candidate-inputs.mjs",
	)
	const (
		releaseVersion = "1.0.0"
		sourceCommit   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		imageDigest    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	newFixture := func(t *testing.T) (string, string) {
		t.Helper()
		candidateDirectory := t.TempDir()
		filePaths := []string{
			"dist/SHA256SUMS",
			"dist/secondbox",
			"dist/secondbox-artifact-evidence",
			"dist/secondbox-guest-agent",
			"dist/secondbox-runner",
			"dist/secondbox-runner-identity",
			"dist/secondboxd",
			"package/secondbox-1.0.0-linux-amd64.SHA256SUMS",
			"package/secondbox-1.0.0-linux-amd64.manifest.json",
			"package/secondbox-1.0.0-linux-amd64.tar.gz",
			"sdk/secondbox-1.0.0-go-sdk.tar.gz",
			"sdk/secondbox-1.0.0-sdk.SHA256SUMS",
			"sdk/secondstack-ai-secondbox-1.0.0.tgz",
			"external-provenance/control-plane-image.intoto.json",
			"external-provenance/runner-image.intoto.json",
			"external-provenance/guest-execution-bundle.intoto.json",
			"external-provenance/guest-artifact-image.intoto.json",
		}
		for _, relativePath := range filePaths {
			filePath := filepath.Join(
				candidateDirectory,
				filepath.FromSlash(relativePath),
			)
			if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filePath, []byte(relativePath), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		identityPath := filepath.Join(candidateDirectory, "protected-workflow-identity.json")
		if err := os.WriteFile(identityPath, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		environmentPath := filepath.Join(candidateDirectory, "protected-environment.json")
		if err := os.WriteFile(environmentPath, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		guestArchive := filepath.Join(t.TempDir(), "guest.tar.gz")
		if err := os.WriteFile(guestArchive, []byte("signed guest"), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(
			"node",
			generator,
			candidateDirectory,
			releaseVersion,
			sourceCommit,
			"ghcr.io/secondstack-ai/secondbox-control-plane@sha256:"+imageDigest,
			"ghcr.io/secondstack-ai/secondbox-runner@sha256:"+imageDigest,
			"ghcr.io/secondstack-ai/secondbox-guest-artifacts@sha256:"+imageDigest,
			guestArchive,
		).CombinedOutput()
		if err != nil {
			t.Fatalf("generate protected candidate fixture: %v\n%s", err, output)
		}
		return candidateDirectory, guestArchive
	}

	runImporter := func(
		t *testing.T,
		candidateDirectory string,
		wantSuccess bool,
		wantFailure string,
	) {
		t.Helper()
		evidenceDirectory := t.TempDir()
		githubEnvironment := filepath.Join(t.TempDir(), "github-env")
		if err := os.WriteFile(githubEnvironment, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(
			"node",
			importer,
			candidateDirectory,
			evidenceDirectory,
			releaseVersion,
			sourceCommit,
			githubEnvironment,
		).CombinedOutput()
		if wantSuccess && err != nil {
			t.Fatalf("candidate importer rejected valid fixture: %v\n%s", err, output)
		}
		if !wantSuccess {
			if err == nil {
				t.Fatal("candidate importer accepted invalid fixture")
			}
			if !strings.Contains(string(output), wantFailure) {
				t.Fatalf("candidate import failure did not contain %q:\n%s", wantFailure, output)
			}
		}
	}

	validCandidate, guestArchive := newFixture(t)
	runImporter(t, validCandidate, true, "")
	guestDigest := sha256.Sum256([]byte("signed guest"))
	var manifest map[string]any
	decodeReleaseJSONFile(
		t,
		filepath.Join(validCandidate, "release-candidate-inputs.json"),
		&manifest,
	)
	if manifest["guestBundle"].(map[string]any)["sha256"] != fmt.Sprintf("%x", guestDigest) {
		t.Fatalf("candidate manifest did not bind guest archive %s", guestArchive)
	}

	t.Run("rejects mutable image", func(t *testing.T) {
		candidateDirectory, _ := newFixture(t)
		manifestPath := filepath.Join(candidateDirectory, "release-candidate-inputs.json")
		var candidateManifest map[string]any
		decodeReleaseJSONFile(t, manifestPath, &candidateManifest)
		candidateManifest["images"].(map[string]any)["runnerImage"] =
			"ghcr.io/secondstack-ai/secondbox-runner:latest"
		writeReleaseJSONFixture(t, manifestPath, candidateManifest)
		runImporter(t, candidateDirectory, false, "digest-pinned")
	})

	t.Run("rejects subject digest drift", func(t *testing.T) {
		candidateDirectory, _ := newFixture(t)
		if err := os.WriteFile(
			filepath.Join(candidateDirectory, "dist", "secondbox"),
			[]byte("changed"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		runImporter(t, candidateDirectory, false, "bytes drifted")
	})

	t.Run("rejects unexpected candidate path", func(t *testing.T) {
		candidateDirectory, _ := newFixture(t)
		manifestPath := filepath.Join(candidateDirectory, "release-candidate-inputs.json")
		var candidateManifest map[string]any
		decodeReleaseJSONFile(t, manifestPath, &candidateManifest)
		files := candidateManifest["files"].([]any)
		files[0].(map[string]any)["path"] = "dist/replacement"
		writeReleaseJSONFixture(t, manifestPath, candidateManifest)
		runImporter(t, candidateDirectory, false, "invalid or duplicate")
	})

	t.Run("rejects unreferenced artifact payload", func(t *testing.T) {
		candidateDirectory, _ := newFixture(t)
		if err := os.WriteFile(
			filepath.Join(candidateDirectory, "dist", "unreferenced"),
			[]byte("payload"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		runImporter(t, candidateDirectory, false, "unexpected")
	})
}

func TestGuestArtifactImageCarriesCompleteSignedBundleAllowlist(t *testing.T) {
	imageDefinition := readReleaseRepositoryFile(
		t,
		"runner/deploy/microvm-artifact-transport.Dockerfile",
	)
	guestPackager := readReleaseRepositoryFile(
		t,
		"scripts/package-release-guest-assets.sh",
	)
	for _, requiredFile := range []string{
		"runtime-manifest.json",
		"toolchain-manifest.json",
	} {
		requiredCopy := "COPY " + requiredFile +
			" /secondbox-runner-microvm/" + requiredFile
		if !strings.Contains(imageDefinition, requiredCopy) {
			t.Errorf("guest artifact image must contain %q", requiredCopy)
		}
		if !strings.Contains(guestPackager, requiredFile) {
			t.Errorf("guest archive allowlist must contain %q", requiredFile)
		}
	}
	for _, required := range []string{
		"guest_package_files=(",
		"canonical signed artifact allowlist",
		"gzip --best --no-name",
	} {
		if !strings.Contains(guestPackager, required) {
			t.Errorf("guest release packaging must contain %q", required)
		}
	}
}

func runProtectedWorkflowVerifier(
	t *testing.T,
	verifier string,
	kind string,
	sourceCommit string,
	run map[string]any,
	jobs map[string]any,
	identity map[string]any,
	wantSuccess bool,
	wantFailure string,
) {
	t.Helper()
	fixtureDirectory := t.TempDir()
	runPath := filepath.Join(fixtureDirectory, "run.json")
	jobsPath := filepath.Join(fixtureDirectory, "jobs.json")
	identityPath := filepath.Join(fixtureDirectory, "identity.json")
	writeReleaseJSONFixture(t, runPath, run)
	writeReleaseJSONFixture(t, jobsPath, jobs)
	writeReleaseJSONFixture(t, identityPath, identity)
	output, err := exec.Command(
		"node",
		verifier,
		runPath,
		jobsPath,
		identityPath,
		kind,
		sourceCommit,
	).CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("protected workflow verifier rejected valid fixture: %v\n%s", err, output)
	}
	if !wantSuccess {
		if err == nil {
			t.Fatal("protected workflow verifier accepted invalid fixture")
		}
		if !strings.Contains(string(output), wantFailure) {
			t.Fatalf("protected workflow failure did not contain %q:\n%s", wantFailure, output)
		}
	}
}

func runGitHubReleaseStateVerifier(
	t *testing.T,
	verifier string,
	configuration map[string]any,
	releases []map[string]any,
	assets []map[string]any,
	expected []string,
	phase string,
	wantSuccess bool,
	wantFailure string,
) {
	t.Helper()
	fixtureDirectory := t.TempDir()
	configurationPath := "-"
	if phase != "public" {
		configurationPath = filepath.Join(fixtureDirectory, "configuration.json")
		writeReleaseJSONFixture(t, configurationPath, configuration)
	}
	releasesPath := filepath.Join(fixtureDirectory, "releases.json")
	assetsPath := filepath.Join(fixtureDirectory, "assets.json")
	expectedPath := filepath.Join(fixtureDirectory, "expected.json")
	writeReleaseJSONFixture(t, releasesPath, releases)
	writeReleaseJSONFixture(t, assetsPath, assets)
	writeReleaseJSONFixture(t, expectedPath, expected)
	output, err := exec.Command(
		"node",
		verifier,
		configurationPath,
		releasesPath,
		assetsPath,
		expectedPath,
		"v1.0.0",
		phase,
	).CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("GitHub release state verifier rejected valid fixture: %v\n%s", err, output)
	}
	if !wantSuccess {
		if err == nil {
			t.Fatal("GitHub release state verifier accepted invalid fixture")
		}
		if !strings.Contains(string(output), wantFailure) {
			t.Fatalf("GitHub release state failure did not contain %q:\n%s", wantFailure, output)
		}
	}
}

func runProtectedEnvironmentVerifier(
	t *testing.T,
	verifier string,
	environment map[string]any,
	wantSuccess bool,
	wantFailure string,
) {
	t.Helper()
	environmentPath := filepath.Join(t.TempDir(), "environment.json")
	writeReleaseJSONFixture(t, environmentPath, environment)
	output, err := exec.Command(
		"node",
		verifier,
		environmentPath,
		"release-qualification",
	).CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("protected environment verifier rejected valid fixture: %v\n%s", err, output)
	}
	if !wantSuccess {
		if err == nil {
			t.Fatal("protected environment verifier accepted invalid fixture")
		}
		if !strings.Contains(string(output), wantFailure) {
			t.Fatalf("protected environment failure did not contain %q:\n%s", wantFailure, output)
		}
	}
}

func cloneJSONSlice(t *testing.T, value []map[string]any) []map[string]any {
	t.Helper()
	var clone []map[string]any
	cloneReleaseJSONValue(t, value, &clone)
	return clone
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	var clone map[string]any
	cloneReleaseJSONValue(t, value, &clone)
	return clone
}

func cloneReleaseJSONValue(t *testing.T, value any, destination any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON fixture clone: %v", err)
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		t.Fatalf("decode JSON fixture clone: %v", err)
	}
}
