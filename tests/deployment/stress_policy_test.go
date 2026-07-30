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
	compose := readRepositoryFile(t, "scripts/scenario-compose.yml")
	driver := readRepositoryFile(t, "tests/scenario/stress/api.go") +
		readRepositoryFile(t, "tests/scenario/stress/workloads.go")
	justfile := readRepositoryFile(t, "Justfile")

	for _, required := range []string{
		"SECONDBOX_REQUIRE_QUALIFIED_STRESS",
		"SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT",
		"trap cleanup EXIT",
		"compose logs control-plane secondbox-runner postgres object-store",
		"--entrypoint /usr/local/bin/microvm-host-network-setup",
		"runner/scripts/microvm-image/verify.sh",
		"SecondBox stress source commit",
		"SecondBox stress Go version",
		"SecondBox stress artifact manifest",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("stress script must contain %q", required)
		}
	}
	for _, required := range []string{
		"/dev/kvm:/dev/kvm",
		"/dev/net/tun:/dev/net/tun",
		"privileged: true",
		"network_mode: host",
		"cgroup: host",
		"secondbox-runner\", \"-healthcheck\"",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("stress Compose topology must contain %q", required)
		}
	}
	if !strings.Contains(driver, "sdk/go/secondboxclient") {
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
