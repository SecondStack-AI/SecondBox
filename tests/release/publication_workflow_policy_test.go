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

func TestQualifiedPublicationWorkflowIsEvidenceTriggeredAndLeastPrivilege(t *testing.T) {
	workflow := readReleaseRepositoryFile(t, ".github/workflows/publish.yml")

	for _, required := range []string{
		"workflow_run:",
		`workflows: ["SecondBox Release Evidence"]`,
		"types: [completed]",
		"permissions: {}",
		"if: github.event.workflow_run.conclusion == 'success'",
		"ref: ${{ github.event.workflow_run.head_sha }}",
		"run-id: ${{ github.event.workflow_run.id }}",
		"name: release-evidence-${{ github.event.workflow_run.head_sha }}",
		"git fetch --no-tags origin refs/heads/main",
		"environment: release",
		"publish-qualified-ghcr-tags:",
		"packages: write",
		"publish-qualified-npm-package:",
		"id-token: write",
		"publish-github-release-last:",
		"contents: write",
		"needs: publish-qualified-npm-package",
		"verify-public-release:",
		"needs: publish-github-release-last",
		"persist-credentials: false",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("qualified publication workflow must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"workflow_dispatch:",
		"docker build ",
		"docker buildx build",
		"docker push",
		"go build",
		"scripts/build-artifacts.sh",
		"scripts/package-release-",
		"--clobber",
		":latest",
		"NODE_AUTH_TOKEN:",
		"NPM_TOKEN:",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("qualified publication workflow must not contain %q", forbidden)
		}
	}

	imagesJob := publicationWorkflowJob(t, workflow, "publish-qualified-ghcr-tags", "publish-qualified-npm-package")
	if strings.Contains(imagesJob, "contents: write") ||
		strings.Contains(imagesJob, "id-token: write") {
		t.Error("GHCR publication job has authority outside package publication")
	}
	npmJob := publicationWorkflowJob(t, workflow, "publish-qualified-npm-package", "publish-github-release-last")
	if strings.Contains(npmJob, "contents: write") ||
		strings.Contains(npmJob, "packages: write") {
		t.Error("npm publication job has GitHub content or package write authority")
	}
	githubJob := publicationWorkflowJob(t, workflow, "publish-github-release-last", "verify-public-release")
	if strings.Contains(githubJob, "packages: write") ||
		strings.Contains(githubJob, "id-token: write") {
		t.Error("GitHub release job has registry or OIDC write authority")
	}
}

