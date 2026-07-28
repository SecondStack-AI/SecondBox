package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessSourceDigestMatchesDockerContextExclusions(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("harness-source-sha256.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "agent", "src"),
		filepath.Join(root, "agent", "node_modules", "dependency"),
		filepath.Join(root, "agent", "dist"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sourcePath := filepath.Join(root, "agent", "src", "turn-runner.ts")
	dependencyPath := filepath.Join(root, "agent", "node_modules", "dependency", "index.js")
	distPath := filepath.Join(root, "agent", "dist", "turn-runner.js")
	write(sourcePath, "source-one")
	write(dependencyPath, "dependency-one")
	write(distPath, "dist-one")

	digest := func() string {
		t.Helper()
		command := exec.Command(script)
		command.Dir = root
		output, runErr := command.CombinedOutput()
		if runErr != nil {
			t.Fatalf("digest harness source: %v\n%s", runErr, output)
		}
		return strings.TrimSpace(string(output))
	}
	initial := digest()
	write(dependencyPath, "dependency-two")
	write(distPath, "dist-two")
	if got := digest(); got != initial {
		t.Fatalf("generated or installed harness files changed source digest: %q != %q", got, initial)
	}
	write(sourcePath, "source-two")
	if got := digest(); got == initial {
		t.Fatal("harness source change did not change source digest")
	}
}

func TestHarnessSourceDigestIsLocaleIndependent(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("harness-source-sha256.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "agent", "src")
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent.ts", "agent-memory.ts", "agent.memory.ts", "agent-manager.ts"} {
		if err := os.WriteFile(filepath.Join(sourceRoot, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := func(locale string) string {
		t.Helper()
		command := exec.Command(script)
		command.Dir = root
		command.Env = append(os.Environ(), "LC_ALL="+locale)
		output, runErr := command.CombinedOutput()
		if runErr != nil {
			t.Fatalf("digest harness source under %s: %v\n%s", locale, runErr, output)
		}
		return strings.TrimSpace(string(output))
	}
	if cDigest, localizedDigest := digest("C"), digest("en_US.utf8"); localizedDigest != cDigest {
		t.Fatalf("locale changed harness source digest: C=%s en_US=%s", cDigest, localizedDigest)
	}
}
