package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
)

func TestComposeUpArgumentsRemoveOrphanedTopology(t *testing.T) {
	base := []string{"compose", "--project-name", "secondbox"}
	got := composeUpArguments(base, "--detach")
	want := []string{"compose", "--project-name", "secondbox", "up", "--remove-orphans", "--detach"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compose up arguments = %#v", got)
	}
	if !reflect.DeepEqual(base, []string{"compose", "--project-name", "secondbox"}) {
		t.Fatalf("Compose base arguments mutated = %#v", base)
	}
}

func TestComposeDownArgumentsRemoveOrphanedTopology(t *testing.T) {
	base := []string{"compose", "--project-name", "secondbox"}
	got := composeDownArguments(base)
	want := []string{"compose", "--project-name", "secondbox", "down", "--remove-orphans"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compose down arguments = %#v", got)
	}
	if !reflect.DeepEqual(base, []string{"compose", "--project-name", "secondbox"}) {
		t.Fatalf("Compose base arguments mutated = %#v", base)
	}
}

func TestDockerComposeReceivesOperatorClientConfiguration(t *testing.T) {
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "environment")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$DOCKER_CONFIG\" \"$SSH_AUTH_SOCK\" > " + strconv.Quote(outputPath) + "\n"
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("DOCKER_CONFIG", "/operator/docker-config")
	t.Setenv("SSH_AUTH_SOCK", "/operator/ssh-agent.sock")
	if err := runDockerCompose([]string{"compose", "version"}); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(output)); got != "/operator/docker-config\n/operator/ssh-agent.sock" {
		t.Fatalf("Docker client environment = %q", got)
	}
}

func TestComposeUsesEmbeddedAssetsOutsideTheRepository(t *testing.T) {
	directory := t.TempDir()
	script := `#!/bin/sh
expect_file=false
for argument in "$@"; do
  if [ "$expect_file" = true ]; then
    case "$argument" in
      /*) ;;
      *) exit 11 ;;
    esac
    test -f "$argument" || exit 12
    expect_file=false
  elif [ "$argument" = "--file" ]; then
    expect_file=true
  fi
done
`
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	manifestPath, err := deployconfig.InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if err := runCompose(manifestPath, "config"); err != nil {
		t.Fatal(err)
	}
}

// Compose derives every container, volume, and network name from the project
// name, so a second deployment that kept the default would bind the first
// deployment's volumes and recreate its containers instead of failing.
func TestComposeProjectNameIsolatesDeploymentsOnOneHost(t *testing.T) {
	tests := []struct {
		name    string
		project string
		want    string
	}{
		{name: "manifest without the field keeps the original project", project: "", want: "secondbox"},
		{name: "manifest with a distinct project deploys beside it", project: "secondbox-v030-test", want: "secondbox-v030-test"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := t.TempDir()
			argvPath := filepath.Join(stub, "argv")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + strconv.Quote(argvPath) + "\n"
			if err := os.WriteFile(filepath.Join(stub, "docker"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", stub)
			manifestPath, err := deployconfig.InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			replacement := "compose_project_name = " + strconv.Quote(test.project)
			if test.project == "" {
				replacement = ""
			}
			updated := strings.Replace(string(original), "compose_project_name = 'secondbox'", replacement, 1)
			if updated == string(original) {
				t.Fatal("initialized manifest did not state a Compose project")
			}
			if err := os.WriteFile(manifestPath, []byte(updated), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(t.TempDir())
			if err := runCompose(manifestPath, "config"); err != nil {
				t.Fatal(err)
			}
			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(argv)), "\n")
			index := slices.Index(fields, "--project-name")
			if index < 0 || index+1 >= len(fields) {
				t.Fatalf("docker argv carried no project name: %#v", fields)
			}
			if fields[index+1] != test.want {
				t.Fatalf("Compose project = %q, want %q", fields[index+1], test.want)
			}
		})
	}
}

func TestRunnerTemplateCommandWritesCreateOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := run([]string{"runner-template", "--output", path}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, deployconfig.RunnerTemplate()) {
		t.Fatal("runner-template command output differs from the deployconfig template")
	}
	if err := run([]string{"runner-template", "--output", path}); err == nil {
		t.Fatal("runner-template command replaced an existing file")
	}
}

func TestRunnerTemplateCommandWritesStdout(t *testing.T) {
	stdoutPath := filepath.Join(t.TempDir(), "stdout")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = stdout
	runErr := run([]string{"runner-template"})
	os.Stdout = original
	closeErr := stdout.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	got, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, deployconfig.RunnerTemplate()) {
		t.Fatal("runner-template stdout differs from the deployconfig template")
	}
}
