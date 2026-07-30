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
	prepareScript := readRepositoryFile(t, "scripts/prepare-stress.sh")
	gitignore := readRepositoryFile(t, ".gitignore")

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
		"SECONDBOX_SCENARIO_DIAGNOSTICS_DIR",
		`"$diagnostics_dir/runner.jsonl"`,
		`"$diagnostics_dir/compose.log"`,
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
		"stop_grace_period: 45s",
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
	if !strings.Contains(justfile, "prepare-stress:\n    scripts/prepare-stress.sh") {
		t.Error("Justfile must expose persistent local stress preparation")
	}
	for _, required := range []string{
		`local_root="${SECONDBOX_STRESS_LOCAL_ROOT:-$repo_root/.secondbox/stress}"`,
		"manifest-private.pem",
		"manifest-public.pem",
		"openssl genpkey",
		"build-secondbox-rootfs-source.sh",
		"microvm-image/build.sh",
		`"$verify_script" "$staged_artifacts"`,
		`mv "$staged_artifacts" "$artifacts_dir"`,
	} {
		if !strings.Contains(prepareScript, required) {
			t.Errorf("local stress preparation must contain %q", required)
		}
	}
	if !strings.Contains(gitignore, "/.secondbox/") {
		t.Error("persistent local stress artifacts and private keys must be ignored")
	}

	command := exec.Command(filepath.Join(
		repositoryRootForDeploymentPolicy(t), "scripts", "test-stress.sh",
	))
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SECONDBOX_STRESS_LOCAL_ROOT=" + filepath.Join(t.TempDir(), "not-prepared"),
	}
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("stress qualification reported success without prepared local state")
	}
	if !strings.Contains(string(output), "just prepare-stress") {
		t.Fatalf("stress prerequisite failure did not name the preparation command:\n%s", output)
	}
}
