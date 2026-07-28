package release_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const qualificationFixtureCommit = "0123456789012345678901234567890123456789"

type qualificationFixture struct {
	directory    string
	manifest     map[string]any
	manifestPath string
	record       map[string]any
	recordPath   string
	gate         string
}

func TestQualificationRecordVerifierRequiresExactSubjectsScenariosAndArtifacts(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	verifier := filepath.Join(repositoryRoot, "scripts", "verify-release-qualification-record.mjs")

	for _, gate := range []string{
		"kvm",
		"multi-runner",
		"durability",
		"data-plane",
		"network",
		"security",
	} {
		t.Run("accepts complete "+gate+" record", func(t *testing.T) {
			fixture := newQualificationFixture(t, gate)
			runQualificationVerifier(t, verifier, fixture, true, "")
		})
	}

	tests := []struct {
		name        string
		mutate      func(*testing.T, *qualificationFixture)
		wantFailure string
	}{
		{
			name: "candidate identity mismatch",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["releaseVersion"] = "different"
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "candidate identity mismatch",
		},
		{
			name: "subject manifest digest mismatch",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["subjectManifestSHA256"] = strings.Repeat("f", 64)
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "subject manifest digest mismatch",
		},
		{
			name: "missing subject",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				subjects := fixture.record["subjects"].([]map[string]any)
				fixture.record["subjects"] = subjects[:len(subjects)-1]
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "subject count",
		},
		{
			name: "duplicate subject",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				subjects := fixture.record["subjects"].([]map[string]any)
				subjects[len(subjects)-1]["id"] = subjects[0]["id"]
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "duplicate id",
		},
		{
			name: "subject digest mismatch",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["subjects"].([]map[string]any)[0]["sha256"] = strings.Repeat("e", 64)
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "digest mismatch",
		},
		{
			name: "subject locator mismatch",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["subjects"].([]map[string]any)[0]["locator"] = "different"
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "locator mismatch",
		},
		{
			name: "gate mismatch",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["gate"] = "network"
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "does not match",
		},
		{
			name: "missing mandatory scenario",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				scenarios := fixture.record["scenarios"].([]map[string]any)
				fixture.record["scenarios"] = scenarios[:len(scenarios)-1]
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "scenario count",
		},
		{
			name: "duplicate scenario",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				scenarios := fixture.record["scenarios"].([]map[string]any)
				scenarios[len(scenarios)-1]["id"] = scenarios[0]["id"]
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "duplicate id",
		},
		{
			name: "failed scenario",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["scenarios"].([]map[string]any)[0]["status"] = "failed"
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "status is failed",
		},
		{
			name: "failed record",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["status"] = "failed"
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "record status is failed",
		},
		{
			name: "scenario without artifact",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["scenarios"].([]map[string]any)[0]["artifacts"] = []map[string]string{}
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "schema validation failed",
		},
		{
			name: "unsafe artifact path",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["scenarios"].([]map[string]any)[0]["artifacts"] = []map[string]string{{
					"path":   "qualification/../outside.log",
					"sha256": strings.Repeat("a", 64),
				}}
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "unsafe artifact path",
		},
		{
			name: "artifact digest mismatch",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				artifacts := fixture.record["scenarios"].([]map[string]any)[0]["artifacts"].([]map[string]string)
				artifacts[0]["sha256"] = strings.Repeat("d", 64)
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "artifact digest mismatch",
		},
		{
			name: "missing artifact",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				artifact := fixture.record["scenarios"].([]map[string]any)[0]["artifacts"].([]map[string]string)[0]
				if err := os.Remove(filepath.Join(fixture.directory, filepath.FromSlash(artifact["path"]))); err != nil {
					t.Fatal(err)
				}
			},
			wantFailure: "qualification artifact",
		},
		{
			name: "symbolic artifact",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				scenario := fixture.record["scenarios"].([]map[string]any)[0]
				original := scenario["artifacts"].([]map[string]string)[0]
				linkRelative := "qualification/scenarios/kvm/symbolic.log"
				linkPath := filepath.Join(fixture.directory, filepath.FromSlash(linkRelative))
				if err := os.Symlink(
					filepath.Base(filepath.Join(fixture.directory, filepath.FromSlash(original["path"]))),
					linkPath,
				); err != nil {
					t.Fatal(err)
				}
				scenario["artifacts"] = []map[string]string{{
					"path":   linkRelative,
					"sha256": original["sha256"],
				}}
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "non-symbolic-link file",
		},
		{
			name: "completion precedes start",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				fixture.record["completedAt"] = "2026-07-28T00:00:00Z"
				fixture.record["startedAt"] = "2026-07-28T00:01:00Z"
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "precedes startedAt",
		},
		{
			name: "missing compose deployment",
			mutate: func(t *testing.T, fixture *qualificationFixture) {
				hosts := fixture.record["hosts"].([]map[string]any)
				hosts[1]["deploymentMode"] = "systemd"
				writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
			},
			wantFailure: "deployed with compose",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newQualificationFixture(t, "kvm")
			test.mutate(t, fixture)
			runQualificationVerifier(t, verifier, fixture, false, test.wantFailure)
		})
	}

	t.Run("multi-runner requires two hosts", func(t *testing.T) {
		fixture := newQualificationFixture(t, "multi-runner")
		hosts := fixture.record["hosts"].([]map[string]any)
		fixture.record["hosts"] = hosts[:1]
		writeReleaseJSONFixture(t, fixture.recordPath, fixture.record)
		runQualificationVerifier(t, verifier, fixture, false, "requires at least 2")
	})
}

