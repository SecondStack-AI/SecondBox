package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestSandboxHostEntrypointCreatesManagerOwnedHarnessStagingDirectory(t *testing.T) {
	source, err := os.ReadFile("container/sandbox-host-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := string(source)
	for _, required := range []string{
		"SANDBOX_HOST_LAUNCHER_HARNESS_STAGING_DIR",
		`install -d -o 10001 -g 10001 -m 0700 "$SANDBOX_HOST_LAUNCHER_HARNESS_STAGING_DIR"`,
	} {
		if !strings.Contains(entrypoint, required) {
			t.Errorf("Sandbox Host entrypoint lost harness staging contract %q", required)
		}
	}
	if !strings.Contains(
		entrypoint,
		`install -d -o 0 -g 10001 -m 0750 "$(dirname "$SANDBOX_HOST_LAUNCHER_SOCKET")"`,
	) {
		t.Error("Sandbox Host entrypoint widened the launcher runtime directory")
	}
}
