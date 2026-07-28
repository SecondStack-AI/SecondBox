package release_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseWorkflowCannotPublishArtifacts(t *testing.T) {
	workflow := readReleaseRepositoryFile(t, ".github/workflows/release-evidence.yml")

	for _, required := range []string{
		"permissions:",
		"actions: read",
		"contents: read",
		"clean-clone-isolation",
		"non-kvm-qualification",
		"release-evidence",
		"publication-eligibility",
		"postgres:18.4-bookworm",
		"SECONDBOX_TEST_DATABASE_URL:",
		"scripts/verify-release-publication-eligibility.sh",
		"scripts/import-release-qualification-evidence.mjs",
		"qualification-run-id",
		"if: always()",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release evidence workflow must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"contents: write",
		"id-token: write",
		"gh release",
		"softprops/action-gh-release",
		"actions/create-release",
		"docker push",
		"cosign sign",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("release evidence workflow must not contain %q", forbidden)
		}
	}
}

func TestReleaseEvidenceSchemaRequiresEveryPublicationGate(t *testing.T) {
	var schema struct {
		Required   []string `json:"required"`
		Properties struct {
			Evidence struct {
				Required []string `json:"required"`
			} `json:"evidence"`
		} `json:"properties"`
	}
	decodeReleaseRepositoryJSON(t, "release/evidence-schema.json", &schema)

	for _, topLevel := range []string{
		"schemaVersion", "releaseVersion", "sourceCommit", "generatedAt",
		"compatibility", "subjects", "evidence",
	} {
		if !containsReleaseString(schema.Required, topLevel) {
			t.Errorf("release evidence schema does not require %q", topLevel)
		}
	}
	for _, gate := range []string{
		"cleanCloneIsolation",
		"nonKVMQualification",
		"kvmQualification",
		"multiRunnerQualification",
		"durabilityQualification",
		"dataPlaneQualification",
		"networkQualification",
		"securityQualification",
		"compatibilityQualification",
		"sboms",
		"vulnerabilityReports",
		"dependencyAge",
		"licenses",
		"checksums",
		"signatures",
		"provenance",
	} {
		if !containsReleaseString(schema.Properties.Evidence.Required, gate) {
			t.Errorf("release evidence schema does not require gate %q", gate)
		}
	}
}

func TestCurrentCompatibilityDoesNotClaimUnimplementedDimensions(t *testing.T) {
	var compatibility struct {
		PublicAPI struct {
			QualifiedMajors []int `json:"qualifiedMajors"`
		} `json:"publicAPI"`
		RunnerProtocol struct {
			QualifiedGenerations []int `json:"qualifiedGenerations"`
		} `json:"runnerProtocol"`
		GuestProtocol struct {
			QualifiedGenerations []int `json:"qualifiedGenerations"`
		} `json:"guestProtocol"`
		Database struct {
			UpgradeQualification string `json:"upgradeQualification"`
		} `json:"database"`
		Profiles struct {
			SchemaQualification string `json:"schemaQualification"`
		} `json:"profiles"`
		Checkpoints struct {
			Qualification string `json:"qualification"`
		} `json:"checkpoints"`
		Artifacts struct {
			Qualification string `json:"qualification"`
		} `json:"artifacts"`
	}
	decodeReleaseRepositoryJSON(t, "release/current-compatibility.json", &compatibility)

	if len(compatibility.PublicAPI.QualifiedMajors) != 1 ||
		compatibility.PublicAPI.QualifiedMajors[0] != 1 {
		t.Error("current compatibility must qualify only public API v1")
	}
	if len(compatibility.RunnerProtocol.QualifiedGenerations) != 1 ||
		compatibility.RunnerProtocol.QualifiedGenerations[0] != 1 {
		t.Error("current compatibility must qualify only Runner protocol generation 1")
	}
	if len(compatibility.GuestProtocol.QualifiedGenerations) != 1 ||
		compatibility.GuestProtocol.QualifiedGenerations[0] != 1 {
		t.Error("current compatibility must qualify only guest protocol generation 1")
	}
	if compatibility.Database.UpgradeQualification != "not-qualified" ||
		compatibility.Profiles.SchemaQualification != "not-versioned" ||
		compatibility.Checkpoints.Qualification != "integrated-current-version-not-qualified" ||
		compatibility.Artifacts.Qualification != "integrated-current-version-not-qualified" {
		t.Error("current compatibility overstates an unimplemented or unqualified dimension")
	}
}