func TestQualificationImporterVerifiesBeforeCopying(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	importer := filepath.Join(repositoryRoot, "scripts", "import-release-qualification-evidence.mjs")

	t.Run("imports a verified partial record set", func(t *testing.T) {
		fixture := newQualificationFixture(t, "network")
		destination := t.TempDir()
		manifestContents, err := os.ReadFile(fixture.manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, "release-subjects.json"), manifestContents, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fixture.manifestPath); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command("node", importer, fixture.directory, destination).CombinedOutput()
		if err != nil {
			t.Fatalf("import structured qualification evidence: %v\n%s", err, output)
		}
		if _, err := os.Stat(filepath.Join(destination, "qualification", "network.json")); err != nil {
			t.Fatalf("imported network record: %v", err)
		}
	})

	t.Run("rejects unreferenced payload", func(t *testing.T) {
		fixture := newQualificationFixture(t, "network")
		destination := t.TempDir()
		manifestContents, err := os.ReadFile(fixture.manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, "release-subjects.json"), manifestContents, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fixture.manifestPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.directory, "qualification", "unreferenced.log"), []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command("node", importer, fixture.directory, destination).CombinedOutput()
		if err == nil {
			t.Fatal("qualification importer accepted an unreferenced payload")
		}
		if !strings.Contains(string(output), "unreferenced file") {
			t.Fatalf("unexpected importer failure:\n%s", output)
		}
	})
}

func TestQualificationAssemblerNeverAcceptsLegacyMarkers(t *testing.T) {
	assembler := readReleaseRepositoryFile(t, "scripts/assemble-release-evidence.sh")
	for _, forbidden := range []string{
		"qualification/kvm.log",
		"qualification/multi-runner.log",
		"qualification/durability.log",
		"qualification/security.log",
	} {
		if strings.Contains(assembler, forbidden) {
			t.Errorf("structured qualification assembler still accepts legacy marker %q", forbidden)
		}
	}
	for _, required := range []string{
		"verify-release-qualification-record.mjs",
		"dataPlaneQualification",
		"networkQualification",
	} {
		if !strings.Contains(assembler, required) {
			t.Errorf("structured qualification assembler must contain %q", required)
		}
	}
}

func TestQualificationAssemblerLeavesMissingRecordsBlocked(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	fixture := newQualificationFixture(t, "kvm")
	if err := os.RemoveAll(filepath.Join(fixture.directory, "qualification")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(fixture.directory, "qualification"), 0o700); err != nil {
		t.Fatal(err)
	}
	assembler := filepath.Join(repositoryRoot, "scripts", "assemble-release-evidence.sh")
	command := exec.Command(
		assembler,
		fixture.directory,
		"1.0.0-test",
		qualificationFixtureCommit,
	)
	command.Env = append(
		os.Environ(),
		"SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP=2026-07-28T00:02:00Z",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("assemble blocked qualification evidence: %v\n%s", err, output)
	}
	var evidence struct {
		Evidence map[string]struct {
			Status string `json:"status"`
			Record string `json:"record"`
		} `json:"evidence"`
	}
	decodeReleaseJSONFile(
		t,
		filepath.Join(fixture.directory, "release-evidence.json"),
		&evidence,
	)
	for _, gate := range []string{
		"kvmQualification",
		"multiRunnerQualification",
		"durabilityQualification",
		"dataPlaneQualification",
		"networkQualification",
		"securityQualification",
	} {
		record := evidence.Evidence[gate]
		if record.Status != "blocked" || record.Record != "" {
			t.Errorf("missing %s evidence = %#v, want blocked without record", gate, record)
		}
	}
}

