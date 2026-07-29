package release_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultiRunnerRecordGeneratorBindsExecutedScenarioArtifacts(t *testing.T) {
	fixture, hostsPath := newMultiRunnerGeneratorFixture(t)
	repositoryRoot := releaseRepositoryRoot(t)
	generator := filepath.Join(repositoryRoot, "scripts", "generate-multirunner-qualification-record.mjs")
	verifier := filepath.Join(repositoryRoot, "scripts", "verify-release-qualification-record.mjs")

	output, err := exec.Command(
		"node",
		generator,
		fixture.manifestPath,
		fixture.directory,
		fixture.recordPath,
		"2026-07-28T00:00:00Z",
		"2026-07-28T00:01:00Z",
		hostsPath,
		"runner-systemd",
		"runner-compose",
		"controller-1",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("multi-Runner record generator rejected complete evidence: %v\n%s", err, output)
	}
	runQualificationVerifier(t, verifier, fixture, true, "")
}

func TestMultiRunnerRecordGeneratorRejectsScenarioFromAnotherCandidate(t *testing.T) {
	fixture, hostsPath := newMultiRunnerGeneratorFixture(t)
	repositoryRoot := releaseRepositoryRoot(t)
	generator := filepath.Join(repositoryRoot, "scripts", "generate-multirunner-qualification-record.mjs")
	scenarios := fixture.record["scenarios"].([]map[string]any)
	firstScenario := scenarios[0]["id"].(string)
	artifactPath := filepath.Join(
		fixture.directory,
		"qualification",
		"multi-runner",
		firstScenario+".json",
	)
	var artifact map[string]any
	artifactContents, readErr := os.ReadFile(artifactPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if decodeErr := json.Unmarshal(artifactContents, &artifact); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	artifact["sourceCommit"] = strings.Repeat("f", 40)
	writeReleaseJSONFixture(t, artifactPath, artifact)

	output, err := exec.Command(
		"node",
		generator,
		fixture.manifestPath,
		fixture.directory,
		fixture.recordPath,
		"2026-07-28T00:00:00Z",
		"2026-07-28T00:01:00Z",
		hostsPath,
		"runner-systemd",
		"runner-compose",
		"controller-1",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("multi-Runner record generator accepted another candidate:\n%s", output)
	}
	if !strings.Contains(string(output), "is not bound to the candidate") {
		t.Fatalf("multi-Runner record generator returned an unclear error:\n%s", output)
	}
}

func newMultiRunnerGeneratorFixture(t *testing.T) (*qualificationFixture, string) {
	t.Helper()
	fixture := newQualificationFixture(t, "multi-runner")
	if err := os.Remove(fixture.recordPath); err != nil {
		t.Fatal(err)
	}
	manifestContents, err := os.ReadFile(fixture.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestContents)
	scenarioDirectory := filepath.Join(fixture.directory, "qualification", "multi-runner")
	if err := os.MkdirAll(scenarioDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range fixture.record["scenarios"].([]map[string]any) {
		scenarioID := scenario["id"].(string)
		writeReleaseJSONFixture(
			t,
			filepath.Join(scenarioDirectory, scenarioID+".json"),
			map[string]any{
				"schemaVersion":         1,
				"sourceCommit":          qualificationFixtureCommit,
				"subjectManifestSHA256": hex.EncodeToString(manifestDigest[:]),
				"scenarioId":            scenarioID,
				"status":                "passed",
				"summary":               "fixture scenario passed",
				"evidence":              map[string]any{"observation": "executed"},
			},
		)
	}
	hosts := append(
		[]map[string]any{{
			"id": "controller-1", "role": "controller", "deploymentMode": "external",
			"operatingSystem": "linux", "architecture": "amd64", "dedicated": false, "kvm": false,
		}},
		fixture.record["hosts"].([]map[string]any)...,
	)
	hostsPath := filepath.Join(fixture.directory, "qualified-hosts.json")
	writeReleaseJSONFixture(t, hostsPath, map[string]any{
		"schemaVersion": 1,
		"hosts":         hosts,
	})
	return fixture, hostsPath
}
