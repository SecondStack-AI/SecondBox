package deployconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestRunnerTemplateSubstitutionValidatesAsSameHostTopology(t *testing.T) {
	template := RunnerTemplate()
	if !bytes.HasSuffix(template, []byte("\n")) {
		t.Fatal("Runner template does not end with a newline")
	}

	manifestPath := initializedDevelopment(t)
	hostRoot := t.TempDir()
	storageDirectory, err := os.MkdirTemp("/dev/shm", "secondbox-template-storage-")
	if err != nil {
		t.Skipf("dedicated tmpfs unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(storageDirectory) })
	rootDevice, err := filesystemDevice("/")
	if err != nil {
		t.Fatal(err)
	}
	workspaceDevice, err := filesystemDevice(storageDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceDevice == rootDevice {
		t.Skip("dedicated tmpfs is on the root filesystem")
	}

	runner := validSameHostTestRunner("runner-template-test")
	runner.PoolID = "standard-amd64"
	runner.IdentityHostDirectory = filepath.Join(hostRoot, "identity")
	runner.ArtifactHostDirectory = filepath.Join(storageDirectory, "release", "artifacts")
	runner.StateHostDirectory = storageDirectory
	runner.WorkspaceHostDirectory = filepath.Join(storageDirectory, "workspaces")
	for _, directory := range []string{runner.ArtifactHostDirectory, runner.WorkspaceHostDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	populated := populateRunnerTemplateForTest(t, string(template), runner)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	withoutEmptyRunners := bytes.Replace(manifestBytes, []byte("runners = []\n"), nil, 1)
	if bytes.Equal(withoutEmptyRunners, manifestBytes) {
		t.Fatal("initialized manifest did not contain the empty Runner declaration")
	}
	runnerType := reflect.TypeOf(runner)
	for index := 0; index < runnerType.NumField(); index++ {
		name := runnerType.Field(index).Tag.Get("toml")
		placeholder := runnerTemplateAssignmentForTest(t, string(template), name)
		candidate := replaceRunnerTemplateAssignmentForTest(t, string(populated), name, placeholder)
		candidateManifest := append(bytes.Clone(withoutEmptyRunners), candidate...)
		if err := os.WriteFile(manifestPath, candidateManifest, 0o600); err != nil {
			t.Fatal(err)
		}
		decoded, validationErr := ReadManifest(manifestPath)
		if validationErr == nil {
			validationErr = validateManifestShape(decoded)
		}
		if validationErr == nil {
			t.Fatalf("placeholder for %s passed same-host manifest validation", name)
		}
	}
	manifestBytes = append(withoutEmptyRunners, populated...)
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Runners) != 1 || manifest.Runners[0].Placement != "same-host" {
		t.Fatalf("substituted Runner declarations = %#v", manifest.Runners)
	}
	if _, err := resolveManifestWithOptions(manifest, filepath.Dir(manifestPath), false); err != nil {
		t.Fatalf("substituted Runner template failed manifest validation: %v", err)
	}
	if _, err := os.Lstat(runner.IdentityHostDirectory); !os.IsNotExist(err) {
		t.Fatalf("identity target existed before runner-init: %v", err)
	}
	if err := RunnerInit(manifestPath, runner.RunnerID, runner.IdentityHostDirectory); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatalf("substituted Runner template failed validate semantics: %v", err)
	}
	if got := resolved.Environment["SECONDBOX_RUNNER_POOL_ID"]; got != "standard-amd64" {
		t.Fatalf("resolved Runner pool = %q", got)
	}
}

func TestRunnerTemplateCoversSchemaAndWritesCreateOnly(t *testing.T) {
	template := string(RunnerTemplate())
	lines := strings.Split(template, "\n")
	assignments := make(map[string]int)
	for index, line := range lines {
		name, _, found := strings.Cut(line, " = ")
		if !found {
			continue
		}
		assignments[name]++
		if index == 0 || !strings.HasPrefix(lines[index-1], "# ") {
			t.Errorf("template field %s has no one-line comment", name)
		}
	}
	runnerType := reflect.TypeOf(Runner{})
	if len(assignments) != runnerType.NumField() {
		t.Fatalf("template field count = %d, Runner field count = %d", len(assignments), runnerType.NumField())
	}
	for index := 0; index < runnerType.NumField(); index++ {
		name := runnerType.Field(index).Tag.Get("toml")
		if assignments[name] != 1 {
			t.Errorf("template field %s count = %d", name, assignments[name])
		}
	}

	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := WriteRunnerTemplate(path); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, RunnerTemplate()) {
		t.Fatal("written Runner template differs from stdout content")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("Runner template mode = %o", info.Mode().Perm())
	}
	if err := WriteRunnerTemplate(path); err == nil || !strings.Contains(err.Error(), "target already exists") {
		t.Fatalf("create-only rewrite error = %v", err)
	}
}

func populateRunnerTemplateForTest(t *testing.T, template string, runner Runner) []byte {
	t.Helper()
	lines := strings.Split(template, "\n")
	runnerType := reflect.TypeOf(runner)
	runnerValue := reflect.ValueOf(runner)
	for index := 0; index < runnerType.NumField(); index++ {
		name := runnerType.Field(index).Tag.Get("toml")
		encoded, err := toml.Marshal(map[string]any{name: runnerValue.Field(index).Interface()})
		if err != nil {
			t.Fatalf("encode replacement for %s: %v", name, err)
		}
		replacement := strings.TrimSpace(string(encoded))
		found := 0
		for lineIndex, line := range lines {
			if strings.HasPrefix(line, name+" = ") {
				lines[lineIndex] = replacement
				found++
			}
		}
		if found != 1 {
			t.Fatalf("template assignment count for %s = %d", name, found)
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func runnerTemplateAssignmentForTest(t *testing.T, template, name string) string {
	t.Helper()
	found := ""
	for _, line := range strings.Split(template, "\n") {
		if strings.HasPrefix(line, name+" = ") {
			if found != "" {
				t.Fatalf("template contains duplicate assignment for %s", name)
			}
			found = line
		}
	}
	if found == "" {
		t.Fatalf("template has no assignment for %s", name)
	}
	return found
}

func replaceRunnerTemplateAssignmentForTest(t *testing.T, template, name, replacement string) []byte {
	t.Helper()
	lines := strings.Split(template, "\n")
	found := 0
	for index, line := range lines {
		if strings.HasPrefix(line, name+" = ") {
			lines[index] = replacement
			found++
		}
	}
	if found != 1 {
		t.Fatalf("template assignment count for %s = %d", name, found)
	}
	return []byte(strings.Join(lines, "\n"))
}