func newQualificationFixture(t *testing.T, gate string) *qualificationFixture {
	t.Helper()
	directory := t.TempDir()
	qualificationDirectory := filepath.Join(directory, "qualification")
	if err := os.MkdirAll(qualificationDirectory, 0o700); err != nil {
		t.Fatal(err)
	}

	subjectIDs := releaseQualificationSubjectIDs(t)
	const subjectDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifestSubjects := make([]map[string]any, 0, len(subjectIDs))
	recordSubjects := make([]map[string]any, 0, len(subjectIDs))
	for _, subjectID := range subjectIDs {
		subject := map[string]any{
			"id":      subjectID,
			"kind":    releaseSubjectKind(subjectID),
			"status":  "passed",
			"summary": "fixture subject resolved",
			"locator": "fixture/" + subjectID,
			"digest":  map[string]string{"sha256": subjectDigest},
		}
		if subjectID == "guest-artifact-image" {
			subject["bindings"] = []map[string]any{{
				"subjectId": "guest-execution-bundle",
				"digest":    map[string]string{"sha256": subjectDigest},
			}}
		}
		manifestSubjects = append(manifestSubjects, subject)
		recordSubjects = append(recordSubjects, map[string]any{
			"id":      subjectID,
			"kind":    releaseSubjectKind(subjectID),
			"locator": "fixture/" + subjectID,
			"sha256":  subjectDigest,
		})
	}
	manifest := map[string]any{
		"schemaVersion":  1,
		"releaseVersion": "1.0.0-test",
		"sourceCommit":   qualificationFixtureCommit,
		"status":         "passed",
		"summary":        "all fixture release subjects resolved",
		"subjects":       manifestSubjects,
	}
	manifestPath := filepath.Join(directory, "release-subjects.json")
	writeReleaseJSONFixture(t, manifestPath, manifest)
	manifestContents, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestContents)

	var requirements struct {
		Gates map[string]struct {
			ScenarioIDs []string `json:"scenarioIds"`
		} `json:"gates"`
	}
	decodeReleaseRepositoryJSON(t, "release/qualification-requirements.json", &requirements)
	gateRequirement, found := requirements.Gates[gate]
	if !found {
		t.Fatalf("unknown qualification fixture gate %q", gate)
	}
	scenarios := make([]map[string]any, 0, len(gateRequirement.ScenarioIDs))
	for _, scenarioID := range gateRequirement.ScenarioIDs {
		relativePath := "qualification/scenarios/" + gate + "/" + scenarioID + ".log"
		artifactPath := filepath.Join(directory, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(artifactPath), 0o700); err != nil {
			t.Fatal(err)
		}
		contents := []byte("verified " + scenarioID + "\n")
		if err := os.WriteFile(artifactPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		artifactDigest := sha256.Sum256(contents)
		scenarios = append(scenarios, map[string]any{
			"id":      scenarioID,
			"status":  "passed",
			"summary": "fixture scenario passed",
			"artifacts": []map[string]string{{
				"path":   relativePath,
				"sha256": hex.EncodeToString(artifactDigest[:]),
			}},
		})
	}
	record := map[string]any{
		"schemaVersion":         1,
		"gate":                  gate,
		"releaseVersion":        "1.0.0-test",
		"sourceCommit":          qualificationFixtureCommit,
		"subjectManifestSHA256": hex.EncodeToString(manifestDigest[:]),
		"startedAt":             "2026-07-28T00:00:00Z",
		"completedAt":           "2026-07-28T00:01:00Z",
		"status":                "passed",
		"summary":               "fixture qualification passed",
		"hosts": []map[string]any{
			{
				"id": "runner-systemd", "role": "runner", "deploymentMode": "systemd",
				"operatingSystem": "linux", "architecture": "amd64", "dedicated": true, "kvm": true,
			},
			{
				"id": "runner-compose", "role": "runner", "deploymentMode": "compose",
				"operatingSystem": "linux", "architecture": "amd64", "dedicated": true, "kvm": true,
			},
		},
		"subjects":  recordSubjects,
		"scenarios": scenarios,
	}
	recordPath := filepath.Join(qualificationDirectory, gate+".json")
	writeReleaseJSONFixture(t, recordPath, record)
	return &qualificationFixture{
		directory: directory, manifest: manifest, manifestPath: manifestPath,
		record: record, recordPath: recordPath, gate: gate,
	}
}

func releaseQualificationSubjectIDs(t *testing.T) []string {
	t.Helper()
	var schema struct {
		Definitions struct {
			Subject struct {
				Properties struct {
					ID struct {
						Enum []string `json:"enum"`
					} `json:"id"`
				} `json:"properties"`
			} `json:"subject"`
		} `json:"$defs"`
	}
	decodeReleaseRepositoryJSON(t, "release/supply-chain-subjects-schema.json", &schema)
	if len(schema.Definitions.Subject.Properties.ID.Enum) == 0 {
		t.Fatal("release subject schema has no subject IDs")
	}
	return schema.Definitions.Subject.Properties.ID.Enum
}

func runQualificationVerifier(
	t *testing.T,
	verifier string,
	fixture *qualificationFixture,
	wantPass bool,
	wantOutput string,
) {
	t.Helper()
	output, err := exec.Command(
		"node",
		verifier,
		fixture.manifestPath,
		fixture.recordPath,
		fixture.gate,
		fixture.directory,
	).CombinedOutput()
	if wantPass && err != nil {
		t.Fatalf("qualification verifier rejected complete evidence: %v\n%s", err, output)
	}
	if !wantPass && err == nil {
		t.Fatalf("qualification verifier accepted invalid evidence:\n%s", output)
	}
	if wantOutput != "" && !strings.Contains(string(output), wantOutput) {
		t.Fatalf("qualification verifier output does not contain %q:\n%s", wantOutput, output)
	}
}
