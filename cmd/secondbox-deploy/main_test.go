package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
	"github.com/SecondStack-AI/SecondBox/internal/install"
)

func TestDeployCommandFailurePreservesExplicitPresentationModes(t *testing.T) {
	err := run([]string{"--output", "plain", "--color", "never", "unknown-command"})
	var presented *deployPresentationError
	if !errors.As(err, &presented) {
		t.Fatalf("failure lacks presentation contract: %v", err)
	}
	if presented.renderer.OutputMode != cliui.OutputPlain || presented.renderer.ColorMode != cliui.ColorNever {
		t.Fatalf("failure renderer = output %s color %s", presented.renderer.OutputMode, presented.renderer.ColorMode)
	}
}

func TestDeployRootHelpIsSuccessfulAndSeparateFromUsageErrors(t *testing.T) {
	var output bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(&output, io.Discard), OutputMode: cliui.OutputAuto, ColorMode: cliui.ColorAuto}
	for _, arguments := range [][]string{nil, {"help"}} {
		output.Reset()
		if err := runCommand(arguments, renderer); err != nil {
			t.Fatalf("secondbox-deploy %v help error = %v", arguments, err)
		}
		if !strings.Contains(output.String(), "SecondBox Deploy\n\nUsage\n") || !strings.Contains(output.String(), "Global options\n") || strings.Contains(output.String(), "\x1b") {
			t.Fatalf("secondbox-deploy %v help output = %q", arguments, output.String())
		}
	}
	if err := runCommand([]string{"unknown"}, renderer); err == nil || !strings.Contains(err.Error(), "run secondbox-deploy help") {
		t.Fatalf("unknown command error = %v", err)
	}
}

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

func TestEveryDeployCommandHasOutputContract(t *testing.T) {
	for _, command := range []string{"help", "version", "install", "init", "runner-template", "verify", "validate", "render", "runner-init", "inspect", "migrate", "compose"} {
		contract, found := deployCommandContracts[command]
		if !found || contract.Command != command || contract.Output == "" || contract.ExitOwner == "" {
			t.Errorf("command %q has incomplete output contract: %#v", command, contract)
		}
	}
	if deployCommandContracts["compose"].Output != "subprocess" || deployCommandContracts["runner-template"].Output != "raw-bytes" || deployCommandContracts["inspect"].Output != "machine-json" {
		t.Fatal("machine-authoritative deploy contracts changed")
	}
}

func TestInstallCheckRendersCompletePreflightAndStops(t *testing.T) {
	facts := install.HostFacts{ObservedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), HostIdentity: "machine-id:test", OS: "linux", Architecture: "amd64", Findings: []install.Finding{{ID: "platform", Class: install.FindingPass, Summary: "Linux amd64 host"}, {ID: "storage", Class: install.FindingWarning, Summary: "Review storage"}}}
	var output bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(&output, io.Discard), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	called := 0
	err := runInstallPreflightWith(context.Background(), []string{"--check"}, renderer, func(context.Context) (install.HostFacts, error) {
		called++
		return facts, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || !strings.Contains(output.String(), "SecondBox single-host preflight") || !strings.Contains(output.String(), "WARNING") {
		t.Fatalf("preflight invocation/output = %d, %q", called, output.String())
	}
}

func TestInstallCheckUsesStableBlockedExitStatus(t *testing.T) {
	facts := install.HostFacts{HostIdentity: "machine-id:test", OS: "linux", Architecture: "arm64", Findings: []install.Finding{{ID: "platform", Class: install.FindingBlocked, Summary: "Linux amd64 is required"}}}
	var output bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(&output, io.Discard), OutputMode: cliui.OutputJSON, ColorMode: cliui.ColorNever}
	err := runInstallPreflightWith(context.Background(), []string{"--check"}, renderer, func(context.Context) (install.HostFacts, error) { return facts, nil })
	var exited interface{ ExitCode() int }
	if !errors.As(err, &exited) || exited.ExitCode() != 2 {
		t.Fatalf("error = %v; want installer exit 2", err)
	}
	if !strings.Contains(output.String(), `"class":"blocked"`) || strings.Contains(output.String(), "\x1b[") {
		t.Fatalf("JSON report = %q", output.String())
	}
}

func TestInstallUsageRejectsUnknownForms(t *testing.T) {
	var output bytes.Buffer
	renderer := cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: cliui.ForWriter(&output, io.Discard), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
	err := runInstallPreflightWith(context.Background(), []string{"--check", "extra"}, renderer, func(context.Context) (install.HostFacts, error) {
		t.Fatal("preflight ran for invalid grammar")
		return install.HostFacts{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "secondbox-deploy") {
		t.Fatalf("usage error = %v", err)
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

func TestDockerComposeExitStatusSurvives(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte("#!/bin/sh\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	err := runDockerCompose([]string{"compose", "version"})
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 42 {
		t.Fatalf("compose error = %v, want exit 42", err)
	}
}

func TestComposePresentationStaysOnDiagnosticStream(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	manifest, err := deployconfig.InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
	if err != nil {
		t.Fatal(err)
	}
	var output, diagnostic bytes.Buffer
	capabilities := cliui.ForWriter(&output, &diagnostic)
	capabilities.Diagnostic.TTY = true
	renderer := cliui.Renderer{Output: &output, Diagnostic: &diagnostic, Capabilities: capabilities, OutputMode: cliui.OutputAuto, ColorMode: cliui.ColorNever}
	if err := runComposePresented(renderer, manifest, "config"); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("presentation contaminated stdout: %q", output.String())
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("presentation contaminated Docker stderr: %q", diagnostic.String())
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

func TestInspectCommandPreservesMachineJSONBytes(t *testing.T) {
	manifestPath, err := deployconfig.InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := deployconfig.Inspect(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	got, runErr := captureDeployStdout(t, func() error { return run([]string{"inspect", manifestPath}) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !bytes.Equal(got, append(want, '\n')) {
		t.Fatalf("inspect bytes changed:\n got %q\nwant %q", got, append(want, '\n'))
	}
}

func captureDeployStdout(t *testing.T, action func() error) ([]byte, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	actionErr := action()
	os.Stdout = original
	closeErr := writer.Close()
	content, readErr := io.ReadAll(reader)
	_ = reader.Close()
	return content, errors.Join(actionErr, closeErr, readErr)
}
