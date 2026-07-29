package release_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var packagedQualificationGates = []string{
	"kvm",
	"durability",
	"data-plane",
	"network",
	"security",
}

type packagedQualificationHarnessFixture struct {
	candidateDirectory  string
	controllerDirectory string
	hostInventoryPath   string
	outputDirectory     string
	subjectManifestPath string
}

func TestPackagedQualificationHarnessExecutesRequiredScenariosAndEmitsVerifiedRecords(
	t *testing.T,
) {
	fixture := newPackagedQualificationHarnessFixture(t)
	repositoryRoot := releaseRepositoryRoot(t)
	harness := filepath.Join(repositoryRoot, "scripts", "run-packaged-release-qualification.mjs")

	output, err := exec.Command(
		"node",
		append(
			[]string{
				harness,
				fixture.candidateDirectory,
				fixture.subjectManifestPath,
				fixture.hostInventoryPath,
				fixture.controllerDirectory,
				fixture.outputDirectory,
			},
			packagedQualificationGates...,
		)...,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("run packaged qualification harness: %v\n%s", err, output)
	}

	verifier := filepath.Join(repositoryRoot, "scripts", "verify-release-qualification-record.mjs")
	for _, gate := range packagedQualificationGates {
		recordPath := filepath.Join(fixture.outputDirectory, "qualification", gate+".json")
		verifyOutput, verifyErr := exec.Command(
			"node",
			verifier,
			fixture.subjectManifestPath,
			recordPath,
			gate,
			fixture.outputDirectory,
		).CombinedOutput()
		if verifyErr != nil {
			t.Fatalf("verify generated %s qualification record: %v\n%s", gate, verifyErr, verifyOutput)
		}
	}
}

func TestFirecrackerEntryPointRunsPackagedKVMGate(t *testing.T) {
	fixture := newPackagedQualificationHarnessFixture(t)
	repositoryRoot := releaseRepositoryRoot(t)
	entryPoint := filepath.Join(repositoryRoot, "scripts", "test-firecracker.sh")
	command := exec.Command(entryPoint)
	command.Env = append(
		os.Environ(),
		"SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY="+fixture.candidateDirectory,
		"SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST="+fixture.subjectManifestPath,
		"SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE="+fixture.hostInventoryPath,
		"SECONDBOX_RELEASE_QUALIFICATION_CONTROLLER_DIRECTORY="+fixture.controllerDirectory,
		"SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY="+fixture.outputDirectory,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run packaged Firecracker entry point: %v\n%s", err, output)
	}
	recordPath := filepath.Join(fixture.outputDirectory, "qualification", "kvm.json")
	verifyOutput, verifyErr := exec.Command(
		"node",
		filepath.Join(repositoryRoot, "scripts", "verify-release-qualification-record.mjs"),
		fixture.subjectManifestPath,
		recordPath,
		"kvm",
		fixture.outputDirectory,
	).CombinedOutput()
	if verifyErr != nil {
		t.Fatalf("verify packaged KVM record: %v\n%s", verifyErr, verifyOutput)
	}
}

