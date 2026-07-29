package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestRunnerEntrypointCreatesOnlyStandaloneRuntimeDirectories(t *testing.T) {
	source, err := os.ReadFile("container/secondbox-runner-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := string(source)
	for _, required := range []string{
		"SECONDBOX_RUNNER_WORKSPACE_ROOT",
		"SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR",
		"SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT",
		`if [[ "$variable" == SECONDBOX_RUNNER_* ]]`,
	} {
		if !strings.Contains(entrypoint, required) {
			t.Errorf("SecondBox runner entrypoint is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"SECONDBOX_GUEST_",
		"INTEGRATION_SERVICE_",
		"HAR" + "NESS",
		"SP" + "OOL",
	} {
		if strings.Contains(entrypoint, forbidden) {
			t.Errorf("SecondBox runner entrypoint retains inherited input %q", forbidden)
		}
	}
}

func TestGuestEntrypointRequiresAssignmentIdentityAndDedicatedVsockPorts(t *testing.T) {
	source, err := os.ReadFile("microvm-image/tool-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := string(source)
	for _, required := range []string{
		"secondbox.instance_id",
		"secondbox.sandbox_generation",
		"secondbox.guest_build_id",
		"secondbox.image_manifest_digest",
		"secondbox.toolchain_manifest_digest",
		"secondbox.guest_control_vsock_port",
		"secondbox.guest_protocol_vsock_port",
		"--control-vsock-port",
		"--protocol-vsock-port",
		"--heartbeat-interval",
	} {
		if !strings.Contains(entrypoint, required) {
			t.Errorf("guest entrypoint is missing %q", required)
		}
	}
	if strings.Contains(entrypoint, "--vsock-port 1024") {
		t.Fatal("guest entrypoint retains the old implicit same-port control protocol")
	}
	initSource, err := os.ReadFile("microvm-image/init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(initSource), "secondbox\\.process_limit=") ||
		strings.Contains(string(initSource), "secondbox-runner\\.process_limit=") {
		t.Fatal("guest init process-limit kernel argument does not match the runner launch contract")
	}
}
