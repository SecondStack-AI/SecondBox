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
		{"log_path", func(r *Runner, value string) { r.LogPath = value }},
		{"log_directory", func(r *Runner, value string) { r.LogDirectory = value }},
		{"firecracker_path", func(r *Runner, value string) { r.FirecrackerPath = value }},
		{"firecracker_jailer_path", func(r *Runner, value string) { r.FirecrackerJailerPath = value }},
		{"firecracker_jail_root", func(r *Runner, value string) { r.FirecrackerJailRoot = value }},
		{"firecracker_kernel_path", func(r *Runner, value string) { r.FirecrackerKernelPath = value }},
		{"firecracker_rootfs_path", func(r *Runner, value string) { r.FirecrackerRootFSPath = value }},
		{"firecracker_shared_image_path", func(r *Runner, value string) { r.FirecrackerSharedImagePath = value }},
		{"firecracker_run_directory", func(r *Runner, value string) { r.FirecrackerRunDirectory = value }},
		{"firecracker_log_directory", func(r *Runner, value string) { r.FirecrackerLogDirectory = value }},
		{"snapshot_template_cache_root", func(r *Runner, value string) { r.SnapshotTemplateCacheRoot = value }},
		{"artifact_public_key", func(r *Runner, value string) { r.ArtifactPublicKey = value }},
		{"sandbox_network_state_directory", func(r *Runner, value string) { r.SandboxNetworkStateDir = value }},
		{"network_policy_nft_path", func(r *Runner, value string) { r.NetworkPolicyNFTPath = value }},
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
	for _, field := range []string{"identity_directory", "workspace_root", "egress_context_config_path", "log_path", "log_directory", "firecracker_path", "firecracker_jailer_path", "firecracker_jail_root", "firecracker_kernel_path", "firecracker_rootfs_path", "firecracker_shared_image_path", "firecracker_run_directory", "firecracker_log_directory", "snapshot_template_cache_root", "artifact_public_key", "sandbox_network_state_directory", "network_policy_nft_path", "workspace_host_directory"} {
		_, runnerContent, found := strings.Cut(string(content), "[[runners]]")
		if !found {
			t.Fatal("rendered manifest has no Runner declaration")
		}
		if strings.Contains(runnerContent, "\n"+field+" =") {
			t.Errorf("same-host manifest repeats derived field %s", field)
		}
	}
	envPath := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	resolved, err := Render(manifestPath, envPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED":    "false",
		"SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE":  "true",
		"SECONDBOX_RUNNER_LOG_PATH":                      "/var/lib/secondbox-runner/state/logs/runner.jsonl",
		"SECONDBOX_RUNNER_LOG_DIR":                       "/var/lib/secondbox-runner/state/logs",
		"SECONDBOX_RUNNER_FIRECRACKER_PATH":              "/usr/local/bin/firecracker",
		"SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH":       "/usr/local/bin/jailer",
		"SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT":         "/var/lib/secondbox-runner/jail",
		"SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH":       "/opt/secondbox-artifacts/kernel",
		"SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH":       "/opt/secondbox-artifacts/rootfs.ext4",
		"SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH": "/opt/secondbox-artifacts/shared.img",
		"SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR":           "/var/lib/secondbox-runner/state/run",
		"SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR":           "/var/lib/secondbox-runner/state/firecracker-logs",
		"SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT":  "/var/lib/secondbox-runner/state/snapshot-template-cache",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY":           "/opt/secondbox-artifacts/signing.pub",
		"SECONDBOX_RUNNER_EXECUTION_IMAGE_PUBLIC_KEY":    "/run/secondbox-image-trust/public.pem",
		"SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR":     "/var/lib/secondbox-runner/state/network",
		"SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH":       "/usr/sbin/nft",
		"SECONDBOX_RUNNER_CLIENT_CERTIFICATE":            "/run/secondbox-runner-identity/runner.crt",
		"SECONDBOX_RUNNER_CLIENT_KEY":                    "/run/secondbox-runner-identity/runner.key",
		"SECONDBOX_RUNNER_CONTROL_PLANE_CA":              "/run/secondbox-runner-identity/runner-ca.crt",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT":                "/var/lib/secondbox-runner/workspaces",
		"SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG":         "/run/secondbox-runner-config/egress-contexts.json",
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
	if mounts["/var/lib/secondbox-runner"] != runner.StateHostDirectory || mounts["/run/secondbox-runner-identity"] != runner.IdentityHostDirectory || mounts["/run/secondbox-runner-config"] == "" || mounts["/opt/secondbox-artifacts"] != runner.ArtifactHostDirectory {
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

func TestRemoteRunnerPathsSurviveEnvironmentRendering(t *testing.T) {
	runner := validTestRunner("runner-remote", "remote")
	runner.LogPath = "/remote/0"
	runner.LogDirectory = "/remote/1"
	runner.FirecrackerPath = "/remote/2"
	runner.FirecrackerJailerPath = "/remote/3"
	runner.FirecrackerJailRoot = "/remote/4"
	runner.FirecrackerKernelPath = "/remote/5"
	runner.FirecrackerRootFSPath = "/remote/6"
	runner.FirecrackerSharedImagePath = "/remote/7"
	runner.FirecrackerRunDirectory = "/remote/8"
	runner.FirecrackerLogDirectory = "/remote/9"
	runner.SnapshotTemplateCacheRoot = "/remote/10"
	runner.ArtifactPublicKey = "/remote/11"
	runner.SandboxNetworkStateDir = "/remote/12"
	runner.NetworkPolicyNFTPath = "/remote/13"
	env := resolveRunnerEnvironment(runner, "runner-credential")
	want := map[string]string{
		"SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED":    "false",
		"SECONDBOX_RUNNER_LOG_PATH":                      "/remote/0",
		"SECONDBOX_RUNNER_LOG_DIR":                       "/remote/1",
		"SECONDBOX_RUNNER_FIRECRACKER_PATH":              "/remote/2",
		"SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH":       "/remote/3",
		"SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT":         "/remote/4",
		"SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH":       "/remote/5",
		"SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH":       "/remote/6",
		"SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH": "/remote/7",
		"SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR":           "/remote/8",
		"SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR":           "/remote/9",
		"SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT":  "/remote/10",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY":           "/remote/11",
		"SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR":     "/remote/12",
		"SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH":       "/remote/13",
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("remote %s = %q, want %q", name, env[name], value)
		}
	}
}

func withoutInstalledBundleForTest(runner Runner) Runner {
	runner.FirecrackerInstalledBundle = boolean(false)
	runner.ArtifactHostDirectory = ""
	runner.ArtifactPublicKeySHA256 = ""
	for _, path := range runner.installedBundlePaths() {
		*path.field = ""
	}
	return runner
}

func TestRunnerInstalledBundleChoiceIsExplicit(t *testing.T) {
	runner := validTestRunner("runner-remote", "remote")
	runner.FirecrackerInstalledBundle = nil
	if err := validateRunner("runners[0]", runner); err == nil || !strings.Contains(err.Error(), "runners[0].firecracker_installed_bundle is required") {
		t.Fatalf("absent installed-bundle choice = %v", err)
	}
	for _, placement := range []string{"remote", "same-host"} {
		runner := validTestRunner("runner-"+placement, placement)
		if placement == "same-host" {
			runner = validSameHostTestRunner("runner-" + placement)
		}
		if err := validateRunner("runners[0]", withoutInstalledBundleForTest(runner)); err != nil {
			t.Fatalf("%s Runner without an installed bundle = %v", placement, err)
		}
	}
}

func TestRunnerWithoutInstalledBundleRejectsEveryBundleSetting(t *testing.T) {
	fields := []struct {
		name string
		set  func(*Runner)
	}{
		{"artifact_host_directory", func(r *Runner) { r.ArtifactHostDirectory = "/var/lib/secondbox/artifacts" }},
		{"artifact_public_key", func(r *Runner) { r.ArtifactPublicKey = "/opt/secondbox/manifest-public.pem" }},
		{"artifact_public_key_sha256", func(r *Runner) { r.ArtifactPublicKeySHA256 = strings.Repeat("a", 64) }},
		{"firecracker_kernel_path", func(r *Runner) { r.FirecrackerKernelPath = "/opt/secondbox/kernel" }},
		{"firecracker_rootfs_path", func(r *Runner) { r.FirecrackerRootFSPath = "/opt/secondbox/rootfs.ext4" }},
		{"firecracker_shared_image_path", func(r *Runner) { r.FirecrackerSharedImagePath = "/opt/secondbox/shared.img" }},
	}
	for _, field := range fields {
		for _, placement := range []string{"remote", "same-host"} {
			t.Run(placement+"/"+field.name, func(t *testing.T) {
				runner := validTestRunner("runner-"+placement, placement)
				if placement == "same-host" {
					runner = validSameHostTestRunner("runner-" + placement)
				}
				runner = withoutInstalledBundleForTest(runner)
				field.set(&runner)
				want := "runners[0]." + field.name + " must be omitted when firecracker_installed_bundle is false"
				if err := validateRunner("runners[0]", runner); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("bundle setting without an installed bundle = %v", err)
				}
			})
		}
	}
}

