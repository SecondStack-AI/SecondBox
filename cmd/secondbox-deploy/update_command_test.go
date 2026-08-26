package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/releaseverify"
)

func TestActivationBoundaryPersistenceFailureRestartsSource(t *testing.T) {
	persistErr := errors.New("sync receipt")
	restored := false
	sourceSubject := "sha256:" + strings.Repeat("a", 64)
	err := journalUpdateActivationBoundary(func(stage install.UpdateStage, evidence map[string]string) error {
		if stage != install.UpdateStageActivationStarted || evidence["controlPlaneAdmission"] != "stopped" || evidence["sourceComposeSubject"] != sourceSubject {
			t.Fatalf("activation boundary = %s %#v", stage, evidence)
		}
		return persistErr
	}, sourceSubject, func() (bool, error) {
		return false, nil
	}, func(problem error) error {
		restored = true
		return problem
	})
	if !errors.Is(err, persistErr) || !restored {
		t.Fatalf("boundary failure = %v, restored=%t", err, restored)
	}
}

func TestActivationBoundaryPersistenceFailureKeepsCommittedFence(t *testing.T) {
	persistErr := errors.New("sync receipt directory")
	restored := false
	err := journalUpdateActivationBoundary(func(install.UpdateStage, map[string]string) error {
		return persistErr
	}, "sha256:"+strings.Repeat("a", 64), func() (bool, error) {
		return true, nil
	}, func(problem error) error {
		restored = true
		return problem
	})
	if !errors.Is(err, persistErr) || restored {
		t.Fatalf("committed boundary failure = %v, restored=%t", err, restored)
	}
}

func TestActivationBoundaryPersistenceFailureKeepsFenceWhenCommitIsAmbiguous(t *testing.T) {
	persistErr := errors.New("sync receipt directory")
	inspectErr := errors.New("read durable receipt")
	restored := false
	err := journalUpdateActivationBoundary(func(install.UpdateStage, map[string]string) error {
		return persistErr
	}, "sha256:"+strings.Repeat("a", 64), func() (bool, error) {
		return false, inspectErr
	}, func(problem error) error {
		restored = true
		return problem
	})
	if !errors.Is(err, persistErr) || !errors.Is(err, inspectErr) || restored || !strings.Contains(err.Error(), "remains stopped") {
		t.Fatalf("ambiguous boundary failure = %v, restored=%t", err, restored)
	}
}

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
  {"id":"sbx_two","tenantRef":"tenant-b","subjectRef":"subject-b","state":"ready","desiredState":"running","workspaceMutation":""},
  {"id":"sbx_missing","tenantRef":"tenant-c","subjectRef":"subject-c","state":"stopped","desiredState":"stopped","workspaceMutation":"missing"},
  {"id":"instance_orphan","tenantRef":"orphan-instance","subjectRef":"sbx_gone","state":"instance ready","desiredState":"missing sandbox","workspaceMutation":""}
]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tenant-a/subject-a/sbx_one (stopped -> running; workspace start)",
		"tenant-b/subject-b/sbx_two (ready -> running)",
		"tenant-c/subject-c/sbx_missing (stopped -> stopped; workspace missing)",
		"orphan-instance/sbx_gone/instance_orphan (instance ready -> missing sandbox)",
	}
	if !slices.Equal(blocking, want) {
		t.Fatalf("blocking Sandboxes = %#v, want %#v", blocking, want)
	}
	if _, err := decodeBlockingUpdateSandboxes([]byte(`{"id":"not-an-array"}`)); err == nil {
		t.Fatal("malformed deployment-wide inventory was accepted")
	}
}

func TestDeploymentQuiescenceQueryFailsClosedOnMissingReferences(t *testing.T) {
	for _, required := range []string{
		"LEFT JOIN secondbox.workspaces",
		"workspace.id IS NOT NULL",
		"COALESCE(workspace.mutation_state, 'missing')",
		"UNION ALL",
		"LEFT JOIN secondbox.sandboxes",
		"AND sandbox.id IS NULL",
	} {
		if !strings.Contains(installedDeploymentQuiescenceQuery, required) {
			t.Fatalf("deployment quiescence query lacks fail-closed clause %q", required)
		}
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

func TestGuidedUpdateRejectsSourceBeforeCleanInstallBoundaryWithoutConsultingReleaseInputs(t *testing.T) {
	plan := install.InstallPlan{Release: installReleasePlanFixture()}
	plan.Release.Version = "0.5.2"
	receipt := install.InstallReceipt{Status: install.OperationSucceeded, CompletedStages: make([]install.StageRecord, len(install.StageSequence))}
	target := installReleasePlanFixture()
	target.Version = "0.6.0"
	verified := false
	err := validateNewUpdate(context.Background(), plan, receipt, target, releaseverify.VerifiedRelease{}, updateDependencies{
		VerifySource: func(context.Context, string) (releaseverify.VerifiedRelease, error) {
			verified = true
			return releaseverify.VerifiedRelease{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "v0.6.0 clean-install boundary") ||
		!strings.Contains(err.Error(), "clean reinstall") || verified {
		t.Fatalf("historical source rejection = %v, verified=%t", err, verified)
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
		VerifySource: func(context.Context, string) (releaseverify.VerifiedRelease, error) {
			return releaseverify.VerifiedRelease{ManifestBytes: []byte("replaced source release")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "journaled public identity") {
		t.Fatalf("source release drift = %v, want journal identity rejection", err)
	}
}

func TestRecordedResumeTargetIgnoresNewerBootstrapVersion(t *testing.T) {
	target := installReleasePlanFixture()
	target.Version = "0.5.2"
	receipt := install.InstallReceipt{Updates: []install.UpdateRecord{{TargetRelease: target, Status: install.UpdateFailed}}}
	requested := ""
	verified := releaseverify.VerifiedRelease{ManifestBytes: []byte("target manifest")}
	resolved := releasePlan(verified, target.ArtifactManifestURL)
	target = resolved
	receipt.Updates[0].TargetRelease = target
	gotVerified, gotPlan, err := verifyRecordedResumeTarget(context.Background(), receipt, updateDependencies{
		TargetVersion: "0.6.0",
		VerifyRelease: func(_ context.Context, location string) (releaseverify.VerifiedRelease, error) {
			requested = location
			return verified, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requested != target.ArtifactManifestURL || !sameUpdateTarget(gotPlan, target) || string(gotVerified.ManifestBytes) != string(verified.ManifestBytes) {
		t.Fatalf("recorded resume target = %q/%#v, want journaled %#v", requested, gotPlan, target)
	}
}

func TestUpdateDiagnosticBufferAcceptsQuiescenceResult(t *testing.T) {
	buffer := &boundedCommandBuffer{maximum: 4 << 20}
	if _, err := buffer.Write([]byte("[]\n")); err != nil || buffer.tooLong || buffer.String() != "[]\n" {
		t.Fatalf("quiescence buffer = %q tooLong=%t err=%v", buffer.String(), buffer.tooLong, err)
	}
}
