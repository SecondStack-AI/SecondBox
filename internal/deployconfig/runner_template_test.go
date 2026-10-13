package deployconfig

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

var runnerTemplateBackendsForTest = []string{computeBackendFirecracker, computeBackendGVisor}

func TestRunnerTemplateSubstitutionValidatesAsSameHostTopology(t *testing.T) {
	for _, backend := range runnerTemplateBackendsForTest {
		t.Run(backend, func(t *testing.T) {
			template, err := RunnerTemplate(backend)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(template, []byte("\n")) {
				t.Fatal("Runner template does not end with a newline")
			}

			manifestPath := initializedDevelopment(t)
			if backend == computeBackendGVisor {
				developmentGVisorReleaseForTest(t, manifestPath)
			}
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
			if backend == computeBackendGVisor {
				runner = validSameHostGVisorTestRunner("runner-template-test")
			}
			runner.PoolID = hostStandardPool(t)
			runner.IdentityHostDirectory = filepath.Join(hostRoot, "identity")
			runner.ArtifactHostDirectory = filepath.Join(storageDirectory, "release", "artifacts")
			runner.StateHostDirectory = storageDirectory
			for _, directory := range []string{runner.ArtifactHostDirectory, runner.workspaceHostDirectory()} {
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
			derived := make(map[string]bool)
			for _, path := range runner.packagedPaths() {
				derived[path.name] = true
			}
			if backend == computeBackendGVisor {
				for name := range runner.sameHostGVisorDerivedSettings() {
					derived[name] = true
				}
			}
			runnerType := reflect.TypeOf(runner)
			for index := 0; index < runnerType.NumField(); index++ {
				name, _, _ := strings.Cut(runnerType.Field(index).Tag.Get("toml"), ",")
				if derived[name] || !runnerTemplateAssignsForTest(string(template), name) {
					continue // Same-host placement derives these, or the backend does not use them.
				}
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
				if validationErr == nil && name != "compute_backend" {
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
			if len(manifest.Runners) != 1 || manifest.Runners[0].Placement != "same-host" || manifest.Runners[0].ComputeBackend != backend {
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
			if got := resolved.Environment["SECONDBOX_RUNNER_POOL_ID"]; got != hostStandardPool(t) {
				t.Fatalf("resolved Runner pool = %q", got)
			}
			if got := resolved.Environment["SECONDBOX_COMPUTE_BACKEND"]; got != backend {
				t.Fatalf("resolved compute backend = %q", got)
			}
		})
	}
}

func TestRunnerTemplateCoversSchemaAndWritesCreateOnly(t *testing.T) {
	covered := make(map[string]int)
	for _, backend := range runnerTemplateBackendsForTest {
		template, err := RunnerTemplate(backend)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(template), "\n")
		assignments := make(map[string]int)
		for index, line := range lines {
			name, _, found := strings.Cut(line, " = ")
			if !found {
				continue
			}
			assignments[name]++
			if index == 0 || !strings.HasPrefix(lines[index-1], "# ") {
				t.Errorf("%s template field %s has no one-line comment", backend, name)
			}
		}
		for name, count := range assignments {
			if count != 1 {
				t.Errorf("%s template field %s count = %d", backend, name, count)
			}
			covered[name]++
		}
		if !strings.Contains(string(template), "compute_backend = '"+backend+"'") {
			t.Errorf("%s template does not state its compute backend", backend)
		}
		// Each template contains exactly the settings its backend owns.
		probe := Runner{ComputeBackend: backend}
		for name := range probe.otherBackendSettings() {
			if assignments[name] != 0 {
				t.Errorf("%s template contains other-backend field %s", backend, name)
			}
		}
	}
	runnerType := reflect.TypeOf(Runner{})
	if len(covered) != runnerType.NumField() {
		t.Fatalf("template field count = %d, Runner field count = %d", len(covered), runnerType.NumField())
	}
	for index := 0; index < runnerType.NumField(); index++ {
		name, _, _ := strings.Cut(runnerType.Field(index).Tag.Get("toml"), ",")
		if covered[name] == 0 {
			t.Errorf("no template covers field %s", name)
		}
	}
	if _, err := RunnerTemplate("microsandbox"); err == nil {
		t.Fatal("Runner template accepted an unsupported backend")
	}

	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := WriteRunnerTemplate(path, computeBackendGVisor); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := RunnerTemplate(computeBackendGVisor)
	if !bytes.Equal(written, want) {
		t.Fatal("written Runner template differs from stdout content")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("Runner template mode = %o", info.Mode().Perm())
	}
	if err := WriteRunnerTemplate(path, computeBackendGVisor); err == nil || !strings.Contains(err.Error(), "target already exists") {
		t.Fatalf("create-only rewrite error = %v", err)
	}
}

// populateRunnerTemplateForTest substitutes every template assignment with the
// Runner's value. A nil value removes the assignment, which is how an operator
// omits a derived same-host setting.
func populateRunnerTemplateForTest(t *testing.T, template string, runner Runner) []byte {
	t.Helper()
	lines := strings.Split(template, "\n")
	runnerType := reflect.TypeOf(runner)
	runnerValue := reflect.ValueOf(runner)
	for index := 0; index < runnerType.NumField(); index++ {
		name, _, _ := strings.Cut(runnerType.Field(index).Tag.Get("toml"), ",")
		field := runnerValue.Field(index)
		if !runnerTemplateAssignsForTest(template, name) {
			if !field.IsZero() {
				t.Fatalf("test Runner sets %s, which its backend template does not contain", name)
			}
			continue
		}
		replacement := ""
		if field.Kind() != reflect.Pointer || !field.IsNil() {
			encoded, err := toml.Marshal(map[string]any{name: field.Interface()})
			if err != nil {
				t.Fatalf("encode replacement for %s: %v", name, err)
			}
			replacement = strings.TrimSpace(string(encoded))
		}
		if name == "egress_contexts" {
			contexts := field.Interface().([]RunnerEgressContext)
			if len(contexts) != 1 || len(contexts[0].Gateways) == 0 {
				t.Fatalf("test Runner egress contexts = %#v", contexts)
			}
			var gateways []string
			for _, gateway := range contexts[0].Gateways {
				gateways = append(gateways, fmt.Sprintf("{ logical_name = %q, address = %q }", gateway.LogicalName, gateway.Address))
			}
			replacement = fmt.Sprintf("egress_contexts = [{ name = %q, gateways = [%s] }]", contexts[0].Name, strings.Join(gateways, ", "))
		}
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

func runnerTemplateAssignsForTest(template, name string) bool {
	for _, line := range strings.Split(template, "\n") {
		if strings.HasPrefix(line, name+" = ") {
			return true
		}
	}
	return false
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
