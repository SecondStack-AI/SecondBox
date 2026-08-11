package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

func installReleasePlanFixture() install.ReleasePlan {
	images := map[string]string{}
	for _, name := range []string{"control-plane", "runner", "microvm-artifacts", "installer-tools", "postgres"} {
		images[name] = "example.invalid/" + name + "@sha256:" + strings.Repeat("b", 64)
	}
	return install.ReleasePlan{Version: "0.5.1", ArtifactManifestURL: "https://example.invalid/manifest.json", ArtifactManifestDigest: "sha256:" + strings.Repeat("a", 64), SigningKeyFingerprint: "SHA256:" + strings.Repeat("A", 64), Images: images, BinaryDigests: map[string]string{"secondbox": strings.Repeat("c", 64), "secondbox-deploy": strings.Repeat("d", 64)}, ExpectedDownloadBytes: 1}
}

func TestDecodeBlockingUpdateSandboxesPreservesProjectAndLifecycleState(t *testing.T) {
	blocking, err := decodeBlockingUpdateSandboxes([]byte(`[
  {"id":"sbx_one","tenantRef":"tenant-a","subjectRef":"subject-a","state":"stopped","desiredState":"running","workspaceMutation":"start"},
  {"id":"sbx_two","tenantRef":"tenant-b","subjectRef":"subject-b","state":"ready","desiredState":"running","workspaceMutation":""}
]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tenant-a/subject-a/sbx_one (stopped -> running; workspace start)",
		"tenant-b/subject-b/sbx_two (ready -> running)",
	}
	if !slices.Equal(blocking, want) {
		t.Fatalf("blocking Sandboxes = %#v, want %#v", blocking, want)
	}
	if _, err := decodeBlockingUpdateSandboxes([]byte(`{"id":"not-an-array"}`)); err == nil {
		t.Fatal("malformed deployment-wide inventory was accepted")
	}
}

func TestParseUpdateArgumentsKeepsCheckAndResumeExplicit(t *testing.T) {
	tests := []struct {
		arguments     []string
		check, resume bool
		directory     string
		valid         bool
	}{
		{[]string{"/srv/secondbox"}, false, false, "/srv/secondbox", true},
		{[]string{"--check", "/srv/secondbox"}, true, false, "/srv/secondbox", true},
		{[]string{"--resume", "/srv/secondbox"}, false, true, "/srv/secondbox", true},
		{nil, false, false, "", false},
		{[]string{"--check"}, false, false, "", false},
		{[]string{"--resume", "one", "two"}, false, false, "", false},
	}
	for _, test := range tests {
		check, resume, directory, err := parseUpdateArguments(test.arguments)
		if (err == nil) != test.valid || check != test.check || resume != test.resume || directory != test.directory {
			t.Fatalf("parseUpdateArguments(%q) = %v/%v/%q, %v", test.arguments, check, resume, directory, err)
		}
	}
}

func TestUpdateTargetIdentityIncludesEveryReleaseInput(t *testing.T) {
	left := installReleasePlanFixture()
	right := installReleasePlanFixture()
	if !sameUpdateTarget(left, right) {
		t.Fatal("identical targets differed")
	}
	right.Images = map[string]string{}
	for name, reference := range left.Images {
		right.Images[name] = reference
	}
	right.Images["runner"] = strings.Replace(right.Images["runner"], strings.Repeat("b", 64), strings.Repeat("c", 64), 1)
	if sameUpdateTarget(left, right) {
		t.Fatal("changed target image was accepted")
	}
}

func TestCompletedUpdateMatchesOnlyExactSucceededTarget(t *testing.T) {
	target := installReleasePlanFixture()
	receipt := install.InstallReceipt{Updates: []install.UpdateRecord{{TargetRelease: target, Status: install.UpdateSucceeded}}}
	if !completedUpdateMatchesTarget(receipt, target) {
		t.Fatal("exact completed update was not accepted as an idempotent resume")
	}
	receipt.Updates[0].Status = install.UpdateFailed
	if completedUpdateMatchesTarget(receipt, target) {
		t.Fatal("failed update was accepted as completed")
	}
	receipt.Updates[0].Status = install.UpdateSucceeded
	target.ArtifactManifestDigest = "sha256:" + strings.Repeat("e", 64)
	if completedUpdateMatchesTarget(receipt, target) {
		t.Fatal("different completed target was accepted")
	}
}

func TestContinueUpdateRejectsSourceIdentityDriftBeforeActivation(t *testing.T) {
	update := install.UpdateRecord{SourceRelease: installReleasePlanFixture()}
	err := continueUpdate(context.Background(), t.TempDir(), install.InstallPlan{}, install.InstallReceipt{}, update, releaseverify.VerifiedRelease{}, cliui.Renderer{}, updateDependencies{
		VerifyRelease: func(context.Context, string) (releaseverify.VerifiedRelease, error) {
			return releaseverify.VerifiedRelease{ManifestBytes: []byte("replaced source release")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "journaled public identity") {
		t.Fatalf("source release drift = %v, want journal identity rejection", err)
	}
}

func TestUpdateSmokeMutationRecoversLostResponsesAndFencesLaterTransitions(t *testing.T) {
	sandbox := contracts.Sandbox{ID: "sbx_0123456789abcdefghijklmn", State: "stopped", DesiredState: contracts.SandboxDesiredStateStopped, Revision: 41}
	mutate, firstKey, err := installedUpdateSandboxMutation(sandbox, "start")
	if err != nil || !mutate || !strings.Contains(firstKey, "revision-41") {
		t.Fatalf("initial start mutation = %t, %q, %v", mutate, firstKey, err)
	}

	// The intent committed but its response was lost. Desired state is the
	// durable acknowledgement, even while observed state still says stopped.
	sandbox.DesiredState = contracts.SandboxDesiredStateRunning
	sandbox.Revision = 42
	if mutate, key, err := installedUpdateSandboxMutation(sandbox, "start"); err != nil || mutate || key != "" {
		t.Fatalf("ambiguous committed start = %t, %q, %v", mutate, key, err)
	}

	// A completed stop advances the revision, so the next smoke retry receives a
	// distinct idempotency key instead of replaying the preceding start.
	sandbox.DesiredState = contracts.SandboxDesiredStateStopped
	sandbox.Revision = 43
	mutate, restartKey, err := installedUpdateSandboxMutation(sandbox, "start")
	if err != nil || !mutate || restartKey == firstKey || !strings.Contains(restartKey, "revision-43") {
		t.Fatalf("post-stop restart = %t, %q, %v", mutate, restartKey, err)
	}
	if _, _, err := installedUpdateSandboxMutation(sandbox, "delete"); err == nil {
		t.Fatal("invalid update smoke action was accepted")
	}
}
