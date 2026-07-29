package operations_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultiRunnerQualificationControllerRequiresExplicitRemoteCandidateInputs(t *testing.T) {
	controller := readRepositoryFile(t, "scripts/test-multirunner.sh")

	for _, required := range []string{
		"SECONDBOX_MULTIRUNNER_QUALIFIED_DEPLOYMENT",
		"SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY",
		"SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST",
		"SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE",
		"SECONDBOX_RELEASE_QUALIFICATION_CONTROLLER_DIRECTORY",
		"SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY",
		"SECONDBOX_MULTIRUNNER_API_URL",
		"SECONDBOX_MULTIRUNNER_API_TOKEN",
		"SECONDBOX_MULTIRUNNER_API_CA_FILE",
		"SECONDBOX_MULTIRUNNER_DATABASE_URL",
		"SECONDBOX_MULTIRUNNER_PROFILE",
		"SECONDBOX_MULTIRUNNER_POOL",
		"SECONDBOX_MULTIRUNNER_RUNNER_A_ID",
		"SECONDBOX_MULTIRUNNER_RUNNER_B_ID",
		"SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID",
		"SECONDBOX_MULTIRUNNER_HOST_A_ID",
		"SECONDBOX_MULTIRUNNER_HOST_B_ID",
		"SECONDBOX_MULTIRUNNER_HOST_A_SSH",
		"SECONDBOX_MULTIRUNNER_HOST_B_SSH",
		"SECONDBOX_MULTIRUNNER_HOST_A_SERVICE",
		"SECONDBOX_MULTIRUNNER_HOST_B_SERVICE",
		"SECONDBOX_MULTIRUNNER_HOST_A_RUNNER_BINARY",
		"SECONDBOX_MULTIRUNNER_HOST_B_RUNNER_BINARY",
		"SECONDBOX_MULTIRUNNER_HOST_A_ENVIRONMENT_FILE",
		"SECONDBOX_MULTIRUNNER_HOST_B_ENVIRONMENT_FILE",
		"SECONDBOX_MULTIRUNNER_HOST_SENTINEL_PATH",
		"SECONDBOX_MULTIRUNNER_SSH_IDENTITY_FILE",
		"SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS_FILE",
		"SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS",
		"SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("multi-Runner qualification controller must require %s", required)
		}
	}
	for _, forbidden := range []string{
		"StrictHostKeyChecking=no",
		"UserKnownHostsFile=/dev/null",
		"|| true",
		"SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS:-",
		"SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS:-",
		"localhost",
	} {
		if strings.Contains(controller, forbidden) {
			t.Errorf("multi-Runner qualification controller must not contain %q", forbidden)
		}
	}
}

func TestMultiRunnerQualificationControllerNamesEveryRequiredScenarioAndVerifier(t *testing.T) {
	controller := readRepositoryFile(t, "scripts/test-multirunner.sh")

	for _, scenarioID := range []string{
		"independent-qualified-runners",
		"distinct-revocable-runner-identities",
		"placement",
		"drain",
		"stopped-sandbox-relocation",
		"runner-crash",
		"stale-runner-rejection",
		"cross-runner-generation-fencing",
	} {
		if !strings.Contains(controller, scenarioID) {
			t.Errorf("multi-Runner qualification controller must exercise %s", scenarioID)
		}
	}
	for _, required := range []string{
		"verify-release-qualification-record.mjs",
		"generate-multirunner-qualification-record.mjs",
		"secondbox-multirunner-admin",
		"secondbox-stale-runner-probe",
		"systemctl kill --kill-who=main --signal=KILL",
		"RequestRunnerDrain",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("multi-Runner qualification controller must contain %q", required)
		}
	}
}

func TestMultiRunnerQualificationControllerFailsBeforeRemoteMutationWithoutAcknowledgement(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	controller := filepath.Join(repositoryRoot, "scripts", "test-multirunner.sh")
	marker := filepath.Join(t.TempDir(), "ssh-called")
	fakeBin := t.TempDir()
	fakeSSH := filepath.Join(fakeBin, "ssh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\n: >\"$SSH_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(controller)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"SSH_MARKER="+marker,
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("multi-Runner qualification controller accepted missing acknowledgement")
	}
	if !strings.Contains(string(output), "SECONDBOX_MULTIRUNNER_QUALIFIED_DEPLOYMENT") {
		t.Fatalf("missing acknowledgement error was not explicit:\n%s", output)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("multi-Runner qualification controller contacted a host before validating acknowledgement")
	}
}

func TestReleaseQualificationWorkflowSuppliesProtectedMultiRunnerAuthority(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/release-qualification.yml")

	for _, required := range []string{
		"secrets.SECONDBOX_MULTIRUNNER_API_TOKEN",
		"secrets.SECONDBOX_MULTIRUNNER_DATABASE_URL",
		"secrets.SECONDBOX_MULTIRUNNER_SSH_PRIVATE_KEY",
		"vars.SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS",
		"vars.SECONDBOX_MULTIRUNNER_API_CA_PEM",
		"vars.SECONDBOX_MULTIRUNNER_RUNNER_A_ID",
		"vars.SECONDBOX_MULTIRUNNER_RUNNER_B_ID",
		"vars.SECONDBOX_MULTIRUNNER_HOST_A_ID",
		"vars.SECONDBOX_MULTIRUNNER_HOST_B_ID",
		"vars.SECONDBOX_MULTIRUNNER_HOST_A_SSH",
		"vars.SECONDBOX_MULTIRUNNER_HOST_B_SSH",
		"vars.SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS",
		"vars.SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS",
		"SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID: ${{ runner.name }}",
		"SECONDBOX_MULTIRUNNER_SSH_IDENTITY_FILE",
		"SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS_FILE",
		"SECONDBOX_MULTIRUNNER_API_CA_FILE",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release qualification workflow must supply %q", required)
		}
	}
}
