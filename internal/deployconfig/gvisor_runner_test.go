package deployconfig

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func developmentGVisorReleaseForTest(t *testing.T, manifestPath string) releasecontract.GVisorArtifact {
	t.Helper()
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), manifest.StandardResources.ArtifactManifest))
	if err != nil {
		t.Fatal(err)
	}
	release, err := releasecontract.DecodeArtifactManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if release.GVisor == nil {
		t.Skip("the development release of this host architecture has no gVisor backend")
	}
	return *release.GVisor
}

func TestSameHostGVisorRunnerRendersItsOverlayAndPackagedSettings(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	gvisorRelease := developmentGVisorReleaseForTest(t, manifestPath)
	runner := validSameHostGVisorTestRunner("runner-gvisor")
	runner.PoolID = hostStandardPool(t)
	runner = provisionSameHostRunnerForTest(t, manifestPath, runner)
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	_, runnerContent, _ := strings.Cut(string(content), "[[runners]]")
	for _, field := range []string{"control_plane_address", "gvisor_network_profile", "gvisor_runsc_path", "firecracker_path", "sandbox_bridge_name", "max_concurrent_global"} {
		if strings.Contains(runnerContent, "\n"+field+" =") {
			t.Errorf("same-host gVisor manifest carries %s", field)
		}
	}
	envPath := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	resolved, err := Render(manifestPath, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.ComposeFiles[len(resolved.ComposeFiles)-1]; !strings.HasSuffix(got, "compose.same-host-gvisor-runner.yml") {
		t.Fatalf("same-host gVisor overlay = %q", got)
	}
	want := map[string]string{
		"SECONDBOX_COMPUTE_BACKEND":                          "gvisor",
		"SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS":             "control-plane:9443",
		"SECONDBOX_GVISOR_RUNSC_PATH":                        "/opt/secondbox-gvisor/bin/runsc",
		"SECONDBOX_GVISOR_AGENT_PATH":                        "/opt/secondbox-gvisor/bin/secondbox-guest-agent",
		"SECONDBOX_GVISOR_FLAT_ROOT_PATH":                    "/opt/secondbox-gvisor/rootfs",
		"SECONDBOX_GVISOR_MATERIALIZATION_PATH":              "/opt/secondbox-gvisor/materialization.json",
		"SECONDBOX_GVISOR_MATERIALIZATION_DIGEST":            gvisorRelease.MaterializationDigest,
		"SECONDBOX_GVISOR_RUNTIME_DIR":                       "/run/secondbox-gvisor",
		"SECONDBOX_GVISOR_NETWORK_PROFILE":                   "0",
		"SECONDBOX_GVISOR_MAXIMUM_VCPUS":                     "8",
		"SECONDBOX_GVISOR_MAXIMUM_INSTANCES":                 "8",
		"SECONDBOX_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES": "8589934592",
		"SECONDBOX_RUNNER_STORAGE_ADMISSION_MODE":            "logical",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT":                    "/var/lib/secondbox-runner/workspaces",
		"SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG":             "/run/secondbox-runner-config/egress-contexts.json",
	}
	if got := resolved.Environment["SECONDBOX_GVISOR_RUNNER_CGROUP_PARENT"]; got != "secondbox-"+resolved.ComposeProject()+"-gvisor.slice" {
		t.Errorf("same-host gVisor Runner cgroup parent = %q", got)
	}
	for name, value := range want {
		if resolved.Environment[name] != value {
			t.Errorf("generated %s = %q, want %q", name, resolved.Environment[name], value)
		}
	}
	for name := range resolved.Environment {
		if strings.HasPrefix(name, "SECONDBOX_RUNNER_FIRECRACKER_") || strings.HasPrefix(name, "SECONDBOX_RUNNER_SANDBOX_") {
			t.Errorf("same-host gVisor environment carries Firecracker setting %s", name)
		}
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose v2 is unavailable")
	}
	arguments := []string{"compose", "--project-name", "secondbox-gvisor-overlay-test", "--env-file", envPath}
	for _, file := range resolved.ComposeFiles {
		arguments = append(arguments, "--file", file)
	}
	output, err := exec.Command("docker", append(arguments, "config", "--format", "json")...).Output()
	if err != nil {
		t.Fatalf("Compose config: %v", err)
	}
	var model struct {
		Services map[string]struct {
			Environment  map[string]string
			Privileged   bool
			Cgroup       string
			CgroupParent string `json:"cgroup_parent"`
			NetworkMode  string `json:"network_mode"`
			Networks     map[string]any
			Devices      []any
			Volumes      []struct{ Source, Target string }
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
	if !service.Privileged || service.Cgroup != "host" || service.NetworkMode != "" || len(service.Devices) != 0 {
		t.Fatalf("Compose gVisor Runner isolation = privileged %v, cgroup %q, network_mode %q, devices %v", service.Privileged, service.Cgroup, service.NetworkMode, service.Devices)
	}
	if service.CgroupParent != "secondbox-"+resolved.ComposeProject()+"-gvisor.slice" {
		t.Fatalf("Compose gVisor Runner cgroup parent = %q", service.CgroupParent)
	}
	if _, joined := service.Networks["secondbox-backend"]; !joined {
		t.Fatalf("Compose gVisor Runner networks = %v", service.Networks)
	}
	mounts := make(map[string]string)
	for _, volume := range service.Volumes {
		mounts[volume.Target] = volume.Source
	}
	if mounts["/var/lib/secondbox-runner"] != runner.StateHostDirectory || mounts["/run/secondbox-runner-identity"] != runner.IdentityHostDirectory || mounts["/opt/secondbox-gvisor"] != runner.ArtifactHostDirectory || mounts["/dev"] != "/dev" {
		t.Fatalf("Compose mounts differ from generated paths: %#v", mounts)
	}
}

func TestGVisorRunnerValidationOwnsBackendSettings(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		runner func() Runner
	}{
		{"missing compute backend", "compute_backend must be firecracker or gvisor", func() Runner {
			runner := validTestRunner("runner", "remote")
			runner.ComputeBackend = ""
			return runner
		}},
		{"Firecracker setting on gVisor", "firecracker_kernel_args does not apply to compute_backend gvisor", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.FirecrackerKernelArgs = "console=ttyS0"
			return runner
		}},
		{"Firecracker capacity on gVisor", "max_concurrent_global does not apply to compute_backend gvisor", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.MaxConcurrentGlobal = integer(4)
			return runner
		}},
		{"gVisor setting on Firecracker", "gvisor_maximum_vcpus does not apply to compute_backend firecracker", func() Runner {
			runner := validTestRunner("runner", "remote")
			runner.GVisorMaximumVCPUs = integer(4)
			return runner
		}},
		{"same-host control-plane address", "control_plane_address is derived for a same-host gVisor Runner", func() Runner {
			runner := validSameHostGVisorTestRunner("runner")
			runner.ControlPlaneAddress = "127.0.0.1:9443"
			return runner
		}},
		{"same-host network profile", "gvisor_network_profile is derived for a same-host gVisor Runner", func() Runner {
			runner := validSameHostGVisorTestRunner("runner")
			runner.GVisorNetworkProfile = integer(1)
			return runner
		}},
		{"same-host runsc path", "gvisor_runsc_path is derived for same-host placement", func() Runner {
			runner := validSameHostGVisorTestRunner("runner")
			runner.GVisorRunscPath = "/usr/local/bin/runsc"
			return runner
		}},
		{"remote runsc path", "gvisor_runsc_path is required", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.GVisorRunscPath = ""
			return runner
		}},
		{"unclean runtime directory", "gvisor_runtime_directory must be a clean absolute Runner-host path", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.GVisorRuntimeDirectory = "/run/secondbox-gvisor/"
			return runner
		}},
		{"remote network profile", "gvisor_network_profile must be an integer from 0 through 15", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.GVisorNetworkProfile = nil
			return runner
		}},
		{"network profile above range", "gvisor_network_profile must be an integer from 0 through 15", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.GVisorNetworkProfile = integer(16)
			return runner
		}},
		{"missing capacity", "gvisor_maximum_memory_bytes must be positive", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.GVisorMaximumMemoryBytes = nil
			return runner
		}},
		{"small workspace template", "gvisor_workspace_template_capacity_bytes must be at least 67108864", func() Runner {
			runner := validGVisorTestRunner("runner", "remote")
			runner.GVisorWorkspaceTemplateCapacityBytes = integer(1 << 20)
			return runner
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRunner("runners[0]", test.runner()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
	for name, runner := range map[string]Runner{"remote": validGVisorTestRunner("runner", "remote"), "same-host": validSameHostGVisorTestRunner("runner")} {
		if err := validateRunner("runners[0]", runner); err != nil {
			t.Fatalf("valid %s gVisor Runner: %v", name, err)
		}
	}
}

// The remote gVisor handoff renders exactly the environment that the Runner's
// production composition loads; runtimeconfig proves the other side.
func TestRemoteGVisorRunnerEnvironmentFixtureMatchesResolvedModel(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	gvisorRelease := developmentGVisorReleaseForTest(t, manifestPath)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{validGVisorTestRunner("runner-conformance", "remote")}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	environment := resolved.RemoteRunnerEnvironment["runner-conformance"]
	if environment["SECONDBOX_GVISOR_MATERIALIZATION_DIGEST"] != gvisorRelease.MaterializationDigest {
		t.Fatalf("remote gVisor materialization digest = %q, want the release pin %q", environment["SECONDBOX_GVISOR_MATERIALIZATION_DIGEST"], gvisorRelease.MaterializationDigest)
	}
	environment["SECONDBOX_RUNNER_CREDENTIAL"] = "<runner-credential>"
	environment["SECONDBOX_GVISOR_MATERIALIZATION_DIGEST"] = "<release-materialization-digest>"
	fixture, err := os.ReadFile(filepath.Join("..", "..", "runner", "internal", "runtimeconfig", "testdata", "runner-environment-gvisor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(environment, want) {
		t.Fatalf("gVisor Runner conformance fixture drifted\nresolved: %#v\nfixture: %#v", environment, want)
	}
}

func TestGVisorRunnerRequiresAReleaseWithAGVisorBackend(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	release, err := developmentReleaseManifest("arm64")
	if err != nil {
		t.Fatal(err)
	}
	encodedRelease, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(filepath.Dir(manifestPath), "arm64-artifact-manifest.json")
	if err := os.WriteFile(releasePath, encodedRelease, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest.StandardResources.ArtifactManifest = filepath.Base(releasePath)
	manifest.StandardResources.RunnerPools = []StandardRunnerPool{{Name: "standard-arm64", Architectures: []string{"arm64"}, Capabilities: manifest.StandardResources.RunnerPools[0].Capabilities, State: "ready"}}
	runner := validGVisorTestRunner("runner-arm64", "remote")
	runner.PoolID = "standard-arm64"
	manifest.Runners = []Runner{runner}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "carries no gVisor release") {
		t.Fatalf("arm64 gVisor Runner error = %v", err)
	}
}
