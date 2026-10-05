package firecracker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

var installedBundleEnvironmentSettings = []string{
	"SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH",
	"SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH",
	"SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH",
	"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY",
	"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256",
}

// setRenderedRunnerEnvironment applies the deployment compiler's canonical
// Firecracker Runner environment.
func setRenderedRunnerEnvironment(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "runtimeconfig", "testdata", "runner-environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var environment map[string]string
	if err := json.Unmarshal(data, &environment); err != nil {
		t.Fatal(err)
	}
	contexts := filepath.Join(t.TempDir(), "egress-contexts.json")
	if err := os.WriteFile(contexts, []byte(`{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[{"name":"tenant-a","gateways":[{"logicalName":"agent-gateway.secondbox.internal","address":"10.210.2.2"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	environment["SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG"] = contexts
	for name, value := range environment {
		t.Setenv(name, value)
	}
	return environment
}

func unsetForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerConfigLoadsInstalledBundle(t *testing.T) {
	environment := setRenderedRunnerEnvironment(t)
	cfg, err := LoadRunnerFirecrackerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MicroVMInstalledBundle ||
		cfg.MicroVMKernelPath != environment["SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"] ||
		cfg.MicroVMPublicKeySHA256 != environment["SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"] {
		t.Fatalf("installed-bundle config = %#v", cfg)
	}
	for _, name := range installedBundleEnvironmentSettings {
		t.Run(name, func(t *testing.T) {
			unsetForTest(t, name)
			if _, err := LoadRunnerFirecrackerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "missing required "+name) {
				t.Fatalf("installed bundle without %s = %v", name, err)
			}
		})
	}
}

func TestRunnerConfigRequiresExplicitInstalledBundleChoice(t *testing.T) {
	setRenderedRunnerEnvironment(t)
	unsetForTest(t, "SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE")
	if _, err := LoadRunnerFirecrackerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "missing required SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE") {
		t.Fatalf("absent installed-bundle choice = %v", err)
	}
	t.Setenv("SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE", "sometimes")
	if _, err := LoadRunnerFirecrackerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "requires boolean SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE") {
		t.Fatalf("invalid installed-bundle choice = %v", err)
	}
}

func TestRunnerConfigLoadsWithoutInstalledBundle(t *testing.T) {
	setRenderedRunnerEnvironment(t)
	t.Setenv("SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE", "false")
	for _, name := range installedBundleEnvironmentSettings {
		unsetForTest(t, name)
	}
	cfg, err := LoadRunnerFirecrackerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MicroVMInstalledBundle || cfg.MicroVMKernelPath != "" || cfg.MicroVMRootfsPath != "" ||
		cfg.MicroVMSharedImagePath != "" || cfg.MicroVMPublicKeyPath != "" || cfg.MicroVMPublicKeySHA256 != "" {
		t.Fatalf("bundle-less config = %#v", cfg)
	}
	for _, name := range installedBundleEnvironmentSettings {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "/opt/secondbox/"+strings.ToLower(name))
			want := "must not set " + name + " when SECONDBOX_RUNNER_FIRECRACKER_INSTALLED_BUNDLE is false"
			if _, err := LoadRunnerFirecrackerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("bundle-less config with %s = %v", name, err)
			}
		})
	}
}

func TestManagerWithoutInstalledBundleRefusesBundleSettings(t *testing.T) {
	for label, set := range map[string]func(*config.Config){
		"kernel path":         func(cfg *config.Config) { cfg.MicroVMKernelPath = "/opt/secondbox/kernel" },
		"artifact public key": func(cfg *config.Config) { cfg.MicroVMPublicKeyPath = "/opt/secondbox/signing.pub" },
	} {
		t.Run(label, func(t *testing.T) {
			cfg := &config.Config{FirecrackerPath: "/bin/true", MicroVMAllowUnjailed: true}
			set(cfg)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "microVM "+label+" must be empty for a Runner without an installed execution bundle") {
				t.Fatalf("bundle-less manager with %s = %v", label, err)
			}
		})
	}
}

func TestMicroVMImageForStartWithoutInstalledBundleRequiresSelectedImage(t *testing.T) {
	manager := &Manager{cfg: &config.Config{}}
	if _, err := manager.microVMImageForStart(runtimemanager.StartOpts{RuntimeClass: runtimemanager.RuntimeClassToolExecutor}); !errors.Is(err, config.ErrNoInstalledExecutionBundle) {
		t.Fatalf("default-image start without an installed bundle = %v", err)
	}
	selected, err := manager.microVMImageForStart(runtimemanager.StartOpts{RuntimeClass: runtimemanager.RuntimeClassToolExecutor, ExecutionImageDirectory: "/var/lib/secondbox-runner/execution-images/abc"})
	if err != nil || selected.KernelPath != "/var/lib/secondbox-runner/execution-images/abc/kernel" || !selected.VerifiedExecutionImage {
		t.Fatalf("selected-image start without an installed bundle = %#v, %v", selected, err)
	}
}