func TestReleaseEvidenceValidatorRejectsSchemaViolation(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	invalidEvidencePath := filepath.Join(t.TempDir(), "invalid-release-evidence.json")
	if err := os.WriteFile(invalidEvidencePath, []byte(`{"schemaVersion": 1}`), 0o600); err != nil {
		t.Fatalf("write invalid release evidence: %v", err)
	}

	validator := filepath.Join(repositoryRoot, "scripts", "validate-release-evidence.mjs")
	output, err := exec.Command("node", validator, invalidEvidencePath).CombinedOutput()
	if err == nil {
		t.Fatal("release evidence schema validator accepted incomplete evidence")
	}
	if !strings.Contains(string(output), "schema validation failed") {
		t.Fatalf("schema validator did not identify schema failure:\n%s", output)
	}
}

func TestReleasePublicationGateRejectsBlockedEvidence(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	evidenceDirectory := t.TempDir()
	blockedEvidencePath := filepath.Join(evidenceDirectory, "blocked-release-evidence.json")
	blockedEvidence := `{
	  "schemaVersion": 1,
	  "releaseVersion": "test",
	  "sourceCommit": "0123456789012345678901234567890123456789",
	  "generatedAt": "2026-07-28T00:00:00Z",
	  "compatibility": "nested/../current-compatibility.json",
	  "subjects": "release-subjects.json",
	  "evidence": {
	    "cleanCloneIsolation": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "nonKVMQualification": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "kvmQualification": {"status": "blocked", "summary": "KVM evidence is absent", "artifacts": []},
	    "multiRunnerQualification": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "durabilityQualification": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "dataPlaneQualification": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "networkQualification": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "securityQualification": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "compatibilityQualification": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "sboms": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "vulnerabilityReports": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "dependencyAge": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "licenses": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "checksums": {"status": "blocked", "summary": "test evidence", "artifacts": []},
	    "signatures": {
	      "status": "blocked",
	      "summary": "test evidence",
	      "artifacts": [],
	      "manifest": "package/../outside-manifest.json",
	      "signature": "manifest.sig",
	      "publicKey": "signing.pub",
	      "publicKeySHA256": "0000000000000000000000000000000000000000000000000000000000000000"
	    },
	    "provenance": {"status": "blocked", "summary": "test evidence", "artifacts": []}
	  }
	}`
	if err := os.WriteFile(blockedEvidencePath, []byte(blockedEvidence), 0o600); err != nil {
		t.Fatalf("write blocked release evidence: %v", err)
	}
	compatibility := readReleaseRepositoryFile(t, "release/current-compatibility.json")
	if err := os.WriteFile(
		filepath.Join(evidenceDirectory, "current-compatibility.json"),
		[]byte(compatibility),
		0o600,
	); err != nil {
		t.Fatalf("write compatibility evidence: %v", err)
	}

	gate := filepath.Join(repositoryRoot, "scripts", "verify-release-publication-eligibility.sh")
	output, err := exec.Command(gate, blockedEvidencePath).CombinedOutput()
	if err == nil {
		t.Fatal("publication gate accepted blocked release evidence")
	}
	if !strings.Contains(string(output), "kvmQualification") {
		t.Fatalf("publication gate did not identify blocked KVM evidence:\n%s", output)
	}
	if !strings.Contains(string(output), "unsafe signature manifest path") {
		t.Fatalf("publication gate did not reject signature path traversal:\n%s", output)
	}
	if !strings.Contains(string(output), "unsafe compatibility evidence path") {
		t.Fatalf("publication gate did not reject compatibility path traversal:\n%s", output)
	}
}

func TestCleanCloneIsolationScanPassesCurrentSourceTree(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	script := filepath.Join(repositoryRoot, "scripts", "test-clean-clone-isolation.sh")
	command := exec.Command(script, "--scan-only")
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clean-clone isolation source scan failed: %v\n%s", err, output)
	}
}

func TestReleasePackagingRefusesDirtySourceTree(t *testing.T) {
	script := readReleaseRepositoryFile(t, "scripts/package-release-artifacts.sh")
	for _, required := range []string{
		"status --porcelain=v1 --untracked-files=all",
		"requires a clean source tree",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("release packaging must contain %q", required)
		}
	}
}

