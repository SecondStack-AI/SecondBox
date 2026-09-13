package deployconfig

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerPathsRequireOnlyPlacementOwnedInputs(t *testing.T) {
	fields := []struct {
		name string
		set  func(*Runner, string)
	}{
		{"identity_directory", func(r *Runner, value string) { r.IdentityDirectory = value }},
		{"workspace_root", func(r *Runner, value string) { r.WorkspaceRoot = value }},
		{"egress_context_config_path", func(r *Runner, value string) { r.EgressContextConfigPath = value }},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			runner := validSameHostTestRunner("runner-local")
			field.set(&runner, "/explicit/path")
			if err := validateRunner("runners[0]", runner); err == nil || !strings.Contains(err.Error(), field.name+" is derived") {
				t.Fatalf("same-host path override = %v", err)
			}
			runner = validTestRunner("runner-remote", "remote")
			field.set(&runner, "")
			if err := validateRunner("runners[0]", runner); err == nil || !strings.Contains(err.Error(), field.name+" is required") {
				t.Fatalf("missing remote path = %v", err)
			}
		})
	}
}

func TestSameHostDerivedPathsMatchComposeMounts(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	runner := provisionSameHostTestRunner(t, manifestPath, "runner-local")
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"identity_directory", "workspace_root", "egress_context_config_path", "workspace_host_directory"} {
		if strings.Contains(string(content), "\n"+field+" =") {
			t.Errorf("same-host manifest repeats derived field %s", field)
		}
	}
	envPath := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	resolved, err := Render(manifestPath, envPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SECONDBOX_RUNNER_CLIENT_CERTIFICATE":    "/run/secondbox-runner-identity/runner.crt",
		"SECONDBOX_RUNNER_CLIENT_KEY":            "/run/secondbox-runner-identity/runner.key",
		"SECONDBOX_RUNNER_CONTROL_PLANE_CA":      "/run/secondbox-runner-identity/runner-ca.crt",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT":        "/var/lib/secondbox-runner/workspaces",
		"SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG": "/run/secondbox-runner-config/egress-contexts.json",
	}
	for name, value := range want {
		if resolved.Environment[name] != value {
			t.Errorf("generated %s = %q, want %q", name, resolved.Environment[name], value)
		}
	}
	if _, exists := resolved.Environment["SECONDBOX_RUNNER_WORKSPACE_HOST_DIR"]; exists {
		t.Fatal("generated environment retains unused workspace bind setting")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose v2 is unavailable")
	}
	arguments := []string{"compose", "--project-name", "secondbox-derived-paths-test", "--env-file", envPath}
	for _, file := range resolved.ComposeFiles {
		arguments = append(arguments, "--file", file)
	}
	output, err := exec.Command("docker", append(arguments, "config", "--format", "json")...).Output()
	if err != nil {
		t.Fatalf("Compose config: %v", err)
	}
	var model struct {
		Services map[string]struct {
			Environment map[string]string
			Volumes     []struct{ Source, Target string }
		}
	}
	if err := json.Unmarshal(output, &model); err != nil {
		t.Fatal(err)
	}
	service := model.Services["same-host-runner"]
	for name, value := range want {
		if service.Environment[name] != value {
			t.Errorf("Compose %s = %q, want %q", name, service.Environment[name], value)
		}
	}
	mounts := make(map[string]string)
	for _, volume := range service.Volumes {
		mounts[volume.Target] = volume.Source
	}
	if mounts["/var/lib/secondbox-runner"] != runner.StateHostDirectory || mounts["/run/secondbox-runner-identity"] != runner.IdentityHostDirectory || mounts["/run/secondbox-runner-config"] == "" {
		t.Fatalf("Compose mounts differ from generated paths: %#v", mounts)
	}
	if _, nested := mounts["/var/lib/secondbox-runner/workspaces"]; nested {
		t.Fatal("Compose introduced a separate workspace mount")
	}
}

func TestSameHostDerivedWorkspaceMustAlreadyExist(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		name := "missing"
		if symlink {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			manifestPath := initializedDevelopment(t)
			runner := provisionSameHostTestRunner(t, manifestPath, "runner-local")
			workspace := filepath.Join(runner.StateHostDirectory, "workspaces")
			if err := os.Remove(workspace); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(t.TempDir(), workspace); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "state_host_directory/workspaces must be an existing") {
				t.Fatalf("unsafe derived workspace accepted: %v", err)
			}
			if !symlink {
				if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
					t.Fatalf("missing workspace was reconstructed: %v", err)
				}
			}
		})
	}
}