func TestPackagedQualificationHarnessFailsBeforeExecutionForMissingInputs(
	t *testing.T,
) {
	t.Run("missing required scenario controller", func(t *testing.T) {
		fixture := newPackagedQualificationHarnessFixture(t)
		missingController := filepath.Join(
			fixture.controllerDirectory,
			"security",
			"control-plane-secret-isolation",
		)
		if err := os.Remove(missingController); err != nil {
			t.Fatal(err)
		}

		output, err := runPackagedQualificationHarness(t, fixture)
		if err == nil {
			t.Fatalf("qualification harness accepted missing controller:\n%s", output)
		}
		if !strings.Contains(string(output), "control-plane-secret-isolation") {
			t.Fatalf("missing-controller failure omitted scenario identity:\n%s", output)
		}
		assertQualificationHarnessDidNotExecute(t, fixture.outputDirectory)
	})

	t.Run("candidate subject bytes drifted", func(t *testing.T) {
		fixture := newPackagedQualificationHarnessFixture(t)
		subjectPath := filepath.Join(
			fixture.candidateDirectory,
			"subjects",
			"secondbox-runner",
		)
		if err := os.WriteFile(subjectPath, []byte("changed candidate bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		output, err := runPackagedQualificationHarness(t, fixture)
		if err == nil {
			t.Fatalf("qualification harness accepted changed candidate bytes:\n%s", output)
		}
		if !strings.Contains(string(output), "secondbox-runner") ||
			!strings.Contains(string(output), "mismatch") {
			t.Fatalf("candidate-drift failure omitted subject evidence:\n%s", output)
		}
		assertQualificationHarnessDidNotExecute(t, fixture.outputDirectory)
	})

	t.Run("host inventory does not satisfy KVM gate", func(t *testing.T) {
		fixture := newPackagedQualificationHarnessFixture(t)
		hostInventory := map[string]any{
			"schemaVersion": 1,
			"hosts": []map[string]any{{
				"id": "runner-without-kvm", "role": "runner", "deploymentMode": "systemd",
				"operatingSystem": "linux", "architecture": "amd64",
				"dedicated": true, "kvm": false,
			}},
		}
		writeReleaseJSONFixture(t, fixture.hostInventoryPath, hostInventory)

		output, err := runPackagedQualificationHarness(t, fixture)
		if err == nil {
			t.Fatalf("qualification harness accepted unqualified hosts:\n%s", output)
		}
		if !strings.Contains(string(output), "dedicated KVM runner") {
			t.Fatalf("host-prerequisite failure was not explicit:\n%s", output)
		}
		assertQualificationHarnessDidNotExecute(t, fixture.outputDirectory)
	})
}

func TestPackagedQualificationHarnessFailsOnRealScenarioPrerequisiteFailure(
	t *testing.T,
) {
	fixture := newPackagedQualificationHarnessFixture(t)
	failingController := filepath.Join(
		fixture.controllerDirectory,
		"kvm",
		"qualified-host-prerequisites",
	)
	if err := os.WriteFile(
		failingController,
		[]byte("#!/usr/bin/env bash\nset -euo pipefail\necho 'read/write /dev/kvm is absent' >&2\nexit 19\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(
		releaseRepositoryRoot(t),
		"scripts",
		"run-packaged-release-qualification.mjs",
	)
	output, err := exec.Command(
		"node",
		harness,
		fixture.candidateDirectory,
		fixture.subjectManifestPath,
		fixture.hostInventoryPath,
		fixture.controllerDirectory,
		fixture.outputDirectory,
		"kvm",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("qualification harness accepted failed KVM prerequisite:\n%s", output)
	}
	if !strings.Contains(string(output), "kvm/qualified-host-prerequisites") ||
		!strings.Contains(string(output), "exit 19") {
		t.Fatalf("scenario prerequisite failure omitted exact identity:\n%s", output)
	}
	if _, statErr := os.Stat(
		filepath.Join(fixture.outputDirectory, "qualification", "kvm.json"),
	); !os.IsNotExist(statErr) {
		t.Fatalf("failed KVM scenario produced a passing record: %v", statErr)
	}
	stderrContents, readErr := os.ReadFile(filepath.Join(
		fixture.outputDirectory,
		"qualification",
		"scenarios",
		"kvm",
		"qualified-host-prerequisites",
		"controller.stderr.log",
	))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(stderrContents), "/dev/kvm is absent") {
		t.Fatalf("failed KVM prerequisite artifact omitted controller evidence:\n%s", stderrContents)
	}
}

func TestPackagedQualificationWorkflowRunsHarnessAgainstProtectedCandidate(t *testing.T) {
	workflow := readReleaseRepositoryFile(t, ".github/workflows/release-qualification.yml")
	for _, required := range []string{
		"SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE",
		"SECONDBOX_RELEASE_QUALIFICATION_CONTROLLER_DIRECTORY",
		"scripts/run-packaged-release-qualification.mjs",
		".tmp/qualification-candidate/release-subjects.json",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("protected qualification workflow must contain %q", required)
		}
	}
	if strings.Contains(workflow, "SECONDBOX_RELEASE_QUALIFICATION_RESULTS_DIRECTORY") {
		t.Error("protected qualification workflow must not import pre-authored non-multi-runner results")
	}

	justfile := readReleaseRepositoryFile(t, "Justfile")
	if !strings.Contains(justfile, "scripts/test-firecracker.sh") {
		t.Error("test-firecracker must remain the operator entry point")
	}
	firecracker := readReleaseRepositoryFile(t, "scripts/test-firecracker.sh")
	for _, required := range []string{
		"SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY",
		"SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST",
		"SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE",
		"SECONDBOX_RELEASE_QUALIFICATION_CONTROLLER_DIRECTORY",
		"SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY",
		"run-packaged-release-qualification.mjs",
		"kvm",
	} {
		if !strings.Contains(firecracker, required) {
			t.Errorf("test-firecracker packaged harness wrapper must contain %q", required)
		}
	}
}

func newPackagedQualificationHarnessFixture(
	t *testing.T,
) packagedQualificationHarnessFixture {
	t.Helper()
	root := t.TempDir()
	candidateDirectory := filepath.Join(root, "candidate")
	controllerDirectory := filepath.Join(root, "controllers")
	outputDirectory := filepath.Join(root, "output")
	for _, directory := range []string{
		candidateDirectory,
		controllerDirectory,
		outputDirectory,
	} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	subjectManifestPath := writePackagedQualificationSubjectManifest(
		t,
		candidateDirectory,
	)
	manifestContents, err := os.ReadFile(subjectManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestContents)
	manifestSHA256 := hex.EncodeToString(manifestDigest[:])

	hostInventoryPath := filepath.Join(root, "qualified-hosts.json")
	writeReleaseJSONFixture(t, hostInventoryPath, map[string]any{
		"schemaVersion": 1,
		"hosts": []map[string]any{
			{
				"id": "runner-systemd", "role": "runner", "deploymentMode": "systemd",
				"operatingSystem": "linux", "architecture": "amd64",
				"dedicated": true, "kvm": true,
			},
			{
				"id": "runner-compose", "role": "runner", "deploymentMode": "compose",
				"operatingSystem": "linux", "architecture": "amd64",
				"dedicated": true, "kvm": true,
			},
		},
	})

	var requirements struct {
		Gates map[string]struct {
			ScenarioIDs []string `json:"scenarioIds"`
		} `json:"gates"`
	}
	decodeReleaseRepositoryJSON(t, "release/qualification-requirements.json", &requirements)
	subjectIDs := releaseQualificationSubjectIDs(t)
	for _, gate := range packagedQualificationGates {
		gateDirectory := filepath.Join(controllerDirectory, gate)
		if err := os.Mkdir(gateDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, scenarioID := range requirements.Gates[gate].ScenarioIDs {
			writePackagedQualificationController(
				t,
				filepath.Join(gateDirectory, scenarioID),
				gate,
				scenarioID,
				manifestSHA256,
				subjectIDs,
			)
		}
	}

	return packagedQualificationHarnessFixture{
		candidateDirectory:  candidateDirectory,
		controllerDirectory: controllerDirectory,
		hostInventoryPath:   hostInventoryPath,
		outputDirectory:     outputDirectory,
		subjectManifestPath: subjectManifestPath,
	}
}

func writePackagedQualificationSubjectManifest(
	t *testing.T,
	candidateDirectory string,
) string {
	t.Helper()
	subjectDirectory := filepath.Join(candidateDirectory, "subjects")
	if err := os.Mkdir(subjectDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	subjects := make([]map[string]any, 0)
	for _, subjectID := range releaseQualificationSubjectIDs(t) {
		kind := releaseSubjectKind(subjectID)
		var locator string
		var digest string
		if kind == "oci-image" {
			contentDigest := sha256.Sum256([]byte(subjectID))
			digest = hex.EncodeToString(contentDigest[:])
			locator = "ghcr.io/secondstack-ai/" + subjectID + "@sha256:" + digest
		} else {
			locator = "subjects/" + subjectID
			content := []byte("packaged subject " + subjectID + "\n")
			if err := os.WriteFile(
				filepath.Join(subjectDirectory, subjectID),
				content,
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			contentDigest := sha256.Sum256(content)
			digest = hex.EncodeToString(contentDigest[:])
		}
		subject := map[string]any{
			"id":      subjectID,
			"kind":    kind,
			"status":  "passed",
			"summary": "packaged harness fixture subject",
			"locator": locator,
			"digest":  map[string]string{"sha256": digest},
		}
		if kind != "oci-image" {
			subject["sizeBytes"] = int64(len("packaged subject " + subjectID + "\n"))
		}
		if subjectID == "guest-artifact-image" {
			guestBundle := subjects[len(subjects)-1]
			subject["bindings"] = []map[string]any{{
				"subjectId": "guest-execution-bundle",
				"digest":    guestBundle["digest"],
			}}
		}
		subjects = append(subjects, subject)
	}
	manifest := map[string]any{
		"schemaVersion":  1,
		"releaseVersion": "1.0.0-test",
		"sourceCommit":   qualificationFixtureCommit,
		"status":         "passed",
		"summary":        "all packaged harness fixture subjects resolved",
		"subjects":       subjects,
	}
	manifestPath := filepath.Join(candidateDirectory, "release-subjects.json")
	writeReleaseJSONFixture(t, manifestPath, manifest)
	return manifestPath
}

func writePackagedQualificationController(
	t *testing.T,
	controllerPath string,
	gate string,
	scenarioID string,
	manifestSHA256 string,
	subjectIDs []string,
) {
	t.Helper()
	subjectIDsJSON, err := json.Marshal(subjectIDs)
	if err != nil {
		t.Fatal(err)
	}
	controller := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 6 ]]; then
  echo "fixture controller requires six explicit arguments" >&2
  exit 2
fi
candidate_directory="$1"
subject_manifest="$2"
host_inventory="$3"
artifact_directory="$4"
gate="$5"
scenario_id="$6"
[[ "$gate" == %q ]]
[[ "$scenario_id" == %q ]]
[[ "$(realpath -e -- "$subject_manifest")" == "$(realpath -e -- "$candidate_directory/release-subjects.json")" ]]
[[ "$(sha256sum "$subject_manifest" | awk '{print $1}')" == %q ]]
[[ -f "$host_inventory" ]]
printf '{"gate":%q,"scenarioId":%q,"candidate":"%%s"}\n' \
  "$candidate_directory" >"$artifact_directory/observations.json"
artifact_sha256="$(sha256sum "$artifact_directory/observations.json" | awk '{print $1}')"
cat >"$artifact_directory/result.json" <<JSON
{
  "schemaVersion": 1,
  "gate": %q,
  "scenarioId": %q,
  "releaseVersion": "1.0.0-test",
  "sourceCommit": %q,
  "subjectManifestSHA256": %q,
  "status": "passed",
  "summary": "fixture executed the packaged scenario",
  "hostIds": ["runner-systemd", "runner-compose"],
  "subjectIds": %s,
  "artifacts": [
    {"path": "observations.json", "sha256": "$artifact_sha256"}
  ]
}
JSON
`, gate, scenarioID, manifestSHA256, gate, scenarioID, gate, scenarioID,
		qualificationFixtureCommit, manifestSHA256, subjectIDsJSON)
	if err := os.WriteFile(controllerPath, []byte(controller), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runPackagedQualificationHarness(
	t *testing.T,
	fixture packagedQualificationHarnessFixture,
) ([]byte, error) {
	t.Helper()
	harness := filepath.Join(
		releaseRepositoryRoot(t),
		"scripts",
		"run-packaged-release-qualification.mjs",
	)
	return exec.Command(
		"node",
		append(
			[]string{
				harness,
				fixture.candidateDirectory,
				fixture.subjectManifestPath,
				fixture.hostInventoryPath,
				fixture.controllerDirectory,
				fixture.outputDirectory,
			},
			packagedQualificationGates...,
		)...,
	).CombinedOutput()
}

func assertQualificationHarnessDidNotExecute(t *testing.T, outputDirectory string) {
	t.Helper()
	entries, err := os.ReadDir(outputDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("qualification harness mutated output before preflight: %#v", entries)
	}
}