func TestReleaseArchiveEnforcesArchitectureTrustBootstrapAndModes(t *testing.T) {
	buildScript := readReleaseRepositoryFile(t, "scripts/build-artifacts.sh")
	if count := strings.Count(buildScript, "GOOS=linux GOARCH=amd64 go build"); count != 6 {
		t.Errorf("release build must explicitly target linux-amd64 for all six binaries; found %d commands", count)
	}

	packageScript := readReleaseRepositoryFile(t, "scripts/package-release-artifacts.sh")
	for _, required := range []string{
		"verify_linux_amd64_binary",
		"readelf -h",
		"ELF64",
		"Advanced Micro Devices X86-64",
		"bin/bootstrap-runner-trust.sh",
		"bin/prepare-development-inventory.sh",
		`if [[ "$deployment_path" == bin/*.sh ]]`,
		"deployment_mode=0755",
		`--arg mode "$(stat -c %a "$file_path")"`,
		`cmp -s "$binary_path" "$package_root/bin/$binary_name"`,
		`cmp -s \`,
	} {
		if !strings.Contains(packageScript, required) {
			t.Errorf("release packaging must contain %q", required)
		}
	}
}

func TestReleaseSupplyChainCoverageRejectsMissingSubjectAndDigestMismatch(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	verifier := filepath.Join(
		repositoryRoot,
		"scripts",
		"verify-release-supply-chain-coverage.mjs",
	)
	requiredSubjectIDs := []string{
		"linux-release-package",
		"secondbox",
		"secondbox-artifact-evidence",
		"secondbox-guest-agent",
		"secondbox-runner",
		"secondbox-runner-identity",
		"secondboxd",
		"control-plane-image",
		"runner-image",
		"guest-artifact-image",
		"guest-execution-bundle",
		"go-sdk-package",
		"typescript-sdk-package",
	}
	const candidateDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	subjects := make([]map[string]any, 0, len(requiredSubjectIDs))
	coverage := make([]map[string]any, 0, len(requiredSubjectIDs))
	for _, subjectID := range requiredSubjectIDs {
		subject := map[string]any{
			"id":      subjectID,
			"kind":    releaseSubjectKind(subjectID),
			"status":  "passed",
			"summary": "fixture subject resolved",
			"locator": fmt.Sprintf("fixture/%s", subjectID),
			"digest":  map[string]string{"sha256": candidateDigest},
		}
		if subjectID == "guest-artifact-image" {
			subject["locator"] = fmt.Sprintf(
				"ghcr.io/secondstack-ai/secondbox-guest-artifacts@sha256:%s",
				candidateDigest,
			)
			subject["bindings"] = []map[string]any{{
				"subjectId": "guest-execution-bundle",
				"digest":    map[string]string{"sha256": candidateDigest},
			}}
		}
		subjects = append(subjects, subject)
		coverage = append(coverage, map[string]any{
			"subjectId":     subjectID,
			"subjectSHA256": candidateDigest,
			"status":        "passed",
			"summary":       "fixture evidence covers the exact subject",
			"artifacts": []map[string]string{{
				"path":   fmt.Sprintf("evidence/%s.json", subjectID),
				"sha256": candidateDigest,
			}},
		})
	}
	subjectManifest := map[string]any{
		"schemaVersion":  1,
		"releaseVersion": "1.0.0-test",
		"sourceCommit":   "0123456789012345678901234567890123456789",
		"status":         "passed",
		"summary":        "all fixture subjects resolved",
		"subjects":       subjects,
	}
	coverageReport := map[string]any{
		"schemaVersion":  1,
		"evidenceType":   "sbom",
		"releaseVersion": "1.0.0-test",
		"sourceCommit":   "0123456789012345678901234567890123456789",
		"status":         "passed",
		"summary":        "fixture coverage is complete",
		"subjects":       coverage,
	}
	fixtureDirectory := t.TempDir()
	subjectPath := filepath.Join(fixtureDirectory, "release-subjects.json")
	coveragePath := filepath.Join(fixtureDirectory, "sbom-status.json")
	writeReleaseJSONFixture(t, subjectPath, subjectManifest)

	t.Run("missing subject", func(t *testing.T) {
		missing := cloneReleaseCoverageReport(t, coverageReport)
		missing["subjects"] = missing["subjects"].([]any)[:len(requiredSubjectIDs)-1]
		writeReleaseJSONFixture(t, coveragePath, missing)
		output, err := exec.Command("node", verifier, subjectPath, coveragePath, "sbom").CombinedOutput()
		if err == nil {
			t.Fatal("supply-chain verifier accepted a report that omitted a required subject")
		}
		if !strings.Contains(string(output), "missing subject") {
			t.Fatalf("missing-subject failure was not specific:\n%s", output)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		mismatched := cloneReleaseCoverageReport(t, coverageReport)
		mismatchedSubjects := mismatched["subjects"].([]any)
		mismatchedSubjects[0].(map[string]any)["subjectSHA256"] =
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		writeReleaseJSONFixture(t, coveragePath, mismatched)
		output, err := exec.Command("node", verifier, subjectPath, coveragePath, "sbom").CombinedOutput()
		if err == nil {
			t.Fatal("supply-chain verifier accepted evidence for a different subject digest")
		}
		if !strings.Contains(string(output), "digest mismatch") {
			t.Fatalf("digest-mismatch failure was not specific:\n%s", output)
		}
	})

	t.Run("subject kind mismatch", func(t *testing.T) {
		mismatched := cloneReleaseCoverageReport(t, subjectManifest)
		mismatchedSubjects := mismatched["subjects"].([]any)
		mismatchedSubjects[0].(map[string]any)["kind"] = "linux-binary"
		writeReleaseJSONFixture(t, subjectPath, mismatched)
		writeReleaseJSONFixture(t, coveragePath, coverageReport)
		output, err := exec.Command("node", verifier, subjectPath, coveragePath, "sbom").CombinedOutput()
		if err == nil {
			t.Fatal("supply-chain verifier accepted the wrong subject kind")
		}
		if !strings.Contains(string(output), "expected release-package") {
			t.Fatalf("subject-kind failure was not specific:\n%s", output)
		}
	})
}

func TestMissingReleaseSubjectsProduceBlockedChecksumAndSignatureEvidence(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	evidenceDirectory := t.TempDir()
	for _, relativeDirectory := range []string{
		"checksums",
		"dist",
		"guest",
		"package",
		"sdk",
		"signatures",
	} {
		if err := os.Mkdir(filepath.Join(evidenceDirectory, relativeDirectory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceCommitOutput, err := exec.Command(
		"git",
		"-C",
		repositoryRoot,
		"rev-parse",
		"HEAD",
	).Output()
	if err != nil {
		t.Fatal(err)
	}
	sourceCommit := strings.TrimSpace(string(sourceCommitOutput))
	subjectPath := filepath.Join(evidenceDirectory, "release-subjects.json")
	candidateEnvironment := releaseEvidenceTestEnvironment(map[string]string{
		"SECONDBOX_RELEASE_VERSION":       "1.0.0-test",
		"SECONDBOX_RELEASE_SOURCE_COMMIT": sourceCommit,
	})

	subjectCommand := exec.Command(
		"node",
		filepath.Join(repositoryRoot, "scripts", "generate-release-subject-manifest.mjs"),
		evidenceDirectory,
		subjectPath,
	)
	subjectCommand.Env = candidateEnvironment
	if output, err := subjectCommand.CombinedOutput(); err != nil {
		t.Fatalf("generate blocked subject manifest: %v\n%s", err, output)
	}
	var subjects struct {
		Status   string `json:"status"`
		Subjects []struct {
			Status string `json:"status"`
		} `json:"subjects"`
	}
	decodeReleaseJSONFile(t, subjectPath, &subjects)
	if subjects.Status != "blocked" || len(subjects.Subjects) != 13 {
		t.Fatalf("missing candidate subjects = %#v", subjects)
	}
	for _, subject := range subjects.Subjects {
		if subject.Status != "blocked" {
			t.Fatalf("missing release subject unexpectedly passed: %#v", subject)
		}
	}

	for _, invocation := range []struct {
		script    string
		outputDir string
		status    string
	}{
		{
			script:    "generate-release-checksum-evidence.sh",
			outputDir: "checksums",
			status:    "checksum-status.json",
		},
		{
			script:    "generate-release-signature-evidence.sh",
			outputDir: "signatures",
			status:    "signature-status.json",
		},
	} {
		command := exec.Command(
			filepath.Join(repositoryRoot, "scripts", invocation.script),
			subjectPath,
			filepath.Join(evidenceDirectory, invocation.outputDir),
		)
		command.Env = candidateEnvironment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s blocked evidence: %v\n%s", invocation.script, err, output)
		}
		var report struct {
			Status   string `json:"status"`
			Subjects []struct {
				Status string `json:"status"`
			} `json:"subjects"`
		}
		decodeReleaseJSONFile(
			t,
			filepath.Join(evidenceDirectory, invocation.outputDir, invocation.status),
			&report,
		)
		if report.Status != "blocked" || len(report.Subjects) != 13 {
			t.Fatalf("%s report = %#v", invocation.script, report)
		}
	}
	if _, err := os.Stat(filepath.Join(evidenceDirectory, "signatures", "release-subjects.sig")); !os.IsNotExist(err) {
		t.Fatal("blocked signature evidence wrote a signature without an approved key")
	}
}

func TestDependencyAgeEvidenceCoversPinnedGuestAndRunnerInputs(t *testing.T) {
	script := readReleaseRepositoryFile(t, "scripts/generate-dependency-age-evidence.sh")

	for _, required := range []string{
		"runner/internal/firecracker/firecracker.lock",
		"firecracker-v%s-x86_64.tgz",
		"runner/scripts/microvm-image/kernel.lock",
		"https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$kernel_version.tar.xz",
		"secondbox-debian-image-definition.json",
		"snapshot.debian.org/archive/debian",
		"rootfs-python.freeze",
		"https://pypi.org/pypi/",
		"upload_time_iso_8601",
		"fileSHA256s",
		"unsupported-freeze-entry",
		"invalid-guest-archive",
		"No dependency-age inputs are mapped to the exact release subject",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("dependency-age evidence must contain %q", required)
		}
	}
	for _, incomplete := range []string{
		"Firecracker release publication age is not yet included",
		"Kernel, Debian snapshot, and resolved guest Python publication ages are not yet included",
	} {
		if strings.Contains(script, incomplete) {
			t.Errorf("dependency-age evidence still contains incomplete gate %q", incomplete)
		}
	}
}

func TestFrozenFlueCompatibilityDoesNotRestoreRuntimeDependency(t *testing.T) {
	packageManifest := readReleaseRepositoryFile(t, "package.json")
	packageLock := readReleaseRepositoryFile(t, "package-lock.json")
	compatibilitySource := readReleaseRepositoryFile(
		t,
		"sdk/typescript/flue-runtime-beta9-compat.ts",
	)

	if strings.Contains(packageManifest, `"@flue/runtime"`) ||
		strings.Contains(packageLock, `"@flue/runtime"`) ||
		strings.Contains(packageLock, "node_modules/@flue/") {
		t.Fatal("frozen Flue compatibility must not restore the @flue/runtime dependency graph")
	}
	for _, required := range []string{
		"SPDX-License-Identifier: Apache-2.0",
		"createSandboxSessionEnv",
		"interface SandboxApi",
		"interface SandboxFactory",
		"interface SessionEnv",
	} {
		if !strings.Contains(compatibilitySource, required) {
			t.Errorf("frozen Flue compatibility source must contain %q", required)
		}
	}
}

func decodeReleaseJSONFile(t *testing.T, path string, destination any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatal(err)
	}
}

func releaseEvidenceTestEnvironment(overrides map[string]string) []string {
	removed := map[string]bool{
		"SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE":       true,
		"SECONDBOX_RELEASE_RUNNER_IMAGE":              true,
		"SECONDBOX_RELEASE_SIGNING_PRIVATE_KEY":       true,
		"SECONDBOX_RELEASE_TRUSTED_PUBLIC_KEY_SHA256": true,
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, setting := range os.Environ() {
		name, _, found := strings.Cut(setting, "=")
		if found && (removed[name] || overrides[name] != "") {
			continue
		}
		environment = append(environment, setting)
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func releaseSubjectKind(subjectID string) string {
	switch subjectID {
	case "control-plane-image", "runner-image", "guest-artifact-image":
		return "oci-image"
	case "guest-execution-bundle":
		return "guest-bundle"
	case "go-sdk-package":
		return "go-sdk"
	case "typescript-sdk-package":
		return "npm-package"
	case "linux-release-package":
		return "release-package"
	default:
		return "linux-binary"
	}
}

func writeReleaseJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode release fixture: %v", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write release fixture: %v", err)
	}
}

func cloneReleaseCoverageReport(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func readReleaseRepositoryFile(t *testing.T, relativePath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(releaseRepositoryRoot(t), filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatalf("read %s: %v", relativePath, err)
	}
	return string(data)
}

func decodeReleaseRepositoryJSON(t *testing.T, relativePath string, destination any) {
	t.Helper()
	data := readReleaseRepositoryFile(t, relativePath)
	if err := json.Unmarshal([]byte(data), destination); err != nil {
		t.Fatalf("decode %s: %v", relativePath, err)
	}
}

func releaseRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve release policy test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
}

func containsReleaseString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
