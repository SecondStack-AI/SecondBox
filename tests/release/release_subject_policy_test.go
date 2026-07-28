package release_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseSubjectSchemaRequiresGuestArtifactImage(t *testing.T) {
	var schema struct {
		Properties struct {
			Subjects struct {
				MinItems int `json:"minItems"`
				MaxItems int `json:"maxItems"`
			} `json:"subjects"`
		} `json:"properties"`
	}
	decodeReleaseRepositoryJSON(t, "release/supply-chain-subjects-schema.json", &schema)
	if schema.Properties.Subjects.MinItems != 13 || schema.Properties.Subjects.MaxItems != 13 {
		t.Fatalf(
			"release subject cardinality = %d..%d, want exactly 13",
			schema.Properties.Subjects.MinItems,
			schema.Properties.Subjects.MaxItems,
		)
	}

	schemaSource := readReleaseRepositoryFile(t, "release/supply-chain-subjects-schema.json")
	generatorSource := readReleaseRepositoryFile(t, "scripts/generate-release-subject-manifest.mjs")
	for _, required := range []string{
		`"guest-artifact-image"`,
		`"bindings"`,
		`"guest-execution-bundle"`,
	} {
		if !strings.Contains(schemaSource, required) {
			t.Errorf("release subject schema must contain %s", required)
		}
	}
	for _, required := range []string{
		`"guest-artifact-image"`,
		`"SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE"`,
		"resolveBoundOCIImageSubject",
		"boundGuestBundle.digest",
	} {
		if !strings.Contains(generatorSource, required) {
			t.Errorf("release subject generator must contain %q", required)
		}
	}
}

func TestPublicationGateSignsExactCanonicalThirteenSubjectManifest(t *testing.T) {
	gate := readReleaseRepositoryFile(t, "scripts/verify-release-publication-eligibility.sh")
	for _, required := range []string{
		`-z "${resolved_subject_manifest-}"`,
		`sha256sum "$resolved_signature_manifest"`,
		`sha256sum "$resolved_subject_manifest"`,
		"signed manifest is not the exact canonical release-subject manifest",
	} {
		if !strings.Contains(gate, required) {
			t.Errorf("release publication gate must contain %q", required)
		}
	}
	if strings.Contains(gate, "length == 12") {
		t.Error("release publication gate still hardcodes the obsolete twelve-subject contract")
	}
}