func TestRunnerEnvironmentStatesInstalledBundleChoice(t *testing.T) {
	bundleSettings := []string{
		"SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH",
		"SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH",
		"SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256",
		"SECONDBOX_RUNNER_ARTIFACT_HOST_DIR",
	}
	for _, placement := range []string{"remote", "same-host"} {
		runner := validTestRunner("runner-"+placement, placement)
		if placement == "same-host" {
			runner = validSameHostTestRunner("runner-" + placement)
		}
		installed := resolveRunnerEnvironment(runner, "runner-credential")
		if installed["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"] != "true" {
			t.Fatalf("%s installed-bundle environment = %q", placement, installed["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"])
		}
		for _, name := range bundleSettings {
			if name == "SECONDBOX_RUNNER_ARTIFACT_HOST_DIR" && placement == "remote" {
				continue
			}
			if installed[name] == "" {
				t.Errorf("%s Runner with an installed bundle omits %s", placement, name)
			}
		}
		absent := resolveRunnerEnvironment(withoutInstalledBundleForTest(runner), "runner-credential")
		if absent["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"] != "false" {
			t.Fatalf("%s bundle-less environment = %q", placement, absent["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"])
		}
		for _, name := range bundleSettings {
			if _, exists := absent[name]; exists {
				t.Errorf("%s Runner without an installed bundle renders %s", placement, name)
			}
		}
	}
}

