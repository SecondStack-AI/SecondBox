package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestRunnerEntrypointCreatesRuntimeDirectoriesAndExecutesRunner(t *testing.T) {
	source, err := os.ReadFile("container/secondbox-runner-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := string(source)
	for _, required := range []string{
		"SECONDBOX_RUNNER_WORKSPACE_ROOT",
		"SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR",
		"SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT",
		`"$1" != "/usr/local/bin/secondbox-runner"`,
		`findmnt -T "$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"`,
		"noexec",
		"nodev",
		`exec "$@"`,
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
		"runner.env",
		"compgen -e",
		"/lib/systemd/systemd",
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
	// The template-mode branch must exec before any identity argument is read, so
	// an identity-neutral template never carries a Sandbox ID into its capture.
	templateBranch := strings.Index(entrypoint, `if [ "$template_mode" = "1" ]`)
	if templateBranch < 0 {
		t.Fatal("guest entrypoint is missing the secondbox.template_mode branch")
	}
	for _, required := range []string{
		"secondbox.template_mode",
		"--template-mode",
	} {
		if !strings.Contains(entrypoint, required) {
			t.Errorf("guest entrypoint is missing %q", required)
		}
	}
	for _, identityArg := range []string{
		"secondbox.instance_id",
		"secondbox.sandbox_id",
		"secondbox.sandbox_generation",
		"secondbox.guest_build_id",
		"secondbox.image_manifest_digest",
		"secondbox.toolchain_manifest_digest",
		"secondbox.guest_heartbeat_interval",
	} {
		if strings.Index(entrypoint, identityArg) < templateBranch {
			t.Errorf("guest entrypoint reads %q before the template-mode exec", identityArg)
		}
	}
}
