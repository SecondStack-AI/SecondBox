package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStressQualificationUsesExternalSDKHarnessAndFailsLoudly(t *testing.T) {
	script := readRepositoryFile(t, "scripts/test-stress.sh")
	scenarioScript := readRepositoryFile(t, "scripts/test-scenario.sh")
	compose := readRepositoryFile(t, "scripts/scenario-compose.yml")
	driver := readRepositoryFile(t, "tests/scenario/stress/api.go") +
		readRepositoryFile(t, "tests/scenario/stress/workloads.go")
	justfile := readRepositoryFile(t, "Justfile")

	for _, required := range []string{
		"SECONDBOX_REQUIRE_QUALIFIED_STRESS",
		"SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1",
		"SECONDBOX_SCENARIO_MODE=stress",
		`exec "$repo_root/scripts/test-scenario.sh"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("stress adapter must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"docker compose",
		"trap cleanup EXIT",
		"openssl req",
		"microvm-image/verify.sh",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("stress adapter duplicates shared scenario harness logic %q", forbidden)
		}
	}
	for _, required := range []string{
		"SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT",
		"trap cleanup EXIT",
		"compose logs --tail 200 control-plane secondbox-runner postgres object-store",
		"--entrypoint /usr/local/bin/microvm-host-network-setup",
		"sha256sum --check --strict SHA256SUMS",
		"SecondBox scenario source commit",
		"SecondBox scenario Go version",
		"SecondBox scenario artifact manifest",
		"--mode prepare",
		"--mode run",
	} {
		if !strings.Contains(scenarioScript, required) {
			t.Errorf("shared scenario harness must contain %q", required)
		}
	}
	for _, required := range []string{
		"/dev/kvm:/dev/kvm",
		"/dev/net/tun:/dev/net/tun",
		"privileged: true",
		"network_mode: host",
		"cgroup: host",
		"- -healthcheck",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("stress Compose topology must contain %q", required)
		}
	}
	if !strings.Contains(driver, "sdk/go/secondboxclient") ||
		!strings.Contains(driver, "tests/scenario/harness") {
		t.Error("stress driver must use sdk/go/secondboxclient")
	}
	for _, forbidden := range []string{
		"github.com/SecondStack-AI/SecondBox/internal/",
		"github.com/SecondStack-AI/SecondBox/runner/",
		"pgx",
		"database/sql",
	} {
		if strings.Contains(driver, forbidden) {
			t.Errorf("stress driver crosses the external boundary with %q", forbidden)
		}
	}
	if !strings.Contains(justfile, "test-stress:\n    scripts/test-stress.sh") {
		t.Error("Justfile must expose the stress qualification")
	}

	command := exec.Command(filepath.Join(
		repositoryRootForDeploymentPolicy(t), "scripts", "test-stress.sh",
	))
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("stress qualification reported success without its opt-in and prerequisites")
	}
	if !strings.Contains(string(output), "SECONDBOX_REQUIRE_QUALIFIED_STRESS") {
		t.Fatalf("stress prerequisite failure did not name the missing variable:\n%s", output)
	}
}