func TestSameHostRunnerWithoutInstalledBundleSelectsNoBundleMount(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	runner := provisionSameHostTestRunner(t, manifestPath, "runner-local")
	if err := os.RemoveAll(runner.ArtifactHostDirectory); err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners[0] = withoutInstalledBundleForTest(manifest.Runners[0])
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	resolved, err := Render(manifestPath, envPath)
	if err != nil {
		t.Fatalf("same-host Runner without an installed bundle: %v", err)
	}
	for _, file := range resolved.ComposeFiles {
		if strings.HasSuffix(file, "compose.same-host-runner-installed-bundle.yml") {
			t.Fatalf("bundle-less Runner selected the installed-bundle overlay: %v", resolved.ComposeFiles)
		}
	}
	if resolved.Environment["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"] != "false" {
		t.Fatalf("rendered installed-bundle choice = %q", resolved.Environment["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"])
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose v2 is unavailable")
	}
	arguments := []string{"compose", "--project-name", "secondbox-bundle-less-test", "--env-file", envPath}
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
	if service.Environment["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"] != "false" {
		t.Fatalf("Compose installed-bundle choice = %q", service.Environment["SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE"])
	}
	if _, exists := service.Environment["SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"]; exists {
		t.Fatal("bundle-less Compose model names an installed kernel")
	}
	for _, volume := range service.Volumes {
		if volume.Target == "/opt/secondbox-artifacts" {
			t.Fatalf("bundle-less Compose model mounts an installed bundle from %s", volume.Source)
		}
	}
}