func TestMissingGuestArtifactImageDigestBlocksSubject(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	evidenceDirectory := t.TempDir()
	for _, relativeDirectory := range []string{"dist", "guest", "package", "sdk"} {
		if err := os.Mkdir(filepath.Join(evidenceDirectory, relativeDirectory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceCommitOutput, err := exec.Command("git", "-C", repositoryRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	subjectPath := filepath.Join(evidenceDirectory, "release-subjects.json")
	command := exec.Command(
		"node",
		filepath.Join(repositoryRoot, "scripts", "generate-release-subject-manifest.mjs"),
		evidenceDirectory,
		subjectPath,
	)
	command.Env = releaseSubjectTestEnvironment(map[string]string{
		"SECONDBOX_RELEASE_VERSION":       "1.0.0-test",
		"SECONDBOX_RELEASE_SOURCE_COMMIT": strings.TrimSpace(string(sourceCommitOutput)),
	})
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate blocked release subjects: %v\n%s", err, output)
	}

	var manifest struct {
		Status   string `json:"status"`
		Subjects []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Summary string `json:"summary"`
		} `json:"subjects"`
	}
	decodeReleaseJSONFile(t, subjectPath, &manifest)
	if manifest.Status != "blocked" || len(manifest.Subjects) != 13 {
		t.Fatalf("blocked subject manifest = %#v", manifest)
	}
	for _, subject := range manifest.Subjects {
		if subject.ID != "guest-artifact-image" {
			continue
		}
		if subject.Status != "blocked" ||
			!strings.Contains(subject.Summary, "SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE") {
			t.Fatalf("missing guest artifact image digest did not block specifically: %#v", subject)
		}
		return
	}
	t.Fatal("release subject manifest omitted guest-artifact-image")
}

func TestGuestArtifactImageBindingDigestCannotDrift(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	fixtureDirectory := t.TempDir()
	const candidateDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const driftedDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	requiredSubjects := []struct {
		ID   string
		Kind string
	}{
		{"linux-release-package", "release-package"},
		{"secondbox", "linux-binary"},
		{"secondbox-artifact-evidence", "linux-binary"},
		{"secondbox-guest-agent", "linux-binary"},
		{"secondbox-runner", "linux-binary"},
		{"secondbox-runner-identity", "linux-binary"},
		{"secondboxd", "linux-binary"},
		{"control-plane-image", "oci-image"},
		{"runner-image", "oci-image"},
		{"guest-execution-bundle", "guest-bundle"},
		{"guest-artifact-image", "oci-image"},
		{"go-sdk-package", "go-sdk"},
		{"typescript-sdk-package", "npm-package"},
	}
	subjects := make([]map[string]any, 0, len(requiredSubjects))
	coverage := make([]map[string]any, 0, len(requiredSubjects))
	for _, definition := range requiredSubjects {
		subject := map[string]any{
			"id":      definition.ID,
			"kind":    definition.Kind,
			"status":  "passed",
			"summary": "fixture",
			"locator": "fixture/" + definition.ID,
			"digest":  map[string]string{"sha256": candidateDigest},
		}
		if definition.ID == "guest-artifact-image" {
			subject["locator"] = "ghcr.io/secondstack-ai/secondbox-guest@sha256:" + candidateDigest
			subject["bindings"] = []map[string]any{{
				"subjectId": "guest-execution-bundle",
				"digest":    map[string]string{"sha256": driftedDigest},
			}}
		}
		subjects = append(subjects, subject)
		coverage = append(coverage, map[string]any{
			"subjectId":     definition.ID,
			"subjectSHA256": candidateDigest,
			"status":        "passed",
			"summary":       "fixture",
			"artifacts":     []map[string]string{},
		})
	}
	subjectPath := filepath.Join(fixtureDirectory, "release-subjects.json")
	coveragePath := filepath.Join(fixtureDirectory, "sbom-status.json")
	writeReleaseJSONFixture(t, subjectPath, map[string]any{
		"schemaVersion":  1,
		"releaseVersion": "1.0.0-test",
		"sourceCommit":   "0123456789012345678901234567890123456789",
		"status":         "passed",
		"summary":        "fixture",
		"subjects":       subjects,
	})
	writeReleaseJSONFixture(t, coveragePath, map[string]any{
		"schemaVersion":  1,
		"evidenceType":   "sbom",
		"releaseVersion": "1.0.0-test",
		"sourceCommit":   "0123456789012345678901234567890123456789",
		"status":         "passed",
		"summary":        "fixture",
		"subjects":       coverage,
	})
	verifier := filepath.Join(repositoryRoot, "scripts", "verify-release-supply-chain-coverage.mjs")
	output, err := exec.Command("node", verifier, subjectPath, coveragePath, "sbom").CombinedOutput()
	if err == nil {
		t.Fatal("supply-chain verifier accepted a guest artifact image bound to another guest digest")
	}
	if !strings.Contains(string(output), "does not bind the exact signed guest bundle digest") {
		t.Fatalf("binding-drift failure was not specific:\n%s", output)
	}
}

func TestGuestArtifactImageHasEverySupplyChainEvidenceMapping(t *testing.T) {
	requiredMappings := map[string][]string{
		"scripts/generate-release-sboms.sh": {
			"guest-artifact-image",
		},
		"scripts/generate-vulnerability-evidence.sh": {
			"guest-artifact-image",
		},
		"scripts/generate-dependency-age-evidence.sh": {
			`guest_subjects='["guest-execution-bundle","guest-artifact-image"]'`,
			"guest-artifact-image",
		},
		"scripts/generate-license-evidence.sh": {
			"guest_artifact_image_status",
			"guest-artifact-image",
		},
		"scripts/generate-release-checksum-evidence.sh": {
			"jq -c '.subjects[]'",
		},
		"scripts/generate-release-signature-evidence.sh": {
			"jq -c '.subjects[]'",
		},
		"scripts/generate-release-provenance.sh": {
			"guest-artifact-image",
			"SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE_PROVENANCE",
			"runner/deploy/microvm-artifact-transport.Dockerfile",
			"boundSubjectSHA256",
			"resolvedDependencies",
		},
	}
	for relativePath, required := range requiredMappings {
		source := readReleaseRepositoryFile(t, relativePath)
		for _, fragment := range required {
			if !strings.Contains(source, fragment) {
				t.Errorf("%s must contain %q", relativePath, fragment)
			}
		}
	}
}

func releaseSubjectTestEnvironment(overrides map[string]string) []string {
	removed := map[string]bool{
		"SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE":  true,
		"SECONDBOX_RELEASE_RUNNER_IMAGE":         true,
		"SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE": true,
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