func TestQualifiedPublicationHelperIsExactMatchOrFailAndNeverBuilds(t *testing.T) {
	helper := readReleaseRepositoryFile(t, "scripts/publish-qualified-release.sh")

	for _, required := range []string{
		"publish_exact_ghcr_tag",
		"already exists at a different digest",
		"could not prove $versioned_reference is absent",
		"verify_published_npm_package",
		"bytes differ from the qualified package",
		"forbids persistent npm tokens",
		"ensure_exact_github_tag",
		"already exists at another Git object",
		"ensure_exact_github_asset",
		"exists with different bytes",
		"published GitHub release is missing immutable asset",
		"publish_qualified_github_release",
		"verify_qualified_public_release",
		"verify_publication_source_is_current_main",
		"qualified source commit is no longer current protected main",
		`.evidence | to_entries[].value.artifacts[]?`,
		"release evidence artifact is unsafe or changed",
		"evidence-${evidence_artifact_digest:0:16}-",
		"control-plane-image",
		"runner-image",
		"guest-artifact-image",
	} {
		if !strings.Contains(helper, required) {
			t.Errorf("qualified publication helper must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"docker build ",
		"docker buildx build",
		"docker push",
		"go build",
		"npm pack ./",
		"--clobber",
		":latest",
		"git tag -f",
		"git push -f",
	} {
		if strings.Contains(helper, forbidden) {
			t.Errorf("qualified publication helper must not contain %q", forbidden)
		}
	}
}

func TestQualifiedPublicationInputVerifierBindsWorkflowAndEverySubject(t *testing.T) {
	repositoryRoot := releaseRepositoryRoot(t)
	fixtureDirectory := t.TempDir()
	const sourceCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const releaseVersion = "1.0.0"

	localSubjects := []struct {
		ID      string
		Kind    string
		Locator string
	}{
		{"linux-release-package", "release-package", "package/secondbox-1.0.0-linux-amd64.tar.gz"},
		{"secondbox", "linux-binary", "dist/secondbox"},
		{"secondbox-artifact-evidence", "linux-binary", "dist/secondbox-artifact-evidence"},
		{"secondbox-guest-agent", "linux-binary", "dist/secondbox-guest-agent"},
		{"secondbox-runner", "linux-binary", "dist/secondbox-runner"},
		{"secondbox-runner-identity", "linux-binary", "dist/secondbox-runner-identity"},
		{"secondboxd", "linux-binary", "dist/secondboxd"},
		{"guest-execution-bundle", "guest-bundle", "guest/secondbox-1.0.0-guest-amd64.tar.gz"},
		{"go-sdk-package", "go-sdk", "sdk/secondbox-1.0.0-go-sdk.tar.gz"},
		{"typescript-sdk-package", "npm-package", "sdk/secondstack-ai-secondbox-1.0.0.tgz"},
	}
	subjects := make([]map[string]any, 0, 13)
	digests := map[string]string{}
	for _, localSubject := range localSubjects {
		subjectPath := filepath.Join(fixtureDirectory, filepath.FromSlash(localSubject.Locator))
		if err := os.MkdirAll(filepath.Dir(subjectPath), 0o700); err != nil {
			t.Fatal(err)
		}
		contents := []byte("qualified-" + localSubject.ID)
		if err := os.WriteFile(subjectPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(contents)
		digests[localSubject.ID] = hex.EncodeToString(digest[:])
		subjects = append(subjects, map[string]any{
			"id":        localSubject.ID,
			"kind":      localSubject.Kind,
			"status":    "passed",
			"locator":   localSubject.Locator,
			"digest":    map[string]any{"sha256": digests[localSubject.ID]},
			"sizeBytes": len(contents),
		})
	}
	for _, imageSubject := range []struct {
		ID         string
		Repository string
	}{
		{"control-plane-image", "ghcr.io/secondstack-ai/secondbox-control-plane"},
		{"runner-image", "ghcr.io/secondstack-ai/secondbox-runner"},
		{"guest-artifact-image", "ghcr.io/secondstack-ai/secondbox-guest-artifacts"},
	} {
		digestBytes := sha256.Sum256([]byte(imageSubject.ID))
		digest := hex.EncodeToString(digestBytes[:])
		subject := map[string]any{
			"id":      imageSubject.ID,
			"kind":    "oci-image",
			"status":  "passed",
			"locator": imageSubject.Repository + "@sha256:" + digest,
			"digest":  map[string]any{"sha256": digest},
		}
		if imageSubject.ID == "guest-artifact-image" {
			subject["bindings"] = []any{map[string]any{
				"subjectId": "guest-execution-bundle",
				"digest": map[string]any{
					"sha256": digests["guest-execution-bundle"],
				},
			}}
		}
		subjects = append(subjects, subject)
	}

	writePublicationJSONFixture(t, filepath.Join(fixtureDirectory, "release-subjects.json"), map[string]any{
		"schemaVersion":  1,
		"releaseVersion": releaseVersion,
		"sourceCommit":   sourceCommit,
		"status":         "passed",
		"subjects":       subjects,
	})
	evidencePath := filepath.Join(fixtureDirectory, "release-evidence.json")
	writePublicationJSONFixture(t, evidencePath, map[string]any{
		"releaseVersion": releaseVersion,
		"sourceCommit":   sourceCommit,
		"subjects":       "release-subjects.json",
	})
	eventPath := filepath.Join(fixtureDirectory, "event.json")
	event := qualifiedPublicationEventFixture(sourceCommit)
	writePublicationJSONFixture(t, eventPath, event)

	verifier := filepath.Join(repositoryRoot, "scripts", "verify-qualified-publication-inputs.mjs")
	command := exec.Command("node", verifier, eventPath, evidencePath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify qualified publication inputs: %v\n%s", err, output)
	}

	event["workflow_run"].(map[string]any)["head_branch"] = "feature/unqualified"
	writePublicationJSONFixture(t, eventPath, event)
	command = exec.Command("node", verifier, eventPath, evidencePath)
	if output, err := command.CombinedOutput(); err == nil ||
		!strings.Contains(string(output), "protected main") {
		t.Fatalf("unprotected branch did not fail closed: %v\n%s", err, output)
	}
}

func publicationWorkflowJob(t *testing.T, workflow, startName, endName string) string {
	t.Helper()
	start := strings.Index(workflow, "  "+startName+":")
	end := strings.Index(workflow, "  "+endName+":")
	if start < 0 || end <= start {
		t.Fatalf("could not isolate publication workflow job %s", startName)
	}
	return workflow[start:end]
}

func qualifiedPublicationEventFixture(sourceCommit string) map[string]any {
	return map[string]any{
		"action": "completed",
		"repository": map[string]any{
			"full_name":  "SecondStack-AI/SecondBox",
			"private":    false,
			"visibility": "public",
		},
		"workflow_run": map[string]any{
			"id":          123,
			"status":      "completed",
			"conclusion":  "success",
			"path":        ".github/workflows/release-evidence.yml",
			"event":       "workflow_dispatch",
			"head_branch": "main",
			"head_sha":    sourceCommit,
			"head_repository": map[string]any{
				"full_name": "SecondStack-AI/SecondBox",
			},
		},
	}
}

func writePublicationJSONFixture(t *testing.T, fixturePath string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
